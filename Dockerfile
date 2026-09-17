# syntax=docker/dockerfile:1

ARG DNSCRYPT_VERSION=2.1.18

# Pin the toolchain to the current Go 1.27 line for reproducible, current builds.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG DNSCRYPT_VERSION
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

RUN apk add --no-cache git ca-certificates
RUN git clone --depth 1 --branch ${DNSCRYPT_VERSION} https://github.com/DNSCrypt/dnscrypt-proxy.git .

# Cache mounts persist Go's module and build caches across rebuilds (when the
# builder backend supports it); a redeploy that only touches the gateway
# source, for example, does not have to recompile dnscrypt-proxy's dependency
# graph from scratch. They have no effect on the resulting image.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/dnscrypt-proxy ./dnscrypt-proxy

COPY doh-gateway /src/doh-gateway
WORKDIR /src/doh-gateway
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/doh-gateway .

FROM alpine:3.24.1

RUN apk add --no-cache ca-certificates wget su-exec && \
    addgroup -S dnscrypt && \
    adduser -S -D -H -s /sbin/nologin -G dnscrypt dnscrypt && \
    mkdir -p /opt/dnscrypt-proxy/cache /var/log/dnscrypt-proxy && \
    chown -R dnscrypt:dnscrypt /opt/dnscrypt-proxy /var/log/dnscrypt-proxy

COPY --from=build /out/dnscrypt-proxy /usr/local/bin/dnscrypt-proxy
COPY --from=build /out/doh-gateway /usr/local/bin/doh-gateway
COPY config/dnscrypt-proxy.toml /opt/dnscrypt-proxy/dnscrypt-proxy.toml
COPY start.sh /usr/local/bin/start.sh

RUN chmod 0755 /usr/local/bin/start.sh /usr/local/bin/dnscrypt-proxy /usr/local/bin/doh-gateway

ENV PORT=8080 \
    DOH_BIND=0.0.0.0 \
    DNS_LISTEN=127.0.0.1:5300 \
    DOH_PATH=/dns-query \
    DOH_UPSTREAM_ADDR=127.0.0.1:5300 \
    DOH_MAX_BODY=65535 \
    DOH_MAX_INFLIGHT=32 \
    DOH_MAX_CONNS=128 \
    GOMAXPROCS=1 \
    DNSCRYPT_GOMEMLIMIT=256MiB \
    DOH_GOMEMLIMIT=32MiB

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=25s --retries=3 \
  CMD wget -q -O - "http://127.0.0.1:${PORT}/healthz" | grep -q '^ok$' || exit 1

USER root
ENTRYPOINT ["/usr/local/bin/start.sh"]
