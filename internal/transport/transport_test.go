package transport

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
)

// CI runners and dev hosts may run a real multipathd; tests that want one
// fake it (fakeMultipath), and none may reach the real one.
func TestMain(m *testing.M) {
	multipathdRunning = func() bool { return false }
	os.Exit(m.Run())
}

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

// scsiDir is a fake iSCSI path's device directory: under a session, as the
// kernel files one.
func scsiDir(root, hctl string) string {
	return filepath.Join(root, "scsi", "session1", hctl)
}

// fakePath builds sys/<sd>/device -> scsiDir(hctl) holding the cached wwid and
// a delete file, the sysfs shape of one iSCSI path.
func fakePath(t *testing.T, root, sd, hctl, wwid string) {
	t.Helper()
	fakePathIn(t, root, scsiDir(root, hctl), sd, wwid)
}

// fakePathIn is fakePath with the device directory, whose last element is the
// H:C:T:L, anywhere.
func fakePathIn(t *testing.T, root, dir, sd, wwid string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("H:C:T:L is not a valid file name on Windows")
	}
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
	b, err := os.ReadFile(filepath.Join(scsiDir(root, hctl), "delete"))
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

	// Another array's volume on LUN 4 of its own target: LUN numbers are per array.
	fakePath(t, root, "sde", "50:0:0:4", "naa.624a93700123456789abcdef000000ff")

	tr := &iscsi{log: slog.Default()}
	want := map[int]string{3: "6fef9ab1157343ec000000aa", 4: "6fef9ab1157343ec000000cc"}
	if err := tr.Sweep(context.Background(), want, "6fef9ab1157343ec", 4, false); err != nil {
		t.Fatal(err)
	}
	if deleted(t, root, "50:0:0:4") {
		t.Fatal("deleted another array's path")
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

// The background check must not cut a path just because its LUN is missing
// from the array's answer; only a LUN given to another volume counts.
func TestSweepCrossedOnly(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	fakePath(t, root, "sda", "34:0:0:4", "naa.624a93706fef9ab1157343ec000000bb") // LUN 4 now carries cc
	fakePath(t, root, "sdb", "34:0:0:9", "naa.624a93706fef9ab1157343ec000000dd") // LUN 9 not in the answer

	tr := &iscsi{log: slog.Default()}
	want := map[int]string{4: "6fef9ab1157343ec000000cc"}
	if err := tr.Sweep(context.Background(), want, "6fef9ab1157343ec", -1, true); err != nil {
		t.Fatal(err)
	}
	if !deleted(t, root, "34:0:0:4") {
		t.Fatal("kept a path that reaches another volume")
	}
	if deleted(t, root, "34:0:0:9") {
		t.Fatal("cut a path only because its LUN was missing from the answer")
	}
}

// This array's LUNs can also reach the node outside the plugin's sessions,
// under another host object whose LUN numbers this host's map knows nothing of.
func TestSweepLeavesPathsOffISCSIAlone(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	fc := filepath.Join(root, "scsi", "rport-7:0-0", "target7:0:0", "7:0:0:4")
	fakePathIn(t, root, fc, "sda", "naa.624a93706fef9ab1157343ec000000bb") // LUN 4 carries cc for this host
	rdm := filepath.Join(root, "scsi", "target2:0:1", "2:0:1:9")
	fakePathIn(t, root, rdm, "sdb", "naa.624a93706fef9ab1157343ec000000dd")      // LUN 9 not connected to this host
	fakePath(t, root, "sdc", "34:0:0:4", "naa.624a93706fef9ab1157343ec000000bb") // the same stale identity over iSCSI

	tr := &iscsi{log: slog.Default()}
	want := map[int]string{4: "6fef9ab1157343ec000000cc"}
	if err := tr.Sweep(context.Background(), want, "6fef9ab1157343ec", -1, false); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{fc, rdm} {
		if b, err := os.ReadFile(filepath.Join(dir, "delete")); err != nil || string(b) == "1" {
			t.Fatalf("deleted a path that is not on an iSCSI session: %s (%v)", dir, err)
		}
	}
	if !deleted(t, root, "34:0:0:4") {
		t.Fatal("kept a stale iSCSI path")
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
	if err := tr.CheckLUN(context.Background(), dev, 3); err != nil {
		t.Fatalf("all paths on LUN 3: %v", err)
	}
	if err := tr.CheckLUN(context.Background(), dev, 4); err == nil {
		t.Fatal("accepted paths on the wrong LUN")
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", "dm-5", "slaves", "sdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tr.CheckLUN(context.Background(), dev, 3); err == nil {
		t.Fatal("accepted a map with a path on another LUN")
	}
}

// One wedged path must not block every mount on the node: only a stale path on
// the LUN being attached is worth failing for.
func TestSweepFailsOnlyForItsOwnLUN(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	fakePath(t, root, "sda", "34:0:0:5", "naa.624a93706fef9ab1157343ec000000aa")
	stuck := filepath.Join(scsiDir(root, "34:0:0:5"), "delete")
	if err := os.Remove(stuck); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stuck, 0o755); err != nil { // writes to it now fail
		t.Fatal(err)
	}
	tr := &iscsi{log: slog.Default()}
	want := map[int]string{5: "6fef9ab1157343ec000000bb"}
	if err := tr.Sweep(context.Background(), want, "6fef9ab1157343ec", 7, false); err != nil {
		t.Fatalf("undeletable path on another LUN failed the sweep: %v", err)
	}
	if err := tr.Sweep(context.Background(), want, "6fef9ab1157343ec", 5, false); err == nil {
		t.Fatal("undeletable stale path on our own LUN must fail")
	}
}

func TestDeletePathAlreadyGone(t *testing.T) {
	old := sysClassBlock
	sysClassBlock = t.TempDir()
	t.Cleanup(func() { sysClassBlock = old })
	if err := deletePath("sdzz"); err != nil {
		t.Fatalf("a path that is already gone counts as deleted: %v", err)
	}
}

// fakeMultipath pretends multipathd is up with map mpathq for wwid, and records
// every command cleanup runs.
func fakeMultipath(t *testing.T, wwid string, lookupErr error) *[]string {
	t.Helper()
	oldRun, oldMP := runner, multipathdRunning
	t.Cleanup(func() { runner, multipathdRunning = oldRun, oldMP })
	multipathdRunning = func() bool { return true }
	var cmds []string
	runner = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		c := name + " " + strings.Join(args, " ")
		cmds = append(cmds, c)
		if strings.HasPrefix(c, "multipathd show maps") {
			return "mpathq " + wwid, lookupErr
		}
		return "", nil
	}
	return &cmds
}

func ran(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

// Detach must not trust a path's cached WWID: one whose LUN the array gives to
// another volume is deleted unflushed, before the map is flushed, and with no
// path left that reaches the volume nothing is flushed at all.
func TestDetachDeletesStalePathsBeforeAnyFlush(t *testing.T) {
	const serial = "6fef9ab1157343ec000000aa"
	for _, tc := range []struct {
		name      string
		want      map[int]string
		flushMap  bool
		flushPath bool
	}{
		{"connected", map[int]string{3: serial, 8: "6fef9ab1157343ec000000cc"}, true, true},
		{"leftover", map[int]string{8: "6fef9ab1157343ec000000cc"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			old := sysClassBlock
			sysClassBlock = filepath.Join(root, "sys")
			t.Cleanup(func() { sysClassBlock = old })
			fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+serial)
			fakePath(t, root, "sdb", "35:0:0:8", "naa.624a9370"+serial)
			cmds := fakeMultipath(t, WWID(serial), nil)
			oldRun := runner
			runner = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
				if name == "blockdev" && !deleted(t, root, "35:0:0:8") {
					t.Errorf("%s %v ran while a path into another volume's LUN was still present", name, args)
				}
				return oldRun(ctx, log, name, args...)
			}

			tr := &iscsi{log: slog.Default()}
			if err := tr.Detach(context.Background(), serial, "", tc.want); err != nil {
				t.Fatal(err)
			}
			if !deleted(t, root, "34:0:0:3") || !deleted(t, root, "35:0:0:8") {
				t.Fatal("both paths of the volume must go")
			}
			if ran(*cmds, "blockdev --flushbufs /dev/sdb") {
				t.Fatal("flushed a path whose LUN carries another volume")
			}
			if got := ran(*cmds, "blockdev --flushbufs /dev/mapper/mpathq"); got != tc.flushMap {
				t.Fatalf("map flushed = %v, want %v (cmds %v)", got, tc.flushMap, *cmds)
			}
			if got := ran(*cmds, "blockdev --flushbufs /dev/sda"); got != tc.flushPath {
				t.Fatalf("valid path flushed = %v, want %v", got, tc.flushPath)
			}
		})
	}
}

// Deleting paths under a map that could not be looked up would leave it
// queueing I/O with nothing beneath it.
func TestDetachTouchesNothingWhenMapLookupFails(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })
	const serial = "6fef9ab1157343ec000000aa"
	fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+serial)
	fakeMultipath(t, WWID(serial), context.DeadlineExceeded)

	tr := &iscsi{log: slog.Default()}
	if err := tr.Detach(context.Background(), serial, "", map[int]string{3: serial}); err == nil {
		t.Fatal("expected the lookup failure")
	}
	if deleted(t, root, "34:0:0:3") {
		t.Fatal("deleted a path although the map could not be found")
	}
}

func TestHasPaths(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	dev := fakeBlock(t, root, "dm-7", "dm/uuid", "mpath-3624a93706fef9ab1157343ec000000aa")
	if hasPaths(dev) {
		t.Fatal("a map without paths is not a usable device")
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", "dm-7", "slaves", "sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasPaths(dev) {
		t.Fatal("map with a path rejected")
	}
}

// fakeMap builds sys/<dm>/dm/{uuid,name} and slave entries for a multipath map.
func fakeMap(t *testing.T, root, dm, name, wwid string, slaves ...string) {
	t.Helper()
	for f, v := range map[string]string{"uuid": "mpath-" + wwid + "\n", "name": name + "\n"} {
		p := filepath.Join(root, "sys", dm, "dm", f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, sl := range slaves {
		if err := os.MkdirAll(filepath.Join(root, "sys", dm, "slaves", sl), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// A path whose identity and LUN agree with the array can still sit in another
// volume's map when multipathd added it under a stale identity. On staging a
// manual "add path" did exactly that; had the map been mounted, its writes
// would have gone to the wrong LUN.
func TestSweepPullsPathOutOfAnotherVolumesMap(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const a, b = "6fef9ab1157343ec000000aa", "6fef9ab1157343ec000000bb"
	fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+a)
	fakePath(t, root, "sdb", "34:0:0:4", "naa.624a9370"+b)
	fakeMap(t, root, "dm-5", "mpathq", WWID(a), "sda", "sdb")
	fakeUdev(t, root, "sdb", "8:16", WWID(b))
	cmds := fakeMultipath(t, WWID(a), nil)

	tr := &iscsi{log: slog.Default()}
	if err := tr.Sweep(context.Background(), map[int]string{3: a, 4: b}, "6fef9ab1157343ec", -1, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"multipathd fail path sdb", "multipathd del path sdb", "multipathd add path sdb"} {
		if !ran(*cmds, want) {
			t.Fatalf("missing %q in %v", want, *cmds)
		}
	}
	if ran(*cmds, "multipathd del path sda") {
		t.Fatal("pulled a path that belongs in the map")
	}
	if deleted(t, root, "34:0:0:4") {
		t.Fatal("deleted a valid path; it only needed to leave the wrong map")
	}
}

// fakeUdev gives sd a device number and a udev record naming wwid.
func fakeUdev(t *testing.T, root, sd, majmin, wwid string) {
	t.Helper()
	old := udevDataDir
	udevDataDir = filepath.Join(root, "udev")
	t.Cleanup(func() { udevDataDir = old })
	for p, v := range map[string]string{
		filepath.Join(root, "sys", sd, "dev"):    majmin + "\n",
		filepath.Join(root, "sys", sd, "uevent"): "",
		filepath.Join(root, "udev", "b"+majmin):  "E:ID_SERIAL=" + wwid + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func reprobed(t *testing.T, root, sd string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "sys", sd, "uevent"))
	return err == nil && string(b) == "change"
}

func TestAddPath(t *testing.T) {
	want := WWID("6fef9ab1157343ec000000aa")
	other := WWID("6fef9ab1157343ec000000bb")
	for _, tc := range []struct {
		name  string
		known map[string]string
		udev  string
		cmds  []string
		probe bool
	}{
		{"stale multipathd entry", map[string]string{"sdt": other}, want, []string{"multipathd del path sdt", "multipathd add path sdt"}, false},
		{"multipathd has it right", map[string]string{"sdt": want}, want, nil, false},
		{"new to multipathd", map[string]string{}, want, []string{"multipathd add path sdt"}, false},
		// multipathd files a path by udev's record: added now, it would join the old volume's map.
		{"udev not caught up", map[string]string{"sdt": other}, other, nil, true},
		{"udev still on a new path", map[string]string{}, "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			old := sysClassBlock
			sysClassBlock = filepath.Join(root, "sys")
			t.Cleanup(func() { sysClassBlock = old })
			fakeUdev(t, root, "sdt", "65:48", tc.udev)
			cmds := fakeMultipath(t, "", nil)
			tr := &iscsi{log: slog.Default()}
			tr.addPath(context.Background(), "sdt", want, tc.known)
			if strings.Join(*cmds, "|") != strings.Join(tc.cmds, "|") {
				t.Fatalf("cmds = %v, want %v", *cmds, tc.cmds)
			}
			if got := reprobed(t, root, "sdt"); got != tc.probe {
				t.Fatalf("reprobe = %v, want %v", got, tc.probe)
			}
		})
	}
}

// Re-adding a pulled-out path while udev still files it under the old volume
// put it straight back into that volume's map on staging.
func TestMembershipFixWaitsForUdevBeforeReadding(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const a, b = "6fef9ab1157343ec000000aa", "6fef9ab1157343ec000000bb"
	fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+a)
	fakePath(t, root, "sdb", "34:0:0:4", "naa.624a9370"+b)
	fakeMap(t, root, "dm-5", "mpathq", WWID(a), "sda", "sdb")
	fakeUdev(t, root, "sdb", "8:16", WWID(a)) // udev still has the old identity
	cmds := fakeMultipath(t, WWID(a), nil)

	tr := &iscsi{log: slog.Default()}
	if err := tr.Sweep(context.Background(), map[int]string{3: a, 4: b}, "6fef9ab1157343ec", -1, true); err != nil {
		t.Fatal(err)
	}
	if !ran(*cmds, "multipathd del path sdb") || ran(*cmds, "multipathd add path sdb") {
		t.Fatalf("want sdb pulled out and not re-added yet: %v", *cmds)
	}
	if !reprobed(t, root, "sdb") {
		t.Fatal("udev not asked to re-read sdb")
	}
}

// multipathd still listing a deleted path means the kernel may hand its name to
// the next disk while multipathd files that disk under the old volume.
func TestDetachWaitsForMultipathdToForgetDeletedPaths(t *testing.T) {
	const serial = "6fef9ab1157343ec000000aa"
	for _, tc := range []struct {
		name      string
		heldFor   int // "show paths" calls that still list sda; <0 = always
		wantError bool
	}{
		{"forgets in time", 3, false},
		{"never forgets", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			old := sysClassBlock
			sysClassBlock = filepath.Join(root, "sys")
			oldPoll, oldWait := forgetPoll, forgetWait
			forgetPoll, forgetWait = time.Millisecond, 50*time.Millisecond
			t.Cleanup(func() { sysClassBlock, forgetPoll, forgetWait = old, oldPoll, oldWait })
			fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+serial)
			cmds := fakeMultipath(t, WWID(serial), nil)
			inner := runner
			shows := 0
			runner = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
				c := name + " " + strings.Join(args, " ")
				if strings.HasPrefix(c, "multipathd show paths") {
					shows++
					if tc.heldFor < 0 || shows <= tc.heldFor {
						return "sda " + WWID(serial), nil
					}
					return "", nil
				}
				return inner(ctx, log, name, args...)
			}

			tr := &iscsi{log: slog.Default()}
			err := tr.Detach(context.Background(), serial, "", map[int]string{3: serial})
			if got := errors.Is(err, ErrPathsHeld); got != tc.wantError {
				t.Fatalf("ErrPathsHeld = %v, want %v (err %v)", got, tc.wantError, err)
			}
			if tc.heldFor > 0 && shows <= tc.heldFor {
				t.Fatal("returned before multipathd let go of the path")
			}
			if tc.wantError && !ran(*cmds, "multipathd del path sda") {
				t.Fatal("did not ask multipathd to drop the path it kept")
			}
		})
	}
}

// A path multipathd files under this map but which reaches another volume is
// one multipathd may add to the map at any moment, so the mount must wait.
func TestCheckLUNRefusesWhileMultipathdFilesAForeignPath(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const a, b = "6fef9ab1157343ec000000aa", "6fef9ab1157343ec000000bb"
	fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+a)
	fakePath(t, root, "sdc", "34:0:0:5", "naa.624a9370"+b)
	dev := fakeBlock(t, root, "dm-5", "dm/uuid", "mpath-"+WWID(a))
	fakeMap(t, root, "dm-5", "mpathq", WWID(a), "sda")
	fakeMultipath(t, WWID(a), nil)
	inner := runner
	runner = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
		if name == "multipathd" && len(args) > 1 && args[0] == "show" && args[1] == "paths" {
			return "sda " + WWID(a) + "\nsdc " + WWID(a), nil
		}
		return inner(ctx, log, name, args...)
	}

	tr := &iscsi{log: slog.Default()}
	if err := tr.CheckLUN(context.Background(), dev, 3); err == nil || !strings.Contains(err.Error(), "sdc") {
		t.Fatalf("expected a refusal naming sdc, got %v", err)
	}
}

func TestCheckLUNRejectsAnotherVolumesPath(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const a, b = "6fef9ab1157343ec000000aa", "6fef9ab1157343ec000000bb"
	fakePath(t, root, "sda", "34:0:0:3", "naa.624a9370"+a)
	fakePath(t, root, "sdb", "35:0:0:3", "naa.624a9370"+b)
	dev := fakeBlock(t, root, "dm-5", "dm/uuid", "mpath-"+WWID(a))
	fakeMap(t, root, "dm-5", "mpathq", WWID(a), "sda", "sdb")

	tr := &iscsi{log: slog.Default()}
	if err := tr.CheckLUN(context.Background(), dev, 3); err == nil || !strings.Contains(err.Error(), "another volume") {
		t.Fatalf("expected a refusal for sdb, got %v", err)
	}
}

// An entry filing a path under another volume, outside any map, makes the
// mount check refuse that volume until something drops it.
func TestSweepDropsMultipathdEntryWithWrongIdentity(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const a, b = "6fef9ab1157343ec000000aa", "6fef9ab1157343ec000000bb"
	fakePath(t, root, "sdc", "34:0:0:5", "naa.624a9370"+b)
	fakeUdev(t, root, "sdc", "8:32", WWID(b))
	cmds := fakeMultipath(t, WWID(a), nil)
	inner := runner
	runner = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
		if name == "multipathd" && len(args) > 1 && args[0] == "show" && args[1] == "paths" {
			return "sdc " + WWID(a), nil
		}
		return inner(ctx, log, name, args...)
	}

	tr := &iscsi{log: slog.Default()}
	if err := tr.Sweep(context.Background(), map[int]string{5: b}, "6fef9ab1157343ec", -1, true); err != nil {
		t.Fatal(err)
	}
	if !ran(*cmds, "multipathd del path sdc") || !ran(*cmds, "multipathd add path sdc") || ran(*cmds, "multipathd fail path sdc") {
		t.Fatalf("want sdc's entry dropped and re-read, not failed: %v", *cmds)
	}
}

func TestConnectLogsInAgainWhenSessionsAreGone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	portal := ln.Addr().String()
	const iqn = "iqn.2010-06.com.purestorage:flasharray.test"
	sessions, logins := "", 0
	old := runner
	t.Cleanup(func() { runner = old })
	runner = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		cmd := name + " " + strings.Join(args, " ")
		switch {
		case cmd == "iscsiadm -m session":
			return sessions, nil
		case strings.HasSuffix(cmd, "--login"):
			logins++
			sessions = "tcp: [1] " + portal + ",1 " + iqn + " (non-flash)"
		}
		return "", nil
	}
	tr := &iscsi{log: slog.Default()}
	ports := []flasharray.Port{{IQN: iqn, Portal: portal}}
	connect := func() {
		t.Helper()
		if err := tr.Connect(context.Background(), ports); err != nil {
			t.Fatal(err)
		}
	}

	connect()
	connect()
	if logins != 1 {
		t.Fatalf("logins with the session up = %d, want 1", logins)
	}
	sessions = "" // the Pure plugin logged every session out
	connect()
	if logins != 2 {
		t.Fatalf("logins after the session went away = %d, want 2", logins)
	}
}

// fakeNVMe builds sys/<node> with the identity files a namespace node has.
func fakeNVMe(t *testing.T, root, node string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "sys", node)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The serial is split around Pure's OUI inside the NGUID, and the per-path
// nodes of native multipath carry the same identity without a /dev node.
func TestNVMeDeviceMatching(t *testing.T) {
	root := t.TempDir()
	old := sysClassBlock
	sysClassBlock = filepath.Join(root, "sys")
	t.Cleanup(func() { sysClassBlock = old })

	const serial = "6FEF9AB1157343EC000000AA"
	const nguid = "006fef9ab1157343" + "24a937" + "ec000000aa"
	if got := nvmeNGUID(serial); got != nguid {
		t.Fatalf("nguid = %s, want %s", got, nguid)
	}
	fakeNVMe(t, root, "nvme0c0n1", map[string]string{"wwid": "eui." + nguid})
	fakeNVMe(t, root, "nvme0c1n1", map[string]string{"wwid": "eui." + nguid})
	fakeNVMe(t, root, "nvme0n1", map[string]string{"wwid": "eui." + nguid})
	fakeNVMe(t, root, "nvme0n1p1", nil)
	fakeNVMe(t, root, "nvme0n2", map[string]string{"wwid": "eui.006fef9ab115734324a937ec000000bb"})

	tr := &nvmeTCP{log: slog.Default()}
	if got := tr.FindDevice(context.Background(), serial); got != "/dev/nvme0n1" {
		t.Fatalf("device = %q, want the head node /dev/nvme0n1", got)
	}
	if got := tr.FindDevice(context.Background(), "6fef9ab1157343ec000000cc"); got != "" {
		t.Fatalf("matched %q for a volume that is not attached", got)
	}

	// A namespace that also reports a UUID puts that in wwid; nguid still has it.
	fakeNVMe(t, root, "nvme1n1", map[string]string{
		"wwid":  "uuid.4a1b2c3d-0000-4000-8000-000000000001",
		"nguid": "006fef9a-b115-7343-24a9-37ec000000dd",
	})
	if got := tr.FindDevice(context.Background(), "6fef9ab1157343ec000000dd"); got != "/dev/nvme1n1" {
		t.Fatalf("device by nguid file = %q", got)
	}
}

func TestLiveControllers(t *testing.T) {
	root := t.TempDir()
	old := sysClassNVMe
	sysClassNVMe = root
	t.Cleanup(func() { sysClassNVMe = old })
	dir := filepath.Join(root, "nvme3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{
		"subsysnqn": "nqn.2010-06.com.purestorage:flasharray.test\n",
		"address":   "traddr=198.51.100.10,trsvcid=4420,src_addr=198.51.100.20\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	live := liveControllers()
	if !live["nqn.2010-06.com.purestorage:flasharray.test|198.51.100.10:4420"] || len(live) != 1 {
		t.Fatalf("live = %v", live)
	}
}
