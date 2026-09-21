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

// run executes a command, returning combined output; the error message
// includes the output so callers can log one line.
func run(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	log.Debug("exec", "cmd", name+" "+strings.Join(args, " "), "rc", exitCode(err), "out", truncate(s, 300))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, truncate(s, 300))
	}
	return s, nil
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

// waitFor polls fn every 250ms until it returns a path or the context/timeout expires.
func waitFor(ctx context.Context, timeout time.Duration, fn func() (string, bool)) (string, error) {
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(250 * time.Millisecond)
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
			b, err := os.ReadFile(filepath.Join(m, f))
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

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
