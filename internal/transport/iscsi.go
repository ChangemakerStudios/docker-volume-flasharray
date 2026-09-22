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
	"strings"
	"sync"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
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

func multipathdRunning() bool {
	b, err := readFileTimeout("/proc/net/unix", 3*time.Second)
	return err == nil && bytes.Contains(b, []byte(multipathdAbstractSocket))
}

func (t *iscsi) Name() string { return "iscsi" }

func (t *iscsi) InitiatorIDs() ([]string, []string, error) {
	b, err := os.ReadFile(hostPath(initiatorNameFile))
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
func (t *iscsi) mpathMap(ctx context.Context, wwid string) string {
	out, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "show", "maps", "raw", "format", "%n %w")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.EqualFold(f[1], wwid) {
			return f[0]
		}
	}
	return ""
}

func (t *iscsi) findDevice(ctx context.Context, wwid string, useMultipath bool) string {
	if !useMultipath {
		if p := "/dev/disk/by-id/scsi-" + wwid; exists(p) {
			return p
		}
		return ""
	}
	if p := "/dev/disk/by-id/dm-uuid-mpath-" + wwid; exists(p) {
		return p
	}
	if name := t.mpathMap(ctx, wwid); name != "" && exists("/dev/mapper/"+name) {
		return "/dev/mapper/" + name
	}
	return ""
}

// WaitForDevice probes first, then escalates in rounds (session rescan, SCSI
// host scan, targeted `multipathd add path`) with a probe after every step so
// a LUN that surfaces mid-round is seen at once. It never runs `multipathd
// reconfigure`, which rebuilds every map on the host and can hide the very
// map being waited for.
func (t *iscsi) WaitForDevice(ctx context.Context, serial string) (string, error) {
	wwid := WWID(serial)
	useMultipath := multipathdRunning()
	if dev := t.findDevice(ctx, wwid, useMultipath); dev != "" {
		return dev, nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.opts.Timeout)
	defer cancel()
	steps := []func(){
		func() { t.rescan(ctx) },
		t.scanSCSIHosts,
		func() {
			if !useMultipath {
				return
			}
			for _, p := range sysBlockMatching("sd*", strings.TrimPrefix(wwid, "3")) {
				if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "add", "path", p); err != nil {
					t.log.Warn("multipathd add path failed", "path", p, "err", err)
				}
			}
		},
	}
	probe := func() (string, bool) {
		d := t.findDevice(ctx, wwid, useMultipath)
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
		if name := t.mpathMap(ctx, wwid); name != "" {
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
// since the kernel reuses sdX names as soon as they are freed.
func (t *iscsi) Detach(ctx context.Context, serial, _ string) error {
	if serial == "" {
		// WWID("") is the bare Pure prefix, which would match every FlashArray LUN on the host.
		return errors.New("detach: empty volume serial; refusing to touch host SCSI devices")
	}
	wwid := WWID(serial)
	needle := strings.TrimPrefix(wwid, "3")
	paths := sysBlockMatching("sd*", needle)
	if multipathdRunning() {
		if name := t.mpathMap(ctx, wwid); name != "" {
			if err := t.removeMap(ctx, name); err != nil {
				return err
			}
		}
	}
	var errs []error
	for _, p := range paths {
		if b, err := readFileTimeout(filepath.Join("/sys/class/block", p, "device", "wwid"), 3*time.Second); err == nil &&
			!strings.Contains(strings.ToLower(string(b)), needle) {
			t.log.Warn("not deleting path: device name now belongs to another LUN", "dev", p, "want", wwid, "have", strings.TrimSpace(string(b)))
			continue
		}
		if _, err := runTimeout(ctx, t.log, 10*time.Second, "blockdev", "--flushbufs", "/dev/"+p); err != nil {
			t.log.Warn("flush before path delete failed", "dev", p, "err", err)
		}
		if err := writeFileTimeout(filepath.Join("/sys/class/block", p, "device", "delete"), "1", 10*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (t *iscsi) removeMap(ctx context.Context, name string) error {
	dev := "/dev/mapper/" + name
	if dm, err := filepath.EvalSymlinks(dev); err == nil {
		if holders, _ := os.ReadDir(filepath.Join("/sys/class/block", filepath.Base(dm), "holders")); len(holders) > 0 {
			return fmt.Errorf("detach: %s is still in use (%d holder(s) in sysfs, e.g. %s); not removing it", dev, len(holders), holders[0].Name())
		}
	}
	if _, err := runTimeout(ctx, t.log, 5*time.Second, "multipathd", "disablequeueing", "map", name); err != nil {
		t.log.Warn("disable queueing failed", "map", name, "err", err)
	}
	if _, err := runTimeout(ctx, t.log, 5*time.Second, "dmsetup", "message", name, "0", "fail_if_no_path"); err != nil {
		t.log.Warn("fail_if_no_path failed", "map", name, "err", err)
	}
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "blockdev", "--flushbufs", dev); err != nil {
		t.log.Warn("flush map failed", "map", name, "err", err)
	}
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipathd", "remove", "map", name); err == nil && !exists(dev) {
		return nil
	}
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "multipath", "-f", name); err == nil {
		return nil
	}
	t.log.Warn("multipath -f failed; forcing removal with dmsetup", "map", name)
	if _, err := runTimeout(ctx, t.log, 10*time.Second, "dmsetup", "remove", "--force", "--retry", name); err != nil {
		return fmt.Errorf("remove multipath map %s: %w", name, err)
	}
	return nil
}

func (t *iscsi) PostDisconnect(context.Context) error { return nil }
