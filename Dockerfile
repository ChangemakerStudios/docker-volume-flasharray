# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
#
# Builds the plugin rootfs. Not meant to be run as a normal container — see
# Makefile / .github/workflows/release.yml for how it becomes a managed plugin.
# Images are pinned by digest; move a pin by hand with the tag beside it.

FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:b754301872c53d3dff666ec39e3fae971dc6f5ecd5a9b01fdefabbebb0f59901 AS build
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
FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e AS rootfs
# The upgrade takes Debian's security fixes released since the pinned base image.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y --no-install-recommends \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      xfsprogs \
      e2fsprogs \
      util-linux \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/* /etc/iscsi /etc/nvme /etc/multipath* \
 && mkdir -p /mnt/flasharray /etc/docker-volume-flasharray /run/docker/plugins /run/lock
COPY --from=build /out/docker-volume-flasharray /usr/local/bin/docker-volume-flasharray
ENTRYPOINT ["/usr/local/bin/docker-volume-flasharray"]
