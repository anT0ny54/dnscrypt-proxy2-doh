# dnscrypt-proxy 2 — SnapDeploy DoH forwarder

A small Docker image that runs **dnscrypt-proxy 2** as the encrypted DNS upstream and exposes a standards-oriented **DNS-over-HTTPS (DoH)** endpoint suitable for a SnapDeploy service.

## What it does

```text
DNS client
   │ HTTPS /dns-query
   ▼
SnapDeploy HTTPS/TLS
   │ HTTP inside container
   ▼
DoH gateway :${PORT}
   │ DNS over UDP/TCP
   ▼
dnscrypt-proxy :127.0.0.1:5300
   │ encrypted DoH upstream
   ▼
HaGeZiDNS1 / HaGeZiDNS2 / HaGeZiDNS3
```

SnapDeploy is expected to provide the public HTTPS/TLS termination. The DoH gateway only listens inside the container's service port and forwards DNS messages to the local dnscrypt-proxy listener. Both processes run as an unprivileged user inside the container (see [Security and behavior notes](#security-and-behavior-notes)).

The image is tuned for a small service budget of **512 MB RAM and 0.25 vCPU** by limiting concurrent clients, reducing resolver certificate-refresh concurrency, using a smaller DNS cache, capping the Go runtime's memory target, and disabling unused relay metadata refreshes.

## Upstream and version

The image pins **dnscrypt-proxy 2.1.18** (the current release as of this writing) for reproducible builds. The upstream project supports DNS-over-HTTPS and DNSCrypt and maintains signed public resolver sources.

The resolver set is **fixed and self-contained**: `HaGeZiDNS1`, `HaGeZiDNS2`, and `HaGeZiDNS3` are defined directly as static resolver stamps in `config/dnscrypt-proxy.toml`. No public-resolver source is enabled, and the resolver set is **not** runtime-configurable — there is no environment variable that changes it. No resolver names are downloaded or selected dynamically; the container uses only these three configured HaGeZi upstreams.

## SnapDeploy configuration

Create a Docker service from this repository. Set the environment variables below (see also `.env.example`):

```text
DOH_PATH=/dns-query
DOH_BIND=0.0.0.0
DNS_LISTEN=127.0.0.1:5300
DOH_MAX_BODY=65535
```

These have sane defaults and don't need to be set explicitly. The full list, including the optional concurrency and memory-tuning variables, is in [Runtime settings](#runtime-settings) below.

**Do not override `PORT`.** SnapDeploy should provide the service port through `PORT`; the DoH gateway binds to that value. It must be an unprivileged port (>1024) — both processes run as a non-root user, so binding a port below 1024 would fail. This is the normal case for SnapDeploy and similar platforms.

After deployment, set `PUBLIC_DOH_URL` to the exact hostname SnapDeploy assigns your container (`*.containers.snapdeploy.app` alone is only a wildcard pattern, not a usable client endpoint):

```text
PUBLIC_DOH_URL=https://*.containers.snapdeploy.app/dns-query
```

This is cosmetic only — it's echoed in the startup log so you can copy the client URL from there. It doesn't affect DNS routing.

Health check:

```text
GET /healthz
```

Expected response:

```text
ok
```

## Local Docker test

Build:

```bash
docker build -t dnscrypt-proxy2-snapdeploy-doh .
```

Run:

```bash
docker run --rm -p 8080:8080 \
  -e PORT=8080 \
  dnscrypt-proxy2-snapdeploy-doh
```

Then check:

```text
http://127.0.0.1:8080/healthz
```

For a real DoH client, place the container behind HTTPS/TLS and use `/dns-query`. Opening `/dns-query` in a normal browser is not a valid DoH test and may return HTTP 400 because no DNS message was supplied — test with a DoH-capable client, or send an RFC 8484 DNS message using GET (`?dns=`) or POST (`Content-Type: application/dns-message`). The root URL `/` returns a simple diagnostic page instead.

## Runtime settings

| Variable | Default | Purpose |
|---|---|---|
| `DOH_PATH` | `/dns-query` | Public DoH path |
| `DOH_BIND` | `0.0.0.0` | Container interface for the SnapDeploy HTTP service |
| `DNS_LISTEN` | `127.0.0.1:5300` | Local DNS listener used by the gateway |
| `DOH_UPSTREAM_ADDR` | `127.0.0.1:5300` | Gateway's local DNS target |
| `DOH_MAX_BODY` | `65535` | Maximum DoH POST body size in bytes |
| `DOH_MAX_INFLIGHT` | `64` | Max concurrent DNS exchanges the gateway will run before returning `503`; keep at or below dnscrypt-proxy's `max_clients` |
| `DOH_MAX_CONNS` | `512` | Max simultaneously open TCP connections to the gateway |
| `DNSCRYPT_GOMEMLIMIT` | `360MiB` | Go soft memory target for the `dnscrypt-proxy` process only |
| `DOH_GOMEMLIMIT` | `48MiB` | Go soft memory target for the `doh-gateway` process only |
| `PUBLIC_DOH_URL` | (see `.env.example`) | Cosmetic: shown in the startup log only |
| `PORT` | `8080` in the image | Managed by SnapDeploy; don't override there. Must be >1024. |

The resolver set (`SERVER_NAMES` in earlier versions of this deployment) is **not** listed here because it's no longer runtime-configurable; see [Upstream and version](#upstream-and-version).

## Resource profile

The image uses these constrained defaults:

```text
max_clients = 64
cert_refresh_concurrency = 2
keepalive = 10
cache_size = 1024
cache_min_ttl = 300
ipv6_servers = false
dnscrypt_servers = false
doh_servers = true
odoh_servers = false
GOMAXPROCS = 1
DNSCRYPT_GOMEMLIMIT = 360MiB   # dnscrypt-proxy only
DOH_GOMEMLIMIT = 48MiB         # doh-gateway only
DOH_MAX_INFLIGHT = 64          # gateway's own concurrency cap
DOH_MAX_CONNS = 512            # gateway's own connection cap
```

These settings keep the service small without disabling the core encrypted-DoH forwarding path. dnscrypt-proxy's own configuration documents `max_clients`, certificate-refresh concurrency, keepalive, cache, and DoH server selection as tunable runtime options. `GOMAXPROCS` and `GOMEMLIMIT` are Go runtime knobs: Go does not read the container's cgroup CPU quota on its own, so pinning `GOMAXPROCS=1` (shared by both processes) avoids the scheduler/GC sizing itself for however many CPUs the host has.

`GOMEMLIMIT` is a *per-process* soft target, not a container-wide one — each Go runtime reads its own copy of the variable and independently tries to grow toward it. Giving both processes the same value (as an earlier version of this image did) would let each grow toward that figure on its own, so the two are set separately in `start.sh`: most of the budget goes to `dnscrypt-proxy` (`DNSCRYPT_GOMEMLIMIT`, default `360MiB`) since it holds the DNS cache, certificate state, and TLS connection pools, while `doh-gateway` (`DOH_GOMEMLIMIT`, default `48MiB`) is a thin byte-shuffling HTTP-to-DNS proxy with very little to cache. `360 + 48 = 408MiB`, leaving roughly 100MB of headroom under the 512MB container limit for goroutine stacks, the Go runtimes themselves, and non-heap OS memory. `DOH_MAX_INFLIGHT` and `DOH_MAX_CONNS` give the gateway its own backpressure — matching `max_clients` on the concurrency side and capping open connections — so a traffic spike returns fast `503`s instead of piling up goroutines on the 0.25 vCPU budget.

## Security and behavior notes

- Public HTTPS is expected to be terminated by SnapDeploy.
- The DoH gateway accepts standard DNS-over-HTTPS GET (`?dns=`) and POST (`application/dns-message`) requests.
- DNS requests from the gateway are sent only to the local dnscrypt-proxy listener, not directly to public port 53.
- dnscrypt-proxy uses only the three static HaGeZi resolver stamps included in the configuration; no public-resolver source is enabled, and the set can't be changed at runtime.
- Bootstrap resolvers are retained only for hostname/certificate bootstrap where a static stamp requires it. They are not configured as normal upstream resolvers.
- Both `dnscrypt-proxy` and `doh-gateway` — including the internet-facing gateway process — run as an unprivileged user inside the container, dropped via `su-exec` at startup.
- The DoH gateway caps its own concurrent DNS exchanges (`DOH_MAX_INFLIGHT`) and open connections (`DOH_MAX_CONNS`), returning `503` once at capacity instead of queuing unboundedly on a 0.25 vCPU budget. Oversized or empty DoH queries are rejected with `400`/`413` before ever reaching dnscrypt-proxy.
- The DoH gateway shuts down gracefully on `SIGTERM`/`SIGINT` (as sent by `start.sh` on container stop), finishing in-flight requests instead of severing them.
- The container exits if either dnscrypt-proxy or the DoH gateway unexpectedly dies, so the platform can restart a broken instance instead of keeping a partially working process alive.

## Files

```text
.
├── Dockerfile
├── .dockerignore
├── .env.example
├── .gitignore
├── CHANGELOG.md
├── README.md
├── start.sh
├── config/
│   └── dnscrypt-proxy.toml
└── doh-gateway/
    ├── go.mod
    └── main.go
```

## Upstream references

- dnscrypt-proxy: https://github.com/DNSCrypt/dnscrypt-proxy
- Public resolver list: https://github.com/DNSCrypt/dnscrypt-resolvers/tree/master/v3
- HaGeZi DNS: https://github.com/hagezi/dns-servers

## Updating dnscrypt-proxy

Change `DNSCRYPT_VERSION` in the Dockerfile, review the upstream release notes, rebuild, and test the resolver names before deploying. The pinned release is deliberate so a platform rebuild does not unexpectedly move to a different upstream version. Also check whether upstream's `go.mod` now requires a newer Go than the `golang:1.26-alpine` builder image pinned in the Dockerfile, and bump that too if so.
