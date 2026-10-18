package mounter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountedAt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dm3 := filepath.Join(root, "dm-3")
	if err := os.WriteFile(dm3, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mapper := filepath.Join(root, "mpathbs")
	if err := os.Symlink(dm3, mapper); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	other := filepath.Join(root, "dm-7")
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// The same device mounted under two names, as a stale by-id link left it.
	info := strings.Join([]string{
		"22 1 0:21 / /proc rw,nosuid shared:12 - proc proc rw",
		"301 29 253:3 / /mnt/flasharray/chromadb_data rw,relatime shared:150 - xfs " + mapper + " rw,attr2",
		"302 29 253:7 / /mnt/flasharray/seq\\040store rw,relatime - xfs " + other + " rw",
	}, "\n") + "\n"
	fixture := filepath.Join(root, "mountinfo")
	if err := os.WriteFile(fixture, []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountinfoPath
	mountinfoPath = fixture
	t.Cleanup(func() { mountinfoPath = old })

	m := &execMounter{}
	cases := map[string]string{
		dm3:    "/mnt/flasharray/chromadb_data", // raw node matches a mount made through its mapper link
		mapper: "/mnt/flasharray/chromadb_data",
		other:  "/mnt/flasharray/seq store",
	}
	for dev, want := range cases {
		got, err := m.MountedAt(dev)
		if err != nil || got != want {
			t.Errorf("MountedAt(%s) = %q, %v; want %q", dev, got, err, want)
		}
	}

	unmounted := filepath.Join(root, "dm-9")
	if err := os.WriteFile(unmounted, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := m.MountedAt(unmounted); err != nil || got != "" {
		t.Errorf("unmounted device reported at %q, %v", got, err)
	}
}
