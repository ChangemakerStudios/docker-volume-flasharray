package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/ctxlock"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/hostexec"
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
	connected map[string]bool // "nqn|host:port"
	nqns      map[string]bool // subsystem NQNs we connected to

	excl ctxlock.Mutex // see Exclusive
}

const hostNQNFile = "/etc/nvme/hostnqn"

// sysClassNVMe is a variable so tests can point it at a fake sysfs.
var sysClassNVMe = "/sys/class/nvme"

func (t *nvmeTCP) Name() string { return "nvme-tcp" }

func (t *nvmeTCP) InitiatorIDs() ([]string, []string, error) {
	b, err := os.ReadFile(hostexec.Path(hostNQNFile))
	if err != nil {
		return nil, nil, fmt.Errorf("read host %s (generate with `nvme gen-hostnqn`): %w", hostNQNFile, err)
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
	// A controller removed behind the plugin (nvme disconnect) is connected again.
	live := liveControllers()
	var errs []error
	n := 0
	for _, p := range ports {
		if p.NQN == "" || !portalAllowed(p.Portal, t.allowed) {
			continue
		}
		host, port, err := net.SplitHostPort(p.Portal)
		if err != nil {
			host, port = p.Portal, "4420"
		}
		key := p.NQN + "|" + net.JoinHostPort(host, port)
		if t.connected[key] && live[key] {
			n++
			continue
		}
		if err := probePortal(ctx, net.JoinHostPort(host, port)); err != nil {
			errs = append(errs, err)
			continue
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
	matches, _ := filepath.Glob(filepath.Join(sysClassNVMe, "nvme*"))
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

// liveControllers returns the kernel's NVMe controllers as "nqn|host:port" keys.
func liveControllers() map[string]bool {
	out := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(sysClassNVMe, "nvme*"))
	for _, m := range matches {
		nqn, err := os.ReadFile(filepath.Join(m, "subsysnqn"))
		if err != nil {
			continue
		}
		addr, err := os.ReadFile(filepath.Join(m, "address"))
		if err != nil {
			continue
		}
		// traddr=198.51.100.10,trsvcid=4420,src_addr=198.51.100.20
		var host, port string
		for _, kv := range strings.Split(strings.TrimSpace(string(addr)), ",") {
			switch k, v, _ := strings.Cut(kv, "="); k {
			case "traddr":
				host = v
			case "trsvcid":
				port = v
			}
		}
		if host != "" && port != "" {
			out[strings.TrimSpace(string(nqn))+"|"+net.JoinHostPort(host, port)] = true
		}
	}
	return out
}

// nvmeNGUID is the namespace ID FlashArray gives a volume: a zero octet, the
// first 7 octets of the serial, Pure's OUI, then the last 5. The serial is
// therefore never one run of digits in the kernel's wwid. Taken from the
// OpenStack Cinder Pure driver; not yet verified on an array.
func nvmeNGUID(serial string) string {
	s := strings.ToLower(serial)
	if len(s) != 24 {
		return ""
	}
	return "00" + s[:14] + "24a937" + s[14:]
}

// nvmeHead matches a namespace's head node. A glob for it also takes
// partitions and, under native multipath, the per-path nodes nvmeXcYnZ, which
// carry the same identity and have no /dev node.
var nvmeHead = regexp.MustCompile(`^nvme\d+n\d+$`)

// nvmeDevices returns the head nodes of the namespace carrying serial.
func nvmeDevices(serial string) []string {
	nguid := nvmeNGUID(serial)
	if nguid == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(sysClassBlock, "nvme*n*"))
	var out []string
	for _, m := range matches {
		if !nvmeHead.MatchString(filepath.Base(m)) {
			continue
		}
		// wwid is "eui.<nguid>" unless the namespace also reports a UUID; the
		// nguid file always has it, printed like a UUID.
		for _, f := range []string{"wwid", "nguid"} {
			b, err := readFileTimeout(filepath.Join(m, f), 3*time.Second)
			if err != nil {
				continue
			}
			id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(string(b))), "-", "")
			if strings.TrimPrefix(id, "eui.") == nguid {
				out = append(out, filepath.Base(m))
				break
			}
		}
	}
	return out
}

func (t *nvmeTCP) FindDevice(_ context.Context, serial string) string {
	if devs := nvmeDevices(serial); len(devs) > 0 {
		return "/dev/" + devs[0]
	}
	return ""
}

func (t *nvmeTCP) WaitForDevice(ctx context.Context, serial string) (string, error) {
	t.rescan(ctx)
	start := time.Now()
	lastRescan := start
	return waitFor(ctx, t.opts.Timeout, 250*time.Millisecond, func() (string, bool) {
		if devs := nvmeDevices(serial); len(devs) > 0 {
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
func (t *nvmeTCP) Detach(ctx context.Context, _, devPath string, _ map[int]string) error {
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

// Sweep and CheckLUN are iSCSI-only for now: NVMe namespace IDs are reused the
// same way, but no deployment of this driver runs NVMe/TCP yet.
func (t *nvmeTCP) Sweep(context.Context, map[int]string, string, int, bool) error { return nil }

func (t *nvmeTCP) Exclusive(ctx context.Context, fn func() error) error {
	if err := t.excl.Lock(ctx); err != nil {
		return fmt.Errorf("wait for this host's attach lock: %w", err)
	}
	defer t.excl.Unlock()
	return fn()
}

func (t *nvmeTCP) CheckLUN(context.Context, string, int) error { return nil }
