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

## Versions verified for this build

- **dnscrypt-proxy 2.1.18** — latest formal upstream GitHub release verified on 2026-09-23.
- **Go 1.27.1** — current Go 1.27 patch release used by the builder.
- **Alpine 3.24.2** — current patched 3.24 release used by the builder and runtime.

Upstream's `master` changelog contains a **2.1.19** section, but the formal GitHub release index still lists **2.1.18** as the latest release. This deployment therefore remains pinned to the tagged 2.1.18 release rather than an unreleased branch.

## Upstream resolvers

The deployment uses three fixed HaGeZi **Full Protection** DoH stamps:

| Name | DoH endpoint | IPv4 in stamp |
|---|---|---:|
| `HaGeZiDNS1` | `https://root.hagezi.org/dns-query` | `188.34.161.210` |
| `HaGeZiDNS2` | `https://wurzn.hagezi.org/dns-query` | `159.69.155.94` |
| `HaGeZiDNS3` | `https://juuri.hagezi.org/dns-query` | `95.217.163.17` |

The stamps are checked into `config/dnscrypt-proxy.toml`. They contain the resolver IP plus the TLS hostname/path, so resolver discovery does not require plaintext DNS.

`ignore_system_dns = true` prevents dnscrypt-proxy from falling back to the container's normal resolver. No remote `public-resolvers` source is enabled, and there is no runtime `SERVER_NAMES` override.

## Resource tuning

The defaults are sized to serve as many users as a **512 MB / 0.25 vCPU** instance can handle while every limit stays bounded:

| Setting | Default | Reason |
|---|---:|---|
| `max_clients` | `64` | Fixed resolver-side concurrency ceiling; the gateway's `DOH_MAX_INFLIGHT` is hard-capped at the same value. |
| `DOH_MAX_INFLIGHT` | `64` | Keeps HTTP and DNS concurrency aligned; values below `1` fall back to `64` and values above `64` clamp to `64`. |
| `DOH_MAX_CONNS` | `256` | Global active TCP-connection cap (about 20 KiB per connection, so roughly 5 MiB at the default); excess connections are accepted then immediately closed. Idle keep-alive connections occupy a slot for up to `DOH_IDLE_TIMEOUT`, so this is what bounds the number of simultaneously connected clients. |
| `DOH_RATE_LIMIT` | `12` (720/min) | Per-source-IP sustained token-bucket refill rate. All Host values share the same bucket. |
| `DOH_RATE_BURST` | `200` | Per-source-IP burst, sized for strict-DoH browser startup/navigation bursts. |
| `GLOBAL_RATE_LIMIT` | `80` (4800/min) | Gateway-wide sustained request rate shared by all source IPs. |
| `GLOBAL_RATE_BURST` | `200` | Gateway-wide burst allowance for short browser request bursts. |
| `IP_CONN_LIMIT` | `32` | Per-source-IP active TCP connection cap; excess connections are immediately closed. Configured trusted proxies are exempt (see below). |
| `DOH_MAX_IP_REQUESTS` | `64` | Concurrent DoH requests per source IP. |
| `DOH_MAX_CLIENT_STATES` | `8192` | Hard-bounded source-IP rate-state entries across 16 shards (about 2 MiB). |
| `DOH_MAX_UDP_PACKET` | `8192` | Resolver-leg UDP packet limit; oversized UDP responses are silently dropped. |
| `DOH_MAX_TCP_FRAME` | `8192` | Resolver-leg DNS-over-TCP frame limit, checked before body allocation/parsing. |
| `DOH_MAX_BODY` | `4096` | Public DoH request DNS-message limit. The effective value cannot exceed `DOH_MAX_TCP_FRAME`. |
| `DNSCRYPT_GOMEMLIMIT` | `288MiB` | Main resolver Go heap target for the 512 MiB container. |
| `DOH_GOMEMLIMIT` | `80MiB` | Gateway Go heap target for the 512 MiB container. |
| `GOMAXPROCS` | `1` | Avoids running two tiny Go services as if the host had many CPUs. |
| DNS cache (`cache_size`) | `16384` entries | Roughly 16-64 MiB of heap; a higher hit rate for many distinct users keeps upstream DoH work (and CPU) low. |
| upstream `timeout` | `5000` ms | Kept below the gateway's 6 s `SERVER_TIMEOUT` so the resolver can answer before the gateway deadline during an upstream outage. |
| upstream `keepalive` | `120` s | Long-lived connections to the three DoH resolvers; avoids repeated TLS handshakes on a small CPU budget. |
| `cert_refresh_concurrency` / `cert_refresh_delay` | `2` / `240` min | Limit how many resolvers are probed at once and how often they are re-probed. |

The gateway sizes request headers for the worst supported Base64 GET representation, including percent-escaped standard Base64, rather than capping them at the DNS-message limit. It uses a **5-second** read timeout, an **8-second** write timeout, and a **120-second** idle timeout. Each DNS exchange gets one combined **6-second** `SERVER_TIMEOUT` budget, including a possible UDP-to-TCP retry.

The public DoH guard uses a sharded token bucket keyed **only by source IP**, plus a gateway-wide token bucket. Multiple browsers/devices behind the same public IP therefore share the per-IP request budget, and changing the Host header cannot create another per-IP bucket. The default sustained per-IP rate is **12 requests per second (720 per minute)** with a **200-request** burst; the gateway-wide default is **80 requests per second (4800 per minute)** with a **200-request** burst. Rate/concurrency rejection happens before base64/DNS parsing, while transport-level overflow is handled separately: oversized UDP datagrams are silently discarded and oversized TCP DNS frames are rejected immediately after the two-byte length prefix, before body allocation. The current TCP fallback opens one connection per DNS exchange, so the maximum queries per TCP connection is fixed at **1**.

The 8-second HTTP write deadline deliberately exceeds the 6-second DNS budget because Go's `net/http` write deadline covers the whole request handling interval, not only the final socket write.

The two `GOMEMLIMIT` values total 368 MiB, leaving roughly 144 MiB of the 512 MiB container budget for non-heap runtime costs, static binaries, stacks, and traffic overhead. `GOMEMLIMIT` is a soft heap target, not a hard cap, and it does not account for goroutine stacks, the Go runtime's own non-heap bookkeeping, the static binaries, or OS/container overhead.

### Running behind a reverse proxy (SnapDeploy)

SnapDeploy terminates TLS in front of the container, so without further configuration **every user reaches the gateway from the platform proxy's address**. The gateway then sees a single client: all users would share one rate bucket. Set `DOH_TRUSTED_PROXY_CIDRS` to the proxy's network(s) so the real client IP is taken from `X-Forwarded-For` (the gateway logs a hint at startup while the variable is unset). Connections from a trusted proxy are also exempt from the per-source-IP connection cap (`IP_CONN_LIMIT`), because they carry many users; they still count against the global `DOH_MAX_CONNS` cap.

Only list networks you actually trust: a trusted peer can choose which client IP a request is attributed to.

### Legacy compatibility

The older environment names `DOH_RATE_RPS` and `DOH_MAX_IP_CONNS` remain accepted as aliases for `DOH_RATE_LIMIT` and `IP_CONN_LIMIT`. When both names are set, the new variable takes precedence.

## DoH behavior and hardening

Supported methods:

- `GET /dns-query?dns=<base64url DNS wire message>`
- `POST /dns-query` with a DNS wire message body
- `OPTIONS /dns-query` for CORS preflight

The gateway accepts both padded and unpadded base64url, plus standard base64 for compatibility with existing clients. It rejects DNS response messages supplied as requests, oversized DNS messages, empty/short messages, and requests to other paths.

Security/resource controls include:

- loopback-only dnscrypt-proxy listener;
- no plaintext upstream DNS;
- fixed, checked-in HaGeZi DoH stamps;
- no query log, NX log, cloaking, forwarding, monitoring UI, or hot reload;
- unprivileged runtime user;
- bounded request size, header size, in-flight work, and connections;
- source-IP-only token-bucket rate limiting and a concurrent-request cap per source IP, plus a gateway-wide request-rate bucket and a per-source-IP TCP connection cap;
- bounded source-IP state with idle-only eviction;
- immediate TCP closes for global/per-source connection abuse;
- silent dropping of oversized UDP response datagrams;
- explicit TCP DNS frame and one-query-per-connection guards;
- client-disconnect cancellation of the local DNS exchange;
- UDP-to-TCP fallback for truncated local responses;
- `/readyz` checks that the local dnscrypt-proxy TCP listener is reachable.

This remains an **open** DoH endpoint with no authentication. The guard adds source-IP rate and concurrency controls, but it is not a substitute for an upstream WAF/CDN or an application-layer authentication policy. With a reverse proxy in front, configure `DOH_TRUSTED_PROXY_CIDRS` so the per-client limits can use the original client IP safely; forwarding headers are otherwise ignored.

The request-rate bucket is keyed only by source IP, so arbitrary `Host` values cannot create additional per-IP buckets. The global `GLOBAL_RATE_LIMIT`, `GLOBAL_RATE_BURST`, `DOH_MAX_INFLIGHT`, `DOH_MAX_CONNS` and `DOH_MAX_CLIENT_STATES` bounds still apply.

## Browser strict-DoH profile

The shipped defaults are tuned for Chrome and Firefox strict DoH / maximum-protection behavior, where navigation can create short request bursts, connections may be opened or closed rapidly, and in-flight requests can be canceled when page or network state changes:

```text
DOH_RATE_LIMIT=12
DOH_RATE_BURST=200
GLOBAL_RATE_LIMIT=80
GLOBAL_RATE_BURST=200
IP_CONN_LIMIT=32
SERVER_TIMEOUT=6
```

The per-IP request controls are keyed only by the source IP. Multiple devices behind one public address therefore share the same per-IP quota; changing `Host` does not create another bucket. `GLOBAL_RATE_LIMIT` and `GLOBAL_RATE_BURST` add an aggregate gateway-wide ceiling on request starts.

A normal browser/client disconnect is expected and is silently canceled. A backend timeout or an invalid DNS response is different: the gateway returns **502 Bad Gateway** instead of sending a truncated or malformed **200 OK** response.

`SERVER_TIMEOUT` is the gateway's total local DNS query deadline. UDP and TCP fallback share that single deadline, so a fallback attempt cannot create another full timeout window.

The runtime has no persisted probe-state database. `/readyz` uses one small in-memory cached result, and therefore cannot load oversized or malformed state or consume unbounded startup memory.

## SnapDeploy environment

Safe defaults are built into the image. Normally only `PORT` needs to match the port supplied by the platform, plus `DOH_TRUSTED_PROXY_CIDRS` when running behind a reverse proxy. Integer variables ignore surrounding whitespace; invalid values fall back to the default and out-of-range values are clamped as noted.

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | Public HTTP port. |
| `DOH_BIND` | `0.0.0.0` | Public gateway bind address. The Docker `HEALTHCHECK` probes `127.0.0.1:$PORT`, so keep a bind address that includes loopback. |
| `DOH_PATH` | `/dns-query` | Public DoH path; `/`, `/healthz`, and `/readyz` are reserved. |
| `DOH_UPSTREAM_ADDR` | `127.0.0.1:5300` | Where the gateway sends queries and probes `/readyz`. It does **not** move the dnscrypt-proxy listener, which is fixed at `127.0.0.1:5300` in `config/dnscrypt-proxy.toml`. Leave unchanged unless you deliberately point the gateway at a different resolver. |
| `DOH_MAX_BODY` | `4096` | Public request DNS-message limit; effective cap is the lower of this value and `DOH_MAX_TCP_FRAME`. |
| `DOH_MAX_INFLIGHT` | `64` | Hard-capped at `64`; keeps concurrent DNS work proportionate to 0.25 vCPU. |
| `DOH_MAX_CONNS` | `256` | Hard-capped at `1024`; excess accepted connections are immediately closed. |
| `DOH_RATE_LIMIT` | `12` (720/min) | Per-source-IP sustained rate; hard-capped at `100`. Legacy `DOH_RATE_RPS` remains accepted as an alias. |
| `DOH_RATE_BURST` | `200` | Per-source-IP burst; hard-capped at `256`. |
| `GLOBAL_RATE_LIMIT` | `80` (4800/min) | Gateway-wide sustained rate; hard-capped at `500`. |
| `GLOBAL_RATE_BURST` | `200` | Gateway-wide burst; hard-capped at `512`. |
| `IP_CONN_LIMIT` | `32` | Per-source-IP active connection cap (trusted proxies exempt); hard-capped at `32`. Legacy `DOH_MAX_IP_CONNS` remains accepted as an alias. |
| `DOH_MAX_IP_REQUESTS` | `64` | Concurrent request cap per source IP; hard-capped at `64`. |
| `DOH_MAX_CLIENT_STATES` | `8192` | Bounded to `16`-`32768`, rounded down to a multiple of the 16 guard shards; rate state is keyed only by source IP. |
| `SERVER_TIMEOUT` | `6` seconds | Shared local DNS exchange deadline, including UDP-to-TCP fallback; client disconnect cancellation remains separate. |
| `DOH_MAX_UDP_PACKET` | `8192` | Resolver-leg UDP packet limit; hard-capped at `65535`. |
| `DOH_MAX_TCP_FRAME` | `8192` | Resolver-leg TCP DNS frame limit; hard-capped at `65535`. |
| `DOH_TRUSTED_PROXY_CIDRS` | unset | Comma-separated trusted proxy prefixes (for example `10.0.0.0/8,172.16.0.0/12`); only then is `X-Forwarded-For` used. **Set this behind the platform proxy**, see above. |
| `GOMAXPROCS` | `1` | Recommended value for 0.25 vCPU. |
| `DNSCRYPT_GOMEMLIMIT` | `288MiB` | Main dnscrypt-proxy Go heap target. |
| `DOH_GOMEMLIMIT` | `80MiB` | DoH gateway Go heap target. |
| `PUBLIC_DOH_URL` | unset | Optional startup log only; does not change routing. |

`DNS_LISTEN` and `SERVER_NAMES` are intentionally not runtime settings. The internal listener and resolver set remain fixed so deployment variables cannot accidentally change the topology or upstream policy. The TCP DNS fallback is intentionally one-shot: each connection handles exactly one query before closing. Client disconnects are treated as normal cancellation and are not logged as upstream failures. Backend timeouts and invalid DNS responses are returned as **502 Bad Gateway**.

## Health endpoints

```text
GET /healthz
```

Liveness check for the gateway process. Returns `ok`.

```text
GET /readyz
```

Readiness check for the gateway plus the local dnscrypt-proxy TCP listener. Returns `ready` only when that backend listener can accept a TCP connection. The probe result is cached in memory for one second, with no persisted probe-state file, so probe state cannot grow without bound or consume unbounded startup memory.

The Docker `HEALTHCHECK` uses `/readyz`.

## Shutdown and supervision

`start.sh` is the container entrypoint (PID 1) and supervises both processes. The checked-in dnscrypt-proxy configuration is validated but never rewritten at runtime. If either one exits unexpectedly, the container exits with status `1` and an `ERROR:` line in the log. On `SIGTERM`/`SIGINT`/`SIGHUP` it stops the gateway first (so in-flight DoH requests can finish), then dnscrypt-proxy, escalating to `SIGKILL` if a child does not exit in time (gateway 7.5 s, dnscrypt-proxy 2 s), and exits with status `0`.

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

1. update `DNSCRYPT_VERSION` to the new **tagged** release;
2. review the upstream release notes;
3. re-check the HaGeZi stamps against the upstream resolver table;
4. run the gateway unit tests and a local container health test;
5. add a dated entry to `CHANGELOG.md`.

Do not build directly from an unreleased upstream `master` commit for the production image unless there is a specific reason to do so.

## References

- dnscrypt-proxy: https://github.com/DNSCrypt/dnscrypt-proxy
- Public resolver list: https://github.com/DNSCrypt/dnscrypt-resolvers/tree/master/v3
- HaGeZi DNS: https://github.com/hagezi/dns-servers

## Validation

The gateway test suite was executed successfully after the strict-DoH changes. The available runner had Go **1.23.2**, while the production module remains pinned to **Go 1.27** in `doh-gateway/go.mod`; the test run therefore used a temporary test-only module-version override. The production file was not changed.

Before deploying, run the same checks with the production toolchain:

```bash
cd doh-gateway
go vet ./...
go test -race ./...
```

and then a local container health test (see above).

## License

See [`LICENSE`](LICENSE).

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
