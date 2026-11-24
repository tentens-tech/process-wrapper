# The builder runs on the build host and cross-compiles for the target
# platform, so a multi-arch build doesn't need emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG BUILDARCH
ARG TARGETOS=linux
ARG TARGETARCH
# Version reported by `process-wrapper -version`. When empty, it's taken from
# `git describe` if the build context has .git, otherwise "unknown".
ARG VERSION=""
ARG upx_version=5.3.0
# sha256 of upx-${upx_version}-<arch>_linux.tar.xz, update together with upx_version
ARG upx_sha256_amd64=79cea8a50b6fa102d10c7b79a419cd77dead3cc2d78661de2dc5e5815dc505c1
ARG upx_sha256_arm64=2a4afa860ac7aa6968f88df7841823746e5d00ba56affdf9342c2f5254fb776f
ARG GOPROXY=https://proxy.golang.org

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# hadolint ignore=DL3008
RUN case "${BUILDARCH}" in \
      amd64) upx_sha256=${upx_sha256_amd64} ;; \
      arm64) upx_sha256=${upx_sha256_arm64} ;; \
      *) echo "unsupported build arch ${BUILDARCH}" >&2; exit 1 ;; \
    esac && \
  apt-get update && apt-get install -y --no-install-recommends xz-utils && \
  curl -fsSL "https://github.com/upx/upx/releases/download/v${upx_version}/upx-${upx_version}-${BUILDARCH}_linux.tar.xz" -o /tmp/upx.tar.xz && \
  echo "${upx_sha256}  /tmp/upx.tar.xz" | sha256sum -c - && \
  tar xJf /tmp/upx.tar.xz -C /tmp && \
  install -m 0755 "/tmp/upx-${upx_version}-${BUILDARCH}_linux/upx" /usr/local/bin/upx && \
  rm -rf /tmp/upx* /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . /app

RUN VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo unknown)}; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o app -ldflags="-s -w -X 'main.version=${VERSION}'" main.go && \
    /usr/local/bin/upx --best --lzma app

FROM alpine:3.24 AS process-wrapper

ARG VERSION=""
LABEL org.opencontainers.image.title="process-wrapper" \
      org.opencontainers.image.description="Process manager for containers" \
      org.opencontainers.image.source="https://github.com/tentens-tech/process-wrapper" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

# Unprivileged runtime user. Numeric UID/GID so that Kubernetes
# `runAsNonRoot: true` can verify it without resolving names.
ARG UID=1010
ARG GID=1010
RUN addgroup -S -g "${GID}" appuser && \
    adduser -S -D -H -u "${UID}" -G appuser -h /nonexistent -s /sbin/nologin appuser

# The binary is owned by root and read-only for everyone, so the runtime user
# can't modify it. The image is designed for a read-only root filesystem:
# the only paths process-wrapper writes to are -readiness-file (default
# /tmp/started) and -pid-file, so mount a writable /tmp (tmpfs / emptyDir).
COPY --from=builder --chown=0:0 --chmod=0555 /app/app /usr/bin/process-wrapper

WORKDIR /tmp
USER ${UID}:${GID}
