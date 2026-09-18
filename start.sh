#!/bin/sh
set -eu

CONFIG_FILE=/opt/dnscrypt-proxy/dnscrypt-proxy.toml
DNSCRYPT_BIN=/usr/local/bin/dnscrypt-proxy
DOH_BIN=/usr/local/bin/doh-gateway

dns_listen=127.0.0.1:5300
port="${PORT:-8080}"
doh_path="${DOH_PATH:-/dns-query}"
doh_bind="${DOH_BIND:-0.0.0.0}"
dnscrypt_gomemlimit="${DNSCRYPT_GOMEMLIMIT:-192MiB}"
doh_gomemlimit="${DOH_GOMEMLIMIT:-24MiB}"
public_doh_url="${PUBLIC_DOH_URL:-}"
doh_upstream="${DOH_UPSTREAM_ADDR:-$dns_listen}"

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

# Both processes stay in the image's unprivileged user context. Logs go to
# stdout/stderr so the hosting platform can collect them without file I/O.
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

# Wait for the backend TCP listener before exposing the public gateway.
ready=0
i=0
while [ "$i" -lt 30 ]; do
    if ! kill -0 "$dns_pid" 2>/dev/null; then
        break
    fi
    if nc -z -w 1 127.0.0.1 5300 >/dev/null 2>&1; then
        ready=1
        break
    fi
    i=$((i + 1))
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

# Fail the container if either service unexpectedly exits. A 1s poll (rather
# than 2s) halves worst-case crash-detection latency for negligible CPU cost
# on the 0.25 vCPU budget: this loop is a syscall and two comparisons, not
# meaningful work.
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
    sleep 1
done
