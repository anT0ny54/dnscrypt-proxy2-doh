# dnscrypt-proxy 2 + DoH gateway for SnapDeploy

A small, self-contained Docker service for SnapDeploy's **512 MB RAM / 0.25 vCPU** tier. It runs dnscrypt-proxy with three fixed HaGeZi DNS-over-HTTPS upstreams and exposes a standards-oriented DoH endpoint at `/dns-query`.

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

The gateway and dnscrypt-proxy both run as the unprivileged `dnscrypt` user. The gateway is the only public listener.

## Upstream choice and DNS-leak behavior

The image uses three static HaGeZi full-protection DoH stamps. The stamps include the resolver IP addresses, so this deployment does **not** configure a plaintext bootstrap resolver and does **not** use the system resolver (`ignore_system_dns = true`). This keeps normal user queries on the encrypted DoH path and removes an unnecessary port-53 bootstrap path.

The three current HaGeZi DoH stamps documented by the upstream project are used unchanged here. HaGeZi currently documents the corresponding endpoints as `root.hagezi.org`, `wurzn.hagezi.org`, and `juuri.hagezi.org`.

No `public-resolvers` source is enabled. The container is intentionally self-contained: resolver names and stamps are in `config/dnscrypt-proxy.toml` and are not exposed as runtime environment overrides.

## Versions

- dnscrypt-proxy: **2.1.18**, pinned for reproducible builds. `doh-gateway/go.mod` targets the same Go 1.27 line as the builder image.
- Go builder: **Go 1.27**.
- Runtime: **Alpine 3.24.1**.

As of September 17, 2026, the dnscrypt-proxy GitHub release page still lists 2.1.18 as the latest tagged release; upstream's current change log also contains a 2.1.19 section, so this project deliberately remains on the latest tagged release rather than an untagged/newer development state.

## SnapDeploy settings

The image has safe defaults; the following variables are optional.

| Variable | Default | Purpose |
|---|---|---|
| `DOH_PATH` | `/dns-query` | Public DoH path |
| `DOH_BIND` | `0.0.0.0` | Interface for the SnapDeploy service listener |
| `DOH_UPSTREAM_ADDR` | `127.0.0.1:5300` | Gateway target |
| `DNS_LISTEN` | `127.0.0.1:5300` | dnscrypt-proxy local listener |
| `DOH_MAX_BODY` | `65535` | Max DoH POST body, bytes |
| `DOH_MAX_INFLIGHT` | `32` | Concurrent DNS exchanges in the gateway |
| `DOH_MAX_CONNS` | `128` | Simultaneously open TCP connections |
| `DNSCRYPT_GOMEMLIMIT` | `256MiB` | Go soft memory target for dnscrypt-proxy |
| `DOH_GOMEMLIMIT` | `32MiB` | Go soft memory target for the gateway |
| `PUBLIC_DOH_URL` | empty | Optional log-only URL |
| `PORT` | `8080` image default | Use the value supplied by SnapDeploy |

Do not replace SnapDeploy's `PORT` with a privileged port. Both application processes are non-root.

### Health check

```text
GET /healthz
```

Expected response: `ok`.

The startup script does not launch the public gateway until dnscrypt-proxy has successfully bound the local DNS listener. If either long-running process later exits unexpectedly, the container exits so the platform can restart it.

## Resource tuning

The defaults are intentionally conservative for 0.25 vCPU / 512 MB: `max_clients=32`, `DOH_MAX_INFLIGHT=32`, `DOH_MAX_CONNS=128`, a 1024-entry DNS cache, certificate refresh concurrency of 2, and split Go memory targets of 256 MiB + 32 MiB.

The split `GOMEMLIMIT` values matter because `GOMEMLIMIT` is applied independently by each Go runtime; one shared value would not be a container-wide cap. The selected limits leave substantial headroom for goroutine stacks, runtime metadata, the Alpine process environment, and kernel memory while avoiding an unnecessarily aggressive heap ceiling.

HTTP idle connections are kept for 15 seconds in the gateway to preserve ordinary reuse without holding hundreds of idle sockets for long periods. Request headers are capped at 8 KiB per connection — far more than a DoH GET/POST ever needs — so a burst of slow or oversized-header clients cannot inflate memory use. If a DoH client disconnects mid-request, the gateway cancels its in-flight upstream query immediately instead of holding the socket and goroutine open for the full timeout, which keeps the small connection budget available to other clients under load.

## DoH compatibility

The gateway accepts standard RFC 8484-style DNS-over-HTTPS GET (`?dns=`) and POST (`application/dns-message`) requests and handles CORS preflight. UDP is used for the local hop to dnscrypt-proxy, with automatic TCP retry on truncated UDP responses. Oversized and empty DNS messages are rejected before they reach the resolver. Responses are sent with an explicit `Content-Length` rather than chunked transfer encoding, since every DNS response fits in a single write.

The gateway intentionally does not require a specific POST `Content-Type` header, which keeps compatibility with clients that omit it while still accepting the standard `application/dns-message` form.

## Operational note: this is an open relay

Anyone who knows the deployed URL can send it DNS queries; there is no per-client authentication or fairness. `DOH_MAX_INFLIGHT` and `DOH_MAX_CONNS` bound the instance's total resource use, but they do not stop one client from using most of that budget. This is an acceptable tradeoff for a small personal or low-traffic deployment; it is not intended as a hardened public resolver for untrusted, high-volume use.

## Local Docker test

```bash
docker build -t dnscrypt-proxy2-snapdeploy-doh .
docker run --rm -p 8080:8080 dnscrypt-proxy2-snapdeploy-doh
```

Then verify:

```text
http://127.0.0.1:8080/healthz
```

Opening `/dns-query` in a browser is not a valid DNS test because a DNS message is required. Use a DoH-capable client or send an RFC 8484 GET/POST request carrying a DNS wire message.

## Keeping a free-tier deployment awake

Two scheduled GitHub Actions workflows exist purely to work around platform-level idle behavior; neither affects the image or the running service itself.

- **`.github/workflows/snapdeploy-keepalive.yml`** sends an HTTP request to a `KEEPALIVE_URL` repository secret every 13 minutes, to stop SnapDeploy's free tier from spinning the container down after a period of no traffic. **Set this secret** (Settings → Secrets and variables → Actions) to the deployed service's URL, e.g. `https://your-service.containers.snapdeploy.app/healthz`, or the workflow will fail with "KEEPALIVE_URL secret is empty or not configured."
- **`.github/workflows/Keep-Alive.yml`** commits a timestamp file every few hours so the repository always shows recent activity. GitHub automatically disables scheduled workflows in public repositories after 60 days without a commit; without this, the keepalive workflow above would eventually stop running on its own.

If the service isn't on SnapDeploy's free tier, or doesn't need to stay warm, both workflows can simply be deleted.

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
├── .github/
│   └── workflows/
│       ├── Keep-Alive.yml
│       └── snapdeploy-keepalive.yml
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

## Updating

Update `DNSCRYPT_VERSION` only to a tagged dnscrypt-proxy release. Review the release notes and re-run the local checks before deployment. The HaGeZi stamps should also be revalidated against the upstream DNS-stamps table when changed.
