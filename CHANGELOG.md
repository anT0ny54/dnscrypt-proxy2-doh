# Changelog
- Fixed the Docker build to checkout the upstream `2.1.18` tag without adding a non-existent `v` prefix.

All notable changes to this SnapDeploy Docker deployment are documented here.

## [1.1.0] - 2026-09-16

### Changed

- Kept `dnscrypt-proxy` pinned to **2.1.18** for reproducible Docker builds.
- Standardized the resolver set to the three supplied static HaGeZi stamps: `HaGeZiDNS1`, `HaGeZiDNS2`, and `HaGeZiDNS3`.
- Disabled the remote public-resolver source entirely; the deployment is now self-contained and cannot dynamically select a different upstream.
- Removed `SERVER_NAMES` as a runtime override so the container can use only the three embedded HaGeZi resolvers.
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
- Environment-driven resolver selection.
