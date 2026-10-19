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
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/hostexec"
)

// ErrNoFilesystem is returned by EnsureFilesystem when dev has no filesystem
// and formatting was not allowed.
var ErrNoFilesystem = errors.New("no filesystem found")

// Mounter wraps blkid/mkfs/mount/umount.
type Mounter interface {
	// EnsureFilesystem creates fstype on dev if blkid finds no filesystem and
	// allowFormat is set; otherwise it returns ErrNoFilesystem. Returns true
	// if it formatted.
	EnsureFilesystem(ctx context.Context, dev, fstype string, mkfsOpts []string, allowFormat bool) (bool, error)
	Mount(ctx context.Context, dev, target, fstype, opts string) error
	Unmount(ctx context.Context, target string) error
	// IsMounted reports whether target is a mountpoint per /proc/self/mountinfo.
	IsMounted(target string) (bool, error)
	// MountedAt returns a mountpoint where dev (after resolving symlinks) is
	// mounted per /proc/self/mountinfo, or "".
	MountedAt(dev string) (string, error)
	// Busy reports whether the kernel still holds dev, e.g. a filesystem
	// mounted in a container's mount namespace this process can't see.
	Busy(dev string) (bool, error)
	// Sync writes the filesystem mounted at target out to its device (syncfs).
	Sync(ctx context.Context, target string) error
	// InUseByContainer reports whether a mount namespace other than the
	// host's and this process's has the filesystem at mountpoint mounted.
	// Needs the host PID namespace (pidhost).
	InUseByContainer(mountpoint string) (bool, error)
}

// mountinfoPath and procRoot are variables so tests can substitute fixtures.
var (
	mountinfoPath = "/proc/self/mountinfo"
	procRoot      = "/proc"
)

// pluginMountPath matches the plugin's own propagated mounts, which
// namespaces copied from the host (systemd PrivateTmp etc.) also carry.
var pluginMountPath = regexp.MustCompile(`/plugins/[0-9a-f]{64}/`)

// New returns the exec-based implementation.
func New(log *slog.Logger) Mounter { return &execMounter{log: log} }

type execMounter struct{ log *slog.Logger }

func (m *execMounter) EnsureFilesystem(ctx context.Context, dev, fstype string, mkfsOpts []string, allowFormat bool) (bool, error) {
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
	// A damaged superblock also makes blkid exit 2, so "no filesystem" alone
	// is not proof the device is blank.
	if !allowFormat {
		return false, fmt.Errorf("%s: %w", dev, ErrNoFilesystem)
	}
	args := append([]string{}, mkfsOpts...)
	args = append(args, dev)
	if b, err := hostexec.Command(ctx, "mkfs."+fstype, args...).CombinedOutput(); err != nil {
		return false, fmt.Errorf("mkfs.%s %s: %w: %s", fstype, dev, err, strings.TrimSpace(string(b)))
	}
	return true, nil
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
	f, err := os.Open(mountinfoPath)
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

func (m *execMounter) MountedAt(dev string) (string, error) {
	want, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return "", err
	}
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Optional fields end at "-", followed by fstype and mount source.
		fields := strings.Fields(sc.Text())
		for i := 6; i+2 < len(fields); i++ {
			if fields[i] != "-" {
				continue
			}
			src := unescape(fields[i+2])
			if src == want {
				return unescape(fields[4]), nil
			}
			if filepath.IsAbs(src) {
				if resolved, err := filepath.EvalSymlinks(src); err == nil && resolved == want {
					return unescape(fields[4]), nil
				}
			}
			break
		}
	}
	return "", sc.Err()
}

// Busy uses an exclusive open: Linux refuses O_EXCL on a block device with
// EBUSY while a mounted filesystem or a dm table claims it, in any namespace.
func (m *execMounter) Busy(dev string) (bool, error) {
	f, err := os.OpenFile(dev, os.O_RDONLY|os.O_EXCL, 0)
	if err == nil {
		return false, f.Close()
	}
	if errors.Is(err, syscall.EBUSY) {
		return true, nil
	}
	return false, err
}

// Sync runs `sync -f` (syncfs) in the plugin's own mount namespace, where
// target lives; the plugin image's coreutils provide it. ctx bounds a sync
// stuck on a wedged device.
func (m *execMounter) Sync(ctx context.Context, target string) error {
	if b, err := hostexec.Command(ctx, "sync", "-f", target).CombinedOutput(); err != nil {
		return fmt.Errorf("sync -f %s: %w: %s", target, err, strings.TrimSpace(string(b)))
	}
	return nil
}

// InUseByContainer only sees namespaces that still have a process in them,
// which is what matters: a stopped container's namespace has none.
func (m *execMounter) InUseByContainer(mountpoint string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(mountpoint, &st); err != nil {
		return false, err
	}
	dev := uint64(st.Dev) // uint32 on some arches
	want := fmt.Sprintf("%d:%d", (dev>>8)&0xfff|(dev>>32)&^0xfff, dev&0xff|(dev>>12)&^0xff)

	seen := map[string]bool{}
	for _, p := range []string{"1", "self"} {
		ns, err := os.Readlink(filepath.Join(procRoot, p, "ns", "mnt"))
		if err != nil {
			return false, fmt.Errorf("read mount namespace of %s: %w", p, err)
		}
		seen[ns] = true
	}
	pids, err := filepath.Glob(filepath.Join(procRoot, "[0-9]*"))
	if err != nil {
		return false, err
	}
	for _, dir := range pids {
		ns, err := os.Readlink(filepath.Join(dir, "ns", "mnt"))
		if err != nil || seen[ns] {
			continue
		}
		seen[ns] = true
		if mountsDevice(filepath.Join(dir, "mountinfo"), want) {
			return true, nil
		}
	}
	return false, nil
}

// mountsDevice reports whether the mountinfo file lists want (major:minor)
// outside the plugin's own propagated mounts. An unreadable file means the
// process has exited.
func mountsDevice(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && fields[2] == want && !pluginMountPath.MatchString(unescape(fields[4])) {
			return true
		}
	}
	return false
}

// unescape handles the \040 style escapes mountinfo uses for spaces etc.
func unescape(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
