# Changelog

All notable changes to this SnapDeploy Docker deployment are documented here, newest first. Dates are `YYYY-MM-DD`.

**How to read this file**

- Headings follow [Keep a Changelog](https://keepachangelog.com/): **Added**, **Changed**, **Fixed**, **Removed**. Two extra headings are used: **Documentation** (docs-only changes) and **Verified** (what was checked, and what could *not* be checked, in that release; these are not code changes).
- Entries are history and are not rewritten. Where a later review found an earlier statement to be inaccurate, the entry carries an inline note marked *Corrected in 1.11.0*.
- `.github/` was not modified in 1.6.0, 1.8.0, 1.9.0, 1.10.0 or 1.11.0.

**Baseline as of 1.11.0**

| Component | Version / value |
|---|---|
| dnscrypt-proxy | `2.1.18` (latest formal tag; unreleased 2.1.19 work on `master` is deliberately not used) |
| Go builder | `1.27.1` |
| Alpine runtime | `3.24.2` |
| Upstream resolvers | three static HaGeZi DoH stamps (`root`, `wurzn`, `juuri`.hagezi.org) |
| Target instance | 512 MB RAM / 0.25 vCPU |

## [1.11.0] - 2026-09-19

Full-project review of the runtime path (`Dockerfile` → `start.sh` → `doh-gateway` → `dnscrypt-proxy.toml`), the docs, and the workflow. One shutdown bug was found and fixed, along with several smaller gateway and image improvements and some documentation contradictions. This supersedes the 1.10.0 conclusion that no issues existed.

### Fixed

- `start.sh`: a normal shutdown was reported as a crash. The `cleanup` trap never called `exit`, so after `SIGTERM`/`SIGINT`/`SIGHUP` the supervision loop resumed, found the children already stopped, logged `ERROR: … exited unexpectedly.`, ran cleanup a second time, and exited with status `1`. The trap was also delayed by up to 1 s because it could not run until the foreground `sleep 1` returned. Signals now exit `0` through a single `EXIT`-trap cleanup, and the loop pauses with `sleep 1 & wait $!` so signals are handled immediately.
- `start.sh`: worst-case shutdown could exceed the common 10 s container stop timeout (1.10.0 allowed up to ~10 s *per child*, sequentially). The gateway is now stopped first, so in-flight DoH requests can finish while dnscrypt-proxy is still answering. It gets 7.5 s, matching its own 7 s graceful-shutdown window, and dnscrypt-proxy then gets 2 s. Worst case is about 9.5 s, then `SIGKILL`.
- `doh-gateway`: when the UDP attempt had already used the whole 5 s budget (timeout, or client gone), the gateway still tried a TCP retry that could not succeed and reported that TCP dial error instead of the real UDP error. It now returns the UDP error directly. Fast UDP failures such as *connection refused* still fall back to TCP.
- `doh-gateway`: a client disconnecting mid-request was logged as `DNS exchange failed` and answered with a 502 nobody would read. This case is now silent.

### Changed

- `doh-gateway`: the 64 KiB UDP receive buffer is now pooled (`sync.Pool`) instead of allocated on every request, which reduces GC churn under the 24 MiB heap target. The response is copied out of the pooled buffer, so returned data never aliases it. The buffer size deliberately stays 64 KiB: a smaller one would silently truncate large UDP responses without setting the TC bit.
- `Dockerfile`: file modes are set at `COPY` time (`--chmod=0755`) instead of by a trailing `RUN chmod`, which had stored a second full copy of both binaries and `start.sh` in an extra image layer.
- `Dockerfile`: the gateway is built with `-buildvcs=false`. `/src` is a git checkout of dnscrypt-proxy, so without the flag Go stamped the gateway binary with dnscrypt-proxy's commit.
- `config/dnscrypt-proxy.toml`: comment only. It now says `force_tcp` affects only the DNSCrypt protocol (unused here). No setting changed.

### Added

- `doh-gateway/main_test.go`: tests for pooled-buffer aliasing, for no TCP retry after a UDP timeout, and for `/readyz` returning `503` when the upstream is unreachable.

### Removed

- `doh-gateway/main_test.go`: an unused-variable workaround (`_ = n`) in the truncated-UDP test.

### Documentation

- `README.md`: removed the statement that `.github/` was excluded from the release. The archive contains `.github/workflows/Keep-Alive.yml`, which is now in the file tree and described (it keeps the fork active; it does not keep the deployed instance awake).
- `README.md`: clarified that `DOH_UPSTREAM_ADDR` only redirects the gateway and does not move the fixed dnscrypt-proxy listener; noted that `GET` queries are also bounded by the 8 KiB header limit; added a *Shutdown and supervision* section.
- `CHANGELOG.md`: restructured. Headings are uniform, misfiled bullets are moved to the right category, long entries are condensed, corrections are annotated inline, and a baseline table is added.

### Verified

- dnscrypt-proxy `2.1.18` (2026-07-18) is still the latest tag (`master` is 15 commits ahead; no newer release). Go `1.27.1` was tagged 2026-09-01. Alpine `3.24` (released 2026-06-09) is confirmed, but the `3.24.2` patch tag could not be independently confirmed in this review; a non-existent tag would fail the build at `FROM`, not silently.
- All three HaGeZi stamps were decoded: DoH (`0x02`), properties `0x03`, no certificate-hash pins (TLS is validated against the system CA store, and `ca-certificates` is installed). Hostnames, IPv4 addresses and path match the README table.
- `start.sh` was tested with stub binaries under `dash`. `SIGTERM`, `SIGINT` and `SIGHUP` each exit `0` in about 0.1–0.2 s with no `ERROR` line and a single cleanup (the previous script exited `1` after ~1 s with a false error). A crashed child exits `1` with the correct message and the sibling is stopped. A child that ignores `SIGTERM` is `SIGKILL`ed at ~7.5 s, with no leftover processes. BusyBox `ash` (Alpine's `/bin/sh`) was not available to test; only POSIX constructs are used.
- **Not verified:** the Go changes were not compiled or run (no Go toolchain or network in the review environment). They were reviewed by hand and braces were checked for balance. Run `go vet ./... && go test ./...` in `doh-gateway/` before deploying.

### Reviewed, intentionally not changed

- `setConnDeadline`'s no-deadline fallback branch is currently unreachable (`dnsExchange` always supplies a deadline). It is harmless defensive code.
- Gateway limits are clamped in `main()` (`envInt`) and again in `dohHandler`. This is redundant but defensive; 1.8.0's "validated once" wording is not literally true.
- `ReadHeaderTimeout` equals `ReadTimeout`, and `http.Server.Addr` is unused because the server uses `Serve(ln)`. Both are harmless.
- Defaults for `PORT`, `DOH_PATH`, `DOH_BIND`, `DOH_UPSTREAM_ADDR` and the memory limits appear in the `Dockerfile`, `start.sh` and `main.go`; keep them in sync when changing one.
- Optional hardening, not applied: pin dnscrypt-proxy to a commit SHA (tags are mutable), check the DNS response ID on the UDP path, and add a CI workflow that runs `go vet` and `go test` (there is currently no CI besides the keep-alive job).
- Left for the owner to decide: the final sections of `README.md` (third-party DoH endpoint list, "Bandwidth Hero Server", donation address) are unrelated to this project, and `.dockerignore`/`.gitignore` still reference an `.env.example` that is not shipped.

## [1.10.0] - 2026-09-19

> Superseded in part: 1.11.0 found a shutdown bug and documentation inconsistencies that this review reported as "no issues".

### Changed

- `doh-gateway`: removed the redundant second `validateDNSQuery` call inside `dnsExchange`; the HTTP handler already validates every query. No behavior change.
- `doh-gateway`: `ReadHeaderTimeout` uses the existing `httpReadTimeout` constant instead of a duplicated literal. No behavior change.
- `doh-gateway`: health endpoint bodies (`/healthz`, `/readyz`, `/`) are pre-computed package-level `[]byte` values.
- `start.sh`: the `cleanup` trap uses a bounded reap loop (up to ~10 s per child) followed by `kill -9`, instead of an unbounded `wait`. *The trap still did not exit the script; fixed in 1.11.0.*

### Removed

- `README.md`: stale references to `.env.example`, which is not part of the archive.

### Verified

- HaGeZi stamps re-decoded: protocol 2 (DoH), properties `0x03` (DNSSEC + no-log); hostnames and IPv4 addresses match the README.
- `go build` / `go test` could not be run (no Go toolchain, no network). The Go edits were mechanical.

## [1.9.0] - 2026-09-18

### Changed

- `doh-gateway/main_test.go`: replaced the hand-rolled `itoa` helper with `strconv.Itoa`.
- `start.sh`: supervision poll tightened from `sleep 2` to `sleep 1`, halving worst-case crash-detection latency at negligible CPU cost.

### Documentation

- `README.md`: added a note under resource tuning explaining that the combined 216 MiB `GOMEMLIMIT` target deliberately leaves headroom inside the 512 MiB budget for non-heap Go runtime overhead and traffic bursts.

### Verified

- Checked against live upstream sources: dnscrypt-proxy `2.1.18` is still the latest tag (`master` was 23 commits ahead); Go `1.27.1` is current (`1.27.0` on 2026-08-19, `1.27.1` on 2026-09-01); Alpine `3.24.2` was reported as released 2026-09-17 (endoflife.date).
- All three HaGeZi stamps matched the upstream `hagezi/dns-servers` README byte-for-byte, including IPv4 addresses.
- Because everything matched, the config, `Dockerfile` pins and resource limits were left unchanged.
- `go build`/`go vet`/`go test` and a Docker build could not be run (no Go toolchain, no network).

## [1.8.0] - 2026-09-18

### Added

- `/readyz` endpoint; the Docker health check now uses it, so it reflects backend readiness instead of gateway-only liveness.
- Lightweight DNS request validation: the gateway rejects short messages and DNS *response* packets submitted as queries before they use the upstream budget.
- `.env.example` with the tuned deployment defaults. *Not present in the current archive; see 1.10.0.*

### Changed

- Builder upgraded to Go `1.27.1`, runtime to Alpine `3.24.2`. dnscrypt-proxy stays on the latest formal tag, `2.1.18`; upstream `master` contains unreleased 2.1.19 work that production builds do not consume.
- Split Go memory targets reduced from `256MiB + 32MiB` to `192MiB + 24MiB` for more headroom in a 512 MiB container.
- Default DoH request-message limit reduced from `65535` to `8192` bytes; overrides up to 65535 are still accepted.
- Upstream HTTP keepalive restored to 30 s to favor connection reuse on the 0.25 vCPU budget.
- `PUBLIC_DOH_URL` has no default any more; it is optional and log-only.
- Gateway limit validation is normalized once at handler creation instead of mutating captured configuration during request handling. *`main()` also clamps the values, so validation happens twice; see 1.11.0.*
- Startup waiting was reworked to use a POSIX counter loop instead of the external `seq`. *That wait loop is no longer in `start.sh`: the gateway starts immediately and `/readyz` reports readiness. The removal was not recorded in earlier entries.*

### Removed

- Unused runtime cache-directory creation. The configuration has no file-backed resolver sources or query logs, and the DNS cache is in memory.

### Documentation

- `README.md` rewritten for the 512 MiB / 0.25 vCPU architecture, resource limits, health behavior, DoH compatibility and open-endpoint threat model.
- Configuration comments updated; stale references to unused features removed.

### Verified

- HaGeZi's Full Protection table still lists the same three hostnames, IPv4 addresses and stamps.
- Source was formatted with `gofmt`. Go unit tests could not be run: the installed Go 1.23.2 tried to download the Go 1.27 toolchain and there was no network. The Docker build also needs network to clone the pinned dnscrypt-proxy release.

## [1.7.0] - 2026-09-18

### Fixed

- `start.sh`: no longer falls back to a hard-coded, unrelated public DoH URL when `PUBLIC_DOH_URL` is unset.
- `doh-gateway`: `DOH_MAX_BODY` now bounds GET-encoded queries (`?dns=…`) the same way it bounded POST bodies. Previously GET was only checked against the 65535-byte ceiling.
- `doh-gateway`: the DNS exchange budget (5 s: one UDP attempt plus a possible TCP retry) is now separate from the HTTP write timeout (raised from 5 s to 7 s). `net/http` sets the write deadline once, when headers are read, and it covers the whole handler plus the response write. With equal values, a request that legitimately used the full exchange budget could have its successful response cut off. The graceful-shutdown grace period was aligned to 7 s for the same reason.

### Added

- Gateway tests for the GET `DOH_MAX_BODY` fix and for the write-timeout/exchange-timeout headroom invariant.

### Documentation

- `README.md`: `DOH_MAX_BODY` description now covers GET as well as POST; added the timeout-headroom explanation; minor grammar and formatting cleanup.

### Verified

- dnscrypt-proxy `2.1.18` (tagged 2026-07-18) was the latest tag; Go `1.27` (2026-08-19) and Alpine `3.24.1` were current; the three HaGeZi stamps matched upstream.
- `/opt/dnscrypt-proxy/cache` was provisioned but unused (no `[sources]`, logs, cloaking or forwarding files; the DNS cache is in memory). It was left in place in this release and removed in 1.8.0.

## [1.6.0] - 2026-09-18

### Changed

- The final image now runs entirely as the unprivileged `dnscrypt` user. Removed the runtime `DNS_LISTEN` config rewrite, `su-exec`, the root requirement, the resolver log file and the unused `DNS_LISTEN` environment variable. The internal listener is fixed at `127.0.0.1:5300`, and container stdout/stderr is the single log stream.
- DNS cache minimum TTL set to `60` s (previously five minutes, which forced short-lived records to stay cached). *Corrected in 1.11.0: this entry originally said 60 s "matches dnscrypt-proxy's current default". Upstream's example config uses `cache_min_ttl = 2400`, so 60 s is a deliberate choice, not the upstream default.*
- Environment parsing for gateway concurrency and connection limits is bounded, so deployment variables cannot exceed values suitable for a small instance.
- `DOH_PATH=/` or `/healthz` falls back to a safe default instead of colliding with built-in endpoints.

### Added

- Gateway unit tests for base64 decoding, listener shutdown under a full connection cap, and UDP-to-TCP fallback.

### Fixed

- Graceful shutdown of the connection-limited listener: a full connection semaphore could leave `Accept()` blocked.
- Request cancellation uses `context.AfterFunc`, avoiding an extra waiting goroutine per in-flight exchange.
- UDP and TCP fallback now share one five-second exchange budget instead of allowing a second full timeout window.
- A truncated UDP response now requires a successful TCP retry; if the retry fails, the gateway reports an upstream error instead of returning the truncated response.

### Documentation

- Docs updated to match the actual file tree and runtime behavior.

### Verified

- The three HaGeZi DoH stamps still match the documented `root`, `wurzn` and `juuri`.hagezi.org endpoints.

## [1.5.0] - 2026-09-17

### Changed

- `doh-gateway` cancels its in-flight upstream exchange as soon as the DoH client disconnects, instead of holding the socket and goroutine for the full 5 s timeout. This frees the connection/in-flight budget faster under load.
- The gateway HTTP server caps request headers at 8 KiB (`net/http`'s default is 1 MiB), bounding per-connection memory use.
- DoH responses carry an explicit `Content-Length`, avoiding chunked transfer encoding for a single small write.
- `doh-gateway/go.mod` declares `go 1.27`, matching the builder image.
- The Docker build mounts Go module and build caches (`--mount=type=cache`); this only affects build speed, not the image.

### Documentation

- Documented two GitHub Actions keep-alive workflows and a `KEEPALIVE_URL` secret. *Only `Keep-Alive.yml` exists in the current archive and it needs no secret; `snapdeploy-keepalive.yml` and `KEEPALIVE_URL` are gone.*
- `README.md`: the project file tree was missing `.github/workflows/`.

### Verified

- dnscrypt-proxy `2.1.18`, Go `1.27` and Alpine `3.24.1` were current; the three HaGeZi stamps matched their documented hostnames and IPs.

## [1.4.0] - 2026-09-17

### Changed

- Defaults reworked for **512 MB RAM / 0.25 vCPU**: `max_clients` and gateway in-flight work are `32`, the gateway connection cap is `128`, and the split Go memory targets are 256 MiB (dnscrypt-proxy) and 32 MiB (gateway).
- Builder updated to Go 1.27 and runtime base to Alpine 3.24.1; dnscrypt-proxy stays on tagged release `2.1.18`.
- Gateway HTTP idle timeout reduced from 30 s to 15 s to lower idle-connection overhead.
- `PUBLIC_DOH_URL` is now optional and log-only.

### Fixed

- Gateway CORS headers are consistent across DoH success responses, errors and `OPTIONS` preflight.
- Gateway defaults and documentation now agree on the same connection, concurrency and memory limits.

### Removed

- The plaintext bootstrap resolver list. The pinned HaGeZi DoH stamps already contain resolver IPs, and `ignore_system_dns = true` prevents fallback to the host resolver.
- Inactive dnscrypt-proxy sections: monitoring, captive portals, anonymized DNS, DNS64, client certificate authentication, empty remote sources and unrelated broken-resolver workarounds.
- The stale hard-coded SnapDeploy hostname from image defaults.

## [1.3.0] - 2026-09-17

### Added

- `doh-gateway` caps its own concurrency (`DOH_MAX_INFLIGHT`) and open connections (`DOH_MAX_CONNS`) without extra dependencies.
- Graceful shutdown on `SIGTERM`/`SIGINT`.

### Changed

- The shared `GOMEMLIMIT` is split between the two Go processes. It applies per Go runtime, so one shared value let both runtimes grow toward the same large target independently. `README.md` documents the split.
- Removed the inactive `netprobe_address` setting while `netprobe_timeout = 0`.

### Fixed

- Misleading DoH error codes for oversized and empty GET queries, and for POST bodies that become oversized while being read.

### Documentation

- Fixed a docs/reality gap: `.dockerignore`, `.env.example` and `.gitignore` were described as present before all of them existed.

## [1.2.0] - 2026-09-17

### Added

- `.dockerignore` and `.env.example` documentation support.

### Changed

- The DoH gateway runs as the unprivileged `dnscrypt` user.
- Moved the unused monitoring UI off the public gateway's port (the UI was removed entirely in 1.4.0).
- Replaced the unrelated `.gitignore` template with one suited to this Go/Docker project.
- Updated builder/runtime images; added `GOMAXPROCS=1` and split memory limits.
- Trimmed the dnscrypt-proxy configuration to the options this deployment uses.

### Removed

- The `SERVER_NAMES` runtime override; resolver names and stamps are the single source of truth.

### Documentation

- `README.md` rewritten around deployment architecture, resource tuning, local testing and security behavior.

## [1.1.0] - 2026-09-16

### Added

- `OPTIONS` handling and CORS headers for browser-based DoH diagnostics.
- A startup grace period before exposing the DoH gateway. *No longer present; the gateway starts immediately and `/readyz` reports readiness. The removal was not recorded in earlier entries.*
- Clean signal handling and process supervision.

### Changed

- Kept dnscrypt-proxy pinned to `2.1.18` for reproducible Docker builds.
- Standardized the resolver set to the three supplied static HaGeZi stamps.
- Tuned connection counts, cache size, refresh concurrency and transport behavior for roughly 512 MB RAM / 0.25 vCPU.
- Simplified the final Docker image.

### Removed

- The remote public-resolver source and runtime resolver selection.
- The runtime dependency on Python for startup configuration injection.

### Documentation

- `README.md` rewritten with deployment architecture, SnapDeploy settings, local Docker tests, resource tuning, security notes and update guidance.

## [1.0.0] - 2026-09-16

### Added

- Initial dnscrypt-proxy 2 Docker deployment for SnapDeploy.
- Local DoH gateway at `/dns-query`.
- Health endpoint at `/healthz`.
- Environment-driven resolver selection via `SERVER_NAMES` (removed in 1.2.0).
