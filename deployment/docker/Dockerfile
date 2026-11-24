# Full image: process-wrapper with the sidecars it usually supervises (envoy,
# proxysql, amqproxy, gomcrouter, the PHP profiler) on debian:12-slim.
# deployment/docker/Dockerfile-slim builds an image with process-wrapper only.
#
# Go stages run on the build host and cross-compile for the target platform.
# The other stages and the final stage run on the target platform.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS process-wrapper
ARG TARGETOS=linux
ARG TARGETARCH
# Version reported by `process-wrapper -version`. When empty, it's taken from
# `git describe` if the build context has .git, otherwise "unknown".
ARG VERSION=""
ARG GOPROXY=https://proxy.golang.org
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . /app
RUN VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo unknown)}; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o app -ldflags="-s -w -X 'main.version=${VERSION}'" main.go

# phpspy in hiload/phpscope is dynamically linked against musl libc and won't run
# on debian. Rebuild it statically from upstream source.
FROM alpine:3.23 AS phpspy-builder
ARG phpspy_version=0.7.0
RUN apk add --no-cache git make gcc musl-dev && \
    git clone --depth 1 --branch v${phpspy_version} https://github.com/adsr/phpspy.git /phpspy && \
    cd /phpspy && LDFLAGS="-static" make

FROM ghcr.io/tentens-tech/gomcrouter:v1.3.3 AS gomcrouter
FROM envoyproxy/envoy:v1.34.2 AS envoy
FROM hiload/phpscope:v0.1.3 AS phpscope

FROM debian:12-slim AS process-wrapper-full
ARG TARGETARCH
ARG VERSION=""
ARG upx_version=5.3.0
# sha256 of upx-${upx_version}-<arch>_linux.tar.xz, update together with upx_version
ARG upx_sha256_amd64=79cea8a50b6fa102d10c7b79a419cd77dead3cc2d78661de2dc5e5815dc505c1
ARG upx_sha256_arm64=2a4afa860ac7aa6968f88df7841823746e5d00ba56affdf9342c2f5254fb776f
ARG amqproxy_version=3.2.2-1
ARG proxysql_version=3.0.2
ARG UID=1010
ARG GID=1010
ENV GOMAXPROCS=2

LABEL org.opencontainers.image.title="process-wrapper" \
      org.opencontainers.image.description="Process manager for containers, with sidecars" \
      org.opencontainers.image.source="https://github.com/tentens-tech/process-wrapper" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=process-wrapper /app/app /usr/bin/process-wrapper
COPY --from=gomcrouter /bin/gomcrouter /usr/bin/gomcrouter
COPY --from=envoy /usr/local/bin/envoy /usr/bin/envoy
COPY --from=phpscope /usr/local/bin/phpscope /usr/bin/phpscope
COPY --from=phpspy-builder /phpspy/phpspy /usr/bin/phpspy

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# Install proxysql and amqproxy, then strip and compress the binaries with UPX
# and remove the build tools.
# hadolint ignore=DL3008
RUN case "${TARGETARCH}" in \
      amd64) upx_sha256=${upx_sha256_amd64} ;; \
      arm64) upx_sha256=${upx_sha256_arm64} ;; \
      *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    apt-get update && apt-get install -y --no-install-recommends \
      binutils \
      xz-utils \
      curl \
      lsb-release \
      gnupg \
      ca-certificates \
      procps && \
    curl -fsSL "https://github.com/upx/upx/releases/download/v${upx_version}/upx-${upx_version}-${TARGETARCH}_linux.tar.xz" -o /tmp/upx.tar.xz && \
    echo "${upx_sha256}  /tmp/upx.tar.xz" | sha256sum -c - && \
    tar xJf /tmp/upx.tar.xz -C /tmp && \
    install -m 0755 "/tmp/upx-${upx_version}-${TARGETARCH}_linux/upx" /usr/local/bin/upx && \
    curl -fsSL "https://repo.proxysql.com/ProxySQL/proxysql-3.0.x/repo_pub_key.gpg" -o /etc/apt/trusted.gpg.d/proxysql-3.0.x-keyring.gpg && \
    echo "deb https://repo.proxysql.com/ProxySQL/proxysql-3.0.x/$(lsb_release -sc)/ ./" > /etc/apt/sources.list.d/proxysql.list && \
    curl -fsSL https://packagecloud.io/cloudamqp/amqproxy/gpgkey | gpg --dearmor > /usr/share/keyrings/amqproxy.gpg && \
    echo "deb [signed-by=/usr/share/keyrings/amqproxy.gpg] https://packagecloud.io/cloudamqp/amqproxy/debian bookworm main" > /etc/apt/sources.list.d/amqproxy.list && \
    apt-get update && \
    apt-get install -y systemd-sysv "proxysql=${proxysql_version}" "amqproxy=${amqproxy_version}" && \
    BINARIES="/usr/bin/proxysql /usr/bin/envoy \
      /usr/bin/gomcrouter /usr/bin/amqproxy /usr/bin/process-wrapper /usr/bin/phpscope \
      /bin/systemctl /bin/tar /lib/systemd/systemd-networkd /lib/systemd/systemd /usr/bin/openssl" && \
    LARGE=$(find /usr/bin/gpg* /usr/bin/perl* -type f -size +100k -executable) && \
    for b in $BINARIES /usr/bin/phpspy $LARGE; do strip "$b" 2>/dev/null || true; done && \
    for b in $BINARIES $LARGE; do upx -q --best --lzma "$b" >/dev/null; done && \
    rm -f /usr/share/doc/libboost*/copyright && \
    apt-get remove -y binutils xz-utils curl lsb-release gnupg && \
    apt-get autoremove -y && \
    rm -f /usr/local/bin/upx && \
    rm -rf /tmp/* /var/lib/apt/lists/* /var/cache/*

# bash has a readonly UID variable, so use sh for the build args UID and GID.
SHELL ["/bin/sh", "-c"]
RUN groupadd --gid "${GID}" appuser && \
    useradd --uid "${UID}" --gid "${GID}" --no-create-home --shell /usr/sbin/nologin appuser

# The working directory is / like in the previous images, set -pid-file to a writable path.

USER ${UID}:${GID}
