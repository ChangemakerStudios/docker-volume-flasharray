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
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/plugin"
)

// --- fakes -----------------------------------------------------------------

type fakeArray struct {
	mu        sync.Mutex
	vols      map[string]*flasharray.Volume
	tags      map[string]string            // dvfa:name only
	extra     map[string]map[string]string // other dvfa tags: volume -> key -> value
	lookupErr error                        // returned by LookupNameTag
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
	out := map[string]string{}
	for k, v := range f.tags {
		if strings.HasPrefix(v, prefix) {
			out[k] = v
		}
	}
	return out, nil
}

func (f *fakeArray) LookupNameTag(_ context.Context, value string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return "", f.lookupErr
	}
	for an, v := range f.tags {
		if v == value {
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

func (f *fakeArray) ListConnections(_ context.Context, volume string) ([]flasharray.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []flasharray.Connection
	for h, lun := range f.conns[volume] {
		out = append(out, flasharray.Connection{Host: h, Volume: volume, LUN: lun})
	}
	return out, nil
}

func (f *fakeArray) Ports(context.Context) ([]flasharray.Port, error) {
	return []flasharray.Port{{Name: "CT0.ETH4", IQN: "iqn.2010-06.com.purestorage:flasharray.test", Portal: "10.10.100.154:3260"}}, nil
}

type fakeTransport struct {
	mu       sync.Mutex
	connects int
	waits    []string
	detaches []string
	failWait bool
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
	return "/dev/mapper/3624a9370" + serial, nil
}
func (t *fakeTransport) Detach(_ context.Context, serial, _ string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.detaches = append(t.detaches, serial)
	return nil
}
func (t *fakeTransport) PostDisconnect(context.Context) error { return nil }

type fakeMounter struct {
	mu        sync.Mutex
	formatted map[string]bool
	mounted   map[string]string // target -> dev
	opts      map[string]string // target -> mount options
	uuidErr   error             // returned by RegenerateUUID
	uuidRegen []string          // devs given a new UUID
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{formatted: map[string]bool{}, mounted: map[string]string{}, opts: map[string]string{}}
}
func (m *fakeMounter) EnsureFilesystem(_ context.Context, dev, _ string, _ []string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.formatted[dev] {
		return false, nil
	}
	m.formatted[dev] = true
	return true, nil
}
func (m *fakeMounter) Mount(_ context.Context, dev, target, _, opts string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = dev
	m.opts[target] = opts
	return nil
}
func (m *fakeMounter) RegenerateUUID(_ context.Context, dev, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.uuidErr != nil {
		return m.uuidErr
	}
	m.uuidRegen = append(m.uuidRegen, dev)
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
		"mongodb-1":             {want: "node1-mongodb-1"},
		"cas_store":             {want: "node1-cas_store"},
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

	if err := d.Remove(&plugin.RemoveRequest{Name: "data_vol"}); err != nil {
		t.Fatal(err)
	}
	if v := fa.vols[an]; v == nil || !v.Destroyed {
		t.Fatalf("expected destroyed (not eradicated): %+v", v)
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
	old, _ := fa.CreateVolume(context.Background(), "sblinuxdev-chromadb_data", 32<<30)

	if err := d.Create(&plugin.CreateRequest{Name: "chromadb_data", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatal(err)
	}
	if fa.tags[old.Name] != "node1/chromadb_data" {
		t.Fatalf("tag = %q", fa.tags[old.Name])
	}
	if len(fa.vols) != 1 {
		t.Fatalf("import must not create a new volume: %v", fa.calls)
	}
	// Every subsequent op resolves through the tag to the old array name.
	r, err := d.Mount(&plugin.MountRequest{Name: "chromadb_data", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fa.conns[old.Name]["node1"]; !ok {
		t.Fatalf("mount attached the wrong volume: %v", fa.conns)
	}
	if tr.waits[0] != old.Serial {
		t.Fatalf("waited for serial %s, want %s", tr.waits[0], old.Serial)
	}
	if err := d.Unmount(&plugin.UnmountRequest{Name: "chromadb_data", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_ = r
	g, err := d.Get(&plugin.GetRequest{Name: "chromadb_data"})
	if err != nil || g.Volume.Status["array_name"] != old.Name {
		t.Fatalf("get = %+v %v", g, err)
	}
	// Idempotent re-import; conflicting re-import refused.
	if err := d.Create(&plugin.CreateRequest{Name: "chromadb_data", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	other, _ := fa.CreateVolume(context.Background(), "sblinuxdev-other", 1<<30)
	if err := d.Create(&plugin.CreateRequest{Name: "chromadb_data", Options: map[string]string{"import": other.Name}}); err == nil {
		t.Fatal("expected conflict importing a second volume under the same docker name")
	}
	if err := d.Create(&plugin.CreateRequest{Name: "second", Options: map[string]string{"import": old.Name}}); err == nil {
		t.Fatal("expected conflict importing an already-claimed volume under another name")
	}
	// Remove goes to the adopted array volume, not the computed name.
	if err := d.Remove(&plugin.RemoveRequest{Name: "chromadb_data"}); err != nil {
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
	old, _ := fa.CreateVolume(context.Background(), "sblinuxdev-chromadb_data", 1<<30)
	if err := d.Create(&plugin.CreateRequest{Name: "chromadb_data", Options: map[string]string{"import": old.Name}}); err != nil {
		t.Fatal(err)
	}
	r, err := d.Mount(&plugin.MountRequest{Name: "chromadb_data", ID: "c1"})
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
	if err := d2.Unmount(&plugin.UnmountRequest{Name: "chromadb_data", ID: "c1"}); err != nil {
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
	if fa.tags[an] != "node1/pg_copy" || fa.extra[an][flasharray.TagKeyClonePending] != "node1-pg" {
		t.Fatalf("tags: name=%q extra=%v", fa.tags[an], fa.extra[an])
	}
	// First mount gives the clone its own filesystem UUID and clears the marker.
	r, err := d.Mount(&plugin.MountRequest{Name: "pg_copy", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fm.uuidRegen) != 1 || fm.opts[r.Mountpoint] != "" {
		t.Fatalf("regen=%v opts=%q", fm.uuidRegen, fm.opts[r.Mountpoint])
	}
	if _, ok := fa.extra[an][flasharray.TagKeyClonePending]; ok {
		t.Fatal("clone marker not cleared")
	}
	// A plain volume never gets its UUID touched.
	if _, err := d.Mount(&plugin.MountRequest{Name: "pg", ID: "c2"}); err != nil {
		t.Fatal(err)
	}
	if len(fm.uuidRegen) != 1 {
		t.Fatalf("regenerated UUID on a non-clone: %v", fm.uuidRegen)
	}
}

func TestCloneFallsBackToNouuid(t *testing.T) {
	fa, tr, fm := newFakeArray(), &fakeTransport{}, newFakeMounter()
	fm.uuidErr = errors.New("xfs_admin: log is dirty")
	d := newTestDriver(t, fa, tr, fm)
	src, _ := fa.CreateVolume(context.Background(), "legacy-data", 1<<30)
	// source may be a raw array volume name, as with the Pure plugin.
	if err := d.Create(&plugin.CreateRequest{Name: "copy", Options: map[string]string{"source": src.Name}}); err != nil {
		t.Fatal(err)
	}
	r, err := d.Mount(&plugin.MountRequest{Name: "copy", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if fm.opts[r.Mountpoint] != "nouuid" {
		t.Fatalf("opts = %q, want nouuid", fm.opts[r.Mountpoint])
	}
	if _, ok := fa.extra["node1-copy"][flasharray.TagKeyClonePending]; !ok {
		t.Fatal("marker must stay so the next mount retries")
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
