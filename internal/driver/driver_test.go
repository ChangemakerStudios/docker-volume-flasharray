package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/config"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/ctxlock"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/mounter"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/plugin"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/transport"
)

// --- fakes -----------------------------------------------------------------

type fakeArray struct {
	mu        sync.Mutex
	vols      map[string]*flasharray.Volume
	tags      map[string]string            // dvfa:name only
	extra     map[string]map[string]string // other dvfa tags: volume -> key -> value
	lookupErr error                        // returned by LookupNameTag
	down      error                        // array unreachable: returned by every read
	hosts     map[string][]string
	conns     map[string]map[string]int // volume -> host -> lun
	nextLUN   int
	calls     []string
}

func newFakeArray() *fakeArray {
	return &fakeArray{vols: map[string]*flasharray.Volume{}, tags: map[string]string{}, extra: map[string]map[string]string{}, hosts: map[string][]string{}, conns: map[string]map[string]int{}, nextLUN: 1}
}

func (f *fakeArray) rec(s string) { f.calls = append(f.calls, s) }

func (f *fakeArray) GetVolume(_ context.Context, name string) (*flasharray.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return nil, f.down
	}
	v, ok := f.vols[name]
	if !ok {
		return nil, flasharray.ErrNotFound
	}
	c := *v
	return &c, nil
}

func (f *fakeArray) CreateVolume(_ context.Context, name string, size int64) (*flasharray.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("create:" + name)
	v := &flasharray.Volume{Name: name, Serial: fmt.Sprintf("%024x", len(f.vols)+1), Provisioned: size, Created: time.Now()}
	f.vols[name] = v
	return v, nil
}

func (f *fakeArray) CopyVolume(_ context.Context, source, name string) (*flasharray.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("copy:" + source + ":" + name)
	src, ok := f.vols[source]
	if !ok {
		return nil, flasharray.ErrNotFound
	}
	v := &flasharray.Volume{Name: name, Serial: fmt.Sprintf("%024x", len(f.vols)+1), Provisioned: src.Provisioned, Created: time.Now()}
	f.vols[name] = v
	return v, nil
}

func (f *fakeArray) SetTag(_ context.Context, volume, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.extra[volume] == nil {
		f.extra[volume] = map[string]string{}
	}
	f.extra[volume][key] = value
	return nil
}

func (f *fakeArray) GetTags(_ context.Context, volume string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.extra[volume] {
		out[k] = v
	}
	if v, ok := f.tags[volume]; ok {
		out[flasharray.TagKeyName] = v
	}
	return out, nil
}

func (f *fakeArray) DeleteTag(_ context.Context, volume, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.extra[volume], key)
	return nil
}

func (f *fakeArray) RenameVolume(_ context.Context, name, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("rename:" + name + ":" + newName)
	v, ok := f.vols[name]
	if !ok {
		return flasharray.ErrNotFound
	}
	if _, taken := f.vols[newName]; taken {
		return errors.New("PATCH /volumes: HTTP 400: volume already exists")
	}
	v.Name = newName
	f.vols[newName] = v
	delete(f.vols, name)
	if t, ok := f.tags[name]; ok {
		f.tags[newName] = t
		delete(f.tags, name)
	}
	if c, ok := f.conns[name]; ok {
		f.conns[newName] = c
		delete(f.conns, name)
	}
	return nil
}

func (f *fakeArray) DestroyVolume(_ context.Context, name string, eradicate bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec(fmt.Sprintf("destroy:%s:%v", name, eradicate))
	v, ok := f.vols[name]
	if !ok {
		return flasharray.ErrNotFound
	}
	v.Destroyed = true
	if eradicate {
		delete(f.vols, name)
		delete(f.tags, name)
	}
	return nil
}

func (f *fakeArray) SetNameTag(_ context.Context, volume, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[volume] = value
	return nil
}

func (f *fakeArray) ListNameTags(_ context.Context, prefix string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return nil, f.down
	}
	out := map[string]string{}
	for k, v := range f.tags {
		if strings.HasPrefix(v, prefix) && !f.destroyed(k) {
			out[k] = v
		}
	}
	return out, nil
}

func (f *fakeArray) destroyed(name string) bool {
	v, ok := f.vols[name]
	return ok && v.Destroyed
}

func (f *fakeArray) LookupNameTag(_ context.Context, value string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return "", f.lookupErr
	}
	if f.down != nil {
		return "", f.down
	}
	for an, v := range f.tags {
		if v == value && !f.destroyed(an) {
			return an, nil
		}
	}
	return "", flasharray.ErrNotFound
}

func (f *fakeArray) ListVolumes(_ context.Context, prefix string) ([]*flasharray.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*flasharray.Volume
	for name, v := range f.vols {
		if strings.HasPrefix(name, prefix) && !v.Destroyed {
			c := *v
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeArray) EnsureHost(_ context.Context, name string, iqns, nqns []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[name] = append(iqns, nqns...)
	return name, nil
}

func (f *fakeArray) Connect(_ context.Context, host, volume string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("connect:" + host + ":" + volume)
	if _, ok := f.vols[volume]; !ok {
		return 0, flasharray.ErrNotFound
	}
	if f.conns[volume] == nil {
		f.conns[volume] = map[string]int{}
	}
	if lun, ok := f.conns[volume][host]; ok {
		return lun, nil
	}
	f.conns[volume][host] = f.nextLUN
	f.nextLUN++
	return f.conns[volume][host], nil
}

func (f *fakeArray) Disconnect(_ context.Context, host, volume string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("disconnect:" + host + ":" + volume)
	delete(f.conns[volume], host)
	return nil
}

func (f *fakeArray) HostLUNs(_ context.Context, host string) (map[int]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.hosts[host]; !ok {
		return nil, fmt.Errorf("GET /connections: %w (%s: Host does not exist.)", flasharray.ErrNotFound, host)
	}
	out := map[int]string{}
	for vol, hosts := range f.conns {
		if lun, ok := hosts[host]; ok {
			out[lun] = strings.ToLower(f.vols[vol].Serial)
		}
	}
	return out, nil
}
func (f *fakeArray) ListConnections(_ context.Context, volume string) ([]flasharray.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.vols[volume]; ok && v.Destroyed {
		// What the array answers for a volume in the destroyed state.
		return nil, errors.New("GET /connections: HTTP 400: " + volume + ": Volume has been destroyed.")
	}
	var out []flasharray.Connection
	for h, lun := range f.conns[volume] {
		out = append(out, flasharray.Connection{Host: h, Volume: volume, LUN: lun})
	}
	return out, nil
}

func (f *fakeArray) Ports(context.Context) ([]flasharray.Port, error) {
	return []flasharray.Port{{Name: "CT0.ETH4", IQN: "iqn.2010-06.com.purestorage:flasharray.test", Portal: "198.51.100.10:3260"}}, nil
}

type fakeTransport struct {
	excl      ctxlock.Mutex
	mu        sync.Mutex
	connects  int
	waits     []string
	detaches  []string
	failWait  bool
	devs      map[string]string // serial -> device, overriding the default path
	sweeps    []sweepCall
	lunErr    error           // returned by CheckLUN
	detachErr error           // returned by Detach
	present   map[string]bool // serial -> device exists on this node (attached, or a planted leftover)
	// lunErrAfter > 0 makes CheckLUN pass that many times before returning lunErr.
	lunErrAfter int
	lunChecks   int
	sweepErr    error // returned by Sweep
}

func (t *fakeTransport) dev(serial string) string {
	if dev, ok := t.devs[serial]; ok {
		return dev
	}
	return "/dev/mapper/3624a9370" + serial
}

func (t *fakeTransport) setPresent(serial string, on bool) {
	if t.present == nil {
		t.present = map[string]bool{}
	}
	if on {
		t.present[serial] = true
	} else {
		delete(t.present, serial)
	}
}

func (t *fakeTransport) Name() string { return "fake" }
func (t *fakeTransport) InitiatorIDs() ([]string, []string, error) {
	return []string{"iqn.1993-08.org.debian:01:test"}, nil, nil
}
func (t *fakeTransport) Connect(context.Context, []flasharray.Port) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connects++
	return nil
}
func (t *fakeTransport) WaitForDevice(_ context.Context, serial string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.waits = append(t.waits, serial)
	if t.failWait {
		return "", errors.New("no device")
	}
	t.setPresent(serial, true)
	return t.dev(serial), nil
}
func (t *fakeTransport) FindDevice(_ context.Context, serial string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.present[serial] {
		return ""
	}
	return t.dev(serial)
}
func (t *fakeTransport) Detach(_ context.Context, serial, _ string, _ map[int]string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.detaches = append(t.detaches, serial)
	if t.detachErr != nil {
		return t.detachErr
	}
	t.setPresent(serial, false)
	return nil
}
func (t *fakeTransport) PostDisconnect(context.Context) error { return nil }
func (t *fakeTransport) Sweep(_ context.Context, want map[int]string, prefix string, own int, crossedOnly bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweeps = append(t.sweeps, sweepCall{want, prefix, own, crossedOnly})
	return t.sweepErr
}
func (t *fakeTransport) Exclusive(ctx context.Context, fn func() error) error {
	if err := t.excl.Lock(ctx); err != nil {
		return err
	}
	defer t.excl.Unlock()
	return fn()
}

type sweepCall struct {
	want        map[int]string
	prefix      string
	own         int
	crossedOnly bool
}

func (t *fakeTransport) CheckLUN(context.Context, string, int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lunChecks++
	if t.lunChecks <= t.lunErrAfter {
		return nil
	}
	return t.lunErr
}

type fakeMounter struct {
	mu        sync.Mutex
	formatted map[string]bool
	mounted   map[string]string // target -> dev
	opts      map[string]string // target -> mount options
	busy      map[string]int    // dev -> Busy calls left that report busy; <0 = forever
	inUse     map[string]bool   // mountpoint -> a container namespace still has it
	syncs     []string          // mountpoints Sync was called for
	shutdowns []string          // mountpoints Shutdown was called for
	// inUseChecks counts InUseByContainer calls.
	inUseChecks int
}

func (m *fakeMounter) Shutdown(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdowns = append(m.shutdowns, target)
	return nil
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{formatted: map[string]bool{}, mounted: map[string]string{}, opts: map[string]string{}, busy: map[string]int{}, inUse: map[string]bool{}}
}
func (m *fakeMounter) Busy(dev string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.busy[dev]
	if n > 0 {
		m.busy[dev] = n - 1
	}
	return n != 0, nil
}
func (m *fakeMounter) EnsureFilesystem(_ context.Context, dev, _ string, _ []string, allowFormat bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.formatted[dev] {
		return false, nil
	}
	if !allowFormat {
		return false, fmt.Errorf("%s: %w", dev, mounter.ErrNoFilesystem)
	}
	m.formatted[dev] = true
	return true, nil
}
func (m *fakeMounter) MountedAt(dev string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for target, d := range m.mounted {
		if d == dev {
			return target, nil
		}
	}
	return "", nil
}
func (m *fakeMounter) Sync(_ context.Context, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncs = append(m.syncs, target)
	return nil
}
func (m *fakeMounter) InUseByContainer(mountpoint string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inUseChecks++
	return m.inUse[mountpoint], nil
}
func (m *fakeMounter) Mount(_ context.Context, dev, target, _, opts string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = dev
	m.opts[target] = opts
	return nil
}
func (m *fakeMounter) Unmount(_ context.Context, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounted, target)
	return nil
}
func (m *fakeMounter) IsMounted(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mounted[target]
	return ok, nil
}

// --- helpers ---------------------------------------------------------------

func newTestDriver(t *testing.T, fa *fakeArray, tr *fakeTransport, fm *fakeMounter) *Driver {
	t.Helper()
	cfg := &config.Config{
		Namespace:     "node1",
		HostName:      "node1",
		Transport:     config.TransportISCSI,
		DefaultSize:   32 << 30,
		FSType:        "xfs",
		MountRoot:     t.TempDir(),
		PreemptRWO:    true,
		AttachTimeout: 5 * time.Second,
	}
	d, err := New(context.Background(), cfg, Deps{Array: fa, Transport: tr, Mounter: fm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// --- tests -----------------------------------------------------------------

func TestArrayName(t *testing.T) {
	d := &Driver{cfg: &config.Config{Namespace: "node1"}}
	cases := map[string]struct {
		want       string
		hashSuffix bool
	}{
		"db-1":                  {want: "node1-db-1"},
		"app_store":             {want: "node1-app_store"},
		"seq.store":             {hashSuffix: true},
		"seq-store":             {want: "node1-seq-store"},
		"a_b":                   {want: "node1-a_b"},
		"a-b":                   {want: "node1-a-b"},
		"CamelCase123":          {want: "node1-CamelCase123"},
		strings.Repeat("x", 80): {hashSuffix: true},
	}
	seen := map[string]string{}
	for in, c := range cases {
		got := d.arrayName(in)
		if len(got) > 63 {
			t.Errorf("%q -> %q exceeds 63 chars", in, got)
		}
		if c.hashSuffix {
			if !strings.HasPrefix(got, "node1-") || len(got) < 14 || got[len(got)-9] != '-' {
				t.Errorf("%q -> %q: expected hash suffix", in, got)
			}
		} else if got != c.want {
			t.Errorf("%q -> %q, want %q", in, got, c.want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %q and %q both map to %q", prev, in, got)
		}
		seen[got] = in
	}
}

func TestCreateMountUnmountRemove(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)

	if err := d.Create(&plugin.CreateRequest{Name: "data_vol", Options: map[string]string{"size": "10GiB"}}); err != nil {
		t.Fatal(err)
	}
	an := d.arrayName("data_vol")
	if v := fa.vols[an]; v == nil || v.Provisioned != 10<<30 {
		t.Fatalf("volume not created with 10GiB: %+v", v)
	}
	if fa.tags[an] != "node1/data_vol" {
		t.Fatalf("tag = %q", fa.tags[an])
	}
	// idempotent create
	if err := d.Create(&plugin.CreateRequest{Name: "data_vol"}); err != nil {
		t.Fatal(err)
	}

	// two containers mount, one unmounts -> still mounted
	r1, err := d.Mount(&plugin.MountRequest{Name: "data_vol", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := d.Mount(&plugin.MountRequest{Name: "data_vol", ID: "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Mountpoint != r2.Mountpoint || filepath.Base(r1.Mountpoint) != "data_vol" {
		t.Fatalf("mountpoints %q %q", r1.Mountpoint, r2.Mountpoint)
	}
	if len(tr.waits) != 1 {
		t.Fatalf("expected one attach, got %d", len(tr.waits))
	}
	if ok, _ := fm.IsMounted(r1.Mountpoint); !ok {
		t.Fatal("not mounted")
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "data_vol", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fm.IsMounted(r1.Mountpoint); !ok {
		t.Fatal("unmounted too early")
	}
	if err := d.Remove(&plugin.RemoveRequest{Name: "data_vol"}); err == nil {
		t.Fatal("remove of mounted volume should fail")
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "data_vol", ID: "c2"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fm.IsMounted(r1.Mountpoint); ok {
		t.Fatal("still mounted")
	}
	if len(fa.conns[an]) != 0 {
		t.Fatalf("array connection not removed: %v", fa.conns[an])
	}
	if len(tr.detaches) != 1 {
		t.Fatalf("detach not called")
	}

	list, err := d.List()
	if err != nil || len(list.Volumes) != 1 || list.Volumes[0].Name != "data_vol" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	g, err := d.Get(&plugin.GetRequest{Name: "data_vol"})
	if err != nil || g.Volume.Status["array_name"] != an {
		t.Fatalf("get = %+v, %v", g, err)
	}

	serial := fa.vols[an].Serial
	if err := d.Remove(&plugin.RemoveRequest{Name: "data_vol"}); err != nil {
		t.Fatal(err)
	}
	var removed *flasharray.Volume
	for _, v := range fa.vols {
		if v.Serial == serial {
			removed = v
		}
	}
	if removed == nil || !removed.Destroyed {
		t.Fatalf("expected destroyed (not eradicated): %+v", removed)
	}
}

func TestMountPreemptsOtherHost(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	fa.conns[an] = map[string]int{"dead-node": 7}

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, still := fa.conns[an]["dead-node"]; still {
		t.Fatal("dead-node connection should have been preempted")
	}
	if _, ok := fa.conns[an]["node1"]; !ok {
		t.Fatal("node1 not connected")
	}

	// and refuses when preemption is disabled
	d.cfg.PreemptRWO = false
	_ = d.Create(&plugin.CreateRequest{Name: "w"})
	fa.conns[d.arrayName("w")] = map[string]int{"other": 1}
	if _, err := d.Mount(&plugin.MountRequest{Name: "w", ID: "c"}); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestMountWaitsForOtherHostToRelease(t *testing.T) {
	releasePoll, maxReleaseWait = time.Millisecond, 2*time.Second
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	fa.conns[an] = map[string]int{"draining-node": 7}
	go func() {
		time.Sleep(20 * time.Millisecond) // its container stops and Unmount disconnects
		fa.mu.Lock()
		delete(fa.conns[an], "draining-node")
		fa.mu.Unlock()
	}()

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range fa.calls {
		if c == "disconnect:draining-node:"+an {
			t.Fatal("preempted a host that released the volume on its own")
		}
	}
}

func TestMountPreemptsOnlyAfterGrace(t *testing.T) {
	releasePoll, maxReleaseWait = time.Millisecond, 5*time.Millisecond
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	fa.conns[an] = map[string]int{"dead-node": 7}

	_, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err == nil || !strings.Contains(err.Error(), "waiting up to") {
		t.Fatalf("expected to wait for dead-node, got %v", err)
	}
	if _, still := fa.conns[an]["dead-node"]; !still {
		t.Fatal("preempted before the grace period")
	}

	d.heldSince[an] = holdState{hosts: "dead-node", since: time.Now().Add(-2 * time.Hour), seen: time.Now()} // Docker kept retrying past the grace
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, still := fa.conns[an]["dead-node"]; still {
		t.Fatal("dead-node not preempted after the grace period")
	}
	if _, held := d.heldSince[an]; held {
		t.Fatal("grace timer not cleared after takeover")
	}
}

// The grace belongs to the holder: a host that only just took the volume is
// live and must get the full grace, however long an earlier holder kept it.
func TestPreemptGraceRestartsWhenHolderChanges(t *testing.T) {
	releasePoll, maxReleaseWait = time.Millisecond, 5*time.Millisecond
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	fa.conns[an] = map[string]int{"new-holder": 7}
	d.heldSince[an] = holdState{hosts: "old-holder", since: time.Now().Add(-2 * time.Hour), seen: time.Now()}

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil || !strings.Contains(err.Error(), "waiting up to") {
		t.Fatalf("expected to wait for new-holder, got %v", err)
	}
	if _, still := fa.conns[an]["new-holder"]; !still {
		t.Fatal("preempted a new holder on the old holder's grace")
	}
}

// Every attach sweeps stale paths before connecting, so no leftover sits on a
// LUN the connect hands out, and after, with a LUN map that includes its own.
func TestMountSweepsStalePathsAroundConnect(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	lun := fa.conns[an]["node1"]
	serial := strings.ToLower(fa.vols[an].Serial)
	if len(tr.sweeps) != 2 {
		t.Fatalf("sweeps = %+v, want one before and one after the connect", tr.sweeps)
	}
	before, after := tr.sweeps[0], tr.sweeps[1]
	if _, ok := before.want[lun]; ok || before.own != -1 || before.crossedOnly {
		t.Fatalf("pre-connect sweep = %+v", before)
	}
	if after.want[lun] != serial || after.own != lun || after.crossedOnly || !strings.HasPrefix(serial, after.prefix) {
		t.Fatalf("post-connect sweep = %+v, want one covering LUN %d scoped to this array", after, lun)
	}
}

// The background check may only cut a path the array positively maps to
// another volume.
func TestPathCheckSweepsCrossedPathsOnly(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.checkPaths(context.Background())
	if len(tr.sweeps) != 0 {
		t.Fatalf("swept before the array prefix was known: %+v", tr.sweeps)
	}
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	n := len(tr.sweeps)
	d.checkPaths(context.Background())
	if len(tr.sweeps) != n+1 {
		t.Fatal("path check did not sweep")
	}
	if c := tr.sweeps[n]; !c.crossedOnly || c.own != -1 || c.prefix == "" {
		t.Fatalf("path check sweep = %+v, want crossedOnly with no own LUN", c)
	}
}

func TestMountRefusesPathsOnAnotherLUN(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	tr.lunErr = errors.New("path sdq is not on LUN 3, where the array connected the volume")

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil || !strings.Contains(err.Error(), "not on LUN") {
		t.Fatalf("expected a wrong-LUN refusal, got %v", err)
	}
	if len(fm.mounted) != 0 {
		t.Fatal("mounted a device with a path on another LUN")
	}
	if len(tr.detaches) != 1 || len(fa.conns[an]) != 0 {
		t.Fatalf("not rolled back: detach=%v conns=%v", tr.detaches, fa.conns[an])
	}
}

// Disconnecting under paths the host keeps frees their LUN number for the next
// volume, which those paths then reach.
func TestRollbackKeepsConnectionWhenHostDetachFails(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	tr.lunErr = errors.New("wrong LUN")
	tr.detachErr = errors.New("multipath map mpathq is still open (1)")

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected the mount to fail")
	}
	if _, ok := fa.conns[an]["node1"]; !ok {
		t.Fatal("disconnected on the array although the host still has the device")
	}
}

func TestUnmountKeepsConnectionWhenHostDetachFails(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	tr.detachErr = errors.New("multipath map mpathq is still open (1)")
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected the detach failure")
	}
	if _, ok := fa.conns[an]["node1"]; !ok {
		t.Fatal("disconnected on the array although the host still has the device")
	}
}

// A connection left behind must not be dropped under the host's paths: their
// LUN would go to the next volume connected.
func TestRemoveDetachesHostBeforeDisconnect(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	serial := fa.vols[an].Serial
	fa.conns[an] = map[string]int{"node1": 4} // leaked by an earlier attach

	tr.detachErr = errors.New("multipath map mpathq is still open (1)")
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err == nil {
		t.Fatal("expected the detach failure")
	}
	if _, ok := fa.conns[an]["node1"]; !ok {
		t.Fatal("disconnected although the host detach failed")
	}

	tr.detachErr = nil
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	if n := len(tr.detaches); n == 0 || tr.detaches[n-1] != serial {
		t.Fatalf("host side not detached before disconnect: %v", tr.detaches)
	}
	if len(fa.conns[an]) != 0 {
		t.Fatalf("connection not removed: %v", fa.conns[an])
	}
}

func TestMountRemovesIdleLeftoverBeforeReconnecting(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	serial := fa.vols[d.arrayName("v")].Serial
	tr.setPresent(serial, true) // a map left behind when the volume was taken away

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(tr.detaches) != 1 || tr.detaches[0] != serial {
		t.Fatalf("leftover not removed before reconnecting: %v", tr.detaches)
	}
}

// A stale mount still holding the leftover would revive on reconnect and
// write old metadata over the volume.
func TestMountRefusesWhileStaleMountHoldsLeftover(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	serial := fa.vols[an].Serial
	tr.setPresent(serial, true)
	fm.busy[tr.dev(serial)] = -1

	_, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err == nil || !strings.Contains(err.Error(), "stale mount") {
		t.Fatalf("expected refusal over a held leftover, got %v", err)
	}
	if len(fa.conns[an]) != 0 || len(tr.detaches) != 0 {
		t.Fatalf("connected or detached under a stale mount: conns=%v detach=%v", fa.conns[an], tr.detaches)
	}
}

// Still connected here (a container started before a plugin restart): the
// device is in legitimate use and must be left alone.
func TestMountKeepsDeviceOfVolumeStillConnectedHere(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	// Plugin restarted under the running container: its own mount table and
	// state are gone, the container's namespace still holds the filesystem.
	d.mounts = map[string]*mountState{}
	delete(fm.mounted, r.Mountpoint)
	fm.busy[tr.dev(fa.vols[an].Serial)] = -1

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c2"}); err != nil {
		t.Fatal(err)
	}
	if len(tr.detaches) != 0 {
		t.Fatalf("detached a device in use on this node: %v", tr.detaches)
	}
}

// docker volume rm must free the name at once: the destroyed volume moves
// aside and stays recoverable, and the name can be created again.
func TestRemoveFreesNameForRecreate(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	name := d.arrayName("v")
	oldSerial := fa.vols[name].Serial
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	var parked *flasharray.Volume
	for n, v := range fa.vols {
		if v.Serial == oldSerial {
			parked = v
			if n == name || !strings.HasPrefix(n, name+"-") || !v.Destroyed {
				t.Fatalf("removed volume should be destroyed under a new name: %s %+v", n, v)
			}
		}
	}
	if parked == nil {
		t.Fatal("removed volume disappeared instead of staying recoverable")
	}
	if _, err := d.Get(&plugin.GetRequest{Name: "v"}); err == nil {
		t.Fatal("Get still answered for the removed volume")
	}

	if err := d.Create(&plugin.CreateRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	g, err := d.Get(&plugin.GetRequest{Name: "v"})
	if err != nil || g.Volume.Status["array_name"] != name || g.Volume.Status["serial"] == oldSerial {
		t.Fatalf("expected a new volume under %s, got %+v, %v", name, g, err)
	}
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatalf("mount of the recreated volume: %v", err)
	}
}

// Destroyed by other means (the array UI): Get keeps answering, so a stack's
// external volume fails loudly instead of Docker creating an empty local one.
// An explicit create still gets a fresh volume beside it.
func TestVolumeDestroyedOutsideDriverStaysVisible(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	name := d.arrayName("v")
	fa.vols[name].Destroyed = true

	g, err := d.Get(&plugin.GetRequest{Name: "v"})
	if err != nil || g.Volume.Status["destroyed"] != true {
		t.Fatalf("Get should answer destroyed=true, got %+v, %v", g, err)
	}
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("mount should fail loudly, got %v", err)
	}

	if err := d.Create(&plugin.CreateRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	g, err = d.Get(&plugin.GetRequest{Name: "v"})
	if err != nil || g.Volume.Status["destroyed"] != false || !strings.HasPrefix(g.Volume.Status["array_name"].(string), name+"-") {
		t.Fatalf("expected a fresh volume beside the destroyed one, got %+v, %v", g, err)
	}
	if !fa.vols[name].Destroyed {
		t.Fatal("destroyed volume should be left alone")
	}
}

func TestMountRollbackWhenDeviceNeverAppears(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{failWait: true}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected error")
	}
	if len(fa.conns[an]) != 0 {
		t.Fatalf("array connection leaked after failed attach: %v", fa.conns[an])
	}
	if len(d.mounts) != 0 {
		t.Fatal("mount state leaked")
	}
}

func TestMountRefusesToFormatWrittenVolume(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	fa.vols[an].Written = 4096 // blkid sees nothing, but data has been written

	_, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err == nil || !errors.Is(err, mounter.ErrNoFilesystem) || !strings.Contains(err.Error(), "refusing to format") {
		t.Fatalf("expected refusal to format, got %v", err)
	}
	if len(fm.formatted) != 0 {
		t.Fatalf("formatted a written volume: %v", fm.formatted)
	}
	if len(fa.conns[an]) != 0 || len(d.mounts) != 0 {
		t.Fatalf("attach not rolled back: conns=%v mounts=%v", fa.conns[an], d.mounts)
	}

	fa.vols[an].Written = 0
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatalf("never-written volume should be formatted and mounted: %v", err)
	}
	if len(fm.formatted) != 1 {
		t.Fatalf("expected one format, got %v", fm.formatted)
	}
}

func TestMountRefusesDeviceAlreadyMountedOnNode(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{devs: map[string]string{}}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "a"})
	_ = d.Create(&plugin.CreateRequest{Name: "b"})
	ra, err := d.Mount(&plugin.MountRequest{Name: "a", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	devA := fm.mounted[ra.Mountpoint]
	tr.devs[fa.vols[d.arrayName("b")].Serial] = devA

	_, err = d.Mount(&plugin.MountRequest{Name: "b", ID: "c2"})
	if err == nil || !strings.Contains(err.Error(), "already mounted at "+ra.Mountpoint) {
		t.Fatalf("expected refusal naming %s, got %v", ra.Mountpoint, err)
	}
	if fm.mounted[ra.Mountpoint] != devA {
		t.Fatal("existing mount was disturbed")
	}
	if _, ok := fa.conns[d.arrayName("a")]["node1"]; !ok {
		t.Fatal("volume a lost its array connection")
	}
	if len(tr.detaches) != 0 {
		t.Fatalf("detached a device that is still mounted: %v", tr.detaches)
	}
}

func TestUnmountWaitsForKernelToReleaseDevice(t *testing.T) {
	idlePoll = time.Millisecond
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	dev := fm.mounted[r.Mountpoint]
	fm.busy[dev] = 3 // a container namespace still flushing the filesystem

	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if fm.busy[dev] != 0 {
		t.Fatalf("detached before the device was released (%d busy polls left)", fm.busy[dev])
	}
	if len(tr.detaches) != 1 || len(fa.conns[an]) != 0 {
		t.Fatalf("detach=%v conns=%v", tr.detaches, fa.conns[an])
	}
}

func TestUnmountKeepsConnectionWhileDeviceStaysBusy(t *testing.T) {
	idlePoll = time.Millisecond
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.AttachTimeout = 20 * time.Millisecond
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	fm.busy[fm.mounted[r.Mountpoint]] = -1

	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("expected still-in-use error, got %v", err)
	}
	if len(tr.detaches) != 0 {
		t.Fatalf("detached a busy device: %v", tr.detaches)
	}
	if _, ok := fa.conns[an]["node1"]; !ok {
		t.Fatal("array connection removed under a busy device")
	}
}

// Docker sent Mounts it never unmounted; trusting their refs left the volume
// mounted after its last container stopped, and the next node preempted it.
func TestUnmountDropsRefsNoContainerUses(t *testing.T) {
	refPending = 0
	t.Cleanup(func() { refPending = 2 * time.Minute })
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "leaked"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}

	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, still := fm.mounted[r.Mountpoint]; still {
		t.Fatal("kept the volume mounted for a ref no container uses")
	}
	if len(tr.detaches) != 1 || len(fa.conns[an]) != 0 {
		t.Fatalf("not detached: detach=%v conns=%v", tr.detaches, fa.conns[an])
	}
}

func TestUnmountKeepsVolumeAContainerStillUses(t *testing.T) {
	refPending = 0
	t.Cleanup(func() { refPending = 2 * time.Minute })
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "b"}); err != nil {
		t.Fatal(err)
	}
	fm.inUse[r.Mountpoint] = true

	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, still := fm.mounted[r.Mountpoint]; !still || len(tr.detaches) != 0 {
		t.Fatal("unmounted a volume another container still uses")
	}
}

// A ref Docker just added may be a container that has not started yet, so
// its namespace can't show the mount.
func TestUnmountKeepsYoungRefs(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "starting"}); err != nil {
		t.Fatal(err)
	}

	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, still := fm.mounted[r.Mountpoint]; !still || len(tr.detaches) != 0 {
		t.Fatal("unmounted under a container that is still starting")
	}
	if fm.inUseChecks != 0 {
		t.Fatal("young ref should keep the mount without asking the kernel")
	}
}

// After a plugin restart the driver no longer tracks mounts that running
// containers still hold; Unmount must not disconnect under them.
func TestUntrackedUnmountWaitsThenDetaches(t *testing.T) {
	idlePoll = time.Millisecond
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	d.mounts = map[string]*mountState{} // plugin restarted
	dev := tr.FindDevice(context.Background(), fa.vols[an].Serial)

	fm.busy[dev] = -1
	d.cfg.AttachTimeout = 20 * time.Millisecond
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("untracked unmount disconnected under a busy device")
	}
	if _, ok := fa.conns[an]["node1"]; !ok || len(tr.detaches) != 0 {
		t.Fatalf("busy device was torn down: conns=%v detach=%v", fa.conns[an], tr.detaches)
	}

	fm.busy[dev] = 2
	d.cfg.AttachTimeout = 5 * time.Second
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(tr.detaches) != 1 || len(fa.conns[an]) != 0 {
		t.Fatalf("released device not detached: conns=%v detach=%v", fa.conns[an], tr.detaches)
	}
}

func TestRemoveRefusesWhenAttachedElsewhere(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	fa.conns[d.arrayName("v")] = map[string]int{"node2": 3}
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err == nil || !strings.Contains(err.Error(), "node2") {
		t.Fatalf("expected refusal naming node2, got %v", err)
	}
}

func TestCreateRejectsUnknownOption(t *testing.T) {
	d := newTestDriver(t, newFakeArray(), &fakeTransport{}, newFakeMounter())
	if err := d.Create(&plugin.CreateRequest{Name: "v", Options: map[string]string{"nope": "1"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestImportAdoptsExistingArrayVolume(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	// A volume the old plugin created under its own naming scheme.
	old, _ := fa.CreateVolume(context.Background(), "prod-app_data", 32<<30)

	if err := d.Create(&plugin.CreateRequest{Name: "app_data", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatal(err)
	}
	if fa.tags[old.Name] != "node1/app_data" {
		t.Fatalf("tag = %q", fa.tags[old.Name])
	}
	if len(fa.vols) != 1 {
		t.Fatalf("import must not create a new volume: %v", fa.calls)
	}
	// Every subsequent op resolves through the tag to the old array name.
	r, err := d.Mount(&plugin.MountRequest{Name: "app_data", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fa.conns[old.Name]["node1"]; !ok {
		t.Fatalf("mount attached the wrong volume: %v", fa.conns)
	}
	if tr.waits[0] != old.Serial {
		t.Fatalf("waited for serial %s, want %s", tr.waits[0], old.Serial)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "app_data", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_ = r
	g, err := d.Get(&plugin.GetRequest{Name: "app_data"})
	if err != nil || g.Volume.Status["array_name"] != old.Name {
		t.Fatalf("get = %+v %v", g, err)
	}
	// Idempotent re-import; conflicting re-import refused.
	if err := d.Create(&plugin.CreateRequest{Name: "app_data", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	other, _ := fa.CreateVolume(context.Background(), "prod-other", 1<<30)
	if err := d.Create(&plugin.CreateRequest{Name: "app_data", Options: map[string]string{"import": other.Name}}); err == nil {
		t.Fatal("expected conflict importing a second volume under the same docker name")
	}
	if err := d.Create(&plugin.CreateRequest{Name: "second", Options: map[string]string{"import": old.Name}}); err == nil {
		t.Fatal("expected conflict importing an already-claimed volume under another name")
	}
	// Remove goes to the adopted array volume, not the computed name.
	if err := d.Remove(&plugin.RemoveRequest{Name: "app_data"}); err != nil {
		t.Fatal(err)
	}
	if !fa.vols[old.Name].Destroyed {
		t.Fatal("remove did not destroy the adopted volume")
	}
}

func TestStaleCachedNameIsResolvedAgain(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	if err := d.Create(&plugin.CreateRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	// Another node removes and eradicates it, then imports a different volume under the same name.
	fa.mu.Lock()
	delete(fa.vols, "node1-v")
	delete(fa.tags, "node1-v")
	fa.mu.Unlock()
	repl, _ := fa.CreateVolume(context.Background(), "legacy-v", 1<<30)
	_ = fa.SetNameTag(context.Background(), repl.Name, "node1/v")

	g, err := d.Get(&plugin.GetRequest{Name: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Volume.Status["array_name"] != repl.Name {
		t.Fatalf("array_name = %v, want %s", g.Volume.Status["array_name"], repl.Name)
	}
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fa.conns[repl.Name]["node1"]; !ok {
		t.Fatalf("mount attached the wrong volume: %v", fa.conns)
	}
}

func TestRecoveredMountOfAdoptedVolumeUnmounts(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	old, _ := fa.CreateVolume(context.Background(), "prod-app_data", 1<<30)
	if err := d.Create(&plugin.CreateRequest{Name: "app_data", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatal(err)
	}
	r, err := d.Mount(&plugin.MountRequest{Name: "app_data", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.Mountpoint, 0o755); err != nil { // the fake mounter doesn't create it
		t.Fatal(err)
	}

	// Plugin restart: a fresh driver must key the recovered mount by the tagged array name.
	d2, err := New(context.Background(), d.cfg, Deps{Array: fa, Transport: tr, Mounter: fm})
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.Unmount(&plugin.UnmountRequest{Name: "app_data", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fm.IsMounted(r.Mountpoint); ok {
		t.Fatal("recovered mount was not unmounted")
	}
	if n := len(tr.detaches); n == 0 || tr.detaches[n-1] != old.Serial {
		t.Fatalf("detaches = %v, want last %s", tr.detaches, old.Serial)
	}
}

func TestCloneFromSource(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	if err := d.Create(&plugin.CreateRequest{Name: "pg", Options: map[string]string{"size": "10GiB"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(&plugin.CreateRequest{Name: "pg_copy", Options: map[string]string{"source": "pg"}}); err != nil {
		t.Fatal(err)
	}
	an := d.arrayName("pg_copy")
	c := fa.vols[an]
	if c == nil || c.Provisioned != 10<<30 {
		t.Fatalf("clone = %+v", c)
	}
	if fa.tags[an] != "node1/pg_copy" {
		t.Fatalf("name tag = %q", fa.tags[an])
	}
	// The clone carries its source's filesystem UUID; nouuid mounts both side by side.
	for _, name := range []string{"pg", "pg_copy"} {
		r, err := d.Mount(&plugin.MountRequest{Name: name, ID: "c-" + name})
		if err != nil {
			t.Fatal(err)
		}
		if fm.opts[r.Mountpoint] != "nouuid" {
			t.Fatalf("%s opts = %q, want nouuid", name, fm.opts[r.Mountpoint])
		}
	}
}

// The driver never rewrites a filesystem UUID; xfs_admin -U is a write to the volume.
func TestXFSMountsUseNouuid(t *testing.T) {
	for _, tc := range []struct{ fstype, opts, want string }{
		{"xfs", "", "nouuid"},
		{"xfs", "discard", "discard,nouuid"},
		{"xfs", "nouuid,discard", "nouuid,discard"},
		{"ext4", "discard", "discard"},
	} {
		fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
		d := newTestDriver(t, fa, tr, fm)
		d.cfg.FSType, d.cfg.MountOptions = tc.fstype, tc.opts
		_ = d.Create(&plugin.CreateRequest{Name: "v"})
		r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
		if err != nil {
			t.Fatal(err)
		}
		if got := fm.opts[r.Mountpoint]; got != tc.want {
			t.Errorf("%s with %q: opts = %q, want %q", tc.fstype, tc.opts, got, tc.want)
		}
	}
}

func TestCreateOptionCombinations(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	old, _ := fa.CreateVolume(context.Background(), "legacy-x", 1<<30)
	for _, opts := range []map[string]string{
		{"source": "a", "size": "1G"},
		{"import": "a", "source": "b"},
		{"import_from_src": "a", "size": "1G"},
	} {
		if err := d.Create(&plugin.CreateRequest{Name: "v", Options: opts}); err == nil {
			t.Errorf("%v: expected error", opts)
		}
	}
	if err := d.Create(&plugin.CreateRequest{Name: "x", Options: map[string]string{"import_from_src": old.Name}}); err != nil {
		t.Fatal(err)
	}
	if fa.tags[old.Name] != "node1/x" {
		t.Fatalf("import_from_src did not adopt: %v", fa.tags)
	}
	if err := d.Create(&plugin.CreateRequest{Name: "y", Options: map[string]string{"source": "missing"}}); err == nil {
		t.Fatal("expected error cloning a missing source")
	}
}

func TestTagLookupFailureDoesNotGuess(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	fa.lookupErr = errors.New("HTTP 503")
	if err := d.Create(&plugin.CreateRequest{Name: "v"}); err == nil {
		t.Fatal("expected create to fail when the tag lookup fails")
	}
	if len(fa.vols) != 0 {
		t.Fatalf("created a volume under a guessed name: %v", fa.calls)
	}
}

func TestGetAnswersFromKnownVolumesWhileArrayDown(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	if err := d.Create(&plugin.CreateRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	fa.down = errors.New("HTTP 503")

	g, err := d.Get(&plugin.GetRequest{Name: "v"})
	if err != nil {
		t.Fatalf("Get during outage must not error (Docker would substitute a local volume): %v", err)
	}
	if g.Volume.Status["array_unreachable"] == nil || g.Volume.Status["array_name"] != "node1-v" {
		t.Fatalf("status = %v", g.Volume.Status)
	}
	if _, err := d.Get(&plugin.GetRequest{Name: "never-seen"}); err == nil {
		t.Fatal("an unknown volume must still be an error")
	}

	// Survives a plugin restart.
	d2, err := New(context.Background(), d.cfg, Deps{Array: fa, Transport: tr, Mounter: fm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Get(&plugin.GetRequest{Name: "v"}); err != nil {
		t.Fatalf("restarted plugin lost the known volume: %v", err)
	}

	// Removed volumes are forgotten.
	fa.down = nil
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	fa.down = errors.New("HTTP 503")
	if _, err := d.Get(&plugin.GetRequest{Name: "v"}); err == nil {
		t.Fatal("removed volume still answered from known volumes")
	}
}

func TestListReconcilesKnownVolumesAndFallsBack(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	for _, n := range []string{"a", "b"} {
		if err := d.Create(&plugin.CreateRequest{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	// Another node removed and eradicated "a"; List reflects the array.
	fa.mu.Lock()
	delete(fa.vols, "node1-a")
	delete(fa.tags, "node1-a")
	fa.mu.Unlock()
	if l, err := d.List(); err != nil || len(l.Volumes) != 1 {
		t.Fatalf("list = %+v, %v", l, err)
	}

	fa.down = errors.New("dial tcp: i/o timeout")
	l, err := d.List()
	if err != nil || len(l.Volumes) != 1 || l.Volumes[0].Name != "b" {
		t.Fatalf("outage list = %+v, %v", l, err)
	}
	if _, err := d.Get(&plugin.GetRequest{Name: "a"}); err == nil {
		t.Fatal("volume dropped by List still answered from known volumes")
	}
}

// A second node's `docker volume rm` finds the volume already destroyed by
// the first; it must succeed so that node's Docker record goes away.
func TestRemoveVolumeAlreadyDestroyedByAnotherNode(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	if err := d.Create(&plugin.CreateRequest{Name: "v"}); err != nil {
		t.Fatal(err)
	}
	fa.vols["node1-v"].Destroyed = true // another node's remove (or an older plugin) got there first
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err != nil {
		t.Fatalf("second remove of a destroyed volume: %v", err)
	}
	d.cfg.EradicateOnRemove = true
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err != nil {
		t.Fatalf("remove with eradicate of a destroyed volume: %v", err)
	}
	if _, ok := fa.vols["node1-v"]; ok {
		t.Fatal("FA_ERADICATE_ON_REMOVE did not eradicate the already-destroyed volume")
	}
}

// The array keeps a destroyed volume's tag until eradication; the Docker name
// must still drop out of List and Get at once and be importable again.
func TestDestroyedImportedVolumeIsNotListedOrResolved(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	old, _ := fa.CreateVolume(context.Background(), "dvfa-migtest", 1<<30)
	if err := d.Create(&plugin.CreateRequest{Name: "migtest", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(&plugin.RemoveRequest{Name: "migtest"}); err != nil {
		t.Fatal(err)
	}
	if !fa.vols[old.Name].Destroyed || fa.tags[old.Name] == "" {
		t.Fatalf("want destroyed with its tag kept: %+v tag=%q", fa.vols[old.Name], fa.tags[old.Name])
	}
	if l, err := d.List(); err != nil || len(l.Volumes) != 0 {
		t.Fatalf("list = %+v, %v", l, err)
	}
	if g, err := d.Get(&plugin.GetRequest{Name: "migtest"}); err == nil {
		t.Fatalf("get resolved the destroyed volume: %+v", g.Volume)
	}
	repl, _ := fa.CreateVolume(context.Background(), "dvfa-migtest2", 1<<30)
	if err := d.Create(&plugin.CreateRequest{Name: "migtest", Options: map[string]string{"import": repl.Name}}); err != nil {
		t.Fatalf("re-import while the old volume awaits eradication: %v", err)
	}
	if g, err := d.Get(&plugin.GetRequest{Name: "migtest"}); err != nil || g.Volume.Status["array_name"] != repl.Name {
		t.Fatalf("get = %+v, %v", g, err)
	}
}

// --- linger after the last Unmount ----------------------------------------

func (t *fakeTransport) detachCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.detaches)
}

func (f *fakeArray) connected(volume, host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.conns[volume][host]
	return ok
}

func (f *fakeArray) connectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "connect:") {
			n++
		}
	}
	return n
}

func (m *fakeMounter) isMounted(target string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mounted[target]
	return ok
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal(what)
}

// Docker's populate step mounts, unmounts and mounts again within a second or
// two; the second Mount must reuse the attachment instead of a new connect.
func TestQuickRemountReusesLingeringVolume(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.UnmountLinger = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "populate"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "populate"}); err != nil {
		t.Fatal(err)
	}
	if !fm.isMounted(r.Mountpoint) || tr.detachCount() != 0 {
		t.Fatal("torn down at once despite the linger")
	}
	connects := fa.connectCalls()
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "start"}); err != nil {
		t.Fatal(err)
	}
	if fa.connectCalls() != connects || tr.detachCount() != 0 {
		t.Fatal("the quick re-mount re-attached instead of reusing the volume")
	}
	d.mu.Lock()
	pending := d.mounts[d.arrayName("v")].linger != nil
	d.mu.Unlock()
	if pending {
		t.Fatal("re-mount left the teardown scheduled")
	}
}

func TestLingerTearsDownWhenNotRemounted(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.UnmountLinger = 20 * time.Millisecond
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "not torn down after the linger", func() bool {
		return !fm.isMounted(r.Mountpoint) && tr.detachCount() == 1 && !fa.connected(an, "node1")
	})
}

// The teardown after the linger must not pull a filesystem from under a
// container that still has it, and must try again rather than give up.
func TestLingerKeepsVolumeAContainerStillUsesThenRetries(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.UnmountLinger = 10 * time.Millisecond
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	fm.inUse[r.Mountpoint] = true
	fm.mu.Unlock()
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !fm.isMounted(r.Mountpoint) || tr.detachCount() != 0 {
		t.Fatal("tore down a volume a container still has mounted")
	}
	fm.mu.Lock()
	fm.inUse[r.Mountpoint] = false
	fm.mu.Unlock()
	eventually(t, "refused teardown was never retried", func() bool {
		return !fm.isMounted(r.Mountpoint) && tr.detachCount() == 1
	})
}

func TestRemoveReleasesLingeringVolume(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.UnmountLinger = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(&plugin.RemoveRequest{Name: "v"}); err != nil {
		t.Fatalf("remove of a lingering volume: %v", err)
	}
	if tr.detachCount() != 1 || fa.connected(an, "node1") {
		t.Fatal("lingering volume not released before remove")
	}
}

// A mount left over when the plugin stopped mid-linger is torn down once the
// restarted plugin's linger runs out and no container uses it.
func TestRecoveredMountTornDownAfterLinger(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}

	// Plugin restart: a new driver over the same mount root finds the mount.
	if err := os.MkdirAll(r.Mountpoint, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := *d.cfg
	cfg.UnmountLinger = 10 * time.Millisecond
	d2, err := New(context.Background(), &cfg, Deps{Array: fa, Transport: tr, Mounter: fm})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "recovered idle mount not torn down", func() bool {
		return !fm.isMounted(r.Mountpoint) && !fa.connected(an, "node1")
	})
	_ = d2
}

// A lingering volume must not hold unwritten data: the old immediate umount
// flushed it, and another node may take the volume over.
func TestLingerFlushesFilesystem(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.UnmountLinger = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.syncs) != 1 || fm.syncs[0] != r.Mountpoint {
		t.Fatalf("syncs = %v, want the lingering mountpoint", fm.syncs)
	}
}

func TestRemoveRefusesRecoveredMountAContainerUses(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.Mountpoint, 0o755); err != nil {
		t.Fatal(err)
	}
	fm.mu.Lock()
	fm.inUse[r.Mountpoint] = true // a container started before the plugin restart
	fm.mu.Unlock()
	cfg := *d.cfg
	cfg.UnmountLinger = time.Hour
	d2, err := New(context.Background(), &cfg, Deps{Array: fa, Transport: tr, Mounter: fm})
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.Remove(&plugin.RemoveRequest{Name: "v"}); err == nil {
		t.Fatal("removed a volume a running container still has mounted")
	}
	if !fm.isMounted(r.Mountpoint) || tr.detachCount() != 0 {
		t.Fatal("tore down a volume a running container still has mounted")
	}
}

// A plugin stopped right after a drain must not leave lingering volumes
// mounted and connected for another node to preempt.
func TestShutdownReleasesLingeringVolumes(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.UnmountLinger = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	r, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	d.Shutdown(context.Background())
	if fm.isMounted(r.Mountpoint) || tr.detachCount() != 1 || fa.connected(an, "node1") {
		t.Fatal("lingering volume still mounted or connected after shutdown")
	}
}

// A hold record no Mount refreshed for minutes is from an attempt that gave
// up; reusing its start time would preempt a live holder at once.
func TestStaleHoldRecordRestartsGrace(t *testing.T) {
	releasePoll, maxReleaseWait = time.Millisecond, 5*time.Millisecond
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	fa.conns[an] = map[string]int{"other": 7}
	d.heldSince[an] = holdState{hosts: "other", since: time.Now().Add(-2 * time.Hour), seen: time.Now().Add(-time.Hour)}

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil || !strings.Contains(err.Error(), "waiting up to") {
		t.Fatalf("expected to wait, got %v", err)
	}
	if !fa.connected(an, "other") {
		t.Fatal("preempted on a stale hold record")
	}
}

// --- slow devices and maps that change under a mount -----------------------

func (f *fakeArray) disconnectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "disconnect:") {
			n++
		}
	}
	return n
}

// A device that is only late must not be rolled back: on staging the rollbacks
// deleted paths multipathd was still building maps from and freed their LUNs
// for the next attaches.
func TestSlowDeviceStaysAttachedForTheRetry(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{failWait: true}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = time.Hour
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected the mount to fail while the device is missing")
	}
	if tr.detachCount() != 0 || fa.disconnectCalls() != 0 || !fa.connected(an, "node1") {
		t.Fatal("rolled back an attach whose device was only late")
	}
	lun := fa.conns[an]["node1"]

	tr.mu.Lock()
	tr.failWait = false
	tr.mu.Unlock()
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c2"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if fa.conns[an]["node1"] != lun || tr.detachCount() != 0 {
		t.Fatal("the retry did not reuse the kept attachment")
	}
	d.mu.Lock()
	_, pending := d.pending[an]
	d.mu.Unlock()
	if pending {
		t.Fatal("the retry left the release scheduled")
	}
}

func TestKeptAttachmentReleasedWhenNoMountComes(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{failWait: true}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = 40 * time.Millisecond
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected the mount to fail")
	}
	eventually(t, "kept attachment never released", func() bool {
		return tr.detachCount() == 1 && !fa.connected(an, "node1")
	})
}

// A change to the map between the pre-mount check and the mount is caught
// after it, and the filesystem is shut down before the unmount can write
// through a path that reaches another volume.
func TestMapChangedUnderTheMountIsBackedOut(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{lunErrAfter: 1}, newFakeMounter()
	tr.lunErr = errors.New("multipathd files path sdq under this volume, but it reaches another volume")
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected the mount to be refused")
	}
	mp := filepath.Join(d.cfg.MountRoot, "v")
	if len(fm.shutdowns) != 1 || fm.shutdowns[0] != mp {
		t.Fatalf("filesystem not shut down before the unmount: %v", fm.shutdowns)
	}
	if fm.isMounted(mp) || tr.detachCount() != 1 || fa.connected(an, "node1") {
		t.Fatal("mount not backed out")
	}
	d.mu.Lock()
	_, tracked := d.mounts[an]
	d.mu.Unlock()
	if tracked {
		t.Fatal("abandoned mount still tracked")
	}
}

// Until multipathd drops deleted paths, a connect would let the kernel hand
// their names to the new volume's disks.
func TestNoConnectWhileMultipathdHoldsDeletedPaths(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	tr.sweepErr = fmt.Errorf("%w: [sdq]", transport.ErrPathsHeld)
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil || !errors.Is(err, transport.ErrPathsHeld) {
		t.Fatalf("expected the mount to stop on held paths, got %v", err)
	}
	if fa.connectCalls() != 0 {
		t.Fatal("connected while multipathd still held deleted paths")
	}
}

// A retry that fails for another reason must not orphan the kept attachment.
func TestKeptAttachmentReleasedAfterARetryFailsOtherwise(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{failWait: true}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	d.cfg.PreemptGrace = 80 * time.Millisecond
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("expected the mount to fail")
	}
	tr.mu.Lock()
	tr.failWait = false
	tr.sweepErr = fmt.Errorf("%w: [sdq]", transport.ErrPathsHeld)
	tr.mu.Unlock()
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c2"}); err == nil {
		t.Fatal("expected the retry to fail")
	}
	tr.mu.Lock()
	tr.sweepErr = nil
	tr.mu.Unlock()
	eventually(t, "kept attachment orphaned by a failed retry", func() bool {
		return tr.detachCount() == 1 && !fa.connected(an, "node1")
	})
}

// A Mount queued behind a long operation must give up within its own budget:
// Docker abandons it after 2 minutes, and work it started after that ran
// unseen.
func TestMountGivesUpWaitingForTheVolumeLock(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	old := mountBudget
	mountBudget = 100 * time.Millisecond
	t.Cleanup(func() { mountBudget = old })
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	unlock := d.lock(d.arrayName("v")) // a long teardown holds the volume
	defer unlock()
	start := time.Now()
	_, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"})
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("want a give-up on the volume lock, got %v", err)
	}
	if time.Since(start) > 2*time.Second || fa.connectCalls() != 0 {
		t.Fatal("mount did not give up promptly, or connected anyway")
	}
}

// Likewise behind the host-wide attach lock, before anything is connected.
func TestMountGivesUpWaitingForTheHostLock(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	old := mountBudget
	mountBudget = 100 * time.Millisecond
	t.Cleanup(func() { mountBudget = old })
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	if err := tr.excl.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr.excl.Unlock()
	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err == nil {
		t.Fatal("want the mount to give up on the host lock")
	}
	if fa.connectCalls() != 0 {
		t.Fatal("connected without the host lock")
	}
}

// A rollback gets its own budget for the host lock: the request's may be spent.
func TestRollbackWaitsWithItsOwnBudget(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	an := d.arrayName("v")
	if _, err := fa.Connect(context.Background(), "node1", an); err != nil {
		t.Fatal(err)
	}
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	d.rollbackAttach(spent, an, fa.vols[an].Serial, "")
	if fa.connected(an, "node1") {
		t.Fatal("rollback skipped because the request's context had ended")
	}
}

func TestMountRecreatesHostObjectTheArrayLost(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	d := newTestDriver(t, fa, tr, fm)
	_ = d.Create(&plugin.CreateRequest{Name: "v"})
	fa.mu.Lock()
	delete(fa.hosts, "node1") // the Pure plugin deleted it after its last detach
	fa.mu.Unlock()

	if _, err := d.Mount(&plugin.MountRequest{Name: "v", ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fa.hosts["node1"]; !ok {
		t.Fatal("host object not recreated")
	}
	if _, ok := fa.conns[d.arrayName("v")]["node1"]; !ok {
		t.Fatal("volume not connected after recreating the host object")
	}
}
