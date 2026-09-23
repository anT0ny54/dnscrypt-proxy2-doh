# syntax=docker/dockerfile:1

ARG DNSCRYPT_VERSION=2.1.18
ARG GO_VERSION=1.27.1
ARG ALPINE_VERSION=3.24

# Build on the requested target platform and cross-compile the two static Go
# binaries. The Go patch version and dnscrypt-proxy release are pinned.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build

ARG DNSCRYPT_VERSION
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

RUN apk add --no-cache git ca-certificates
RUN git clone --depth 1 --filter=blob:none --branch "${DNSCRYPT_VERSION}" \
    https://github.com/DNSCrypt/dnscrypt-proxy.git .

# BuildKit cache mounts affect build speed only; they are not copied into the
# runtime image. GOTOOLCHAIN=local prevents an unexpected toolchain download.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOTOOLCHAIN=local GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/dnscrypt-proxy ./dnscrypt-proxy

# -buildvcs=false: /src is a git checkout of dnscrypt-proxy, so without it Go
# would stamp the gateway binary with dnscrypt-proxy's commit as its own.
COPY doh-gateway /src/doh-gateway
WORKDIR /src/doh-gateway
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOTOOLCHAIN=local GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/doh-gateway .

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates && \
    addgroup -S dnscrypt && \
    adduser -S -D -H -s /sbin/nologin -G dnscrypt dnscrypt

# Modes are set at COPY time. A separate `RUN chmod` would rewrite the file
# metadata in a new layer and store a second full copy of every binary.
COPY --from=build --chmod=0755 /out/dnscrypt-proxy /usr/local/bin/dnscrypt-proxy
COPY --from=build --chmod=0755 /out/doh-gateway /usr/local/bin/doh-gateway
COPY --chown=dnscrypt:dnscrypt config/dnscrypt-proxy.toml /opt/dnscrypt-proxy/dnscrypt-proxy.toml
COPY --chown=dnscrypt:dnscrypt --chmod=0755 start.sh /usr/local/bin/start.sh

ENV PORT=8080 \
    DOH_BIND=0.0.0.0 \
    DOH_PATH=/dns-query \
    DOH_UPSTREAM_ADDR=127.0.0.1:5300 \
    DOH_MAX_BODY=4096 \
    DOH_MAX_INFLIGHT=64 \
    DOH_MAX_CONNS=96 \
    DOH_RATE_RPS=1.6666667 \
    DOH_RATE_BURST=80 \
    DOH_MAX_IP_CONNS=32 \
    DOH_MAX_IP_REQUESTS=64 \
    DOH_MAX_CLIENT_STATES=4096 \
    DOH_IDLE_TIMEOUT=120 \
    DOH_MAX_UDP_PACKET=8192 \
    DOH_MAX_TCP_FRAME=8192 \
    DOH_TRUSTED_PROXY_CIDRS= \
    GOMAXPROCS=1 \
    DNSCRYPT_GOMEMLIMIT=288MiB \
    DOH_GOMEMLIMIT=80MiB

EXPOSE 8080

# /readyz checks both the gateway and the local dnscrypt-proxy listener.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O - "http://127.0.0.1:${PORT}/readyz" | grep -q '^ready$' || exit 1

USER dnscrypt
ENTRYPOINT ["/usr/local/bin/start.sh"]
