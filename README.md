# docker-volume-flasharray

A Docker managed volume plugin for Pure Storage FlashArray over **iSCSI** or
**NVMe/TCP**. Volumes are FlashArray block volumes, formatted on first use and
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
`/etc/nvme/hostnqn` for NVMe/TCP. The host's
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

On unmount the plugin refuses to touch a map that still has holders, turns
queueing off for that map (`multipathd disablequeueing`, `dmsetup message …
fail_if_no_path`) before flushing it, removes it with `multipathd remove
map`, falling back to `multipath -f` then `dmsetup remove --force`, and
deletes each SCSI path only after checking it still carries the volume's
WWID, because the kernel reuses `sdX` names as soon as they are freed. If
any step fails it leaves the array connection in place rather than pull the
LUN out from under the host.

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
snapshot at the application level, when that matters. The copy also carries
its source's filesystem UUID, and XFS refuses to mount a UUID that is already
mounted on the host, so the first mount of a clone gives it a new one
(`xfs_admin -U generate`, or `tune2fs -U random` for ext4). If that fails
because the clone's log is dirty, XFS mounts it with `nouuid` once, which
replays the log, and the next mount tries again. Clones are marked on the
array with a `dvfa:clone-pending=<source>` tag until that is done.

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

A block volume is single-writer. When swarm reschedules a task off a node
that died without unmounting, the new node takes the connection over
(`FA_PREEMPT_RWO=true`). Do not rely on this to move a volume between two
*live* nodes — that is a filesystem corruption waiting to happen.

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
