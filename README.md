# docker-volume-flasharray

A Docker managed volume plugin for Pure Storage FlashArray over **iSCSI** or
**NVMe/TCP**. Volumes are FlashArray block volumes, formatted on first use and
mounted into containers. Works with plain Docker and Docker Swarm (`Scope:
global`, so a volume created on one node can be mounted on another).

> **Unofficial.** Not affiliated with or supported by Pure Storage, Inc.
> FlashArray is a trademark of Pure Storage. This is a from-scratch
> replacement for the end-of-life `purestorage/docker-plugin`.

Zero Go dependencies outside the standard library. The runtime image carries
`iscsiadm`, `nvme`, `multipath` and `mkfs.*`; the plugin talks to the host's
own `iscsid`/`multipathd` through `/run` and the host network namespace.

## Why another one

The original plugin's attach loop calls `multipathd reconfigure` repeatedly
while waiting for a device. On multipath-tools ≥ 0.8.8 (Ubuntu 22.04+) that
is a full flush-and-rebuild of every map, and with 8 portals × N LUNs it can
stall the host for minutes. This driver:

- logs in to array portals **once at plugin start**, not lazily during the
  first container mount after boot;
- waits for the device via `/dev/disk/by-id` and only ever nudges multipathd
  with targeted `add path` calls, never a global reconfigure;
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
    { "endpoint": "10.50.0.5", "apiToken": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", "insecureSkipVerify": true }
  ]
}
EOF
sudo chmod 600 /etc/docker-volume-flasharray/flasharray.json

docker plugin install --alias flasharray --grant-all-permissions \
  ghcr.io/changemakerstudios/docker-volume-flasharray:latest \
  FA_TRANSPORT=iscsi \
  FA_ALLOWED_CIDRS=10.10.100.0/24
```

Host prerequisites: `open-iscsi` (running `iscsid`) and `multipath-tools`
for iSCSI; `nvme-cli` and an `/etc/nvme/hostnqn` for NVMe/TCP. The host's
`/etc/iscsi/initiatorname.iscsi` or `/etc/nvme/hostnqn` is what identifies
the node to the array; the plugin creates (or reuses) a FlashArray host
object named after `FA_HOST_NAME` (default: hostname).

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

Volumes are named `<namespace>-<name>` on the array (with a short hash suffix
when the Docker name contains characters the array rejects, e.g. `_`), and
tagged `dvfa:name=<namespace>/<docker name>` so `docker volume ls` shows the
original names from any node.

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

1. Snapshot the volumes on the array (instant, free insurance).
2. Stop the stacks that use them.
3. On **every** node: `docker plugin disable -f pure`, then for each volume
   `docker volume rm -f <name>`. With the driver disabled, `rm -f` only drops
   Docker's local reference — it cannot reach the array. Do **not** do this
   with the Pure plugin enabled: its `Remove` destroys the array volume.
4. Adopt the volumes (once, from any node with the credentials file):

   ```bash
   sudo FA_NAMESPACE=docker ./docker-volume-flasharray adopt --prefix sblinuxdev- --dry-run
   sudo FA_NAMESPACE=docker ./docker-volume-flasharray adopt --prefix sblinuxdev-
   ```

   The binary is on each GitHub release (and as a build artifact of every
   `develop` run). `--volume <array>=<docker>` handles one-offs whose Docker
   name isn't simply the array name minus the prefix.
5. Change `driver: pure` to `driver: flasharray` in the stack files and
   `docker stack deploy`. `Create` is idempotent and resolves names through
   the tag, so the services come up on their existing data.

A single volume can also be imported straight from Docker:

```bash
docker volume create -d flasharray -o import=sblinuxdev-chromadb_data chromadb_data
```

Array volumes keep their original names; only the `dvfa:name` tag is written.

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

Early. The array client, name mapping, mount reference counting and failure
rollback are unit-tested against fakes; the transports are exercised only on
real hardware. Expect the iSCSI path to be the first one hardened.

## License

Apache-2.0.
