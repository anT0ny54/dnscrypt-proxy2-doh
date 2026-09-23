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
doh_gomemlimit="${DOH_GOMEMLIMIT:-64MiB}"
public_doh_url="${PUBLIC_DOH_URL:-}"
doh_upstream="${DOH_UPSTREAM_ADDR:-$dns_listen}"

# dnscrypt-proxy's max_clients is the resolver's own client concurrency
# ceiling; it must not sit below the DoH gateway's own DOH_MAX_INFLIGHT or
# the gateway can admit more concurrent exchanges than the resolver behind
# it will actually service, silently reintroducing backpressure at the
# resolver even after DOH_MAX_INFLIGHT is raised. Rather than keep a second,
# independent value hardcoded in the checked-in TOML, derive it here from
# the same env var and the same fallback (64) the Go binary uses.
doh_max_inflight="${DOH_MAX_INFLIGHT:-64}"
case "$doh_max_inflight" in
    ''|*[!0-9]*)
        echo "WARNING: DOH_MAX_INFLIGHT=\"$doh_max_inflight\" is invalid; using 64 for dnscrypt-proxy max_clients." >&2
        doh_max_inflight=64
        ;;
    *)
        # Match the Go gateway's envInt() behavior: values below 1 fall back
        # to 32, while values above the hard maximum are clamped to 64.
        while [ "${doh_max_inflight#0}" != "$doh_max_inflight" ]; do
            doh_max_inflight=${doh_max_inflight#0}
        done
        [ -n "$doh_max_inflight" ] || doh_max_inflight=0
        case "$doh_max_inflight" in
            0)
                echo "WARNING: DOH_MAX_INFLIGHT=\"${DOH_MAX_INFLIGHT:-}\" is below the minimum; using 64 for dnscrypt-proxy max_clients." >&2
                doh_max_inflight=64
                ;;
            [1-9]|[1-5][0-9]|6[0-4])
                ;;
            *)
                echo "WARNING: DOH_MAX_INFLIGHT=\"${DOH_MAX_INFLIGHT:-}\" exceeds the maximum; using 64 for dnscrypt-proxy max_clients." >&2
                doh_max_inflight=64
                ;;
        esac
        ;;
esac
sed -i "s/^max_clients = .*/max_clients = ${doh_max_inflight}/" "$CONFIG_FILE"

# Validate the checked-in configuration (with max_clients now synced to
# DOH_MAX_INFLIGHT above) before starting either service.
echo "Checking dnscrypt-proxy configuration..."
if ! "$DNSCRYPT_BIN" -config "$CONFIG_FILE" -check; then
    echo "ERROR: dnscrypt-proxy configuration check failed." >&2
    exit 1
fi

echo "Starting dnscrypt-proxy 2 + DoH gateway"
echo "  dnscrypt-proxy : $dns_listen (max_clients=$doh_max_inflight, synced to DOH_MAX_INFLIGHT)"
echo "  resolvers      : HaGeZiDNS1, HaGeZiDNS2, HaGeZiDNS3 (static)"
echo "  DoH endpoint   : ${doh_bind}:$port$doh_path"
echo "  memory target  : ${dnscrypt_gomemlimit} dnscrypt-proxy + ${doh_gomemlimit} doh-gateway"
echo "  CPU target     : 0.25 vCPU (GOMAXPROCS=${GOMAXPROCS:-1})"
if [ -n "$public_doh_url" ]; then
    echo "  Public DoH URL : $public_doh_url"
fi

# Both processes stay in the image's unprivileged user context. Logs go to
# stdout/stderr so the hosting platform can collect them without file I/O.
dns_pid=0
doh_pid=0
cleanup_done=0

# stop_child PID TICKS
# Send SIGTERM, wait up to TENTHS x 0.1s for a clean exit, then SIGKILL and
# reap. A no-op when PID is 0 (never started) or already gone. The short poll
# interval matters: an exited-but-unreaped child still answers `kill -0`, so
# each poll tick is also what lets the shell reap it.
stop_child() {
    pid=$1
    ticks=$2
    [ "$pid" -gt 0 ] || return 0
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        while [ "$ticks" -gt 0 ] && kill -0 "$pid" 2>/dev/null; do
            sleep 0.1
            ticks=$((ticks - 1))
        done
        if kill -0 "$pid" 2>/dev/null; then
            kill -9 "$pid" 2>/dev/null || true
        fi
    fi
    wait "$pid" 2>/dev/null || true
}

# Runs exactly once, from the EXIT trap. Stop the gateway first so in-flight
# DoH requests can finish while dnscrypt-proxy is still answering, then stop
# dnscrypt-proxy. The gateway's own graceful-shutdown window is 7s, so it gets
# 7.5s here; dnscrypt-proxy gets 2s. Worst case is ~9.5s, inside the common
# 10s container stop timeout.
cleanup() {
    [ "$cleanup_done" -eq 0 ] || return 0
    cleanup_done=1
    # Ignore further signals so a repeated SIGTERM cannot cut the drain short.
    trap '' INT TERM HUP
    echo "Stopping services..."
    stop_child "$doh_pid" 75
    stop_child "$dns_pid" 20
}

# A termination signal exits the script normally (status 0), which in turn
# fires the EXIT trap once. The signal trap must call `exit`: without it the
# main loop below would resume after the handler returned and misreport the
# already-stopped children as an unexpected crash.
trap cleanup EXIT
trap 'exit 0' INT TERM HUP

GOMEMLIMIT="$dnscrypt_gomemlimit" "$DNSCRYPT_BIN" -config "$CONFIG_FILE" &
dns_pid=$!

# The gateway has its own /readyz endpoint and the image health check uses it.
# Starting it immediately avoids a redundant TCP probe and keeps the runtime
# image free of an external netcat dependency. /readyz returns 503 until the
# local dnscrypt-proxy listener is reachable.
GOMEMLIMIT="$doh_gomemlimit" \
    DOH_UPSTREAM_ADDR="$doh_upstream" \
    "$DOH_BIN" &
doh_pid=$!

# Fail the container if either service unexpectedly exits. The 1s poll costs
# a syscall and two comparisons per tick, negligible on 0.25 vCPU.
#
# The pause is a background sleep that we `wait` on: a trapped signal
# interrupts `wait` immediately, whereas a foreground `sleep 1` would delay
# the signal trap by up to a full second.
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
    sleep 1 &
    wait $! || true
done
