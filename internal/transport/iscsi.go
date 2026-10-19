package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/hostexec"
)

// iscsi drives open-iscsi (iscsiadm) and relies on the host's multipathd to
// assemble the paths into a dm device. It deliberately never calls
// `multipathd reconfigure`: on multipath-tools 0.8.8+ a global reconfigure
// with dozens of paths is expensive and, when repeated in a wait loop, can
// starve the host. Attach/detach sequencing follows jt-pve-storage-purestorage
// (github.com/jasoncheng7115/jt-pve-storage-purestorage).
type iscsi struct {
	opts    Options
	allowed []*net.IPNet
	log     *slog.Logger

	mu       sync.Mutex
	loggedIn map[string]bool // "iqn|portal"

	// scanMu makes rescans exclusive with Exclusive callers, which change
	// what LUN numbers carry on this host.
	scanMu sync.Mutex
}

// Exclusive runs fn with rescans held off. A rescan between a detach and the
// array disconnect rebuilds the volume's paths onto a LUN about to be freed,
// and one in flight across a connect can scan a reassigned LUN under its old
// identity.
func (t *iscsi) Exclusive(fn func() error) error {
	t.scanMu.Lock()
	defer t.scanMu.Unlock()
	return fn()
}

const (
	iscsiadmExitSessionExists = 15
	iscsiadmExitNoObjects     = 21
	initiatorNameFile         = "/etc/iscsi/initiatorname.iscsi"
	// multipathdAbstractSocket is where multipathd listens. It is an abstract
	// socket, so it never appears under /run; the plugin sees it because it
	// shares the host network namespace.
	multipathdAbstractSocket = "@/org/kernel/linux/storage/multipathd"
)

// multipathdRunning is a variable so tests can pretend multipathd is up.
var multipathdRunning = func() bool {
	b, err := readFileTimeout("/proc/net/unix", 3*time.Second)
	return err == nil && bytes.Contains(b, []byte(multipathdAbstractSocket))
}

func (t *iscsi) Name() string { return "iscsi" }

func (t *iscsi) InitiatorIDs() ([]string, []string, error) {
	b, err := os.ReadFile(hostexec.Path(initiatorNameFile))
	if err != nil {
		return nil, nil, fmt.Errorf("read host %s (is open-iscsi installed?): %w", initiatorNameFile, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "InitiatorName=") {
			return []string{strings.TrimPrefix(line, "InitiatorName=")}, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("%s has no InitiatorName", initiatorNameFile)
}

func (t *iscsi) Connect(ctx context.Context, ports []flasharray.Port) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loggedIn == nil {
		t.loggedIn = map[string]bool{}
	}
	var errs []error
	n := 0
	for _, p := range ports {
		if p.IQN == "" || !portalAllowed(p.Portal, t.allowed) {
			continue
		}
		key := p.IQN + "|" + p.Portal
		if t.loggedIn[key] {
			n++
			continue
		}
		addr := p.Portal
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "3260")
		}
		if err := probePortal(ctx, addr); err != nil {
			errs = append(errs, err)
			continue
		}
		// Create the node record (idempotent) and set manual startup so the
		// host's iscsid does not race us at boot; the plugin owns these sessions.
		if _, err := run(ctx, t.log, "iscsiadm", "-m", "node", "-T", p.IQN, "-p", p.Portal, "-o", "new"); err != nil {
			t.log.Debug("node record create", "portal", p.Portal, "err", err)
		}
		if _, err := run(ctx, t.log, "iscsiadm", "-m", "node", "-T", p.IQN, "-p", p.Portal, "-o", "update", "-n", "node.startup", "-v", "manual"); err != nil {
			t.log.Warn("set node.startup=manual failed; host iscsid may also log in at boot", "portal", p.Portal, "err", err)
		}
		// Ride out a controller failover or switch reload instead of failing I/O;
		// set explicitly because a node record may carry a lower value.
		if _, err := run(ctx, t.log, "iscsiadm", "-m", "node", "-T", p.IQN, "-p", p.Portal, "-o", "update", "-n", "node.session.timeo.replacement_timeout", "-v", "120"); err != nil {
			t.log.Warn("set replacement_timeout failed", "portal", p.Portal, "err", err)
		}
		_, err := run(ctx, t.log, "iscsiadm", "-m", "node", "-T", p.IQN, "-p", p.Portal, "--login")
		if err != nil && exitCode(err) != iscsiadmExitSessionExists && !strings.Contains(err.Error(), "already present") {
			errs = append(errs, err)
			continue
		}
		t.loggedIn[key] = true
		n++
	}
	if n == 0 {
		if len(errs) > 0 {
			return fmt.Errorf("no iSCSI sessions established: %w", errors.Join(errs...))
		}
		return errors.New("no eligible iSCSI portals (check FA_ALLOWED_CIDRS and that the array has iSCSI ports)")
	}
	if len(errs) > 0 {
		t.log.Warn("some iSCSI portals failed to log in", "ok", n, "failed", len(errs), "err", errors.Join(errs...))
	}
	return nil
}

func (t *iscsi) rescan(ctx context.Context) {
	if _, err := runTimeout(ctx, t.log, 30*time.Second, "iscsiadm", "-m", "session", "--rescan"); err != nil && exitCode(err) != iscsiadmExitNoObjects {
		t.log.Warn("iscsi rescan", "err", err)
	}
}

// scanSCSIHosts asks every iSCSI SCSI host to enumerate new LUNs, which a
// session rescan alone sometimes leaves undone.
func (t *iscsi) scanSCSIHosts() {
	hosts, _ := filepath.Glob("/sys/class/iscsi_host/host*")
	for _, h := range hosts {
		f := filepath.Join("/sys/class/scsi_host", filepath.Base(h), "scan")
		if err := writeFileTimeout(f, "- - -", 10*time.Second); err != nil {
			t.log.Warn("scsi host scan", "host", filepath.Base(h), "err", err)
		}
	}
}

// mpathMap returns the name of multipathd's map for wwid, or "". Asking
// multipathd directly doesn't depend on udev having created by-id links yet.
func (t *iscsi) mpathMap(ctx context.Context, wwid string) (string, error) {
	out, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "show", "maps", "raw", "format", "%n %w")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.EqualFold(f[1], wwid) {
			return f[0], nil
		}
	}
	return "", nil
}

// findDevice only returns a path whose sysfs identity is wwid: a stale udev
// link once mounted one volume's filesystem from another volume's LUN.
// With requirePaths a multipath map counts only once it has a path: one can
// outlive its paths, or exist before multipathd adds them, and is no device to
// mount. Leftover checks pass false, since an open map without paths is
// exactly the stale mount they must see.
func (t *iscsi) findDevice(ctx context.Context, wwid string, useMultipath, requirePaths bool) string {
	if !useMultipath {
		p := "/dev/disk/by-id/scsi-" + wwid
		if exists(p) {
			if sdCarries(p, wwid) {
				return p
			}
			t.log.Warn("ignoring stale by-id link: it points at another LUN", "link", p, "wwid", wwid)
		}
		return ""
	}
	if name, _ := t.mpathMap(ctx, wwid); name != "" {
		if p := "/dev/mapper/" + name; dmCarries(p, wwid) && (!requirePaths || hasPaths(p)) {
			return p
		}
	}
	p := "/dev/disk/by-id/dm-uuid-mpath-" + wwid
	if exists(p) {
		if !dmCarries(p, wwid) {
			t.log.Warn("ignoring stale by-id link: it points at another LUN", "link", p, "wwid", wwid)
		} else if !requirePaths || hasPaths(p) {
			return p
		}
	}
	return ""
}

// pathvec returns multipathd's own record of each path's WWID (sdX -> wwid).
// A path it lists without an identity yet maps to "".
func (t *iscsi) pathvec(ctx context.Context) (map[string]string, error) {
	out, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "show", "paths", "raw", "format", "%d %w")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		switch f := strings.Fields(line); len(f) {
		case 1:
			m[f[0]] = ""
		case 2:
			m[f[0]] = strings.ToLower(f[1])
		}
	}
	return m, nil
}

// udevDataDir holds udev's per-device properties; a variable so tests can
// plant them.
var udevDataDir = hostexec.Path("/run/udev/data")

// udevWWID is the identity udev recorded for SCSI disk sd (ID_SERIAL), which
// is what multipathd groups paths by. ok is false only when udev's database
// can't be read at all; "" with ok means udev has no identity for sd yet.
func udevWWID(sd string) (wwid string, ok bool) {
	if _, err := os.Stat(udevDataDir); err != nil {
		return "", false
	}
	b, err := readFileTimeout(filepath.Join(sysClassBlock, sd, "dev"), 3*time.Second)
	if err != nil {
		return "", true
	}
	data, err := readFileTimeout(filepath.Join(udevDataDir, "b"+strings.TrimSpace(string(b))), 3*time.Second)
	if err != nil {
		return "", true
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, found := strings.CutPrefix(line, "E:ID_SERIAL="); found {
			return strings.ToLower(strings.TrimSpace(v)), true
		}
	}
	return "", true
}

// reprobe has the kernel re-announce sd so udev reads its identity again.
func (t *iscsi) reprobe(sd string) {
	if err := writeFileTimeout(filepath.Join(sysClassBlock, sd, "uevent"), "change", 5*time.Second); err != nil {
		t.log.Warn("asking udev to re-read a path failed", "path", sd, "err", err)
	}
}

// addPath asks multipathd to take p for wwid, but only when that can help.
// multipathd groups paths by the identity udev recorded and keeps what it read
// per sdX name, so after the kernel reuses a name for another LUN a stale udev
// record or multipathd entry files the path under the old volume. On staging
// that put one volume's paths into another's map, and re-added paths the
// membership check had just pulled out. A path udev has not re-read yet is
// re-announced instead of added, a stale multipathd entry is dropped first,
// and one multipathd already has right is left alone: "add path" only fails
// for it, and under load that floods a daemon that is already behind.
func (t *iscsi) addPath(ctx context.Context, p, wwid string, known map[string]string) {
	wwid = strings.ToLower(wwid)
	have, inPathvec := known[p]
	if inPathvec && have == wwid {
		return
	}
	if u, ok := udevWWID(p); ok && u != wwid {
		if u == "" {
			// udev is still on this new path; multipathd gets it when udev is done.
			return
		}
		t.log.Warn("udev still files this path under another volume; asking it to re-read the path before multipathd takes it", "path", p, "udev_wwid", u, "wwid", wwid)
		t.reprobe(p)
		return
	}
	if inPathvec {
		t.log.Warn("multipathd remembers this path as another volume; dropping its entry first", "path", p, "multipathd_wwid", have, "wwid", wwid)
		if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "del", "path", p); err != nil {
			t.log.Warn("multipathd del path failed; not adding it", "path", p, "err", err)
			return
		}
	}
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "add", "path", p); err != nil {
		t.log.Warn("multipathd add path failed", "path", p, "err", err)
	}
}

// forgetPoll and forgetWait are variables so tests needn't wait.
var (
	forgetPoll = 250 * time.Millisecond
	forgetWait = 20 * time.Second
)

// forgetDeleted waits until multipathd has dropped the deleted SCSI paths,
// removing any it still keeps once forgetWait is up. The kernel gives a freed
// sdX name to the next disk it scans, and while multipathd still holds the old
// entry it files that disk under the old volume. On staging a burst of
// attaches left multipathd behind on its uevents, and a rollback's freed names
// came back as other volumes' paths inside the wrong maps.
func (t *iscsi) forgetDeleted(ctx context.Context, deleted []string) error {
	if len(deleted) == 0 || !multipathdRunning() {
		return nil
	}
	held := func() ([]string, error) {
		known, err := t.pathvec(ctx)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, p := range deleted {
			if _, ok := known[p]; ok {
				out = append(out, p)
			}
		}
		return out, nil
	}
	deadline := time.Now().Add(forgetWait)
	for {
		left, err := held()
		if err == nil && len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			for _, p := range left {
				if _, derr := runTimeout(context.WithoutCancel(ctx), t.log, 10*time.Second, "multipathd", "del", "path", p); derr != nil {
					t.log.Warn("multipathd del path failed", "path", p, "err", derr)
				}
			}
			if left, err = held(); err == nil && len(left) == 0 {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%w: cannot ask multipathd: %w", ErrPathsHeld, err)
			}
			return fmt.Errorf("%w: %v", ErrPathsHeld, left)
		}
		select {
		case <-ctx.Done():
		case <-time.After(forgetPoll):
		}
	}
}

// kernelWWID is a SCSI disk's WWID as the kernel read it ("3624a9370..."), or
// "" if it cannot be read.
func kernelWWID(sd string) string {
	b, err := readFileTimeout(filepath.Join(sysClassBlock, sd, "device", "wwid"), 3*time.Second)
	if err != nil {
		return ""
	}
	id := strings.ToLower(strings.TrimSpace(string(b)))
	if rest, ok := strings.CutPrefix(id, "naa."); ok {
		return "3" + rest
	}
	return ""
}

// mapWWID is the WWID a multipath map serves, from its dm uuid, or "".
func mapWWID(dm string) string {
	b, err := readFileTimeout(filepath.Join(sysClassBlock, dm, "dm", "uuid"), 3*time.Second)
	if err != nil {
		return ""
	}
	w, _ := strings.CutPrefix(strings.ToLower(strings.TrimSpace(string(b))), "mpath-")
	return w
}

// fixMembership pulls every path of this array out of a map that serves a
// different volume, then lets multipathd re-read it so it joins its own map.
// The LUN check in Sweep cannot see this: the path's identity and LUN agree
// with the array, it is only in the wrong map. It also drops multipathd
// entries that file a path under another volume while in no map yet: the
// mount check refuses while one names the volume being mounted, and nothing
// else would ever clear it.
func (t *iscsi) fixMembership(ctx context.Context, arrayPrefix string) {
	dms, _ := filepath.Glob(filepath.Join(sysClassBlock, "dm-*"))
	for _, d := range dms {
		dm := filepath.Base(d)
		mw := mapWWID(dm)
		if !strings.HasPrefix(mw, "3624a9370"+arrayPrefix) {
			continue
		}
		slaves, _ := os.ReadDir(filepath.Join(d, "slaves"))
		for _, sl := range slaves {
			kw := kernelWWID(sl.Name())
			if kw == "" || kw == mw {
				continue
			}
			b, _ := readFileTimeout(filepath.Join(d, "dm", "name"), 3*time.Second)
			name := strings.TrimSpace(string(b))
			t.log.Error("path is in another volume's map; pulling it out", "path", sl.Name(), "map", name, "map_wwid", mw, "path_wwid", kw)
			t.refile(ctx, sl.Name(), kw, true)
		}
	}
	if !multipathdRunning() {
		return
	}
	known, err := t.pathvec(ctx)
	if err != nil {
		t.log.Warn("cannot read multipathd's paths to check their identities", "err", err)
		return
	}
	for sd, w := range known {
		kw := kernelWWID(sd)
		if w == "" || kw == "" || w == kw || !strings.HasPrefix(kw, "3624a9370"+arrayPrefix) {
			continue
		}
		t.log.Warn("multipathd files a path under another volume; dropping its entry", "path", sd, "multipathd_wwid", w, "path_wwid", kw)
		t.refile(ctx, sd, kw, false)
	}
}

// refile drops multipathd's entry for sd (failing the path first if it sits
// in a map) and has it read sd again as kw. Re-added while udev still files
// it under the old volume, the path would go straight back to it, so then
// udev is asked to re-read sd and the next sweep adds it.
func (t *iscsi) refile(ctx context.Context, sd, kw string, inMap bool) {
	var steps [][]string
	if inMap {
		steps = append(steps, []string{"fail", "path", sd})
	}
	steps = append(steps, []string{"del", "path", sd})
	u, ok := udevWWID(sd)
	readd := !ok || u == kw
	if readd {
		steps = append(steps, []string{"add", "path", sd})
	}
	for _, args := range steps {
		if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", args...); err != nil {
			t.log.Warn("multipathd "+args[0]+" path failed", "path", sd, "err", err)
		}
	}
	if !readd {
		t.reprobe(sd)
	}
}

// hasPaths reports whether a multipath map has any path.
func hasPaths(dev string) bool {
	resolved, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return false
	}
	slaves, err := os.ReadDir(filepath.Join(sysClassBlock, filepath.Base(resolved), "slaves"))
	return err == nil && len(slaves) > 0
}

func (t *iscsi) FindDevice(ctx context.Context, serial string) string {
	return t.findDevice(ctx, WWID(serial), multipathdRunning(), false)
}

// WaitForDevice probes first, then escalates in rounds (session rescan, SCSI
// host scan, targeted `multipathd add path`) with a probe after every step so
// a LUN that surfaces mid-round is seen at once. It never runs `multipathd
// reconfigure`, which rebuilds every map on the host and can hide the very
// map being waited for.
func (t *iscsi) WaitForDevice(ctx context.Context, serial string) (string, error) {
	wwid := WWID(serial)
	useMultipath := multipathdRunning()
	if dev := t.findDevice(ctx, wwid, useMultipath, true); dev != "" {
		return dev, nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.opts.Timeout)
	defer cancel()
	scan := func(f func()) func() {
		return func() {
			t.scanMu.Lock()
			defer t.scanMu.Unlock()
			f()
		}
	}
	steps := []func(){
		scan(func() { t.rescan(ctx) }),
		scan(t.scanSCSIHosts),
		func() {
			if !useMultipath {
				return
			}
			known, err := t.pathvec(ctx)
			if err != nil {
				t.log.Warn("cannot read multipathd's paths; not adding any", "err", err)
				return
			}
			for _, p := range sysBlockMatching("sd*", strings.TrimPrefix(wwid, "3")) {
				t.addPath(ctx, p, wwid, known)
			}
		},
	}
	probe := func() (string, bool) {
		d := t.findDevice(ctx, wwid, useMultipath, true)
		return d, d != ""
	}
	for {
		for _, step := range steps {
			step()
			if dev, ok := probe(); ok {
				return dev, nil
			}
			if ctx.Err() != nil {
				return "", fmt.Errorf("%w\n%s", ErrTimeout, t.describe(wwid, useMultipath))
			}
		}
		// Cheap polling until the next round.
		if dev, err := waitFor(ctx, 5*time.Second, time.Second, probe); err == nil {
			return dev, nil
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w\n%s", ErrTimeout, t.describe(wwid, useMultipath))
		}
	}
}

// describe summarizes what the host knows about wwid for the timeout error:
// whether the SCSI paths arrived, whether multipathd built a map, whether
// udev made the links. Bounded and best effort.
func (t *iscsi) describe(wwid string, useMultipath bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var b strings.Builder
	paths := sysBlockMatching("sd*", strings.TrimPrefix(wwid, "3"))
	fmt.Fprintf(&b, "  scsi paths for %s: %d %v\n", wwid, len(paths), paths)
	if len(paths) == 0 {
		b.WriteString("  (no paths: the LUN never reached this host; check the array connection, sessions and FA_ALLOWED_CIDRS)\n")
	}
	switch {
	case !useMultipath:
		b.WriteString("  multipathd: not running (no " + multipathdAbstractSocket + " in /proc/net/unix); expecting a single-path device\n")
	default:
		if name, _ := t.mpathMap(ctx, wwid); name != "" {
			fmt.Fprintf(&b, "  multipath map: %s (/dev/mapper/%s present: %v)\n", name, name, exists("/dev/mapper/"+name))
		} else {
			b.WriteString("  multipath map: none (paths not claimed; check the multipath.conf blacklist and find_multipaths)\n")
		}
	}
	links, _ := filepath.Glob("/dev/disk/by-id/*" + wwid)
	fmt.Fprintf(&b, "  /dev/disk/by-id links: %v", links)
	return b.String()
}

// Detach tears down the host side of one LUN before the array connection is
// removed. Order matters: with queue_if_no_path, flushing a map whose paths
// are gone blocks forever, so queueing is switched off first; only this map
// is removed (never `multipath -F`, which flushes every unused map on the
// host); and each path is checked to still carry this WWID before deletion,
// since the kernel reuses sdX names as soon as they are freed. With want (the
// array's LUN map for this host), a path whose LUN no longer carries this
// volume is deleted unflushed first: its cached WWID is stale, and anything
// flushed through it would land in whichever volume holds that LUN now.
func (t *iscsi) Detach(ctx context.Context, serial, _ string, want map[int]string) error {
	if serial == "" {
		// WWID("") is the bare Pure prefix, which would match every FlashArray LUN on the host.
		return errors.New("detach: empty volume serial; refusing to touch host SCSI devices")
	}
	wwid := WWID(serial)
	needle := strings.TrimPrefix(wwid, "3")
	var mapName string
	if multipathdRunning() {
		// Deleting paths under a map we could not find leaves it queueing I/O
		// with nothing beneath it.
		name, err := t.mpathMap(ctx, wwid)
		if err != nil {
			return fmt.Errorf("detach: look up multipath map for %s: %w", wwid, err)
		}
		mapName = name
	}
	var errs []error
	var gone []string
	paths := sysBlockMatching("sd*", needle)
	if want != nil {
		var valid []string
		for _, p := range paths {
			if lun, ok := pathLUN(filepath.Join(sysClassBlock, p)); ok && want[lun] == strings.ToLower(serial) {
				valid = append(valid, p)
				continue
			}
			t.log.Warn("deleting path unflushed: its LUN no longer carries this volume", "dev", p, "serial", serial)
			if err := deletePath(p); err != nil {
				errs = append(errs, err)
			} else {
				gone = append(gone, p)
			}
		}
		paths = valid
	}
	if mapName != "" {
		// With no path left that reaches the volume, a flush has nowhere right to go.
		if err := t.removeMap(ctx, mapName, want == nil || len(paths) > 0); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	for _, p := range paths {
		if b, err := readFileTimeout(filepath.Join(sysClassBlock, p, "device", "wwid"), 3*time.Second); err == nil &&
			!strings.Contains(strings.ToLower(string(b)), needle) {
			t.log.Warn("not deleting path: device name now belongs to another LUN", "dev", p, "want", wwid, "have", strings.TrimSpace(string(b)))
			continue
		}
		if _, err := runTimeout(ctx, t.log, 10*time.Second, "blockdev", "--flushbufs", "/dev/"+p); err != nil {
			t.log.Warn("flush before path delete failed", "dev", p, "err", err)
		}
		if err := deletePath(p); err != nil {
			errs = append(errs, err)
		} else {
			gone = append(gone, p)
		}
	}
	if err := t.forgetDeleted(ctx, gone); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// deletePath removes a SCSI disk; one already gone counts as removed.
func deletePath(p string) error {
	err := writeFileTimeout(filepath.Join(sysClassBlock, p, "device", "delete"), "1", 10*time.Second)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}

func (t *iscsi) removeMap(ctx context.Context, name string, flush bool) error {
	dev := "/dev/mapper/" + name
	if dm, err := filepath.EvalSymlinks(dev); err == nil {
		if holders, _ := os.ReadDir(filepath.Join(sysClassBlock, filepath.Base(dm), "holders")); len(holders) > 0 {
			return fmt.Errorf("detach: %s is still in use (%d holder(s) in sysfs, e.g. %s); not removing it", dev, len(holders), holders[0].Name())
		}
	}
	t.stopQueueing(ctx, name)
	return t.dropMap(ctx, name, flush)
}

// stopQueueing makes I/O on map name fail once no path is usable, instead of
// queueing forever. multipathd re-applies no_path_retry on every table reload,
// so both its own switch and the dm message are needed.
func (t *iscsi) stopQueueing(ctx context.Context, name string) {
	if _, err := runTimeout(ctx, t.log, 5*time.Second, "multipathd", "disablequeueing", "map", name); err != nil {
		t.log.Warn("disable queueing failed", "map", name, "err", err)
	}
	if _, err := runTimeout(ctx, t.log, 5*time.Second, "dmsetup", "message", name, "0", "fail_if_no_path"); err != nil {
		t.log.Warn("fail_if_no_path failed", "map", name, "err", err)
	}
}

// dropMap removes map name, flushing its buffers first only when asked: that
// is right only while every path still reaches the map's volume.
func (t *iscsi) dropMap(ctx context.Context, name string, flush bool) error {
	dev := "/dev/mapper/" + name
	if flush {
		if _, err := runTimeout(ctx, t.log, 10*time.Second, "blockdev", "--flushbufs", dev); err != nil {
			t.log.Warn("flush map failed", "map", name, "err", err)
		}
	}
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "remove", "map", name); err == nil && !exists(dev) {
		return nil
	}
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipath", "-f", name); err == nil {
		return nil
	}
	// A forced remove swaps an open map's table for an error target, which is
	// the same as pulling the disk from under a filesystem still flushing.
	if out, err := runTimeout(ctx, t.log, 5*time.Second, "dmsetup", "info", "-c", "--noheadings", "-o", "open", name); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n > 0 {
			return fmt.Errorf("detach: multipath map %s is still open (%d); not forcing its removal", name, n)
		}
	}
	t.log.Warn("multipath -f failed; forcing removal with dmsetup", "map", name)
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "dmsetup", "remove", "--force", "--retry", name); err != nil {
		return fmt.Errorf("remove multipath map %s: %w", name, err)
	}
	return nil
}

func (t *iscsi) PostDisconnect(context.Context) error { return nil }

// purePrefix is the NAA prefix of every FlashArray LUN's WWID in sysfs.
const purePrefix = "naa.624a9370"

// Sweep deletes every SCSI path from this array whose LUN number the array
// gives to a different volume than the path's cached WWID names, or to none.
// Linux reads a path's identity once, at scan time: when a volume is
// disconnected while the host keeps its paths (a failed detach, or a peer
// preempting this host) and the array hands the LUN number to the next volume
// connected, the old path still claims the old volume while its I/O reaches
// the new one. On staging that cross-wrote volumes.
//
// Only paths whose serial starts with arrayPrefix are judged: LUN numbers are
// per array, and another array's paths are none of this map's business. A map
// whose every path is stale has queueing switched off first, so whatever still
// holds it gets I/O errors rather than hanging, and is removed unflushed
// afterwards (its buffers belong to a volume it no longer reaches). Only a
// stale path on ownLUN that cannot be deleted is an error; others are logged,
// so one wedged device does not block every mount on the node.
//
// crossedOnly limits the sweep to paths on a LUN the array gives to a
// different volume. A LUN missing from want then proves nothing: I/O to an
// unmapped LUN fails on its own, and a list missing an entry must never be
// grounds to cut a live volume's path.
func (t *iscsi) Sweep(ctx context.Context, want map[int]string, arrayPrefix string, ownLUN int, crossedOnly bool) error {
	arrayPrefix = strings.ToLower(arrayPrefix)
	devs, _ := filepath.Glob(filepath.Join(sysClassBlock, "sd*"))
	stale := map[string]int{} // sd -> LUN
	for _, p := range devs {
		b, err := readFileTimeout(filepath.Join(p, "device", "wwid"), 3*time.Second)
		if err != nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(string(b)))
		serial, ok := strings.CutPrefix(id, purePrefix)
		if !ok || !strings.HasPrefix(serial, arrayPrefix) {
			continue
		}
		lun, ok := pathLUN(p)
		if !ok || want[lun] == serial || (crossedOnly && want[lun] == "") {
			continue
		}
		t.log.Warn("deleting stale path: the array gives its LUN to another volume", "dev", filepath.Base(p), "lun", lun, "path_serial", serial, "array_serial", want[lun])
		stale[filepath.Base(p)] = lun
	}
	defer t.fixMembership(ctx, arrayPrefix)
	if len(stale) == 0 {
		return nil
	}
	var whollyStale []string
	for dm, name := range holderMaps(stale) {
		slaves, _ := os.ReadDir(filepath.Join(sysClassBlock, dm, "slaves"))
		all := true
		for _, sl := range slaves {
			if _, ok := stale[sl.Name()]; !ok {
				all = false
			}
		}
		if all {
			t.stopQueueing(ctx, name)
			whollyStale = append(whollyStale, name)
		}
	}
	var errs []error
	var gone []string
	for sd, lun := range stale {
		if err := deletePath(sd); err != nil {
			if lun == ownLUN {
				errs = append(errs, err)
			} else {
				t.log.Error("could not delete stale path", "dev", sd, "lun", lun, "err", err)
			}
		} else {
			gone = append(gone, sd)
		}
	}
	for _, name := range whollyStale {
		if err := t.dropMap(ctx, name, false); err != nil {
			t.log.Error("map left without paths by the sweep is still open; whatever holds it gets I/O errors", "map", name, "err", err)
		}
	}
	if err := t.forgetDeleted(ctx, gone); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// holderMaps returns dm device -> map name for the maps holding paths.
func holderMaps(paths map[string]int) map[string]string {
	out := map[string]string{}
	for sd := range paths {
		holders, _ := os.ReadDir(filepath.Join(sysClassBlock, sd, "holders"))
		for _, h := range holders {
			if b, err := readFileTimeout(filepath.Join(sysClassBlock, h.Name(), "dm", "name"), 3*time.Second); err == nil {
				out[h.Name()] = strings.TrimSpace(string(b))
			}
		}
	}
	return out
}

// CheckLUN also refuses while multipathd files a path of another volume under
// this map's WWID: it adds such a path whenever it next handles it, and on
// staging that happened between this check passing and the mount, so the
// mount read one volume's log through another's paths.
func (t *iscsi) CheckLUN(ctx context.Context, dev string, lun int) error {
	resolved, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return err
	}
	base := filepath.Base(resolved)
	paths := []string{base}
	if slaves, err := os.ReadDir(filepath.Join(sysClassBlock, base, "slaves")); err == nil && len(slaves) > 0 {
		paths = paths[:0]
		for _, s := range slaves {
			paths = append(paths, s.Name())
		}
	}
	mw := mapWWID(base)
	for _, p := range paths {
		got, ok := pathLUN(filepath.Join(sysClassBlock, p))
		if !ok || got != lun {
			return fmt.Errorf("%s: path %s is not on LUN %d, where the array connected the volume", dev, p, lun)
		}
		if mw != "" && kernelWWID(p) != mw {
			return fmt.Errorf("%s: path %s belongs to another volume (%s)", dev, p, kernelWWID(p))
		}
	}
	if mw == "" || !multipathdRunning() {
		return nil
	}
	known, err := t.pathvec(ctx)
	if err != nil {
		return fmt.Errorf("%s: cannot ask multipathd which paths it files under this volume: %w", dev, err)
	}
	for p, w := range known {
		if w != mw {
			continue
		}
		if kw := kernelWWID(p); kw != "" && kw != mw {
			return fmt.Errorf("%s: multipathd files path %s under this volume, but it reaches another volume (%s)", dev, p, kw)
		}
	}
	return nil
}

// pathLUN reads a SCSI disk's LUN from its device link, .../H:C:T:L.
func pathLUN(sysBlock string) (int, bool) {
	target, err := filepath.EvalSymlinks(filepath.Join(sysBlock, "device"))
	if err != nil {
		return 0, false
	}
	f := strings.Split(filepath.Base(target), ":")
	if len(f) != 4 {
		return 0, false
	}
	n, err := strconv.Atoi(f[3])
	return n, err == nil
}
