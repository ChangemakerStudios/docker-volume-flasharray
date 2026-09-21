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

// iscsi drives open-iscsi (iscsiadm) and relies on the host's multipathd to
// assemble the paths into a dm device. It deliberately never calls
// `multipathd reconfigure`: on multipath-tools 0.8.8+ a global reconfigure
// with dozens of paths is expensive and, when repeated in a wait loop, can
// starve the host. If a map hasn't appeared after a grace period we add the
// specific paths instead.
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
	multipathdSocket          = "/run/multipathd.sock"
)

func (t *iscsi) Name() string { return "iscsi" }

func (t *iscsi) InitiatorIDs() ([]string, []string, error) {
	b, err := os.ReadFile(initiatorNameFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (is /etc/iscsi bind-mounted from the host?): %w", initiatorNameFile, err)
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
		// Create the node record (idempotent) and set manual startup so the
		// host's iscsid does not race us at boot; the plugin owns these sessions.
		if _, err := run(ctx, t.log, "iscsiadm", "-m", "node", "-T", p.IQN, "-p", p.Portal, "-o", "new"); err != nil {
			t.log.Debug("node record create", "portal", p.Portal, "err", err)
		}
		if _, err := run(ctx, t.log, "iscsiadm", "-m", "node", "-T", p.IQN, "-p", p.Portal, "-o", "update", "-n", "node.startup", "-v", "manual"); err != nil {
			t.log.Warn("set node.startup=manual failed; host iscsid may also log in at boot", "portal", p.Portal, "err", err)
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
	if _, err := run(ctx, t.log, "iscsiadm", "-m", "session", "--rescan"); err != nil && exitCode(err) != iscsiadmExitNoObjects {
		t.log.Warn("iscsi rescan", "err", err)
	}
}

func (t *iscsi) WaitForDevice(ctx context.Context, serial string) (string, error) {
	wwid := WWID(serial)
	mpath := "/dev/disk/by-id/dm-uuid-mpath-" + wwid
	single := "/dev/disk/by-id/scsi-" + wwid
	useMultipath := exists(multipathdSocket)

	t.rescan(ctx)
	start := time.Now()
	lastRescan := start
	nudged := false
	return waitFor(ctx, t.opts.Timeout, func() (string, bool) {
		if useMultipath {
			if exists(mpath) {
				return mpath, true
			}
		} else if exists(single) {
			return single, true
		}
		// Paths present but no map yet? After 5s ask multipathd to add them
		// explicitly (cheap, targeted) instead of a global reconfigure.
		if useMultipath && !nudged && time.Since(start) > 5*time.Second {
			paths := sysBlockMatching("sd*", strings.TrimPrefix(wwid, "3"))
			if len(paths) > 0 {
				nudged = true
				for _, p := range paths {
					if _, err := run(ctx, t.log, "multipathd", "add", "path", p); err != nil {
						t.log.Warn("multipathd add path failed", "path", p, "err", err)
					}
				}
			}
		}
		// Periodically re-issue a rescan in case the LUN mapping landed after our first one.
		if time.Since(lastRescan) > 10*time.Second {
			lastRescan = time.Now()
			t.rescan(ctx)
		}
		return "", false
	})
}

func (t *iscsi) Detach(ctx context.Context, serial, _ string) error {
	if serial == "" {
		// WWID("") is the bare Pure prefix, which would match every FlashArray LUN on the host.
		return errors.New("detach: empty volume serial; refusing to touch host SCSI devices")
	}
	wwid := WWID(serial)
	var errs []error
	if exists(multipathdSocket) {
		// Flush the map first so no I/O is queued to paths we are about to delete.
		if out, err := run(ctx, t.log, "multipath", "-f", wwid); err != nil && !strings.Contains(out, "not found") {
			errs = append(errs, err)
		}
	}
	for _, p := range sysBlockMatching("sd*", strings.TrimPrefix(wwid, "3")) {
		if _, err := run(ctx, t.log, "blockdev", "--flushbufs", "/dev/"+p); err != nil {
			t.log.Warn("flush before path delete failed", "dev", p, "err", err)
		}
		if err := os.WriteFile(filepath.Join("/sys/class/block", p, "device", "delete"), []byte("1"), 0o200); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (t *iscsi) PostDisconnect(context.Context) error { return nil }
