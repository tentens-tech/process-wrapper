FROM golang:1.26-bookworm AS builder

ARG upx_version=5.3.0
# sha256 of upx-${upx_version}-amd64_linux.tar.xz, update together with upx_version
ARG upx_sha256=79cea8a50b6fa102d10c7b79a419cd77dead3cc2d78661de2dc5e5815dc505c1
ARG GOPROXY=https://proxy.golang.org

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends xz-utils && \
  curl -fsSL "https://github.com/upx/upx/releases/download/v${upx_version}/upx-${upx_version}-amd64_linux.tar.xz" -o /tmp/upx.tar.xz && \
  echo "${upx_sha256}  /tmp/upx.tar.xz" | sha256sum -c - && \
  tar xJf /tmp/upx.tar.xz -C /tmp && \
  install -m 0755 "/tmp/upx-${upx_version}-amd64_linux/upx" /usr/local/bin/upx && \
  rm -rf /tmp/upx* /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . /app

RUN VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo unknown); \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o app -ldflags="-s -w -X 'main.version=${VERSION}'" main.go && \
    /usr/local/bin/upx --best --lzma app

FROM alpine:3.24 AS process-wrapper

# Unprivileged runtime user. Numeric UID/GID so that Kubernetes
# `runAsNonRoot: true` can verify it without resolving names.
ARG UID=65532
ARG GID=65532
RUN addgroup -S -g "${GID}" nonroot && \
    adduser -S -D -H -u "${UID}" -G nonroot -h /nonexistent -s /sbin/nologin nonroot

# The binary is owned by root and read-only for everyone, so the runtime user
# can't modify it. The image is designed for a read-only root filesystem:
# the only paths process-wrapper writes to are -readiness-file (default
# /tmp/started) and -pid-file, so mount a writable /tmp (tmpfs / emptyDir).
COPY --from=builder --chown=0:0 --chmod=0555 /app/app /usr/bin/process-wrapper

WORKDIR /tmp
USER ${UID}:${GID}
