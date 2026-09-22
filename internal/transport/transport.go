// Package transport moves block devices between the array and the host over
// iSCSI or NVMe/TCP. The driver only ever sees a device path.
package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
)

// Transport is the host-side half of an attach.
type Transport interface {
	// Name is "iscsi" or "nvme-tcp".
	Name() string
	// InitiatorIDs returns this host's IQNs and NQNs for host-object creation.
	InitiatorIDs() (iqns, nqns []string, err error)
	// Connect establishes sessions to every eligible array port. Idempotent;
	// called at plugin start (optional) and before every attach.
	Connect(ctx context.Context, ports []flasharray.Port) error
	// WaitForDevice rescans and blocks until the block device for the volume
	// serial appears, returning its path. Called after the array connection is
	// made.
	WaitForDevice(ctx context.Context, serial string) (string, error)
	// Detach removes host-side state for a device before the array connection
	// is deleted (flush multipath map, delete SCSI paths). Best effort.
	Detach(ctx context.Context, serial, devPath string) error
	// PostDisconnect runs after the array connection is deleted (e.g. NVMe
	// namespace rescan). Best effort.
	PostDisconnect(ctx context.Context) error
}

// ErrTimeout is returned when a device doesn't appear within the deadline.
var ErrTimeout = errors.New("timed out waiting for device")

// Options shared by both transports.
type Options struct {
	AllowedCIDRs []string
	Timeout      time.Duration
	Logger       *slog.Logger
}

// New picks a transport by name.
func New(name string, o Options) (Transport, error) {
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	nets, err := parseCIDRs(o.AllowedCIDRs)
	if err != nil {
		return nil, err
	}
	switch name {
	case "iscsi":
		return &iscsi{opts: o, allowed: nets, log: o.Logger.With("transport", "iscsi")}, nil
	case "nvme-tcp":
		return &nvmeTCP{opts: o, allowed: nets, log: o.Logger.With("transport", "nvme-tcp")}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", name)
	}
}

// WWID returns the SCSI WWID (NAA-3 identifier) FlashArray presents for a volume serial.
func WWID(serial string) string { return "3624a9370" + strings.ToLower(serial) }

func parseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("allowed CIDR %q: %w", c, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// portalAllowed applies the CIDR allow-list (empty list = everything).
func portalAllowed(portal string, allowed []*net.IPNet) bool {
	if len(allowed) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(portal)
	if err != nil {
		host = portal
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, n := range allowed {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostTools are clients of host daemons (iscsid, multipathd, udev) or read
// host config (/etc/iscsi, /etc/multipath.conf, /etc/nvme). They run as the
// host's own binaries in the host mount and IPC namespaces, as the Pure plugin
// did: iscsiadm and iscsid speak a binary IPC that breaks across open-iscsi
// versions ("initiator reported error (12 - iSCSI driver not found)"), and
// dmsetup/multipath wait on udev through SysV semaphores that only exist in
// the host IPC namespace.
var hostTools = map[string]bool{"iscsiadm": true, "multipathd": true, "multipath": true, "dmsetup": true, "nvme": true}

// onHost reports whether the plugin shares the host PID namespace (plugin
// config pidhost), which is what makes PID 1 the host's init.
var onHost = os.Getpid() != 1

// hostPath maps a host file path to one readable from the plugin.
func hostPath(p string) string {
	if onHost {
		return "/proc/1/root" + p
	}
	return p
}

// run executes a command, returning combined output; the error message
// includes the output so callers can log one line.
func run(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
	bin, argv := name, args
	if onHost && hostTools[name] {
		bin, argv = "nsenter", append([]string{"--target", "1", "--mount", "--ipc", "--", name}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	log.Debug("exec", "cmd", name+" "+strings.Join(args, " "), "rc", exitCode(err), "out", truncate(s, 300))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, truncate(s, 300))
	}
	return s, nil
}

// runTimeout is run with its own deadline, for cleanup steps that must not
// eat the whole request budget when a device is wedged.
func runTimeout(ctx context.Context, log *slog.Logger, d time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return run(ctx, log, name, args...)
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// waitFor polls fn every interval until it returns a path or the context/timeout expires.
func waitFor(ctx context.Context, timeout, interval time.Duration, fn func() (string, bool)) (string, error) {
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if p, ok := fn(); ok {
			return p, nil
		}
		if time.Now().After(deadline) {
			return "", ErrTimeout
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// sysBlockMatching returns /sys/class/block entries whose wwid file contains
// needle (lowercase compare). pattern is a glob like "sd*" or "nvme*n*".
func sysBlockMatching(pattern, needle string) []string {
	needle = strings.ToLower(needle)
	matches, _ := filepath.Glob("/sys/class/block/" + pattern)
	var out []string
	for _, m := range matches {
		for _, f := range []string{"wwid", "device/wwid"} {
			b, err := readFileTimeout(filepath.Join(m, f), 3*time.Second)
			if err != nil {
				continue
			}
			if strings.Contains(strings.ToLower(string(b)), needle) {
				out = append(out, filepath.Base(m))
				break
			}
		}
	}
	return out
}

// probePortal checks the portal accepts TCP before iscsiadm/nvme spend their
// much longer login timeouts on it.
func probePortal(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("portal %s unreachable: %w", addr, err)
	}
	return conn.Close()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// readFileTimeout and writeFileTimeout bound sysfs access, which can block in
// uninterruptible sleep on a dead device. On timeout the goroutine is
// abandoned (it cannot be interrupted); the caller moves on.
func readFileTimeout(path string, d time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := os.ReadFile(path)
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(d):
		return nil, fmt.Errorf("read %s: timed out after %s", path, d)
	}
}

func writeFileTimeout(path, data string, d time.Duration) error {
	ch := make(chan error, 1)
	go func() { ch <- os.WriteFile(path, []byte(data), 0o200) }()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		return fmt.Errorf("write %s: timed out after %s", path, d)
	}
}
