# Changelog

All notable changes to this SnapDeploy Docker deployment are documented here.

## [1.9.0] - 2026-09-18

### Audit (no functional or security issues found)

This pass re-verified every pinned version and the resolver configuration against live upstream sources rather than assuming the previous entry was still accurate:

- **dnscrypt-proxy 2.1.18** confirmed still the latest tagged release (`2.1.18`, 18 Jul 2026); master has 23 commits ahead but no newer formal release tag exists.
- **Go 1.27.1** confirmed still current: `go1.27.0` (19 Aug 2026) with minor revision `go1.27.1` (1 Sep 2026) is the latest per the official Go release history.
- **Alpine 3.24.2** confirmed current: released 17 Sep 2026, one day before this audit, per endoflife.date's Alpine Linux tracking.
- All three HaGeZi Full Protection DoH stamps (`root`/`wurzn`/`juuri`.hagezi.org) were diffed byte-for-byte against the stamps currently published in the upstream `hagezi/dns-servers` README and matched exactly, including the underlying IPv4 addresses.

Given all of that checked out, `config/dnscrypt-proxy.toml`, the `Dockerfile` version pins, and the resource-limit defaults were left unchanged rather than churned for the sake of a diff.

### Changed

- `doh-gateway/main_test.go`: replaced the hand-rolled `itoa` helper with the standard library's `strconv.Itoa`, which the package already imports elsewhere. Removes duplicated logic with no behavior change.
- `start.sh`: tightened the service-supervision poll from `sleep 2` to `sleep 1`, halving worst-case crash-detection latency. The loop body is a `kill -0` syscall and two comparisons, so the added wake-frequency has no meaningful CPU cost on the 0.25 vCPU budget.

### Documentation

- `README.md`: added a short arithmetic note under resource tuning explaining that the combined 216 MiB `GOMEMLIMIT` target intentionally leaves headroom within the 512 MiB budget for non-heap Go runtime overhead and traffic bursts, rather than that headroom being unexplained slack.

### Verification

- Could not run `go build`/`go vet`/`go test` or a Docker build in this environment: no Go toolchain is installed and outbound network access is unavailable (required both to install Go and to `git clone` the pinned dnscrypt-proxy release during the Docker build). The test-file edit was reviewed by hand instead; it is a pure function-call substitution.
- `.github/` was not modified.

## [1.8.0] - 2026-09-18

### Changed

- Upgraded the Docker builder to **Go 1.27.1** and the runtime image to **Alpine 3.24.2**.
- Kept dnscrypt-proxy pinned to the latest formal tagged release, **2.1.18**. Upstream's master changelog contains 2.1.19 work, but the release index does not list it as a formal release, so production builds do not consume unreleased code.
- Reduced the split Go memory targets from `256MiB + 32MiB` to `192MiB + 24MiB`, leaving more headroom inside a 512 MiB container while retaining bounded heaps for both processes.
- Reduced the default DoH request-message limit from `65535` to `8192` bytes. The gateway still accepts overrides up to the DNS wire-format ceiling of 65535 bytes.
- Restored a 30-second upstream HTTP keepalive to favor connection reuse and reduce repeated TLS setup on the 0.25 vCPU budget.
- Added a `/readyz` endpoint and switched the Docker health check from gateway-only liveness to backend-aware readiness.
- Removed the unused runtime cache-directory creation. This configuration has no file-backed resolver sources or query logs; the DNS cache is in-memory.
- Removed the hardcoded default value for `PUBLIC_DOH_URL`; it is now strictly optional and log-only.
- Added `.env.example` containing the tuned deployment defaults.

### Fixed / hardened

- Added lightweight DNS request validation so the gateway rejects short messages and DNS response packets submitted as queries before using the upstream connection budget.
- Normalized gateway limit validation once during handler creation instead of mutating captured configuration during request handling.
- Reworked startup waiting to use a POSIX shell counter loop rather than the external `seq` utility.
- Kept the internal resolver on `127.0.0.1:5300` and the final image fully unprivileged.

### Documentation

- Rewrote `README.md` for the current 512 MiB / 0.25 vCPU architecture, resource limits, health behavior, DoH compatibility, and open-endpoint threat model.
- Updated the checked-in resolver configuration comments for clarity and removed stale references to unused features.

### Verification

- HaGeZi's current Full Protection table still lists `root.hagezi.org`, `wurzn.hagezi.org`, and `juuri.hagezi.org` with the same IPv4 addresses and DoH stamps used here.
- `.github/` was not modified.
- Local Go unit-test execution could not be completed in this environment because the installed Go 1.23.2 tool attempted to download the required Go 1.27 toolchain and outbound DNS/network access was unavailable. The source was formatted with `gofmt`; the final Docker build also requires network access to clone the pinned upstream dnscrypt-proxy release.

## [1.7.0] - 2026-09-18

### Fixed

- `start.sh` no longer falls back to a hardcoded, unrelated public DoH URL when `PUBLIC_DOH_URL` is left unset; an unset value now stays unset.
- `doh-gateway`: `DOH_MAX_BODY` now bounds GET-encoded DNS queries (`?dns=...`) the same way it already bounded POST bodies. Previously a GET query was only checked against the hard 65535-byte ceiling, so lowering `DOH_MAX_BODY` had no effect on GET requests.
- `doh-gateway`: separated the DNS exchange budget (5s, covering one UDP attempt plus a possible TCP retry) from the HTTP server's write timeout (now 7s, previously also 5s). `net/http`'s `WriteTimeout` deadline is set once, when request headers are read, and covers the entire handler plus the response write — it is not reset afterward. At equal values, a request that legitimately used the full exchange budget could have its already-successful response cut off before it could be written back to the client. The graceful-shutdown grace period was aligned to the new write timeout for the same reason, so an in-flight request within its legitimate budget isn't killed early during a redeploy.

### Added

- Gateway unit tests covering the GET-path `DOH_MAX_BODY` fix and asserting the write-timeout/exchange-timeout headroom invariant.

### Documentation

- README: corrected `DOH_MAX_BODY`'s description to cover GET as well as POST; added a short explanation of the exchange-timeout/write-timeout headroom in the resource-tuning section; minor grammar and formatting cleanup.

### Verified

- dnscrypt-proxy 2.1.18 (tagged 2026-07-18) remains the latest tagged upstream release; no newer tag has shipped.
- Go 1.27 (released 2026-08-19) and Alpine 3.24.1 remain current, compatible choices for the builder and runtime base images.
- All three HaGeZi full-protection DoH stamps (`root`/`wurzn`/`juuri`.hagezi.org) match the current values published by the upstream `hagezi/dns-servers` repository.
- `/opt/dnscrypt-proxy/cache` is provisioned but unused by this configuration: there is no `[sources]` block, no query/nx logging, and no cloaking or forwarding files configured, and the DNS response cache (`cache = true`) is in-memory only. Left in place rather than removed — at this resource tier an unused empty directory costs nothing, while removing it on the chance it's needed for something undocumented (e.g. a future dnscrypt-proxy feature) is a risk with no corresponding benefit.

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
