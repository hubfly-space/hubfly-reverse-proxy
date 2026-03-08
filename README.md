# hubfly-reverse-proxy

Host-native reverse proxy manager for NGINX + Certbot with a Go API.

## Runtime Model

Hubfly runs directly on the host (no container required) and uses host-installed tools.

Required tools:
- `nginx`
- `certbot`

Optional:
- Docker socket only if you enable container upstream sync (`-enable-docker-sync`).

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

## Versioning

Build metadata is injected at build time and shown by APIs.

```bash
go build -ldflags "-X main.appVersion=v1.0.0 -X main.gitCommit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o main ./cmd/hubfly
```

Print binary version only:

```bash
./main version
```

Expected output (string only):

```text
v1.0.0
```

## Run

```bash
./main -config-dir . -port 81
```

With Docker upstream sync enabled:

```bash
./main -config-dir . -port 81 -enable-docker-sync -docker-sock /var/run/docker.sock
```

## Release

Tag push like `v1.2.0` triggers `.github/workflows/release.yml`.
It publishes a GitHub Release zip containing:
- `bin/hubfly`
- `bin/migrate-json-to-sqlite`
- `nginx/`
- `templates/`
- `static/`
- migration script and runtime folder skeleton

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

## API Usage And Testing

Base URL (default): `http://localhost:81`

### 1. Health

```bash
curl -s http://localhost:81/v1/health | jq
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
curl -s http://localhost:81/v1/management/version | jq
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

### 3. Create Site (HTTP)

```bash
curl -s -X POST http://localhost:81/v1/sites \
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
curl -s -X POST http://localhost:81/v1/sites \
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
curl -s http://localhost:81/v1/sites | jq
```

### 6. Get Site

```bash
curl -s http://localhost:81/v1/sites/example.local | jq
```

### 7. Patch Site

```bash
curl -s -X PATCH http://localhost:81/v1/sites/example.local \
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
curl -s -X DELETE http://localhost:81/v1/sites/example.local | jq
```

Delete and revoke certificate:

```bash
curl -s -X DELETE "http://localhost:81/v1/sites/secure-site-1?revoke_cert=true" | jq
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
curl -s "http://localhost:81/v1/sites/example.local/logs?type=access&limit=20" | jq
```

Error logs:

```bash
curl -s "http://localhost:81/v1/sites/example.local/logs?type=error&limit=20" | jq
```

Search and time filter:

```bash
curl -s "http://localhost:81/v1/sites/example.local/logs?type=access&search=POST&since=2026-03-08T00:00:00Z&until=2026-03-08T23:59:59Z" | jq
```

### 10. Manual Certificate Retry

```bash
curl -s -X POST http://localhost:81/v1/sites/example.local/cert/retry | jq
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
curl -s -X PATCH http://localhost:81/v1/sites/example.local \
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
curl -s http://localhost:81/v1/sites/example.local/firewall | jq
```

Clear firewall section:

```bash
curl -s -X DELETE "http://localhost:81/v1/sites/example.local/firewall?section=ip_rules" | jq
```

Clear all firewall:

```bash
curl -s -X DELETE "http://localhost:81/v1/sites/example.local/firewall" | jq
```

### 12. Create Stream (TCP/UDP)

Auto-assigned listen port (30000-30100):

```bash
curl -s -X POST http://localhost:81/v1/streams \
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
curl -s http://localhost:81/v1/streams | jq
```

### 14. Get Stream

```bash
curl -s http://localhost:81/v1/streams/db-1 | jq
```

### 15. Delete Stream

```bash
curl -s -X DELETE http://localhost:81/v1/streams/db-1 | jq
```

Example response:

```json
{
  "status": "deleted"
}
```

### 16. Manual NGINX Reload

```bash
curl -s -X POST http://localhost:81/v1/control/reload | jq
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
curl -s -X POST http://localhost:81/v1/control/full-check | jq
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

### 18. Wildcard Certificate Mapping (Optional)

Default lookup path:
- `<config-dir>/certbot/config/wildcards/config.json`

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
