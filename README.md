# dnscrypt-proxy 2 + DoH gateway for SnapDeploy

A small Docker service tuned for SnapDeploy's **512 MB RAM / 0.25 vCPU** tier. It runs dnscrypt-proxy behind a lightweight HTTP DoH gateway and uses three fixed HaGeZi full-protection DoH resolvers.

## Architecture

```text
DoH client
   │ HTTPS /dns-query
   ▼
SnapDeploy TLS termination
   │ HTTP
   ▼
DoH gateway :$PORT
   │ DNS over UDP, TCP fallback
   ▼
dnscrypt-proxy :127.0.0.1:5300
   │ encrypted DoH
   ├── root.hagezi.org
   ├── wurzn.hagezi.org
   └── juuri.hagezi.org
```

The public gateway and the resolver both run as the unprivileged `dnscrypt` user. The resolver listener is bound only to loopback, so there is no second public DNS listener.

## Upstream resolvers and DNS-leak behavior

The deployment uses these three fixed HaGeZi full-protection DoH stamps:

- `root.hagezi.org` — `188.34.161.210`
- `wurzn.hagezi.org` — `159.69.155.94`
- `juuri.hagezi.org` — `95.217.163.17`

The stamps contain the resolver IP addresses, so the container does not need a plaintext bootstrap resolver. `ignore_system_dns = true` also prevents the normal system resolver from being used as a fallback for resolver discovery.

No remote `public-resolvers` source is enabled. The checked-in stamps are the source of truth and cannot be replaced through a runtime `SERVER_NAMES` setting.

## Versions

- dnscrypt-proxy: **2.1.18**, pinned to the latest tagged release currently listed upstream.
- Go builder: **Go 1.27**.
- Runtime: **Alpine 3.24.1**.

The upstream master change log already contains a 2.1.19 section, but the upstream release index still lists 2.1.18 as the latest tagged release. This project therefore keeps the tagged 2.1.18 build until a newer release is formally tagged.

## SnapDeploy environment

The image ships with safe defaults; only override settings you actually need.

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` image default | Public HTTP port; use the port supplied by SnapDeploy |
| `DOH_BIND` | `0.0.0.0` | Interface for the public gateway |
| `DOH_PATH` | `/dns-query` | Public DoH path |
| `DOH_UPSTREAM_ADDR` | `127.0.0.1:5300` | Gateway target; normally leave unchanged |
| `DOH_MAX_BODY` | `65535` | Maximum DoH POST body in bytes |
| `DOH_MAX_INFLIGHT` | `32` | Maximum concurrent DNS exchanges; hard-capped at 64 |
| `DOH_MAX_CONNS` | `128` | Maximum open TCP connections; hard-capped at 256 |
| `GOMAXPROCS` | `1` | Keeps both Go services appropriate for 0.25 vCPU |
| `DNSCRYPT_GOMEMLIMIT` | `256MiB` | dnscrypt-proxy Go heap target |
| `DOH_GOMEMLIMIT` | `32MiB` | DoH gateway Go heap target |
| `PUBLIC_DOH_URL` | empty | Optional log-only public URL |

`DNS_LISTEN` and `SERVER_NAMES` are intentionally not runtime settings. The bundled DNS listener and resolver set are fixed so deployment environment variables cannot accidentally break the internal topology or change the upstream policy.


## Resource tuning

The defaults are deliberately conservative for a small instance:

- `max_clients = 32` limits resolver work.
- `DOH_MAX_INFLIGHT = 32` prevents an HTTP burst from creating unbounded DNS work.
- `DOH_MAX_CONNS = 128` bounds idle and active connection state.
- `cache_size = 1024` provides useful reuse without turning the DNS cache into a memory consumer.
- `cert_refresh_concurrency = 2` keeps certificate refresh work small.
- `GOMAXPROCS = 1` avoids scheduling a tiny service as if it had the host's full CPU count.
- Split `GOMEMLIMIT` values keep the two Go runtimes from independently targeting the same large heap budget.

The DNS cache minimum TTL is set to 60 seconds, matching dnscrypt-proxy's current default rather than artificially extending short authoritative TTLs.

The gateway caps request headers at 8 KiB, uses a 15-second idle timeout, and cancels its local DNS exchange when the client request is canceled. UDP responses marked truncated are retried over TCP; a failed TCP fallback is returned as an upstream error rather than serving the truncated packet.

## DoH compatibility

The gateway accepts RFC 8484-style DoH GET (`?dns=`) and POST requests. It supports the standard `application/dns-message` response type and CORS preflight, and it does not require a specific POST `Content-Type` header.

The gateway accepts unpadded/padded base64url and standard base64 for compatibility with common DoH clients. Responses include an explicit `Content-Length` so a single DNS response is not unnecessarily chunked.

The bundled `healthz` endpoint returns `ok` and is used by the Docker health check:

```text
GET /healthz
```

## Container security and logging

The final image runs as the non-root `dnscrypt` user. The startup script no longer edits the DNS configuration at runtime, uses no root privilege helper, and writes no resolver log file. Both long-running services use container stdout/stderr, which avoids extra filesystem I/O and lets SnapDeploy collect logs normally.

The internal resolver is bound to `127.0.0.1:5300`; only the HTTP DoH gateway is intended to be reachable from the network.

## Operational note: open DoH endpoint

Anyone who knows the public URL can send DNS queries. `DOH_MAX_INFLIGHT` and `DOH_MAX_CONNS` bound total instance usage, but they do not provide per-client fairness or authentication. This is suitable for a personal or low-traffic deployment and is not a hardened public DNS service.

## Local Docker test

Build and run:

```bash
docker build -t dnscrypt-proxy2-snapdeploy-doh .
docker run --rm -p 8080:8080 dnscrypt-proxy2-snapdeploy-doh
```

Then verify the health endpoint:

```text
http://127.0.0.1:8080/healthz
```

Opening `/dns-query` directly in a browser is not a DNS query. Use a DoH-capable client or send an RFC 8484 request containing a DNS wire-format message.

## Project files

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

The `.github/` directory is intentionally left unchanged by this optimization pass.

## Updating

Update `DNSCRYPT_VERSION` only to a formally tagged dnscrypt-proxy release. Review the release notes, re-check the HaGeZi stamps against the upstream resolver table, and run the local tests before deployment.

## Upstream references

- dnscrypt-proxy: https://github.com/DNSCrypt/dnscrypt-proxy
- Public resolver list: https://github.com/DNSCrypt/dnscrypt-resolvers/tree/master/v3
- HaGeZi DNS: https://github.com/hagezi/dns-servers

## 📄 License

See [`LICENSE`](LICENSE).


## 🔗 Other projects by the maintainer

These are unrelated to the projects above but are run by the same maintainer.

**My Free DNS** — DNS-over-HTTPS resolvers using HaGeZi Blocklists Multi Pro + TIF:

| Service | DNS-over-HTTPS URL |
| --- | --- |
| Multi Pro + TIF (Recommended) | `https://freedns.koyeb.app/dns-query` |
| Multi Pro + TIF (Recommended) | `https://freedns-six.vercel.app/api/doh/dns-query` |
| Multi Pro + TIF (Backup) | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF (Recommended, but will sleep if not use in 15 minute) | `https://dns-93aca.containers.snapdeploy.app/dns-query` |

**Bandwidth Hero Server** — a lightweight image proxy that fetches remote images, compresses them, and returns optimized versions for faster loading and lower data use: https://bhserv.netlify.app/

## 💜 Support this project

If you'd like to support development, consider donating:

**Bitcoin:** `1HntwKxyGCfnSGvGLMUTRAqLnTvLarAQP`
