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

- **dnscrypt-proxy 2.1.18** — latest formal upstream GitHub release as of 2026-09-18.
- **Go 1.27.1** — current Go 1.27 patch release used by the builder.
- **Alpine 3.24.2** — current Alpine 3.24 patch release used by the runtime image.

Upstream's `master` changelog already contains a **2.1.19** section, but it is not shown as a formal release in the upstream release index. This deployment therefore stays on the tagged 2.1.18 release rather than building from an unreleased branch.

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

The default limits are intentionally bounded for the small instance:

| Setting | Default | Reason |
|---|---:|---|
| `max_clients` | `32` | Bounds dnscrypt-proxy work. |
| `DOH_MAX_INFLIGHT` | `32` | Keeps HTTP and DNS concurrency aligned. |
| `DOH_MAX_CONNS` | `128` | Caps active + idle TCP connection state. |
| `DOH_MAX_BODY` | `8192` | Limits oversized public DoH queries while retaining room for normal DNS/EDNS use. |
| `DNSCRYPT_GOMEMLIMIT` | `192MiB` | Leaves headroom for the rest of the 512 MiB container budget. |
| `DOH_GOMEMLIMIT` | `24MiB` | Small gateway heap target; the gateway has no external Go dependencies. |
| `GOMAXPROCS` | `1` | Avoids running two tiny Go services as if the host had many CPUs. |
| DNS cache | `1024` entries | Useful cache reuse without turning the resolver into a large memory consumer. |
| `cert_refresh_concurrency` | `2` | Keeps maintenance work low on 0.25 vCPU. |
| upstream HTTP keepalive | `30s` | Favors connection reuse and avoids repeated TLS setup. |

The gateway also caps request headers at **8 KiB**, uses a **5-second** read timeout, a **7-second** write timeout, and a **15-second** idle timeout. Each DNS exchange gets one combined **5-second** budget, including a possible UDP-to-TCP retry.

The 7-second HTTP write deadline deliberately exceeds the 5-second DNS budget because Go's `net/http` write deadline covers the whole request handling interval, not only the final socket write.

The two `GOMEMLIMIT` values total 216 MiB, deliberately well under the 512 MiB container budget. `GOMEMLIMIT` is a soft heap target, not a hard cap, and it doesn't account for goroutine stacks, the Go runtime's own non-heap bookkeeping, the static binaries, or OS/container overhead. The remaining headroom (roughly 296 MiB) absorbs that non-heap overhead plus traffic bursts, rather than being unused capacity.

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
- client-disconnect cancellation of the local DNS exchange;
- UDP-to-TCP fallback for truncated local responses;
- `/readyz` checks that the local dnscrypt-proxy TCP listener is reachable.

This is still an **open** DoH endpoint: there is no authentication and no per-client quota. The connection and in-flight caps are instance-level resource controls, not an abuse-prevention system. For a personal or low-traffic public endpoint, the limits are intended to keep the service within the small container budget.

## SnapDeploy environment

Safe defaults are built into the image. Normally only `PORT` needs to match the port supplied by the platform.

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | Public HTTP port. |
| `DOH_BIND` | `0.0.0.0` | Public gateway bind address. |
| `DOH_PATH` | `/dns-query` | Public DoH path; `/`, `/healthz`, and `/readyz` are reserved. |
| `DOH_UPSTREAM_ADDR` | `127.0.0.1:5300` | Internal resolver target; normally leave unchanged. |
| `DOH_MAX_BODY` | `8192` | Request DNS-message limit, up to `65535`. |
| `DOH_MAX_INFLIGHT` | `32` | Hard-capped at `64`. |
| `DOH_MAX_CONNS` | `128` | Hard-capped at `256`. |
| `GOMAXPROCS` | `1` | Recommended value for 0.25 vCPU. |
| `DNSCRYPT_GOMEMLIMIT` | `192MiB` | dnscrypt-proxy Go heap target. |
| `DOH_GOMEMLIMIT` | `24MiB` | gateway Go heap target. |
| `PUBLIC_DOH_URL` | unset | Optional startup log only; does not change routing. |

`DNS_LISTEN` and `SERVER_NAMES` are intentionally not runtime settings. The internal listener and resolver set remain fixed so deployment variables cannot accidentally change the topology or upstream policy.

A ready-to-copy example is provided in `.env.example`.

## Health endpoints

```text
GET /healthz
```

Liveness check for the gateway process. Returns `ok`.

```text
GET /readyz
```

Readiness check for the gateway plus the local dnscrypt-proxy TCP listener. Returns `ready` only when that backend listener can accept a TCP connection.

The Docker `HEALTHCHECK` uses `/readyz`.

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
├── .dockerignore
├── .env.example
├── .gitignore
├── CHANGELOG.md
├── LICENSE
├── README.md
├── start.sh
├── config/
│   └── dnscrypt-proxy.toml
└── doh-gateway/
    ├── go.mod
    ├── main.go
    └── main_test.go
```

The `.github/` directory is intentionally excluded from this release and was not modified.

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

## License

See [`LICENSE`](LICENSE).

## 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not use in 15 minute) |

---

# ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:
- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

