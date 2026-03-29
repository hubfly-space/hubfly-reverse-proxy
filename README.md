# hubfly-reverse-proxy

Host-native reverse proxy manager for NGINX + Certbot with a Go API.

## Runtime Model

Hubfly runs directly on the host (no container required) and uses host-installed tools.

Required tools:
- `nginx version: nginx/1.29.6 (important) its have nginx stream module built-in`
- `certbot`
- nginx stream module (`ngx_stream_module`) is required.

Optional:
- Docker endpoint for container-name upstream resolution.
- `-enable-docker-sync` enables background Docker-triggered full-checks in addition to initial container-name resolution.

Install stream module:

Ubuntu/Debian:
```bash
sudo apt update
sudo apt install -y libnginx-mod-stream
```

RHEL/CentOS/Rocky/Alma (package name varies by repo):
```bash
sudo dnf install -y nginx-mod-stream || sudo dnf install -y nginx-all-modules
```

Verify module file exists (one common path example):
```bash
ls -l /usr/lib/nginx/modules/ngx_stream_module.so
```

Default runtime paths are rooted in `-config-dir` (default `.`):
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

Default ports:
- `10003/tcp`: Hubfly API (management API)
- `10004/tcp`: Web UI (nginx management UI + `/v1` proxy)
- `10005/tcp`: GoAccess WebSocket backend (`/goaccess-ws` upstream)
- `80/tcp`: public HTTP
- `443/tcp`: public HTTPS

## Versioning

Build metadata is injected at build time and shown by APIs.

```bash
go build -ldflags "-X main.appVersion=v1.0.0 -X main.gitCommit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o hubfly-reverse-proxy ./cmd/hubfly
```

Print binary version only:

```bash
./hubfly-reverse-proxy version
```

Expected output (string only):

```text
v1.0.0
```

Recreate nginx configs from stored Hubfly state:

```bash
./hubfly-reverse-proxy recreate -config-dir .
```

This command:
- prunes stale generated `sites/*.conf`, `streams/*.conf`, and `staging/*.conf`
- reloads site definitions from the configured store
- rebuilds all site and stream nginx config files
- performs one final nginx reload

Store source selection:
- `-source-store auto`: prefer SQLite if `data/http/sites.db` or `data/tcp/streams.db` exists, otherwise fall back to legacy JSON
- `-source-store sqlite`: rebuild only from SQLite state
- `-source-store json`: rebuild from legacy `sites.json` / `streams.json` / `metadata.json`

Example recreate commands:

```bash
./hubfly-reverse-proxy recreate -config-dir . -source-store auto
./hubfly-reverse-proxy recreate -config-dir . -source-store sqlite
./hubfly-reverse-proxy recreate -config-dir . -source-store json
```

## Run

```bash
./hubfly-reverse-proxy -config-dir . -port 10003
```

With Docker upstream sync enabled:

```bash
./hubfly-reverse-proxy -config-dir . -port 10003 -enable-docker-sync -docker-sock 127.0.0.1:10010
```

When Docker sync mode is enabled, Hubfly:
- runs a smart background full-check every 5 minutes
- triggers the same smart background full-check on Docker `start`, `restart`, and `unpause` container events

Smart full-check means Docker-backed upstreams are re-evaluated, but nginx is only reloaded if Hubfly detects an actual site or stream config change.

Unix socket example:

```bash
./hubfly-reverse-proxy -config-dir . -docker-sock /var/run/docker.sock
```

## Release

Tag push like `v1.2.0` triggers `.github/workflows/release.yml`.
It publishes a GitHub Release zip containing:
- `hubfly-reverse-proxy` (archive root)
- `nginx/`
- `templates/`
- `static/`
- runtime folder skeleton (`data/`, `logs/`, `certbot/`)

## JSON to SQLite Migration

Legacy JSON files supported as source:
- `sites.json` (or legacy `metadata.json`)
- `streams.json`

```bash
./scripts/migrate_json_to_sqlite.sh <legacy_json_dir> <hubfly_runtime_dir>
```

or

```bash
go run ./cmd/migrate-json-to-sqlite -input-dir <legacy_json_dir> -output-dir <hubfly_runtime_dir>
```

After migrating legacy JSON into SQLite, rebuild clean nginx config from the migrated state:

```bash
./hubfly-reverse-proxy recreate -config-dir <hubfly_runtime_dir> -source-store sqlite
```

## API Usage And Testing

Full endpoint reference:
- [API.md](/home/bonheur/Desktop/Projects/hubfly-tools/hubfly-reverse-proxy/API.md)

Base URL (default): `http://localhost:10003`

Web UI URL (default): `http://localhost:10004`

Load-balanced HTTP sites use the same generated proxy behavior as non-balanced sites:
- `proxy_http_version 1.1`
- `Upgrade` / `Connection` websocket headers
- forwarded host/proto/port headers
- custom `proxy_set_header` overrides

That means enabling `load_balancing` changes upstream selection, not the external API shape or the proxied request header model.

### 1. Health

```bash
curl -s http://localhost:10003/v1/health | jq
```

## Recreate Workflow

Use `recreate` when:
- generated nginx files were manually edited and you want Hubfly to own them again
- old generated config files are left behind after experiments
- you migrated from legacy JSON and want a clean regeneration pass
- you changed code generation format and want all site/stream configs rewritten consistently

Recommended sequence for a legacy JSON runtime:

```bash
go run ./cmd/migrate-json-to-sqlite -input-dir . -output-dir .
./hubfly-reverse-proxy recreate -config-dir . -source-store sqlite
```

Recommended sequence for a live SQLite runtime:

```bash
./hubfly-reverse-proxy recreate -config-dir . -source-store sqlite
```

Expected output format:

```text
recreate complete: source_store=sqlite sites=3 streams=2 stream_ports=2 config_dir=/absolute/path
```

Basic recreate verification:

```bash
find ./sites -maxdepth 1 -name '*.conf' | sort
find ./streams -maxdepth 1 -name '*.conf' | sort
curl -s http://localhost:10003/v1/health | jq
```

API-triggered recreate from a running Hubfly instance:

```bash
curl -s -X POST http://localhost:10003/v1/control/recreate | jq
```

Example response:

```json
{
  "status": "recreated",
  "sites_recreated": 3,
  "streams_recreated": 2,
  "stream_ports_rebuilt": 2,
  "requested_at_utc_time": "2026-03-12T10:00:00Z"
}
```

This endpoint uses the running service's configured runtime paths automatically, so it does not need `-config-dir` or any request body.

If you want to verify legacy JSON rebuild without starting the API service first:

```bash
./hubfly-reverse-proxy recreate -config-dir . -source-store json
nginx -t -c ./nginx/nginx.conf
```

Project-level regression tests:

```bash
go test ./internal/api ./internal/nginx ./internal/recreate
```

Example response:

```json
{
  "status": "ok",
  "time": "2026-03-08T19:00:00Z",
  "service": {
    "version": "v1.0.0",
    "commit": "abc123",
    "build_time": "2026-03-08T18:00:00Z",
    "go_version": "go1.25.3",
    "started_at": "2026-03-08T19:00:00Z",
    "uptime": "5m10s"
  },
  "nginx": {
    "available": true,
    "running": true,
    "binary": "/usr/sbin/nginx",
    "version": "nginx version: nginx/1.24.0"
  },
  "certbot": {
    "available": true,
    "binary": "/usr/bin/certbot",
    "version": "certbot 2.11.0"
  },
  "docker": {
    "available": false,
    "error": "docker sync disabled"
  },
  "store": {
    "sites_count": 0,
    "streams_count": 0
  }
}
```

### 2. Management Version Endpoint

```bash
curl -s http://localhost:10003/v1/management/version | jq
```

Example response:

```json
{
  "service": {
    "version": "v1.0.0",
    "commit": "abc123",
    "build_time": "2026-03-08T18:00:00Z",
    "go_version": "go1.25.3"
  },
  "tools": {
    "nginx": {
      "available": true,
      "binary": "/usr/sbin/nginx",
      "version": "nginx version: nginx/1.24.0"
    },
    "certbot": {
      "available": true,
      "binary": "/usr/bin/certbot",
      "version": "certbot 2.11.0"
    }
  }
}
```

### 2.1. Container Port Discovery

Scan a container IP for listening TCP ports.

Example: full range scan

```bash
curl -s -X POST http://localhost:10003/v1/management/container-ports \
  -H "Content-Type: application/json" \
  -d '{
    "container": "determined_bassi",
    "from_port": 1,
    "to_port": 65535,
    "timeout_ms": 150,
    "concurrency": 512
  }' | jq
```

Example: targeted scan

```bash
curl -s -X POST http://localhost:10003/v1/management/container-ports \
  -H "Content-Type: application/json" \
  -d '{
    "container": "determined_bassi",
    "ports": [80, 443, 8000, 8080],
    "network": "bridge"
  }' | jq
```

Example response:

```json
{
  "container": "determined_bassi",
  "ports_scanned": 4,
  "timeout_ms": 150,
  "concurrency": 512,
  "duration_ms": 19,
  "results": [
    {
      "ip": "172.18.0.4",
      "network": "bridge",
      "open_ports": [8000]
    }
  ]
}
```

### 3. Create Site (HTTP)

```bash
curl -s -X POST http://localhost:10003/v1/sites \
  -H "Content-Type: application/json" \
  -d '{
    "id": "example.local",
    "domain": "example.local",
    "upstreams": ["127.0.0.1:8080"],
    "ssl": false,
    "force_ssl": false,
    "templates": []
  }' | jq
```

Example response:

```json
{
  "id": "example.local",
  "domain": "example.local",
  "upstreams": ["127.0.0.1:8080"],
  "ssl": false,
  "force_ssl": false,
  "templates": [],
  "status": "provisioning",
  "created_at": "2026-03-08T19:10:00Z",
  "updated_at": "2026-03-08T19:10:00Z"
}
```

### 4. Create Site (SSL)

```bash
curl -s -X POST http://localhost:10003/v1/sites \
  -H "Content-Type: application/json" \
  -d '{
    "id": "secure-site-1",
    "domain": "testing-33.hubfly.app",
    "upstreams": ["127.0.0.1:8080"],
    "ssl": true,
    "force_ssl": true,
    "templates": ["security-headers", "basic-caching"]
  }' | jq
```

### 5. List Sites

```bash
curl -s http://localhost:10003/v1/sites | jq
```

### 6. Get Site

```bash
curl -s http://localhost:10003/v1/sites/example.local | jq
```

### 7. Patch Site

```bash
curl -s -X PATCH http://localhost:10003/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "upstreams": ["127.0.0.1:8081", "127.0.0.1:8082"],
    "force_ssl": false,
    "load_balancing": {
      "enabled": true,
      "algorithm": "round_robin",
      "weights": [5, 1]
    }
  }' | jq
```

### 8. Delete Site

```bash
curl -s -X DELETE http://localhost:10003/v1/sites/example.local | jq
```

Delete and revoke certificate:

```bash
curl -s -X DELETE "http://localhost:10003/v1/sites/secure-site-1?revoke_cert=true" | jq
```

Example response:

```json
{
  "status": "deleted"
}
```

### 9. Site Logs

Access logs:

```bash
curl -s "http://localhost:10003/v1/sites/example.local/logs?type=access&limit=20" | jq
```

Error logs:

```bash
curl -s "http://localhost:10003/v1/sites/example.local/logs?type=error&limit=20" | jq
```

Search and time filter:

```bash
curl -s "http://localhost:10003/v1/sites/example.local/logs?type=access&search=POST&since=2026-03-08T00:00:00Z&until=2026-03-08T23:59:59Z" | jq
```

### 10. Manual Certificate Retry

```bash
curl -s -X POST http://localhost:10003/v1/sites/example.local/cert/retry | jq
```

Example response:

```json
{
  "status": "retry-started",
  "site_id": "example.local",
  "cert_issue_status": "retrying",
  "next_cert_retry_at": "2026-03-08T19:20:00Z"
}
```

### 11. Firewall Rules

Set firewall via site patch:

```bash
curl -s -X PATCH http://localhost:10003/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "firewall": {
      "ip_rules": [
        {"action": "allow", "value": "192.168.1.100"},
        {"action": "deny", "value": "192.168.1.0/24"},
        {"action": "allow", "value": "all"}
      ],
      "block_rules": {
        "user_agents": ["curl", "wget"],
        "methods": ["DELETE", "PUT"],
        "paths": ["/admin", "/private"]
      },
      "rate_limit": {
        "enabled": true,
        "rate": 10,
        "unit": "r/s",
        "burst": 20
      }
    }
  }' | jq
```

Get firewall:

```bash
curl -s http://localhost:10003/v1/sites/example.local/firewall | jq
```

Clear firewall section:

```bash
curl -s -X DELETE "http://localhost:10003/v1/sites/example.local/firewall?section=ip_rules" | jq
```

Clear all firewall:

```bash
curl -s -X DELETE "http://localhost:10003/v1/sites/example.local/firewall" | jq
```

### 12. Create Stream (TCP/UDP)

Auto-assigned listen port (30000-30100):

```bash
curl -s -X POST http://localhost:10003/v1/streams \
  -H "Content-Type: application/json" \
  -d '{
    "id": "db-1",
    "upstream": "127.0.0.1:5432",
    "protocol": "tcp"
  }' | jq
```

Example response:

```json
{
  "id": "db-1",
  "listen_port": 30073,
  "upstream": "127.0.0.1:5432",
  "protocol": "tcp",
  "status": "provisioning",
  "created_at": "2026-03-08T19:25:00Z",
  "updated_at": "2026-03-08T19:25:00Z"
}
```

### 13. List Streams

```bash
curl -s http://localhost:10003/v1/streams | jq
```

### 14. Get Stream

```bash
curl -s http://localhost:10003/v1/streams/db-1 | jq
```

### 15. Delete Stream

```bash
curl -s -X DELETE http://localhost:10003/v1/streams/db-1 | jq
```

Example response:

```json
{
  "status": "deleted"
}
```

### 16. Manual NGINX Reload

```bash
curl -s -X POST http://localhost:10003/v1/control/reload | jq
```

Example response:

```json
{
  "status": "reloaded",
  "time": "2026-03-08T19:30:00Z"
}
```

### 17. Manual Full Check And Reload (Docker Sync Mode)

Requires `-enable-docker-sync`.

```bash
curl -s -X POST http://localhost:10003/v1/control/full-check | jq
```

Example response:

```json
{
  "status": "checked",
  "reloaded": true,
  "sites_changed": 1,
  "stream_ports_changed": 1,
  "next_scheduled_check": "2026-03-08T20:30:00Z",
  "requested_at_utc_time": "2026-03-08T19:30:00Z"
}
```

If Docker sync is disabled, response is:

```json
{
  "error": "docker engine is unavailable",
  "code": 503
}
```

Container-name upstreams like `my_container:8000` still work as long as Docker is reachable via `-docker-sock`. The `-enable-docker-sync` flag enables the 5-minute background full-check loop, Docker event-triggered full-checks, and the manual full-check endpoint.

### 18. Wildcard Certificate Mapping (Optional)

Default lookup path:
- `<config-dir>/certbot/config/wildcards/config.json`

The release package includes a starter file at:
- `certbot/config/wildcards/config.json`

Optional startup flag:
- `-wildcard-certs-config /path/to/config.json`

Example config:

```json
{
  "wildcards": [
    {
      "domain": "eu1.hubfly.app",
      "cert_path": "wildcards/eu1/fullchain.pem",
      "key_path": "wildcards/eu1/privkey.pem"
    },
    {
      "domain": "rw1.hubfly.app",
      "cert_path": "wildcards/rw1/fullchain.pem",
      "key_path": "wildcards/rw1/privkey.pem"
    },
    {
      "domain": "us1.hubfly.app",
      "cert_path": "wildcards/us1/fullchain.pem",
      "key_path": "wildcards/us1/privkey.pem"
    }
  ]
}
```

Notes:
- `domain` matches exact host and subdomains.
- Relative cert/key paths are resolved under `-certs-dir`.

## Project Structure

- `cmd/hubfly`: main API binary
- `cmd/migrate-json-to-sqlite`: one-time migration binary
- `internal/api`: HTTP handlers and orchestration
- `internal/nginx`: config generation and process controls
- `internal/certbot`: certbot wrapper and health checks
- `internal/store`: storage implementations
- `internal/logmanager`: site log query helpers
- `nginx/`, `templates/`, `static/`: runtime assets

## Troubleshooting

### 1. `curl 127.0.0.1` returns `403 Forbidden`

Check runtime nginx error log:

```bash
tail -n 100 <config-dir>/logs/nginx/nginx.error.log
```

If you see `Permission denied` under `/home/<user>/...`, start Hubfly with `sudo`. Hubfly rewrites nginx worker `user` to the invoking runtime user (from `SUDO_USER`) so paths under your home directory are accessible.

### 2. Existing nginx already running on host

Hubfly takeover is now safe:
- It validates Hubfly nginx config before takeover.
- It ignores container-owned nginx processes.
- It takes over only host nginx master processes.

To verify what owns ports:

```bash
ss -ltnp | rg ':80|:443|:10003|:10004|:10005'
```

If another service still owns `:80/:443`, Hubfly nginx cannot bind those ports.

### 2.1 `unknown directive "stream"`

This means nginx stream module is missing. Install it, then restart Hubfly:

```bash
sudo apt update && sudo apt install -y libnginx-mod-stream
pm2 restart hubfly-reverse-proxy
```

### 3. Static pages not loading

Hubfly syncs `static/` into `<config-dir>/www/static` at boot.
Check both:

```bash
ls -la <config-dir>/static
ls -la <config-dir>/www/static
```
