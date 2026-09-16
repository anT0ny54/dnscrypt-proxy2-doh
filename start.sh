#!/bin/sh
set -eu

CONFIG_DIR=/opt/dnscrypt-proxy
CONFIG_FILE="$CONFIG_DIR/dnscrypt-proxy.toml"
DNSCRYPT_BIN=/usr/local/bin/dnscrypt-proxy
DOH_BIN=/usr/local/bin/doh-gateway

dns_listen="${DNS_LISTEN:-127.0.0.1:5300}"
server_names="${SERVER_NAMES:-HaGeZiDNS1,HaGeZiDNS2,HaGeZiDNS3}"

# Convert SERVER_NAMES=Name1,Name2 into TOML string-array syntax.
# Resolver names are expected to be simple public-resolver identifiers.
toml_names=""
old_ifs=$IFS
IFS=','
for name in $server_names; do
    name=$(printf '%s' "$name" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
    [ -n "$name" ] || continue
    [ -z "$toml_names" ] || toml_names="$toml_names, "
    # Escape backslash and single quote for TOML single-quoted strings.
    name=$(printf '%s' "$name" | sed "s/\\\\/\\\\\\\\/g; s/'/''/g")
    toml_names="$toml_names'$name'"
done
IFS=$old_ifs

[ -n "$toml_names" ] || {
    echo "ERROR: SERVER_NAMES must contain at least one resolver name." >&2
    exit 1
}

# Write only the runtime-controlled values; keep the rest of the pinned config immutable.
sed -i \
    -e "s#^server_names = .*#server_names = [$toml_names]#" \
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
# Optional public URL shown in startup logs/documentation. Replace this value with
# the hostname assigned by SnapDeploy. It does not control DNS routing.
PUBLIC_DOH_URL="${PUBLIC_DOH_URL:-https://dns20-pmp6r1vn.b4a.run/dns-query}"

echo "Starting dnscrypt-proxy 2 + DoH gateway"
echo "  dnscrypt-proxy : $dns_listen"
echo "  server_names   : $server_names (static-only)"
echo "  DoH endpoint   : ${doh_bind}:$port$doh_path"
echo "  Public DoH URL : $PUBLIC_DOH_URL"
echo "  memory target  : 512 MB"
echo "  CPU target     : 0.25 vCPU"

# Keep dnscrypt-proxy as a child so the shell can stop both processes cleanly.
DNSCRYPT_LOG=/var/log/dnscrypt-proxy/dnscrypt-proxy-runtime.log
: > "$DNSCRYPT_LOG"
chown dnscrypt:dnscrypt "$DNSCRYPT_LOG"
su-exec dnscrypt "$DNSCRYPT_BIN" -config "$CONFIG_FILE" >"$DNSCRYPT_LOG" 2>&1 &
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

"$DOH_BIN" &
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
