# dnscrypt-proxy 2 + DoH gateway for SnapDeploy

A minimal public DNS-over-HTTPS service designed for **512 MB RAM / 0.25 vCPU** SnapDeploy instances.

The container has two processes:

```text
Internet client
      │ HTTPS
      ▼
SnapDeploy TLS termination
      │ HTTP
      ▼
DoH gateway :$PORT
      │ local DNS over UDP, TCP fallback
      ▼
dnscrypt-proxy 127.0.0.1:5300
      │ HTTPS / DoH
      ├── root.hagezi.org
      ├── wurzn.hagezi.org
      └── juuri.hagezi.org
```

Only the HTTP DoH gateway is intended to be public. The resolver listener is loopback-only, and the final image runs as the unprivileged `dnscrypt` user.

## Versions and upstream policy

- **dnscrypt-proxy 2.1.18** is pinned to a tagged upstream release.
- **Go 1.27** is the gateway/build toolchain family configured by the image.
- **Alpine 3.24** is the runtime/build base family configured by the image.

The deployment uses three fixed HaGeZi **Full Protection** DoH stamps:

| Name | DoH endpoint | IPv4 in stamp |
|---|---|---:|
| `HaGeZiDNS1` | `https://root.hagezi.org/dns-query` | `188.34.161.210` |
| `HaGeZiDNS2` | `https://wurzn.hagezi.org/dns-query` | `159.69.155.94` |
| `HaGeZiDNS3` | `https://juuri.hagezi.org/dns-query` | `95.217.163.17` |

The stamps are checked into `config/dnscrypt-proxy.toml`. Each contains a resolver IP plus TLS hostname/path, so resolver discovery does not require plaintext DNS. `ignore_system_dns = true` prevents dnscrypt-proxy from falling back to the container's normal resolver. No remote `public-resolvers` source is enabled.

The gateway's DNS backend is intentionally fixed at `127.0.0.1:5300`; there is no runtime `DOH_UPSTREAM_ADDR` override that could introduce hostname resolution outside dnscrypt-proxy.

## Resource tuning

The defaults target a small 512 MiB / 0.25 vCPU instance without allocating unbounded per-request state; rate-limit state is kept per source IP and bounded:

| Setting | Default | Reason |
|---|---:|---|
| `max_clients` | `64` | Resolver-side concurrency ceiling; aligned with the gateway's hard `DOH_MAX_INFLIGHT` cap. |
| `DOH_MAX_INFLIGHT` | `64` | Global concurrent DNS-exchange ceiling. Requests over the ceiling fail fast with HTTP 503. |
| `GLOBAL_CONN_LIMIT` | `512` | Canonical global active TCP-connection ceiling enforced at `Accept` time; when unset, the legacy `DOH_MAX_CONNS` value is used before falling back to `512`. Idle keep-alive connections are bounded here; actual DNS work remains capped at 64. |
| `IP_CONN_LIMIT` | `64` | Per-client active TCP connection ceiling. With a trusted reverse proxy, the slot follows the forwarded client identity rather than the proxy socket. |
| `DOH_MAX_IP_REQUESTS` | `16` | Per-client concurrent DoH exchange ceiling. This remains separate from the source-IP rate limit. |
| `DOH_MAX_IP_REQUESTS_PER_MINUTE` | `100` | Per-source-IP request-rate ceiling using a bounded rolling 60-second window; hard-capped at 100 requests/minute. Excess requests receive HTTP 429. |
| `DOH_MAX_CLIENT_STATES` | `8192` | Hard-bounded source-IP state across 16 shards; only idle states are evicted. |
| `DOH_MAX_UDP_PACKET` | `8192` | Resolver-leg UDP packet limit; oversized datagrams are discarded before DNS parsing. |
| `DOH_MAX_TCP_FRAME` | `8192` | Resolver-leg DNS-over-TCP frame limit, checked before body allocation. |
| `DOH_MAX_BODY` | `4096` | Public DoH request DNS-message limit; effective value cannot exceed the TCP frame limit. |
| `DNSCRYPT_GOMEMLIMIT` | `288MiB` | Main resolver Go heap target; paired with `64MiB` gateway heap target, leaving non-heap/runtime headroom inside a 512 MiB instance. |
| `DOH_GOMEMLIMIT` | `64MiB` | Gateway Go heap target; per-source-IP rate state is stored in the existing bounded client-state map. |
| `GOMAXPROCS` | `1` | Appropriate for a 0.25 vCPU service. |
| DNS cache (`cache_size`) | `8192` entries | Keeps cache memory bounded for a 512 MiB instance while retaining useful hit rate across clients. |
| upstream `timeout` | `5000` ms | Below the gateway's default 6 s local exchange deadline. |
| upstream `keepalive` | `120` s | Avoids repeated TLS setup for the three fixed upstream DoH resolvers. |
| `cert_refresh_concurrency` / `cert_refresh_delay` | `2` / `240` min | Bounds background certificate-refresh concurrency and probing frequency. |

For the requested profile, the effective public limits are **`GLOBAL_CONN_LIMIT=512`**, **`IP_CONN_LIMIT=64`**, **`DOH_MAX_BODY=4096`**, **`SERVER_TIMEOUT=6s`**, **`DOH_MAX_INFLIGHT=64`**, **`DOH_MAX_IP_REQUESTS=16`**, and **`DOH_MAX_IP_REQUESTS_PER_MINUTE=100`**. The 100 requests/minute source-IP limit is enforced independently from the concurrency limits. In this dnscrypt-proxy adaptation, `UPSTREAM_MAX_CONNS=4` is not exposed as a separate setting because dnscrypt-proxy owns the encrypted upstream HTTPS connection management internally; the gateway talks only to `127.0.0.1:5300`, while `max_clients=64` bounds resolver-side concurrency. There is likewise no separate `CACHE_MAX_ENTRY_BYTES` knob in dnscrypt-proxy; the public DNS-message limit is `4096` bytes and the resolver-leg transport frame ceiling is `8192` bytes.

The gateway uses a **5-second** HTTP read deadline, an **8-second** write deadline, and a **120-second** idle timeout. Each DNS exchange has one combined **6-second** local deadline, including a possible UDP-to-TCP retry.

The gateway uses a bounded **per-source-IP rolling 60-second window at 100 requests/minute**. Each source-IP state keeps at most 100 request timestamps in a fixed ring, so rate-limit memory is bounded and independent of total traffic. The 101st request inside the active 60-second window fails fast with HTTP 429; the global 64-exchange ceiling continues to protect the 0.25-vCPU instance.

The gateway rejects malformed/oversized input before DNS exchange, buffers and bounds backend responses before returning them, validates DNS response headers against the original query, and never writes a partial backend response as a successful `200 OK`.

## DNS-leak properties

Under the shipped configuration, the DNS path is:

```text
Client HTTPS
    ↓
DoH gateway
    ↓ 127.0.0.1:5300 only
socket bound by dnscrypt-proxy
    ↓ HTTPS/DoH to fixed IPs from checked-in stamps
HaGeZi upstreams
```

The following design choices prevent a normal plaintext-DNS fallback:

- `listen_addresses = ['127.0.0.1:5300']` keeps the resolver off the public network.
- The three static stamps contain their resolver IP addresses.
- `ignore_system_dns = true` disables system-DNS fallback inside dnscrypt-proxy.
- `netprobe_address` is a literal IP/port, so startup probing does not require hostname lookup.
- The DoH gateway always uses the fixed literal `127.0.0.1:5300` backend.

Changing a deployment variable to a hostname elsewhere in the container is outside this shipped topology; the image itself does not expose a configurable external DoH backend.

## Running behind a reverse proxy (SnapDeploy)

SnapDeploy terminates TLS in front of the container, so without further configuration every user reaches the gateway from the platform proxy's address. Set `DOH_TRUSTED_PROXY_CIDRS` to the proxy's network(s) so the real client IP can be taken from `X-Forwarded-For`.

Only list networks you actually trust: a trusted peer can choose which client IP a request is attributed to. For a trusted proxy connection, the per-client connection slot is bound from the **final `X-Forwarded-For` value** on each request, so an HTTP keep-alive connection can move between clients without permanently charging the proxy socket address. If the forwarded identity is missing or malformed, the request is rejected instead of being placed in a shared bucket.

When `DOH_TRUSTED_PROXY_CIDRS` is unset, forwarding headers are ignored and the direct TCP peer address is used as the client identity.

## DoH behavior and hardening

Supported methods:

- `GET /dns-query?dns=<base64url DNS wire message>`
- `POST /dns-query` with a DNS wire message body
- `OPTIONS /dns-query` for CORS preflight

The gateway accepts padded and unpadded base64url plus standard base64 for compatibility. It rejects response messages supplied as requests, oversized messages, empty/short messages, and requests to other paths.

Security/resource controls include:

- loopback-only dnscrypt-proxy listener;
- fixed, checked-in HaGeZi DoH stamps;
- no plaintext DNS fallback;
- no runtime external DoH backend override;
- no query log, NX log, cloaking, forwarding, monitoring UI, or hot reload;
- unprivileged runtime user;
- bounded request/body/header sizes, in-flight exchanges, and connections;
- per-source-IP concurrent request and connection caps;
- per-source-IP rate limit of 100 requests/minute with a bounded rolling-window ring;
- bounded source-IP state with idle-only eviction;
- immediate TCP closes for global/per-source connection overflow;
- oversized UDP datagram rejection before DNS parsing;
- explicit TCP DNS frame and one-query-per-connection guards;
- client-disconnect cancellation of the local DNS exchange;
- UDP-to-TCP fallback under one shared deadline;
- `/readyz` checks that the local dnscrypt-proxy TCP listener is reachable.

This remains an **open** DoH endpoint with no authentication. The resource guard is not a substitute for an upstream WAF/CDN or an application-layer authentication policy.

## SnapDeploy environment

Safe defaults are built into the image. Normally only `PORT` needs to match the port supplied by the platform, plus `DOH_TRUSTED_PROXY_CIDRS` when running behind a reverse proxy. Integer variables ignore surrounding whitespace; invalid values fall back to the default and out-of-range values are clamped as noted.

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | Public HTTP port. |
| `DOH_BIND` | `0.0.0.0` | Public gateway bind address. Must be an IP literal; hostnames are rejected so gateway startup cannot trigger system DNS resolution. |
| `DOH_PATH` | `/dns-query` | Public DoH path; `/`, `/healthz`, and `/readyz` are reserved. |
| `DOH_MAX_BODY` | `4096` | Public DNS-message limit; effective cap is the lower of this value and `DOH_MAX_TCP_FRAME`. |
| `DOH_MAX_INFLIGHT` | `64` | Global concurrent DNS-exchange cap; hard-capped at `64`. |
| `DOH_MAX_CONNS` | `512` | Legacy compatibility alias used only when `GLOBAL_CONN_LIMIT` is unset/empty; hard-capped at `512`. |
| `IP_CONN_LIMIT` | `64` | Per-client active connection cap; hard-capped at `64`. With a trusted proxy, identity comes from final `X-Forwarded-For` and the connection slot can rebind on keep-alive requests. |
| `DOH_MAX_IP_REQUESTS` | `16` | Per-source-IP concurrent DoH request cap; hard-capped at `64`. This is separate from the per-minute rate limit. |
| `DOH_MAX_IP_REQUESTS_PER_MINUTE` | `100` | Per-source-IP rate cap; hard-capped at `100` requests/minute. Excess requests receive `429 Too Many Requests`. |
| `DOH_MAX_CLIENT_STATES` | `8192` | Bounded to `16`-`32768`, rounded down to a multiple of the 16 guard shards. |
| `SERVER_TIMEOUT` | `6` seconds | Shared local DNS exchange deadline, including UDP-to-TCP fallback. |
| `DOH_IDLE_TIMEOUT` | `120` seconds | HTTP keep-alive idle timeout; active DNS exchanges are bounded separately. |
| `DOH_MAX_UDP_PACKET` | `8192` | Resolver-leg UDP packet limit; hard-capped at `65535`. |
| `DOH_MAX_TCP_FRAME` | `8192` | Resolver-leg TCP DNS frame limit; hard-capped at `65535`. |
| `DOH_TRUSTED_PROXY_CIDRS` | unset | Comma-separated trusted proxy prefixes; only then are forwarding headers used. |
| `GOMAXPROCS` | `1` | Recommended for 0.25 vCPU. |
| `DNSCRYPT_GOMEMLIMIT` | `288MiB` | Main dnscrypt-proxy Go heap target for the 512 MiB instance. |
| `DOH_GOMEMLIMIT` | `64MiB` | DoH gateway Go heap target. |
| `PUBLIC_DOH_URL` | unset | Optional startup log only; does not change routing. |

`DNS_LISTEN`, `SERVER_NAMES`, and an external DoH gateway upstream are intentionally not runtime settings. The internal listener and resolver set remain fixed so deployment variables cannot accidentally change the DNS topology or upstream policy. TCP fallback is one-shot: each connection handles exactly one query before closing.

## Health endpoints

```text
GET /healthz
```

Liveness check for the gateway process. Returns `ok`.

```text
GET /readyz
```

Readiness check for the gateway plus the local dnscrypt-proxy TCP listener. Returns `ready` only when that backend listener can accept a TCP connection. The probe result is cached in memory for one second.

The Docker `HEALTHCHECK` uses `/readyz`.

## Shutdown and supervision

`start.sh` is the container entrypoint (PID 1) and supervises both processes. The checked-in dnscrypt-proxy configuration is validated but never rewritten at runtime. If either process exits unexpectedly, the container exits with status `1`. On `SIGTERM`/`SIGINT`/`SIGHUP`, it stops the gateway first so in-flight DoH requests can finish, then dnscrypt-proxy, escalating to `SIGKILL` if necessary.

## Local Docker test

Build and run:

```bash
docker build -t dnscrypt-proxy2-snapdeploy-doh .
docker run --rm -p 8080:8080 dnscrypt-proxy2-snapdeploy-doh
```

Then check:

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

Use a DoH-capable client for `/dns-query`; opening that URL directly in a browser is not a DNS query.

## Project tree

```text
.
├── Dockerfile
├── CHANGELOG.md
├── LICENSE
├── README.md
├── start.sh
├── config/
│   └── dnscrypt-proxy.toml
└── doh-gateway/
    ├── go.mod
    ├── guard.go
    ├── main.go
    └── main_test.go
```

## Updating

When a new dnscrypt-proxy release is available:

1. update `DNSCRYPT_VERSION` to a new **tagged** release;
2. review the upstream release notes;
3. re-check the HaGeZi stamps against the upstream resolver table;
4. run `go vet ./...` and `go test -race ./...` with the production Go toolchain;
5. run a local container health test;
6. add a dated entry to `CHANGELOG.md`.

Do not build directly from an unreleased upstream branch for the production image unless there is a specific reason to do so.

## References

- dnscrypt-proxy: https://github.com/DNSCrypt/dnscrypt-proxy
- Public resolver list: https://github.com/DNSCrypt/dnscrypt-resolvers/tree/master/v3
- HaGeZi DNS: https://github.com/hagezi/dns-servers

## 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |

## ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE).
