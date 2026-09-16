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

SnapDeploy is expected to provide the public HTTPS/TLS termination. The DoH gateway only listens inside the container's service port and forwards DNS messages to the local dnscrypt-proxy listener.

The image is tuned for a small service budget of **512 MB RAM and 0.25 vCPU** by limiting concurrent clients, reducing resolver certificate-refresh concurrency, using a smaller DNS cache, and disabling unused relay metadata refreshes.

## Upstream and version

The image pins **dnscrypt-proxy 2.1.18** for reproducible builds. The upstream project supports DNS-over-HTTPS and DNSCrypt and maintains signed public resolver sources.

The runtime defaults to:

```text
SERVER_NAMES=HaGeZiDNS1,HaGeZiDNS2,HaGeZiDNS3
```

The resolver set is **fully self-contained**: `HaGeZiDNS1`, `HaGeZiDNS2`, and `HaGeZiDNS3` are defined directly as static resolver stamps in `config/dnscrypt-proxy.toml`. No public-resolver source is enabled and no resolver names are downloaded or selected dynamically. The container uses only these three configured HaGeZi upstreams.

## SnapDeploy configuration

Create a Docker service from this repository. Set the environment variables below:

```text
DOH_PATH=/dns-query
DNS_LISTEN=127.0.0.1:5300
DOH_MAX_BODY=65535
```

**Do not override `PORT`.** SnapDeploy should provide the service port through `PORT`; the DoH gateway binds to that value.

After deployment, the public resolver URL is:

```text
https://<your-snapdeploy-domain>/dns-query
```

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
  -e SERVER_NAMES='HaGeZiDNS1,HaGeZiDNS2,HaGeZiDNS3' \
  -e PORT=8080 \
  dnscrypt-proxy2-snapdeploy-doh
```

Then check:

```text
http://127.0.0.1:8080/healthz
```

For a real DoH client, place the container behind HTTPS/TLS and use `/dns-query`.

## Runtime settings

| Variable | Default | Purpose |
|---|---|---|
| `DOH_PATH` | `/dns-query` | Public DoH path |
| `DNS_LISTEN` | `127.0.0.1:5300` | Local DNS listener used by the gateway |
| `DOH_UPSTREAM_ADDR` | `127.0.0.1:5300` | Gateway's local DNS target |
| `DOH_MAX_BODY` | `65535` | Maximum DoH POST body size in bytes |
| `PORT` | `8080` in the image | Managed by SnapDeploy; don't override there |

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
```

These settings keep the service small without disabling the core encrypted-DoH forwarding path. dnscrypt-proxy's own configuration documents `max_clients`, certificate-refresh concurrency, keepalive, cache, and DoH server selection as tunable runtime options.

## Security and behavior notes

- Public HTTPS is expected to be terminated by SnapDeploy.
- The DoH gateway accepts standard DNS-over-HTTPS GET (`?dns=`) and POST (`application/dns-message`) requests.
- DNS requests from the gateway are sent only to the local dnscrypt-proxy listener, not directly to public port 53.
- dnscrypt-proxy uses only the three static HaGeZi resolver stamps included in the configuration; no public-resolver source is enabled.
- Bootstrap resolvers are retained only for hostname/certificate bootstrap where a static stamp requires it. They are not configured as normal upstream resolvers.
- The container exits if either dnscrypt-proxy or the DoH gateway unexpectedly dies, so the platform can restart a broken instance instead of keeping a partially working process alive.

## Files

```text
.
├── Dockerfile
├── .dockerignore
├── .env.example
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

Change `DNSCRYPT_VERSION` in the Dockerfile, review the upstream release notes, rebuild, and test the resolver names before deploying. The pinned release is deliberate so a platform rebuild does not unexpectedly move to a different upstream version. The current upstream release page lists 2.1.18 as the latest tagged release in the checked source.
