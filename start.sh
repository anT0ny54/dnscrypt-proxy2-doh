#!/bin/sh
set -eu

CONFIG_DIR=/opt/dnscrypt-proxy
CONFIG_FILE="$CONFIG_DIR/dnscrypt-proxy.toml"
DNSCRYPT_BIN=/usr/local/bin/dnscrypt-proxy
DOH_BIN=/usr/local/bin/doh-gateway

dns_listen="${DNS_LISTEN:-127.0.0.1:5300}"

# The resolver set (server_names / [static.*] stamps) is intentionally NOT
# runtime-configurable. This deployment is documented as fully self-contained,
# using only the three pinned HaGeZi stamps baked into dnscrypt-proxy.toml, so
# only the local wiring (listen address) is rewritten here.
sed -i \
    -e "s#^listen_addresses = .*#listen_addresses = ['$dns_listen']#" \
    "$CONFIG_FILE"

mkdir -p "$CONFIG_DIR/cache" /var/log/dnscrypt-proxy
chown -R dnscrypt:dnscrypt "$CONFIG_DIR" /var/log/dnscrypt-proxy

# Validate the final runtime configuration before starting the resolver.
# dnscrypt-proxy supports -check and exits non-zero on invalid TOML/stamps.
echo "Checking dnscrypt-proxy configuration..."
if ! "$DNSCRYPT_BIN" -config "$CONFIG_FILE" -check; then
    echo "ERROR: dnscrypt-proxy configuration check failed." >&2
    exit 1
fi

port="${PORT:-8080}"
doh_path="${DOH_PATH:-/dns-query}"
doh_bind="${DOH_BIND:-0.0.0.0}"

# GOMEMLIMIT is a per-Go-runtime soft target, not a container-wide one. Giving
# both processes the same value would let each independently grow toward it,
# so the two are split here: most of the budget goes to dnscrypt-proxy (cache,
# cert refresh, TLS), and the gateway -- a thin byte-shuffling proxy -- gets a
# small slice. 360MiB + 48MiB leaves headroom under the 512MB container limit
# for goroutine stacks, the runtime itself, and non-heap OS memory.
dnscrypt_gomemlimit="${DNSCRYPT_GOMEMLIMIT:-360MiB}"
doh_gomemlimit="${DOH_GOMEMLIMIT:-48MiB}"
# Optional public URL shown in startup logs/documentation. Replace this value with
# the hostname assigned by SnapDeploy. It does not control DNS routing.
PUBLIC_DOH_URL="${PUBLIC_DOH_URL:-https://dns-871de.containers.snapdeploy.app/dns-query}"

# Read back the pinned resolver names for an accurate startup log line,
# instead of duplicating them as a literal string that could drift from the config.
resolver_names=$(sed -n "s/^server_names = \[\(.*\)\]\$/\1/p" "$CONFIG_FILE" | tr -d "'" | head -n1)

echo "Starting dnscrypt-proxy 2 + DoH gateway"
echo "  dnscrypt-proxy : $dns_listen"
echo "  resolvers      : ${resolver_names:-see config} (pinned, static-only)"
echo "  DoH endpoint   : ${doh_bind}:$port$doh_path"
echo "  Public DoH URL : $PUBLIC_DOH_URL"
echo "  memory target  : ${dnscrypt_gomemlimit} dnscrypt-proxy + ${doh_gomemlimit} doh-gateway (512 MB container limit)"
echo "  CPU target     : 0.25 vCPU"

# Keep dnscrypt-proxy as a child so the shell can stop both processes cleanly.
DNSCRYPT_LOG=/var/log/dnscrypt-proxy/dnscrypt-proxy-runtime.log
: > "$DNSCRYPT_LOG"
chown dnscrypt:dnscrypt "$DNSCRYPT_LOG"
GOMEMLIMIT="$dnscrypt_gomemlimit" su-exec dnscrypt "$DNSCRYPT_BIN" -config "$CONFIG_FILE" >"$DNSCRYPT_LOG" 2>&1 &
dns_pid=$!
doh_pid=0

cleanup() {
    echo "Stopping services..."
    kill "$dns_pid" 2>/dev/null || true
    if [ "$doh_pid" -gt 0 ]; then
        kill "$doh_pid" 2>/dev/null || true
        wait "$doh_pid" 2>/dev/null || true
    fi
    wait "$dns_pid" 2>/dev/null || true
}
trap cleanup INT TERM HUP EXIT

# Wait until dnscrypt-proxy is actually accepting TCP queries.
# Checking only that the process exists can mark the container healthy even when
# the resolver has failed to bind or has not finished initializing.
ready=0
for i in $(seq 1 30); do
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

# The DoH gateway is the internet-facing component (SnapDeploy forwards public
# HTTPS to it directly), so it runs as the unprivileged user too, matching
# dnscrypt-proxy. It needs no root capability: PORT is expected to be an
# unprivileged port (>1024), as SnapDeploy and similar platforms assign.
GOMEMLIMIT="$doh_gomemlimit" su-exec dnscrypt "$DOH_BIN" &
doh_pid=$!

# Exit if either component dies. This makes the container fail fast instead of
# serving a misleadingly healthy HTTP endpoint with no DNS upstream.
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
