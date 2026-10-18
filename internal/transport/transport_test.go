package transport

import (
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
