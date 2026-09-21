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

# Runtime needs the host-side storage tooling the driver shells out to. The
# daemons these packages ship (iscsid, multipathd) are never started here; the
# plugin talks to the host's copies via /run and the host network namespace.
FROM debian:bookworm-slim AS rootfs
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      open-iscsi \
      multipath-tools \
      nvme-cli \
      xfsprogs \
      e2fsprogs \
      util-linux \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/* /etc/iscsi /etc/nvme /etc/multipath* \
 && mkdir -p /mnt/flasharray /etc/docker-volume-flasharray /run/docker/plugins
COPY --from=build /out/docker-volume-flasharray /usr/local/bin/docker-volume-flasharray
ENTRYPOINT ["/usr/local/bin/docker-volume-flasharray"]
