package transport

import (
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

// nvmeTCP drives nvme-cli against FlashArray NVMe/TCP ports. With the kernel's
// native NVMe multipath (nvme_core.multipath=Y, the default on modern
// kernels) every controller to the same subsystem NQN collapses into one
// /dev/nvmeXnY, so there is no dm-multipath, no multipathd and no
// /etc/multipath.conf involved — and roughly one block device per volume
// instead of one per path.
type nvmeTCP struct {
	opts    Options
	allowed []*net.IPNet
	log     *slog.Logger

	mu        sync.Mutex
	connected map[string]bool // "nqn|portal"
	nqns      map[string]bool // subsystem NQNs we connected to
}

const hostNQNFile = "/etc/nvme/hostnqn"

func (t *nvmeTCP) Name() string { return "nvme-tcp" }

func (t *nvmeTCP) InitiatorIDs() ([]string, []string, error) {
	b, err := os.ReadFile(hostNQNFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (is /etc/nvme bind-mounted from the host? generate with `nvme gen-hostnqn`): %w", hostNQNFile, err)
	}
	nqn := strings.TrimSpace(string(b))
	if nqn == "" {
		return nil, nil, fmt.Errorf("%s is empty", hostNQNFile)
	}
	return nil, []string{nqn}, nil
}

func (t *nvmeTCP) Connect(ctx context.Context, ports []flasharray.Port) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.connected == nil {
		t.connected = map[string]bool{}
		t.nqns = map[string]bool{}
	}
	var errs []error
	n := 0
	for _, p := range ports {
		if p.NQN == "" || !portalAllowed(p.Portal, t.allowed) {
			continue
		}
		key := p.NQN + "|" + p.Portal
		if t.connected[key] {
			n++
			continue
		}
		host, port, err := net.SplitHostPort(p.Portal)
		if err != nil {
			host, port = p.Portal, "4420"
		}
		out, err := run(ctx, t.log, "nvme", "connect", "-t", "tcp", "-a", host, "-s", port, "-n", p.NQN,
			"--ctrl-loss-tmo", "-1", "--reconnect-delay", "5")
		if err != nil && !strings.Contains(strings.ToLower(out), "already connected") {
			errs = append(errs, err)
			continue
		}
		t.connected[key] = true
		t.nqns[p.NQN] = true
		n++
	}
	if n == 0 {
		if len(errs) > 0 {
			return fmt.Errorf("no NVMe/TCP controllers connected: %w", errors.Join(errs...))
		}
		return errors.New("no eligible NVMe/TCP portals (check FA_ALLOWED_CIDRS and that the array has NVMe/TCP services enabled)")
	}
	if len(errs) > 0 {
		t.log.Warn("some NVMe/TCP portals failed to connect", "ok", n, "failed", len(errs), "err", errors.Join(errs...))
	}
	return nil
}

// controllers returns /dev/nvmeN for every controller attached to one of our subsystem NQNs.
func (t *nvmeTCP) controllers() []string {
	t.mu.Lock()
	nqns := make(map[string]bool, len(t.nqns))
	for k, v := range t.nqns {
		nqns[k] = v
	}
	t.mu.Unlock()
	matches, _ := filepath.Glob("/sys/class/nvme/nvme*")
	var out []string
	for _, m := range matches {
		b, err := os.ReadFile(filepath.Join(m, "subsysnqn"))
		if err != nil {
			continue
		}
		if len(nqns) == 0 || nqns[strings.TrimSpace(string(b))] {
			out = append(out, "/dev/"+filepath.Base(m))
		}
	}
	return out
}

func (t *nvmeTCP) rescan(ctx context.Context) {
	for _, c := range t.controllers() {
		if _, err := run(ctx, t.log, "nvme", "ns-rescan", c); err != nil {
			t.log.Warn("nvme ns-rescan failed", "ctrl", c, "err", err)
		}
	}
}

// WaitForDevice matches the namespace by the volume serial embedded in its
// NGUID/EUI-64 (FlashArray derives both from the volume serial). The match is
// a case-insensitive substring test against the kernel wwid, which is robust
// to the exact encoding the array uses.
func (t *nvmeTCP) WaitForDevice(ctx context.Context, serial string) (string, error) {
	t.rescan(ctx)
	start := time.Now()
	lastRescan := start
	return waitFor(ctx, t.opts.Timeout, func() (string, bool) {
		if devs := sysBlockMatching("nvme*n*", serial); len(devs) > 0 {
			// With native multipath the head node (nvmeXnY under the subsystem)
			// is what /sys/class/block lists; per-path nodes are nvmeXcYnZ and
			// are excluded by the glob.
			return "/dev/" + devs[0], true
		}
		if time.Since(lastRescan) > 5*time.Second {
			lastRescan = time.Now()
			t.rescan(ctx)
		}
		return "", false
	})
}

// Detach is a no-op for NVMe: namespaces vanish on rescan once the array
// disconnects them, so the work happens in PostDisconnect.
func (t *nvmeTCP) Detach(ctx context.Context, _, devPath string) error {
	if devPath != "" {
		if _, err := run(ctx, t.log, "blockdev", "--flushbufs", devPath); err != nil {
			t.log.Warn("flush before disconnect failed", "dev", devPath, "err", err)
		}
	}
	return nil
}

func (t *nvmeTCP) PostDisconnect(ctx context.Context) error {
	t.rescan(ctx)
	return nil
}
