#!/bin/sh
set -eu

CONFIG_FILE=/opt/dnscrypt-proxy/dnscrypt-proxy.toml
DNSCRYPT_BIN=/usr/local/bin/dnscrypt-proxy
DOH_BIN=/usr/local/bin/doh-gateway

dns_listen=127.0.0.1:5300
port="${PORT:-8080}"
doh_path="${DOH_PATH:-/dns-query}"
doh_bind="${DOH_BIND:-0.0.0.0}"
dnscrypt_gomemlimit="${DNSCRYPT_GOMEMLIMIT:-256MiB}"
doh_gomemlimit="${DOH_GOMEMLIMIT:-32MiB}"
public_doh_url="${PUBLIC_DOH_URL:-https://dns-93aca.containers.snapdeploy.app/dns-query}"
doh_upstream="${DOH_UPSTREAM_ADDR:-$dns_listen}"

mkdir -p /opt/dnscrypt-proxy/cache

# Validate the checked-in configuration before starting either service.
echo "Checking dnscrypt-proxy configuration..."
if ! "$DNSCRYPT_BIN" -config "$CONFIG_FILE" -check; then
    echo "ERROR: dnscrypt-proxy configuration check failed." >&2
    exit 1
fi

echo "Starting dnscrypt-proxy 2 + DoH gateway"
echo "  dnscrypt-proxy : $dns_listen"
echo "  resolvers      : HaGeZiDNS1, HaGeZiDNS2, HaGeZiDNS3 (static)"
echo "  DoH endpoint   : ${doh_bind}:$port$doh_path"
echo "  memory target  : ${dnscrypt_gomemlimit} dnscrypt-proxy + ${doh_gomemlimit} doh-gateway"
echo "  CPU target     : 0.25 vCPU (GOMAXPROCS=${GOMAXPROCS:-1})"
if [ -n "$public_doh_url" ]; then
    echo "  Public DoH URL : $public_doh_url"
fi

# Run both services as the image's unprivileged user. Container stdout/stderr
# is the single log stream; no runtime log file or root helper is required.
GOMEMLIMIT="$dnscrypt_gomemlimit" "$DNSCRYPT_BIN" -config "$CONFIG_FILE" &
dns_pid=$!
doh_pid=0

cleanup() {
    echo "Stopping services..."
    kill "$dns_pid" 2>/dev/null || true
    if [ "$doh_pid" -gt 0 ]; then
        kill "$doh_pid" 2>/dev/null || true
    fi
    wait "$dns_pid" 2>/dev/null || true
    if [ "$doh_pid" -gt 0 ]; then
        wait "$doh_pid" 2>/dev/null || true
    fi
}
trap cleanup INT TERM HUP EXIT

# Wait for the backend listener before exposing the DoH gateway.
ready=0
for _ in $(seq 1 30); do
    if ! kill -0 "$dns_pid" 2>/dev/null; then
        break
    fi
    if nc -z -w 1 127.0.0.1 5300 >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done

if [ "$ready" -ne 1 ]; then
    echo "ERROR: dnscrypt-proxy is not accepting TCP on $dns_listen within 30s." >&2
    exit 1
fi

GOMEMLIMIT="$doh_gomemlimit" \
    DOH_UPSTREAM_ADDR="$doh_upstream" \
    "$DOH_BIN" &
doh_pid=$!

# Fail the container if either service unexpectedly exits.
while :; do
    if ! kill -0 "$dns_pid" 2>/dev/null; then
        wait "$dns_pid" 2>/dev/null || true
        echo "ERROR: dnscrypt-proxy exited unexpectedly." >&2
        exit 1
    fi
    if ! kill -0 "$doh_pid" 2>/dev/null; then
        wait "$doh_pid" 2>/dev/null || true
        echo "ERROR: DoH gateway exited unexpectedly." >&2
        exit 1
    fi
    sleep 2
done
