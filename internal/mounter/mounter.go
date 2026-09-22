// Package mounter formats and mounts block devices.
package mounter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/hostexec"
)

// Mounter wraps blkid/mkfs/mount/umount.
type Mounter interface {
	// EnsureFilesystem creates fstype on dev if blkid finds no filesystem.
	// Returns true if it formatted.
	EnsureFilesystem(ctx context.Context, dev, fstype string, mkfsOpts []string) (bool, error)
	Mount(ctx context.Context, dev, target, fstype, opts string) error
	// RegenerateUUID gives the (unmounted) filesystem on dev a new UUID.
	RegenerateUUID(ctx context.Context, dev, fstype string) error
	Unmount(ctx context.Context, target string) error
	// IsMounted reports whether target is a mountpoint per /proc/self/mountinfo.
	IsMounted(target string) (bool, error)
}

// New returns the exec-based implementation.
func New(log *slog.Logger) Mounter { return &execMounter{log: log} }

type execMounter struct{ log *slog.Logger }

func (m *execMounter) EnsureFilesystem(ctx context.Context, dev, fstype string, mkfsOpts []string) (bool, error) {
	out, err := hostexec.Command(ctx, "blkid", "-o", "value", "-s", "TYPE", dev).Output()
	// blkid exits 2 when it finds no filesystem; any other failure (device not
	// ready, I/O error) also prints nothing, and formatting then would wipe data.
	var ee *exec.ExitError
	noFilesystem := errors.As(err, &ee) && ee.ExitCode() == 2
	if err != nil && !noFilesystem {
		return false, fmt.Errorf("blkid %s: %w", dev, err)
	}
	if existing := strings.TrimSpace(string(out)); existing != "" {
		if existing != fstype {
			m.log.Warn("device already has a different filesystem; mounting as-is", "dev", dev, "have", existing, "want", fstype)
		}
		return false, nil
	}
	args := append([]string{}, mkfsOpts...)
	args = append(args, dev)
	if b, err := hostexec.Command(ctx, "mkfs."+fstype, args...).CombinedOutput(); err != nil {
		return false, fmt.Errorf("mkfs.%s %s: %w: %s", fstype, dev, err, strings.TrimSpace(string(b)))
	}
	return true, nil
}

func (m *execMounter) RegenerateUUID(ctx context.Context, dev, fstype string) error {
	var name string
	var args []string
	switch fstype {
	case "xfs":
		name, args = "xfs_admin", []string{"-U", "generate", dev}
	case "ext2", "ext3", "ext4":
		name, args = "tune2fs", []string{"-U", "random", dev}
	default:
		return fmt.Errorf("regenerate UUID: unsupported filesystem %q", fstype)
	}
	if b, err := hostexec.Command(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return nil
}

func (m *execMounter) Mount(ctx context.Context, dev, target, fstype, opts string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	args := []string{"-t", fstype}
	if opts != "" {
		args = append(args, "-o", opts)
	}
	args = append(args, dev, target)
	if b, err := hostexec.Command(ctx, "mount", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s %s: %w: %s", dev, target, err, strings.TrimSpace(string(b)))
	}
	return nil
}

func (m *execMounter) Unmount(ctx context.Context, target string) error {
	if b, err := hostexec.Command(ctx, "umount", target).CombinedOutput(); err != nil {
		if strings.Contains(string(b), "not mounted") {
			return nil
		}
		return fmt.Errorf("umount %s: %w: %s", target, err, strings.TrimSpace(string(b)))
	}
	return nil
}

func (m *execMounter) IsMounted(target string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescape(fields[4]) == target {
			return true, nil
		}
	}
	return false, sc.Err()
}

// unescape handles the \040 style escapes mountinfo uses for spaces etc.
func unescape(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
