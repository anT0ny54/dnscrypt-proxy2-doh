All notable changes to this SnapDeploy Docker deployment are documented here.

## [1.3.0] - 2026-09-17

### Fixed

- **Corrects a documentation/reality gap from 1.2.0.** That entry described
  `.dockerignore`, `.env.example`, and a real `.gitignore` as already added,
  but none had actually landed in the shipped files: `.dockerignore` and
  `.env.example` were missing entirely, and `.gitignore` was still the
  unrelated Ada/object-file template. All three are genuinely present now.
- **Split the shared `GOMEMLIMIT` between the two Go processes.**
  `GOMEMLIMIT` is a *per-runtime* soft target, not a container-wide one, so
  the single `GOMEMLIMIT=300MiB` env var let `dnscrypt-proxy` and
  `doh-gateway` each independently grow toward 300MiB — up to ~600MB
  combined, over the 512MB hard limit. Replaced with `DNSCRYPT_GOMEMLIMIT`
  (default `360MiB`) and `DOH_GOMEMLIMIT` (default `48MiB`), applied
  separately in `start.sh` when each binary is launched.
- **Fixed misleading DoH error codes.** An oversized or empty GET query
  (`?dns=`) fell through to `dnsExchange`'s internal size check and surfaced
  as a generic `502 upstream DNS failure`. It's now validated up front and
  returns `400`/`413` like the POST path already did. Also fixed POST so a
  body that exceeds `DOH_MAX_BODY` while being *read* (as opposed to a
  `Content-Length` that's already too large) returns `413` instead of a
  generic `400`.

### Added

- `doh-gateway` now caps its own concurrency (`DOH_MAX_INFLIGHT`, default
  `64`, matching dnscrypt-proxy's `max_clients`) and open connections
  (`DOH_MAX_CONNS`, default `512`), both implemented without new
  dependencies. Past either limit, new requests get a fast `503` instead of
  piling up goroutines on the 0.25 vCPU budget.
- `doh-gateway` now shuts down gracefully on `SIGTERM`/`SIGINT`, letting
  in-flight requests finish instead of cutting them off.

### Changed

- Commented out `netprobe_address` in `config/dnscrypt-proxy.toml`: with
  `netprobe_timeout = 0`, the startup probe is skipped entirely, making the
  address setting dead configuration.
- Updated `README.md`'s runtime-settings table and resource-profile section
  to document the new variables and explain the GOMEMLIMIT split.

## [1.2.0] - 2026-09-17

### Fixed

- Removed the `SERVER_NAMES` runtime override from `start.sh`. It was still
  reading and templating the variable even though 1.1.0 documented the
  resolver set as fixed and self-contained; the config's `server_names` and
  `[static.*]` stamps are the single source of truth now.
- The DoH gateway (`doh-gateway`) now runs as the unprivileged `dnscrypt` user
  via `su-exec`, matching `dnscrypt-proxy`. Previously the internet-facing
  process ran as root while the backend resolver dropped privileges.
- Moved `monitoring_ui.listen_address` off `127.0.0.1:8080`: it collided with
  the DoH gateway's own `0.0.0.0:$PORT` (default 8080), since `0.0.0.0`
  covers the loopback interface too. It's `127.0.0.1:8081` now.
- Replaced the generic, unrelated `.gitignore` (leftover Ada/object-file
  template) with one that actually covers this Go/Docker project.
- Added the `.dockerignore` and `.env.example` files the README already
  referenced but which weren't in the repository.

### Changed

- Builder image bumped from `golang:1.25-alpine` to `golang:1.26-alpine`,
  matching upstream dnscrypt-proxy's current `go.mod` requirement.
- Runtime base image bumped from `alpine:3.22` to `alpine:3.24` (current
  stable).
- Added `GOMAXPROCS=1` and `GOMEMLIMIT=300MiB` to the image defaults. Go does
  not read the container's cgroup CPU quota automatically, so on a 0.25 vCPU
  service the runtime would otherwise size its scheduler/GC for however many
  CPUs the host machine has; `GOMEMLIMIT` gives the GC a soft cap safely under
  the 512MB hard limit instead of relying on the OS to OOM-kill the container.
- Trimmed `config/dnscrypt-proxy.toml` from ~1000 lines to a few hundred by
  cutting upstream's example-file prose down to short comments. Every active
  setting keeps its exact prior value (verified against the previous file),
  aside from the `monitoring_ui` port fix above.
- Rewrote `README.md` for clarity: merged two overlapping sections about
  `PUBLIC_DOH_URL`, updated the local-Docker example to drop the now-inert
  `SERVER_NAMES` variable, and added a note that `PORT` must be unprivileged
  (>1024) now that the gateway runs as non-root.

## [1.1.0] - 2026-09-16

### Changed

- Kept `dnscrypt-proxy` pinned to **2.1.18** for reproducible Docker builds.
- Standardized the resolver set to the three supplied static HaGeZi stamps: `HaGeZiDNS1`, `HaGeZiDNS2`, and `HaGeZiDNS3`.
- Disabled the remote public-resolver source entirely; the deployment is now self-contained and cannot dynamically select a different upstream.
- Tuned the runtime for approximately **512 MB RAM / 0.25 vCPU**:
  - `max_clients = 64`
  - `cert_refresh_concurrency = 2`
  - `keepalive = 10`
  - `cache_size = 1024`
  - `cache_min_ttl = 300`
  - IPv6 public resolvers disabled
  - unused relay source refresh disabled
- Removed the runtime dependency on Python for `start.sh` configuration injection.
- Added clean signal handling and container supervision so an unexpected dnscrypt-proxy or DoH gateway exit terminates the container.
- Simplified the final Docker image by removing unused runtime packages.
- Improved Docker multi-architecture build arguments (`TARGETOS` / `TARGETARCH`).
- Clarified that SnapDeploy owns the public `PORT` value and the container should not replace it.
- Added OPTIONS handling and CORS headers for browser-based DoH diagnostics.
- Added a short resolver startup grace period before exposing the DoH gateway.

### Documentation

- Rewrote `README.md` with:
  - deployment architecture
  - SnapDeploy environment settings
  - local Docker test steps
  - resource profile
  - security/behavior notes
  - update procedure
- Added this changelog.

## [1.0.0] - 2026-09-16

### Added

- Initial dnscrypt-proxy 2 Docker deployment for SnapDeploy.
- Local DoH gateway at `/dns-query`.
- Health endpoint at `/healthz`.
- Environment-driven resolver selection via `SERVER_NAMES` (later removed in 1.2.0; see above).
