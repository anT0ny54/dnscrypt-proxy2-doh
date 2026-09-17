# Changelog

All notable changes to this SnapDeploy Docker deployment are documented here.

## [1.6.0] - 2026-09-18

### Changed

- Reworked the container startup path so the final image runs entirely as the unprivileged `dnscrypt` user.
- Removed the runtime `DNS_LISTEN` config rewrite, `su-exec`, root privilege requirement, and resolver log file. The internal listener is now fixed at `127.0.0.1:5300`, while container stdout/stderr is the single log stream.
- Removed the unused `DNS_LISTEN` image environment variable and documented the internal topology as fixed.
- Restored the DNS cache minimum TTL to `60` seconds, matching dnscrypt-proxy's current default instead of forcing short-lived records to remain cached for five minutes.
- Added bounded environment parsing for gateway request concurrency and connection limits so deployment variables cannot accidentally exceed values suitable for a small instance.
- Added a safe fallback for `DOH_PATH=/` or `/healthz`, preventing an environment override from colliding with built-in endpoints.
- Added gateway unit tests for base64 decoding, listener shutdown under a full connection cap, and UDP-to-TCP DNS fallback.

### Fixed

- Fixed graceful shutdown of the connection-limited listener: a full connection semaphore could previously leave `Accept()` blocked during shutdown.
- Reworked request cancellation to use `context.AfterFunc`, avoiding an extra waiting goroutine per in-flight DNS exchange.
- Put UDP and TCP fallback inside one five-second gateway exchange budget instead of allowing the fallback path to create a second full timeout window.
- A truncated UDP response now requires a successful TCP retry; if that retry fails, the gateway reports an upstream error rather than returning the truncated response.
- Updated documentation to match the actual file tree and runtime behavior.

### Verified

- The three HaGeZi DoH stamps remain aligned with the upstream documented `root.hagezi.org`, `wurzn.hagezi.org`, and `juuri.hagezi.org` endpoints.
- `.github/` was not modified.

## [1.5.0] - 2026-09-17

### Changed

- `doh-gateway` now cancels its in-flight upstream DNS exchange as soon as the DoH client disconnects, instead of holding the local socket and goroutine open for the full 5-second timeout. This frees the connection/in-flight budget faster under load on a 0.25 vCPU instance.
- `doh-gateway`'s HTTP server now caps request headers at 8 KiB (`net/http`'s default is 1 MiB), bounding per-connection memory use against slow or oversized-header clients.
- DoH responses now carry an explicit `Content-Length` instead of relying on the standard library to infer one, avoiding chunked transfer encoding for what is always a single, small write.
- `doh-gateway/go.mod` now declares `go 1.27`, matching the pinned `golang:1.27-alpine` builder instead of trailing it.
- The Docker build now mounts Go's module and build caches (`--mount=type=cache`) so BuildKit-backed builds don't recompile the full dependency graph on every rebuild; this has no effect on the built image.

### Fixed

- Documented the two GitHub Actions keepalive workflows and the `KEEPALIVE_URL` repository secret they require. `snapdeploy-keepalive.yml` previously had no setup instructions anywhere in the project and fails outright without that secret.
- `README.md`'s project file tree was missing `.github/workflows/`.

### Verified, no change needed

- dnscrypt-proxy 2.1.18, Go 1.27, and Alpine 3.24.1 were verified against upstream sources at the time of that release; the three HaGeZi DoH stamps matched their documented hostnames and IP addresses.

## [1.4.0] - 2026-09-17

### Changed

- Reworked defaults specifically for **512 MB RAM / 0.25 vCPU**: `max_clients` and gateway in-flight work are now 32, gateway connection cap is 128, and split Go memory targets are 256 MiB for dnscrypt-proxy and 32 MiB for the gateway.
- Removed the unnecessary plaintext bootstrap resolver list. The three pinned HaGeZi DoH stamps already contain resolver IPs, while `ignore_system_dns = true` prevents fallback to the host resolver.
- Removed inactive dnscrypt-proxy sections for monitoring, captive portals, anonymized DNS, DNS64, client certificate authentication, empty remote sources, and unrelated broken-resolver workarounds.
- Removed the stale hard-coded SnapDeploy hostname from image defaults; `PUBLIC_DOH_URL` is now optional and log-only.
- Updated the builder to Go 1.27 and the runtime base to Alpine 3.24.1. dnscrypt-proxy remains pinned to tagged release 2.1.18.
- Reduced gateway HTTP idle timeout from 30s to 15s to lower idle-connection overhead on the small instance.

### Fixed

- Gateway CORS headers are now consistent for DoH success responses, errors, and OPTIONS preflight requests.
- Gateway defaults and documentation now agree on the same connection, concurrency, and memory limits.

## [1.3.0] - 2026-09-17

### Fixed

- Corrected a documentation/reality gap where `.dockerignore`, `.env.example`, and `.gitignore` were described as present before all of them actually existed.
- Split the shared `GOMEMLIMIT` between the two Go processes. `GOMEMLIMIT` is per Go runtime, so one shared value could let both runtimes grow toward the same large target independently.
- Fixed misleading DoH error codes for oversized and empty GET queries and for POST bodies that become oversized while being read.

### Added

- `doh-gateway` caps its own concurrency (`DOH_MAX_INFLIGHT`) and open connections (`DOH_MAX_CONNS`) without extra dependencies.
- Graceful shutdown on `SIGTERM`/`SIGINT`.

### Changed

- Removed the inactive `netprobe_address` setting when `netprobe_timeout = 0`.
- Updated `README.md` to document the split memory limits.

## [1.2.0] - 2026-09-17

### Fixed

- Removed the `SERVER_NAMES` runtime override; the resolver names and stamps are the single source of truth.
- The DoH gateway now runs as the unprivileged `dnscrypt` user.
- Moved the unused monitoring UI off the public gateway's port.
- Replaced the unrelated `.gitignore` template with one suitable for this Go/Docker project.
- Added `.dockerignore` and `.env.example` documentation support.

### Changed

- Updated the builder/runtime images and added `GOMAXPROCS=1` plus split memory limits.
- Trimmed the dnscrypt-proxy configuration to only the options used by this deployment.
- Rewrote `README.md` around deployment architecture, resource tuning, local testing, and security behavior.

## [1.1.0] - 2026-09-16

### Changed

- Kept dnscrypt-proxy pinned to 2.1.18 for reproducible Docker builds.
- Standardized the resolver set to the three supplied static HaGeZi stamps.
- Disabled the remote public-resolver source and removed runtime resolver selection.
- Tuned connection counts, cache size, refresh concurrency, and transport behavior for approximately 512 MB RAM / 0.25 vCPU.
- Removed the runtime dependency on Python for startup configuration injection.
- Added clean signal handling and process supervision.
- Simplified the final Docker image.
- Added OPTIONS handling and CORS headers for browser-based DoH diagnostics.
- Added a startup grace period before exposing the DoH gateway.

### Documentation

- Rewrote `README.md` with deployment architecture, SnapDeploy settings, local Docker tests, resource tuning, security notes, and update guidance.

## [1.0.0] - 2026-09-16

### Added

- Initial dnscrypt-proxy 2 Docker deployment for SnapDeploy.
- Local DoH gateway at `/dns-query`.
- Health endpoint at `/healthz`.
- Environment-driven resolver selection via `SERVER_NAMES` (later removed in 1.2.0).
