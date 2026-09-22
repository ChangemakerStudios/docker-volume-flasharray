// Package hostexec runs host tools in the host's namespaces. With the plugin
// config's pidhost, PID 1 is the host's init, so nsenter can reach its mount
// and IPC namespaces; the plugin then uses the host's own binaries, as the
// Pure plugin did, instead of copies from its image that may not match the
// host's daemons or kernel.
package hostexec

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// hostTools run as host binaries. iscsiadm and iscsid speak a binary IPC that
// breaks across open-iscsi versions ("initiator reported error (12 - iSCSI
// driver not found)"); dmsetup and multipath wait on udev through SysV
// semaphores that exist only in the host IPC namespace; and mkfs from a newer
// xfsprogs than the host's enables features its kernel cannot mount.
var hostTools = map[string]bool{
	"iscsiadm": true, "multipathd": true, "multipath": true, "dmsetup": true, "nvme": true,
	"blkid": true, "xfs_admin": true, "tune2fs": true,
}

// OnHost reports whether the plugin shares the host PID namespace.
var OnHost = os.Getpid() != 1

// Command is exec.CommandContext, entering the host mount and IPC namespaces
// for host tools. mount/umount deliberately stay local: the mount has to land
// in the plugin's propagated mount for Docker to see it.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if OnHost && (hostTools[name] || strings.HasPrefix(name, "mkfs.")) {
		return exec.CommandContext(ctx, "nsenter", append([]string{"--target", "1", "--mount", "--ipc", "--", name}, args...)...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// Path maps a host file path to one readable from the plugin.
func Path(p string) string {
	if OnHost {
		return "/proc/1/root" + p
	}
	return p
}
