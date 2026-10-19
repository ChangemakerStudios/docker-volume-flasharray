package transport

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// fakeBlock builds a sysfs entry and a /dev link pointing at it, the shape a
// udev by-id link has: link -> devNode, with identity under sys/<devNode>.
func fakeBlock(t *testing.T, root, devNode, idFile, id string) string {
	t.Helper()
	node := filepath.Join(root, "dev", devNode)
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(root, "sys", devNode, idFile)
	if err := os.MkdirAll(filepath.Dir(sys), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sys, []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "dev", "by-id-"+devNode)
	if err := os.Symlink(node, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return link
}

func TestDeviceIdentityChecks(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const ours = "3624a93706fef9ab1157343ec00c48bb3"
	const theirs = "3624a93706fef9ab1157343ec00d4a11a"

	// The by-id link for "ours" outlived its map; dm-3 now belongs to "theirs".
	stale := fakeBlock(t, root, "dm-3", "dm/uuid", "mpath-"+theirs)
	if dmCarries(stale, ours) {
		t.Fatal("stale link accepted for another LUN's map")
	}
	if !dmCarries(stale, theirs) {
		t.Fatal("map rejected for its own wwid")
	}

	sd := fakeBlock(t, root, "sdq", "device/wwid", "naa.624a93706fef9ab1157343ec00c48bb3")
	if !sdCarries(sd, ours) || sdCarries(sd, theirs) {
		t.Fatal("sd identity check wrong")
	}

	if dmCarries(filepath.Join(root, "dev", "missing"), ours) {
		t.Fatal("missing device accepted")
	}
}

// fakePath builds sys/<sd>/device -> scsi/<hctl> holding the cached wwid and a
// delete file, the sysfs shape of one SCSI path.
func fakePath(t *testing.T, root, sd, hctl, wwid string) {
	t.Helper()
	dir := filepath.Join(root, "scsi", hctl)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"wwid": wwid + "\n", "delete": ""} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", sd), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(root, "sys", sd, "device")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func deleted(t *testing.T, root, hctl string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "scsi", hctl, "delete"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b) == "1"
}

// A path keeps the WWID it was scanned with after the array gives its LUN to
// another volume; on staging such paths cross-wrote volumes.
func TestSweepDeletesPathsOnReassignedLUNs(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	fakePath(t, root, "sda", "34:0:0:3", "naa.624a93706fef9ab1157343ec000000aa")
	fakePath(t, root, "sdb", "34:0:0:4", "naa.624a93706fef9ab1157343ec000000bb")
	fakePath(t, root, "sdc", "34:0:0:9", "naa.624a93706fef9ab1157343ec000000dd")
	fakePath(t, root, "sdd", "2:0:0:4", "naa.6000c2900000000000000000000000ee")

	tr := &iscsi{log: slog.Default()}
	want := map[int]string{3: "6fef9ab1157343ec000000aa", 4: "6fef9ab1157343ec000000cc"}
	if err := tr.Sweep(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if deleted(t, root, "34:0:0:3") {
		t.Fatal("deleted a path the array still maps to its volume")
	}
	if !deleted(t, root, "34:0:0:4") {
		t.Fatal("kept a path whose LUN now carries another volume")
	}
	if !deleted(t, root, "34:0:0:9") {
		t.Fatal("kept a path on a LUN the array no longer connects")
	}
	if deleted(t, root, "2:0:0:4") {
		t.Fatal("touched a non-FlashArray disk")
	}
}

func TestCheckLUN(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	fakePath(t, root, "sda", "34:0:0:3", "naa.624a93706fef9ab1157343ec000000aa")
	fakePath(t, root, "sde", "35:0:0:3", "naa.624a93706fef9ab1157343ec000000aa")
	fakePath(t, root, "sdb", "36:0:0:4", "naa.624a93706fef9ab1157343ec000000aa")
	dev := fakeBlock(t, root, "dm-5", "dm/uuid", "mpath-3624a93706fef9ab1157343ec000000aa")
	for _, s := range []string{"sda", "sde"} {
		if err := os.MkdirAll(filepath.Join(root, "sys", "dm-5", "slaves", s), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tr := &iscsi{log: slog.Default()}
	if err := tr.CheckLUN(dev, 3); err != nil {
		t.Fatalf("all paths on LUN 3: %v", err)
	}
	if err := tr.CheckLUN(dev, 4); err == nil {
		t.Fatal("accepted paths on the wrong LUN")
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", "dm-5", "slaves", "sdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tr.CheckLUN(dev, 3); err == nil {
		t.Fatal("accepted a map with a path on another LUN")
	}
}
