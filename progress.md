# Progress

## 2026-03-08
- Audited architecture and mapped migration impact points.
- Added host-native SQLite persistence backend:
  - `internal/store/sqlite_store.go`
  - Data paths: `data/http/sites.db`, `data/tcp/streams.db`.
- Switched app bootstrap to SQLite by default.
- Switched default runtime layout to project-local directories rooted at `-config-dir`.
- Added startup runtime checks/logging for service + nginx + certbot status/version.
- Updated certbot execution to use project-local certbot config/work/log dirs.
- Added management versions endpoint: `GET /v1/management/version`.
- Adjusted health degradation behavior so Docker is optional (disabled by default).
- Added JSON-to-SQLite migration command and script:
  - `cmd/migrate-json-to-sqlite`
  - `scripts/migrate_json_to_sqlite.sh`
- Replaced Docker deploy workflow with tag-based release workflow:
  - `.github/workflows/release.yml`
- Removed Docker deployment/runtime files:
  - `Dockerfile`, `docker-compose.yml`, `deploy.sh`, `start.sh`
- Updated README for host-native runtime and release packaging.
