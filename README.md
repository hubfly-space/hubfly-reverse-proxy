# hubfly-reverse-proxy

Host-native reverse proxy manager for NGINX + Certbot with a Go API.

## What Changed

- Runs directly on host (no Docker required).
- Uses host-installed `nginx` and `certbot`.
- Persists state in SQLite instead of JSON:
  - `data/http/sites.db`
  - `data/tcp/streams.db`
- Runtime logs now go to `logs/runtime/` with per-boot files and 7-day retention.
- New management endpoint:
  - `GET /v1/management/version`
- Release pipeline now creates a tagged GitHub Release zip containing binaries and required runtime files.

## Runtime Layout (default)

All files are rooted in the working directory (or `-config-dir`).

- `nginx/nginx.conf`
- `sites/`
- `streams/`
- `staging/`
- `templates/`
- `www/`
- `logs/nginx/`
- `logs/runtime/`
- `certbot/config/`
- `certbot/work/`
- `certbot/logs/`
- `data/http/sites.db`
- `data/tcp/streams.db`

## Build

```bash
go build -ldflags "-X main.appVersion=v1.0.0 -X main.gitCommit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o hubfly ./cmd/hubfly
```

## Run

```bash
./hubfly -config-dir . -port 81
```

Optional Docker-sync mode (disabled by default):

```bash
./hubfly -enable-docker-sync -docker-sock /var/run/docker.sock
```

## Startup Stability Checks

On boot, Hubfly logs runtime checks for:

- service version/build metadata
- nginx availability/version/running state
- certbot availability/version

Hubfly continues to use existing reload/ensure-running behavior for NGINX.

## API

Existing endpoints are preserved.

Health:

```bash
curl http://localhost:81/v1/health
```

Management versions:

```bash
curl http://localhost:81/v1/management/version
```

## JSON -> SQLite Migration

Legacy files supported as input:

- `sites.json` (or `metadata.json`)
- `streams.json`

Run migration helper:

```bash
./scripts/migrate_json_to_sqlite.sh <legacy_json_dir> <hubfly_runtime_dir>
```

Or directly:

```bash
go run ./cmd/migrate-json-to-sqlite -input-dir <legacy_json_dir> -output-dir <hubfly_runtime_dir>
```

## Release Workflow

Tag push (for example `v1.2.0`) triggers `.github/workflows/release.yml` which:

- builds `hubfly` and `migrate-json-to-sqlite` binaries (linux/amd64)
- packages required runtime files (`nginx/`, `templates/`, `static/`, migration script)
- publishes `hubfly-<tag>-linux-amd64.zip` to GitHub Releases
