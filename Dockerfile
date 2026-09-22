# syntax=docker/dockerfile:1.7
#
# Builds the plugin rootfs. Not meant to be run as a normal container — see
# Makefile / .github/workflows/release.yml for how it becomes a managed plugin.

ARG GO_VERSION=1.23

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/docker-volume-flasharray ./cmd/docker-volume-flasharray

# The plugin runs the host's own iscsiadm, multipath tools, dmsetup, nvme, mkfs,
# blkid, xfs_admin and tune2fs in the host mount and IPC namespaces (nsenter),
# so they match the host's daemons and kernel. The image carries mount/umount
# (the mount must land in the plugin's propagated mount), blockdev and nsenter;
# xfsprogs/e2fsprogs stay only as a fallback when not run with pidhost.
FROM debian:bookworm-slim AS rootfs
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      xfsprogs \
      e2fsprogs \
      util-linux \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/* /etc/iscsi /etc/nvme /etc/multipath* \
 && mkdir -p /mnt/flasharray /etc/docker-volume-flasharray /run/docker/plugins /run/lock
COPY --from=build /out/docker-volume-flasharray /usr/local/bin/docker-volume-flasharray
ENTRYPOINT ["/usr/local/bin/docker-volume-flasharray"]
