# docker-volume-flasharray

A Docker managed volume plugin for Pure Storage FlashArray over **iSCSI** or
**NVMe/TCP**. Volumes are FlashArray block volumes, formatted on first use
(only while the array reports nothing has ever been written to them) and
mounted into containers. Works with plain Docker and Docker Swarm (`Scope:
global`, so a volume created on one node can be mounted on another).

> **Unofficial.** Not affiliated with or supported by Pure Storage, Inc.
> FlashArray is a trademark of Pure Storage. This is a from-scratch
> replacement for the end-of-life `purestorage/docker-plugin`.

Zero Go dependencies outside the standard library. Like the Pure plugin, it
runs the **host's own** `iscsiadm`, `multipathd`, `multipath`, `dmsetup`,
`nvme`, `mkfs.*`, `blkid`, `xfs_admin` and `tune2fs` (via `nsenter` into the
host mount and IPC namespaces, which is why it uses the host PID namespace;
`CAP_SYS_PTRACE` and `CAP_SYS_CHROOT` are what `nsenter` needs to enter
PID 1's namespaces and read host files through `/proc/1/root`).
The client then always matches the host's `iscsid`/`multipathd`, reads the
host's `/etc/iscsi`, `/etc/nvme` and `multipath.conf` directly, and formats
with the host's own xfsprogs, so a new filesystem never has features the
host kernel cannot mount. Only `mount`, `umount` and `blockdev` run from the
plugin image; the mount has to land in the plugin's propagated mount. It
does not mount the host's `/run`: on a systemd host that is a shared mount,
and Docker's own per-plugin mount under it would propagate back onto the
host and break plugin startup.

## Why another one

The original plugin's attach loop calls `multipathd reconfigure` repeatedly
while waiting for a device. On multipath-tools ≥ 0.8.8 (Ubuntu 22.04+) that
is a full flush-and-rebuild of every map, and with 8 portals × N LUNs it can
stall the host for minutes. This driver:

- logs in to array portals **once at plugin start**, not lazily during the
  first container mount after boot;
- waits for the device by asking multipathd for the map with the volume's
  WWID, and only ever nudges it with targeted `add path` calls, never a
  global reconfigure;
- tears a device down in the order that can't hang the host (see
  [Multipath safety](#multipath-safety));
- supports **NVMe/TCP**, where the kernel's native multipath means one block
  device per volume instead of one per path and no `multipath.conf` at all;
- lets you restrict which array portals are used (`FA_ALLOWED_CIDRS`).

## Install

On every node:

```bash
sudo mkdir -p /etc/docker-volume-flasharray
sudo tee /etc/docker-volume-flasharray/flasharray.json >/dev/null <<'EOF'
{
  "arrays": [
    { "endpoint": "192.0.2.10", "apiToken": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", "insecureSkipVerify": true }
  ]
}
EOF
sudo chmod 600 /etc/docker-volume-flasharray/flasharray.json

docker plugin install --alias flasharray --grant-all-permissions \
  ghcr.io/changemakerstudios/docker-volume-flasharray:latest \
  FA_TRANSPORT=iscsi \
  FA_ALLOWED_CIDRS=198.51.100.0/24
```

Host prerequisites (the plugin uses these host binaries rather than shipping
its own): `xfsprogs` (or `e2fsprogs` for ext4); `open-iscsi` (running
`iscsid`) and `multipath-tools` for iSCSI,
configured as in [Multipath safety](#multipath-safety); `nvme-cli` and an
`/etc/nvme/hostnqn` for NVMe/TCP. On cgroup v2 hosts (Ubuntu 22.04+), Docker
must put containers in their own slice; see item 5 of
[Multipath safety](#multipath-safety). The host's
`/etc/iscsi/initiatorname.iscsi` or `/etc/nvme/hostnqn` is what identifies
the node to the array; the plugin creates (or reuses) a FlashArray host
object named after `FA_HOST_NAME` (default: hostname).

`endpoint` must be the array's **virtual** management IP (`vir0`, see
`purenetwork list`) or a name resolving to it, not a controller's own
address: a controller's address leaves with it on failover, and every
create, mount and remove fails until it returns. Running containers are
unaffected either way; the data path fails over through multipath.

The API token needs create/modify/delete/list on volumes and hosts, plus
list on ports. On Purity 6.6+ prefer a token from a user inside a realm: the
array then scopes what the plugin can see and touch, and caps its capacity,
instead of the token being array-wide.

## Multipath safety

Adapted from [jt-pve-storage-purestorage](https://github.com/jasoncheng7115/jt-pve-storage-purestorage),
whose author found these the hard way: getting them wrong has left host
processes in uninterruptible sleep, recoverable only by a reboot.

1. **Never run `multipath -F`** (capital F). It flushes every unused map on
   the host, including other storage that happens to be idle. Flush one map
   with `multipath -f <name>`. The plugin only ever removes the map for the
   volume it is detaching.
2. **Restart multipathd after editing its config** (`systemctl restart
   multipathd`); `reload` only re-reads the file.
3. **Don't queue I/O for long.** `no_path_retry queue` on a device whose
   paths are gone makes `multipath -f`, `sync`, `blockdev --flushbufs` and
   anything that opens the device hang. Bound it, and keep the bound short:
   the queue time is `no_path_retry × polling_interval`, and while a stale
   node queues, a volume taken over by another node (`FA_PREEMPT_RWO`) has a
   writer that hasn't failed yet. `3` × `10`s rides out a brief path blip
   and errors within 30 seconds. Larger values (the Proxmox plugin uses 30,
   i.e. five minutes, for VM boot resilience) trade that for tolerance of
   longer outages; `0` fails at once, as soon as every path is down.
4. **Let multipathd claim a Pure LUN on first sight.** With the default
   `find_multipaths on` (multipath-tools 0.8.x, e.g. Ubuntu 22.04), a freshly
   attached LUN may get no map even with all paths up; the plugin's targeted
   `multipathd add path` can paper over it, but set `find_multipaths
   greedy` (or `yes`) and blacklist the local disks so greedy doesn't claim
   them. A device that never appears reports "multipath map: none (paths not
   claimed…)" in its error when this is the cause.

   `polling_interval` and `find_multipaths` belong in `defaults`; in a
   `device` block they are silently ignored. In `/etc/multipath/conf.d/pure.conf`
   or `/etc/multipath.conf`:

   ```
   defaults {
       polling_interval   10
       find_multipaths    greedy
       no_path_retry      3
       fast_io_fail_tmo   5
       dev_loss_tmo       60
   }
   blacklist {
       # local system disks; match yours (`lsblk -o NAME,VENDOR,MODEL,WWN`)
       device {
           vendor  "VMware"
           product "Virtual disk"
       }
   }
   devices {
       device {
           vendor               "PURE"
           product              "FlashArray"
           path_selector        "queue-length 0"
           path_grouping_policy group_by_prio
           prio                 alua
           hardware_handler     "1 alua"
           failback             immediate
           no_path_retry        3
           fast_io_fail_tmo     5
           dev_loss_tmo         60
       }
   }
   ```
5. **On cgroup v2 hosts (Ubuntu 22.04 and later), give containers their own
   slice.** There Docker uses the systemd cgroup driver, and each container
   becomes a scope in `system.slice` beside `systemd-udevd`, `multipathd`,
   `iscsid` and this plugin, at the same CPU weight. When a node starts many
   containers at once (a drain, a failover), udev and multipathd get a sliver
   of the CPU and new LUNs take longer than `FA_ATTACH_TIMEOUT` to become a
   map. The failed attaches then free LUN numbers while multipathd still holds
   their old paths, which on staging put one volume's paths into another's
   map. cgroup v1 hosts (Ubuntu 20.04, `cgroupfs` driver) are unaffected: all
   containers share one `/docker` group. Add to `/etc/docker/daemon.json`,
   keeping its other keys:

   ```json
   "cgroup-parent": "docker.slice"
   ```

   Restart Docker on a drained node; containers move as they are recreated.
   The plugin is not a container scope and stays in `system.slice`. `docker
   info --format '{{.CgroupVersion}} {{.CgroupDriver}}'` shows which case a
   host is in, and a container's `/proc/<pid>/cgroup` should read
   `0::/docker.slice/…`. In a burst test on 4 vCPUs under CPU load (100
   container starts across 50 volumes), half the starts failed without it and
   none with it.

Before any teardown the plugin waits, up to `FA_ATTACH_TIMEOUT`, until an
exclusive open of the device succeeds. A stopped container's filesystem can
still be flushing, and after a plugin disable or upgrade the mounts of running
containers live only in their namespaces, so the plugin no longer tracks them.
Disconnecting then loses writes and corrupts XFS. If the device stays busy,
unmount fails and leaves the device and the array connection in place.

Docker sometimes sends a Mount it never pairs with an Unmount, so the
plugin does not trust its own count of mount IDs. When the last expected
Unmount leaves IDs behind, it checks the mount namespaces of every process
(the plugin runs with the host PID namespace) and unmounts if no container
still has the filesystem; IDs younger than two minutes may belong to a
container still starting and always keep it mounted. Mount and Unmount
finish inside Docker's own limits (2 and 1 minute), past which Docker counts
the call as failed while the plugin would carry on unseen. That includes time
a Mount spends queued behind another operation on the same volume or behind
the host-wide attach lock: it gives up there, before connecting anything,
rather than start late. Unmount waits as long as it takes, since giving up
would leave a mount behind, and rollbacks and teardowns get a budget of their
own.

**Upgrade or disable the plugin only on a drained node.** Docker refuses a
plain `docker plugin disable` while it knows any volume of the plugin, mounted
or not, so `-f` is needed; under running containers it tears the plugin's
mounts out from under them ([moby#32515](https://github.com/moby/moby/issues/32515)).
Before `-f`, check the node has no running container on a flasharray volume,
nothing under the plugin's `propagated-mount`, and no open Pure device.

On unmount the plugin refuses to touch a map that still has holders, turns
queueing off for that map (`multipathd disablequeueing`, `dmsetup message …
fail_if_no_path`) before flushing it, removes it with `multipathd remove
map`, falling back to `multipath -f` then, only if the map is not open,
`dmsetup remove --force`, and
deletes each SCSI path only after checking it still carries the volume's
WWID, because the kernel reuses `sdX` names as soon as they are freed. If
any step fails it leaves the array connection in place rather than pull the
LUN out from under the host.

Before connecting a volume, the plugin checks for a device the node still
has for it without an array connection, left behind when the volume was taken
away. An idle one is removed so the attach builds a fresh device. A busy one
is a stale mount: reconnecting would let multipathd revive its paths and the
stale filesystem write old metadata over the volume, so the mount is refused
until whatever holds it is stopped or the node rebooted.

The array reuses LUN numbers: disconnect a volume and the next one connected
to the host may get its number. Linux reads a SCSI path's identity once, at
scan time, so a path the host kept for the old volume still claims to be it
while its I/O reaches the new one, which cross-wrote volumes on staging. Right
after each connect the plugin asks the array which volume every LUN number on
the host carries and deletes each path of this array (matched by serial
prefix; another array's LUNs are its own) whose LUN and cached WWID disagree,
without flushing it: its buffers belong to a volume it no longer reaches. A
map whose every path is stale has queueing switched off first, so whatever
holds it gets I/O errors instead of hanging, and is then removed. Connects,
detach-then-disconnect and every rescan hold one host-wide lock, so no rescan
can rebuild a volume's paths between its detach and its disconnect. Detach
itself deletes, unflushed, any path whose LUN no longer carries the volume.
A mount checks every path of its device sits on the LUN the array assigned,
and ignores a map that has no paths. Rollback of a failed mount disconnects on
the array only once the host-side devices are gone. A stale path that cannot
be deleted fails only the mount whose LUN it sits on. The sweep also runs just
before each connect, so no leftover path sits on a LUN number the connect may
hand out, and every 30 seconds in the background, where it deletes only paths
on a LUN the array gives to a different volume: a LUN missing from the
array's answer is never reason enough to cut a live volume's path. Every
sweep also checks that each path sits in the map of the volume it carries:
multipathd keeps its own record of each sdX name, and after the kernel
reuses a name for another LUN it can add the path to the old volume's map. A
path found in another volume's map is pulled out and re-read, and before
asking multipathd to add a path the plugin drops a stale record of it.
multipathd files a path under the identity udev recorded (`ID_SERIAL`), so a
path udev still files under another volume is re-announced to udev instead of
added, one udev is still reading is left to finish, and one multipathd already
has right is left alone. (NVMe/TCP has no such sweep yet.)

A device that does not appear within `FA_ATTACH_TIMEOUT` is usually only late:
udev and multipathd fall behind while a node starts many containers. Rolling
such an attach back deleted paths multipathd was still building maps from and
freed their LUNs for the next attaches, which on staging mixed two volumes in
one map. So the volume stays connected for half of `FA_PREEMPT_GRACE`, Docker's
retry finds the device, and one nothing mounts by then is released. Every
detach and sweep waits until multipathd has dropped the paths it deleted (and
drops any it keeps) before the lock that holds off rescans is released, so the
kernel cannot hand their names to new disks multipathd would file under the
old volume; a connect waits while it cannot. The mount check also refuses
while multipathd files another volume's path under the map, and runs again
right after the mount: a map that changed in between gets its filesystem shut
down without a log flush before it is unmounted, so nothing more is written
through it.

On mount the same rule applies to dm names: a `/dev/disk/by-id` or
`/dev/mapper` link can outlive its map and point at a dm number the kernel has
given to another LUN, so the device is used only after its sysfs `dm/uuid`
(or the SCSI `wwid`) matches the volume. A device already mounted elsewhere on
the node is refused, and a volume whose filesystem blkid can't find is
formatted only if the array's `space.virtual` is 0; otherwise the mount fails
with the bytes written, since a damaged superblock also reads as "no
filesystem".

XFS volumes are always mounted with `nouuid`, as the Pure plugin did: copies
of a volume carry its filesystem UUID, and XFS would otherwise refuse to mount
two of them on one node. The plugin never rewrites a UUID, since that is a
write to the volume. With XFS's own duplicate check off, the plugin's refusal
to mount a device already mounted on the node is what stops a double mount.

## Use

```bash
docker volume create -d flasharray -o size=50GiB pgdata
docker run --rm -v pgdata:/var/lib/postgresql/data postgres:16
```

Compose / stack:

```yaml
volumes:
  pgdata:
    driver: flasharray
    driver_opts:
      size: 50GiB
```

| Option | Meaning |
| --- | --- |
| `size` | provisioned size, e.g. `50GiB`, `500M`, `1T` (default `FA_DEFAULT_SIZE`) |
| `source` | create the volume as an array-side copy of another: a Docker volume in this namespace, or else an array volume name. Not combinable with `size`; a clone is the size of its source |
| `import` | adopt an existing array volume under this Docker name instead of creating one (see [Migrating from the Pure plugin](#migrating-from-the-pure-plugin)). `import_from_src`, the Pure plugin's spelling, is accepted too |

```bash
docker volume create -d flasharray -o source=pgdata pgdata-test
```

A clone is instant and crash-consistent: cloning a volume that is mounted and
being written gives you what a power cut would have left. Stop the writer, or
snapshot at the application level, when that matters. The copy carries its
source's filesystem UUID, which `nouuid` (above) lets it mount beside its
source.

New volumes are named `<namespace>-<name>` on the array, as the Pure plugin
named them (with a short hash suffix when the Docker name contains
characters the array rejects, i.e. `.`, or would exceed 63 characters),
and tagged `dvfa:name=<namespace>/<docker name>` so `docker volume ls` shows
the original names from any node.

The tag, not the array name, is what the driver goes by: every operation
looks the Docker name up through `dvfa:name` and only falls back to the
computed `<namespace>-<name>` when nothing is tagged. If the lookup itself
fails (array unreachable) the operation fails rather than guess, since the
computed name may be a different volume. That is why an imported volume
keeps whatever name it already had on the array. Each node caches the
mapping; if a cached array volume turns out to be gone or destroyed (another
node removed and recreated it), the node drops the entry and looks it up
again.

`docker volume rm` renames the array volume out of its computed name (adding
a short suffix) before destroying it, so the name can be created again at
once while the destroyed volume stays recoverable for the array's 24 hours;
its `dvfa:name` tag still maps it back after a recover. A volume destroyed by
other means (the array UI) keeps its name, and `Get` keeps answering for it
with `destroyed: true`: a stack volume declared without a driver then fails
to mount instead of Docker quietly creating an empty `local` volume in its
place. An explicit `docker volume create` of such a name gets a fresh array
volume beside the destroyed one.

### When the array's management API is unreachable

Docker treats any error from a volume driver's `Get` as "no such volume".
For a volume declared **without** a driver (`external: true`), a container
start during an array outage (controller failover, Purity upgrade) then
makes Docker silently create a `local` volume of the same name, which
shadows the real one on that node until it is removed and pruned. Two
things prevent it:

- **Name the driver in the stack file** instead of `external: true`. Docker
  then asks this driver specifically and never substitutes `local`; during
  an outage the task fails and swarm retries it.

  ```yaml
  volumes:
    pgdata:
      name: pgdata          # no stack-name prefix
      driver: flasharray
  ```

- **The driver answers from what it already knows.** Each node keeps the
  Docker names it has seen, and their array volumes, in
  `.dvfa-known.json` in the plugin's mount directory (survives plugin
  restarts; every successful `docker volume ls` replaces it with the array's
  view). When a `Get` or `List` can't reach the array, a volume in that file
  is still reported as existing, with `array_unreachable` in its status and
  a warning in the log. Only lookups fall back; creating, mounting and
  removing still need the array. Only the array's own answer that an object
  does not exist (HTTP 400 with its JSON error) counts as "not found": a 404
  or an HTML page from a controller mid-failover is treated as an outage, so
  it never bypasses this fallback. A volume this node has never seen is still
  an error.

## Settings

Set at install time or with `docker plugin set flasharray KEY=value` (plugin
must be disabled).

| Variable | Default | Meaning |
| --- | --- | --- |
| `FA_CONFIG` | `/etc/docker-volume-flasharray/flasharray.json` | credentials file |
| `FA_NAMESPACE` | `docker` | prefix for array volume names; **same value on every node** of a cluster |
| `FA_HOST_NAME` | hostname | FlashArray host object for this node (created on first start if no host has this node's IQN/NQN) |
| `FA_TRANSPORT` | `iscsi` | `iscsi` or `nvme-tcp` |
| `FA_DEFAULT_SIZE` | `32GiB` | size when `-o size=` is omitted |
| `FA_FS_TYPE` | `xfs` | `xfs` or `ext4` |
| `FA_MKFS_OPTS` | `-q` (xfs) | extra mkfs args |
| `FA_MOUNT_OPTS` | | `mount -o` options |
| `FA_ALLOWED_CIDRS` | all | only log in to array portals inside these CIDRs |
| `FA_PREEMPT_RWO` | `true` | take over a volume still connected to another host (needed for swarm rescheduling off a dead node; unsafe if the other node is actually alive and writing) |
| `FA_PREEMPT_GRACE` | `90s` | how long another host may keep a volume after a mount here asks for it before it is presumed dead and preempted; `0` preempts at once |
| `FA_UNMOUNT_LINGER` | `15s` | how long a volume stays mounted and attached after its last container lets go, so Docker's populate-then-start re-mount (and a quick restart) reuses it instead of a detach and re-attach; `0` tears down at once |
| `FA_ERADICATE_ON_REMOVE` | `false` | `docker volume rm` eradicates immediately instead of leaving 24h to recover |
| `FA_CONNECT_ON_START` | `true` | log in to portals at plugin start |
| `FA_ATTACH_TIMEOUT` | `60s` | how long to wait for the device |
| `FA_LOG_LEVEL` | `info` | `debug` for `iscsiadm`/`nvme` command traces |

## Namespaces and swarms

`FA_NAMESPACE` scopes volume names on the array and must be identical on
every node of a swarm so `pgdata` resolves to the same array volume
everywhere. Point staging and production at different namespaces (or
different arrays). The FlashArray host object is per node: it is found by
the node's IQN/NQN and created as `FA_HOST_NAME` (hostname) if missing.

A block volume is single-writer. On a drain swarm starts the new task while
the old one is still stopping, so a mount first waits (up to 30s per attempt)
for the other host to release the volume itself, which its unmount does once
the old container is gone. Only a host that keeps it past `FA_PREEMPT_GRACE`,
counted across Docker's retries and restarted whenever the holder changes,
is presumed dead and has its connection
taken over (`FA_PREEMPT_RWO=true`); until then the mount fails with a
"waiting for … to release" error and swarm retries. A grace of `0` restores
immediate takeover, which cut writes a live node was still flushing and
corrupted XFS.

After the last container lets go, a volume stays mounted and attached for
`FA_UNMOUNT_LINGER` before it is torn down. Docker mounts a volume, unmounts
it and mounts it again within a second or two when it populates a new
container from its image; tearing down in between churned LUN numbers, and
the paths those quick cycles left behind were what the LUN checks above kept
catching. A Mount during the linger reuses the attachment. The filesystem is
flushed (`syncfs`) as the linger starts, so a lingering volume holds no
unwritten data. The teardown never runs while a container namespace still has
the filesystem (it retries instead), `docker volume rm` releases a lingering
volume at once, the plugin tears lingering volumes down when it is stopped,
and a mount left over anyway is torn down after the linger once the plugin
restarts. On a drain, the old node releases each volume up to
`FA_UNMOUNT_LINGER` later. The plugin refuses to start unless the linger is
under 30 seconds and at most half of `FA_PREEMPT_GRACE` (and 0 when the grace
is 0), so no other node preempts a volume that is only lingering.

**Services on these volumes must update and roll back `stop-first`** (swarm's
default). With `start-first` the old task keeps the volume until the new one
is healthy, the new one cannot mount until the old one lets go, and after
`FA_PREEMPT_GRACE` the old, still-live task is preempted mid-write.

## Migrating from the Pure plugin

Nothing is copied. Pure's plugin created ordinary FlashArray volumes named
`<PURE_DOCKER_NAMESPACE>-<docker name>` with XFS on them; this driver mounts
the same volumes once they carry its name tag. What changes is metadata: the
tag on the array, Docker's local record of which driver owns the name, and
`driver:` in your stack files.

1. Install this plugin on **every** node that runs any of those services, with
   the credentials file in place and the same `FA_NAMESPACE` everywhere,
   before touching the Pure plugin; a service rescheduled onto a node without
   it cannot start. Use the Pure plugin's `PURE_DOCKER_NAMESPACE`
   (`docker plugin inspect pure --format '{{json .Settings.Env}}'`) as
   `FA_NAMESPACE`: the computed array names then equal Pure's, so Docker names
   and tags line up. Both plugins can be installed side by side; don't use
   one volume through both at once.
2. Snapshot the volumes on the array (instant, free insurance).
3. Stop the stacks that use them.
4. On **every** node: `docker plugin disable -f pure`, then for each volume
   `docker volume rm -f <name>`. With the driver disabled, `rm -f` only drops
   Docker's local reference — it cannot reach the array. Do **not** do this
   with the Pure plugin enabled: its `Remove` destroys the array volume.
5. Adopt the volumes (once, from any node with the credentials file), with
   `PURE_DOCKER_NAMESPACE=prod` for example:

   ```bash
   sudo FA_NAMESPACE=prod ./docker-volume-flasharray adopt --prefix prod- --dry-run
   sudo FA_NAMESPACE=prod ./docker-volume-flasharray adopt --prefix prod-
   ```

   The binary is on each GitHub release (and as a build artifact of every
   `develop` run). `--volume <array>=<docker>` handles one-offs whose Docker
   name isn't simply the array name minus the prefix.

   Each line of the plan is `would` (will be tagged), `skip` (already tagged
   correctly) or `CONFLICT`: the array volume is already tagged with another
   name, or the Docker name already maps to a different array volume
   (including an earlier line of the same plan). `--dry-run` reports every
   conflict the real run would hit and exits non-zero if there are any, so
   fix those before running it for real. Destroyed volumes are left out.
6. Change `driver: pure` to `driver: flasharray` in the stack files and
   `docker stack deploy`. `Create` is idempotent and resolves names through
   the tag, so the services come up on their existing data.

A single volume can also be imported straight from Docker:

```bash
docker volume create -d flasharray -o import=prod-app_data app_data
```

Array volumes keep their original names; only the `dvfa:name` tag is written.

## Known issues

**Slow first minute after boot on systemd 249 (Ubuntu 22.04).** With
`FA_CONNECT_ON_START=true` (the default) the plugin logs in to every portal
when it starts, so the cold iSCSI login, and the flood of LUNs for every
volume still connected to this node's host object, lands at boot instead of
inside the first container's mount. On systemd 249 that burst of new block
devices has been seen to keep PID 1 and logind busy for a minute or more. It
passes, but it looks like a hang. To shorten it: keep the host object lean
(disconnect volumes this node no longer uses from its host object on the
array). `FA_ALLOWED_CIDRS` restricted to one iSCSI subnet halves the
sessions and paths too, but gives up the other subnet's redundancy: a
switch or fabric outage on the remaining one then takes every path down.

## Development

```bash
make test                 # vet + race tests (no hardware needed)
make plugin PLUGIN_TAG=dev
sudo make enable          # on a node with the credentials file in place
docker volume create -d ghcr.io/changemakerstudios/docker-volume-flasharray:dev -o size=1GiB t1
```

`make rootfs` uses `docker buildx --output type=local` to produce the plugin
rootfs directly; no `docker create`/`export` step.

### Branches and releases (gitflow)

- `develop` — every push runs tests and publishes a prerelease plugin tagged
  with the [GitVersion](https://gitversion.net) semver (e.g. `0.1.0-alpha.7`)
  plus a moving `:develop` tag; that is what to install on a staging swarm.
- `main` — tested only. Merge `develop` into it when releasing.
- Release: tag `main` with a bare version, `git tag 0.1.0 && git push origin 0.1.0`.
  `release.yml` builds once, pushes `:0.1.0` and `:latest`, and creates a
  GitHub release with the binary and plugin bundle attached.

## Status

Early. The array client (including retries), name mapping, adoption,
cloning, mount reference counting and failure rollback are unit-tested
against fakes; the transports are exercised only on real hardware. The iSCSI
attach/detach sequence follows jt-pve-storage-purestorage, which has been
through many field releases; NVMe/TCP device matching has not yet been
verified on an array.

The array client retries 429 for any request and 5xx or transport errors
for everything but POST (a create whose response was lost may already have
happened), and logs in again on 401. When a device does not appear in time,
the error carries what the host knows about the WWID: SCSI paths found,
whether multipathd built a map, and the `/dev/disk/by-id` links.

## License

Apache-2.0.
