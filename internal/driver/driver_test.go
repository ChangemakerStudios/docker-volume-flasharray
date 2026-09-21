package driver

import (
	"context"
	"errors"
	"fmt"
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
	mu      sync.Mutex
	vols    map[string]*flasharray.Volume
	tags    map[string]string
	hosts   map[string][]string
	conns   map[string]map[string]int // volume -> host -> lun
	nextLUN int
	calls   []string
}

func newFakeArray() *fakeArray {
	return &fakeArray{vols: map[string]*flasharray.Volume{}, tags: map[string]string{}, hosts: map[string][]string{}, conns: map[string]map[string]int{}, nextLUN: 1}
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
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{formatted: map[string]bool{}, mounted: map[string]string{}}
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
func (m *fakeMounter) Mount(_ context.Context, dev, target, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = dev
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
		"cas_store":             {hashSuffix: true},
		"seq.store":             {hashSuffix: true},
		"a_b":                   {hashSuffix: true},
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
	if r1.Mountpoint != r2.Mountpoint || !strings.HasSuffix(r1.Mountpoint, "/data_vol") {
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
