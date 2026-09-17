#!/bin/sh
set -eu

CONFIG_DIR=/opt/dnscrypt-proxy
CONFIG_FILE="$CONFIG_DIR/dnscrypt-proxy.toml"
DNSCRYPT_BIN=/usr/local/bin/dnscrypt-proxy
DOH_BIN=/usr/local/bin/doh-gateway

dns_listen="${DNS_LISTEN:-127.0.0.1:5300}"

# Only the local listener is runtime-configurable. Resolver names and stamps
# remain fixed in the checked-in config so deployment env cannot bypass the
# intended HaGeZi-only upstream set.
sed -i \
    -e "s#^listen_addresses = .*#listen_addresses = ['$dns_listen']#" \
    "$CONFIG_FILE"

mkdir -p "$CONFIG_DIR/cache" /var/log/dnscrypt-proxy
chown -R dnscrypt:dnscrypt "$CONFIG_DIR" /var/log/dnscrypt-proxy

# Validate the final configuration before starting either service.
echo "Checking dnscrypt-proxy configuration..."
if ! "$DNSCRYPT_BIN" -config "$CONFIG_FILE" -check; then
    echo "ERROR: dnscrypt-proxy configuration check failed." >&2
    exit 1
fi

port="${PORT:-8080}"
doh_path="${DOH_PATH:-/dns-query}"
doh_bind="${DOH_BIND:-0.0.0.0}"
dnscrypt_gomemlimit="${DNSCRYPT_GOMEMLIMIT:-256MiB}"
doh_gomemlimit="${DOH_GOMEMLIMIT:-32MiB}"
public_doh_url="${PUBLIC_DOH_URL:-https://dns-93aca.containers.snapdeploy.app/dns-query}"

resolver_names=$(sed -n "s/^server_names = //p" "$CONFIG_FILE" | tr -d "'" | head -n1)

echo "Starting dnscrypt-proxy 2 + DoH gateway"
echo "  dnscrypt-proxy : $dns_listen"
echo "  resolvers      : ${resolver_names:-see config} (static HaGeZi-only)"
echo "  DoH endpoint   : ${doh_bind}:$port$doh_path"
echo "  memory target  : ${dnscrypt_gomemlimit} dnscrypt-proxy + ${doh_gomemlimit} doh-gateway"
echo "  CPU target     : 0.25 vCPU (GOMAXPROCS=${GOMAXPROCS:-1})"
if [ -n "$public_doh_url" ]; then
    echo "  Public DoH URL : $public_doh_url"
fi

DNSCRYPT_LOG=/var/log/dnscrypt-proxy/dnscrypt-proxy-runtime.log
: > "$DNSCRYPT_LOG"
chown dnscrypt:dnscrypt "$DNSCRYPT_LOG"

# Both long-running processes are supervised by this PID 1 shell. Each is
# explicitly memory-limited and drops to the unprivileged dnscrypt user.
GOMEMLIMIT="$dnscrypt_gomemlimit" su-exec dnscrypt "$DNSCRYPT_BIN" -config "$CONFIG_FILE" >"$DNSCRYPT_LOG" 2>&1 &
dns_pid=$!
doh_pid=0

cleanup() {
    echo "Stopping services..."
    kill "$dns_pid" 2>/dev/null || true
    if [ "$doh_pid" -gt 0 ]; then
        kill "$doh_pid" 2>/dev/null || true
    fi
    if [ "$doh_pid" -gt 0 ]; then
        wait "$doh_pid" 2>/dev/null || true
    fi
    wait "$dns_pid" 2>/dev/null || true
}
trap cleanup INT TERM HUP EXIT

# Wait for the backend listener before exposing the DoH gateway.
ready=0
for _ in $(seq 1 30); do
    if ! kill -0 "$dns_pid" 2>/dev/null; then
        break
    fi
    dns_host=${dns_listen%:*}
    dns_port=${dns_listen##*:}
    if busybox nc -z -w 1 "$dns_host" "$dns_port" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done

if [ "$ready" -ne 1 ]; then
    echo "ERROR: dnscrypt-proxy is not accepting TCP on $dns_listen within 30s." >&2
    echo "Last resolver log:" >&2
    tail -n 80 "$DNSCRYPT_LOG" >&2 || true
    exit 1
fi

GOMEMLIMIT="$doh_gomemlimit" su-exec dnscrypt "$DOH_BIN" &
doh_pid=$!

# Fail the container if either service unexpectedly exits.
while :; do
    if ! kill -0 "$dns_pid" 2>/dev/null; then
        wait "$dns_pid" 2>/dev/null || true
        echo "ERROR: dnscrypt-proxy exited unexpectedly. Last resolver log:" >&2
        tail -n 80 "$DNSCRYPT_LOG" >&2 || true
        exit 1
    fi
    if ! kill -0 "$doh_pid" 2>/dev/null; then
        wait "$doh_pid" 2>/dev/null || true
        echo "ERROR: DoH gateway exited unexpectedly." >&2
        exit 1
    fi
    sleep 2
done
