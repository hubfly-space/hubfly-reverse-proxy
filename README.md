# hubfly-reverse-proxy

A lightweight, single container reverse proxy appliance wrapping NGINX and Certbot with a Go based REST API. It provides safe, atomic configuration management and automated SSL certificate handling.

## Runtime Model

Hubfly runs NGINX, Certbot, and the Go API in one container. The container is expected to run with host networking and have Docker Engine socket access:

- `network_mode: host`
- `/var/run/docker.sock:/var/run/docker.sock`

Default in-container paths remain:

- Config/data: `/etc/hubfly`
- Certificates: `/etc/letsencrypt`
- Webroot: `/var/www/hubfly`
- Logs: `/var/log/hubfly`
- Go app logs: `/var/log/hubfly-go`
- NGINX config: `/etc/nginx/nginx.conf`

Go app logging:
- Structured JSON logs are written hourly to `/var/log/hubfly-go/hubfly-go-YYYYMMDD-HH.log`.
- Default retention is `1h` with periodic cleanup.
- Retention is configurable via `--app-log-retention` (Go duration format).

State files:
- Sites: `/etc/hubfly/sites.json`
- Streams: `/etc/hubfly/streams.json`

Upstream controller behavior:
- API accepts container-name upstreams (e.g. `my_app:3000`).
- Hubfly stores both the current IP upstream and container metadata in `sites.json`/`streams.json`.
- Every 1 hour, Hubfly checks Docker for container IP changes and updates those files.
- During a check cycle, Hubfly batches all detected changes, then performs a single NGINX reload at the end of the cycle.
- Manual controls are available via:
  - `POST /v1/control/reload`
  - `POST /v1/control/full-check`

## Versioning

Build metadata is injected at build time and reported by `GET /v1/health`.

```bash
go build -ldflags "-X main.appVersion=v1.0.0 -X main.gitCommit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o hubfly ./cmd/hubfly
```

## How to Run

The easiest way to run Hubfly is using Docker Compose. This sets up the API, NGINX, and necessary volumes.

### Prerequisites
- Docker
- Docker Compose

### Start the Service
```bash
docker-compose up --build
```

- **API**: `http://localhost:81`
- **Management UI**: `http://localhost:82`
- **HTTP**: Port `80`
- **HTTPS**: Port `443`

## Deployment

A helper script `deploy.sh` is provided to simplify deploying to a remote server via SSH. It handles building the image locally, compressing it, transferring it to the remote server, and starting it with a production-optimized configuration.

### Usage

```bash
./deploy.sh ubuntu@hub-1
```

This script performs the following steps:
1.  **Build**: Builds the Docker image locally.
2.  **Transfer**: Saves and compresses the image, then transfers it via `scp` (showing a progress bar).
3.  **Configure**: Generates a production `docker-compose.yml` on the remote server.
4.  **Run**: Loads the image and starts the service remotely.

**Note on Resource Naming:**
The project uses explicit naming for Docker volumes and networks (e.g., `name: hubfly_proxy_data`). This prevents Docker Compose from prepending the directory name (avoiding names like `hubfly-reverse-proxy_hubfly_proxy_data`) and ensures resources are named consistently across different environments (`hubfly_proxy_data`, `hubfly_proxy_certs`, `hubfly_proxy_webroot`).

---

## Analytics (GoAccess)

Hubfly integrates **GoAccess** for real-time, visual web traffic analytics.

- **Dashboard URL**: `http://localhost:82/analytics.html`
- **Access Control**: The analytics dashboard is **only available on the Management Port (82)**. It is blocked on the public HTTP port (80) to prevent unauthorized access.
- **Privacy**: Access to the analytics dashboard itself is not logged to prevent self-tracking.

### Features
- **Real-time**: Updates via WebSocket (`/goaccess-ws`) on port 82.
- **Visualizations**: Interactive graphs for visitors, bandwidth, requested files, and more.
- **Metrics**: detailed breakdown of status codes, operating systems, browsers, and geo-location (if configured).

---

---

## API Usage & Testing

Here are `curl` commands to interact with the API.

### 1. Check Health
Verify the service is running.
```bash
curl -i http://localhost:81/v1/health
```

Health now includes:
- service version/commit/build time/go version/uptime
- nginx availability, running state, binary, config, pid, version
- certbot availability, binary, version, cert root, webroot
- docker engine availability/version via `/var/run/docker.sock`
- store counts (sites/streams)

### 1.1 Manual NGINX/Sync Controls
```bash
# Force nginx reload immediately
curl -X POST http://localhost:81/v1/control/reload

# Run full upstream check against Docker, apply any changes, then reload once
curl -X POST http://localhost:81/v1/control/full-check
```

### 2. Create a Simple Site (HTTP)
Forward traffic from `example.local` to a local upstream (e.g., a container IP or external site).
```bash
curl -X POST http://localhost:81/v1/sites \
  -H "Content-Type: application/json" \
  -d '{
    "id": "example.local",
    "domain": "example.local",
    "upstreams": ["simple-server:80", "simple-server-2:80"],
    "load_balancing": {
      "algorithm": "round_robin",
      "weights": [5, 1]
    },
    "ssl": false,
    "force_ssl": false,
    "templates": []
  }'
```
*Note: To test this locally, add `127.0.0.1 example.local` to your `/etc/hosts`.*

### 3. Create a Site with SSL (Production)
**Prerequisite:** The domain must point to this server's public IP, and port 80/443 must be open.
```bash "basic-caching", 
curl -X POST http://localhost:81/v1/sites \
  -H "Content-Type: application/json" \
  -d '{
    "id": "secure-site-1",
    "domain": "testing-33.hubfly.app",
    "upstreams": ["youthful_margulis:80"],
    "ssl": true,
    "force_ssl": true,
    "templates": ["security-headers","basic-caching"]
  }'
```

### 4. List All Sites
See all configured sites and their status.
```bash
curl http://localhost:81/v1/sites
```

### 5. Get Site Details
View configuration for a specific site.
```bash
curl http://localhost:81/v1/sites/example.local
```

### 6. Delete a Site
Remove the NGINX config. Add `?revoke_cert=true` to also revoke the SSL certificate.
```bash
curl -X DELETE http://localhost:81/v1/sites/example.local
# OR with revocation
# curl -X DELETE "http://localhost:81/v1/sites/secure-site?revoke_cert=true"
```

### 7. TCP/UDP Stream Proxying (Databases, SSH, etc.)
Hubfly can also proxy TCP and UDP traffic (Layer 4). This is useful for exposing databases, game servers, or other non-HTTP services.

**Important:** You must ensure the `listen_port` is exposed in your Docker container (e.g., via `-p` flags in `docker run` or `ports` in `docker-compose.yml`).

#### Basic TCP Stream (e.g., Postgres)
Forward traffic from an automatically assigned port (30000-30100) on the host to a container named `postgres_db` on port `5432`. If `listen_port` is omitted, it will be automatically assigned. The assigned port will be returned in the response.

```bash
curl -X POST http://localhost:81/v1/streams \
  -H "Content-Type: application/json" \
  -d '{
    "upstream": "jolly_kare:5432",
    "protocol": "tcp",
    "id":"jolly_kare:5432"
  }'
```

response:
{"id":"db-1:3306","listen_port":30073,"upstream":"db-1:3306","protocol":"tcp","status":"provisioning","created_at":"2025-11-27T12:40:20.176747778Z","updated_at":"2025-11-27T12:40:20.176747878Z"}


#### List Streams
```bash
curl http://100.106.206.92:81/v1/streams
```

#### Delete a Stream
```bash
# For a basic stream, the ID is typically 'stream-{port}' or manually provided
curl -X DELETE http://localhost:81/v1/streams/db-1:3306

# For an SNI stream, use the provided ID
curl -X DELETE http://localhost:81/v1/streams/mysql-db1
```

### 8. Retrieve Site Logs
Access detailed logs for a specific site. Logs are stored individually per domain (`.access.log` and `.error.log`).

**Endpoint:** `GET /v1/sites/{id}/logs`

**Query Parameters:**
- `type` (optional): `access` (default) or `error`.
- `limit` (optional): Number of recent lines to return (default: 100).
- `search` (optional): Filter logs containing a specific string.
- `since` (optional): Filter logs after a specific timestamp (RFC3339 format, e.g., `2025-12-26T10:00:00Z`).
- `until` (optional): Filter logs before a specific timestamp.

**Example: Get recent errors**
```bash
curl "http://localhost:81/v1/sites/example.local/logs?type=error&limit=50"
```

**Example: Search access logs for POST requests**
```bash
curl "http://localhost:81/v1/sites/example.local/logs?type=access&search=POST&limit=20"
```

### 9. Firewall Management
Configure advanced access control rules per site.

**IP-based Access Control**
Allow or deny traffic from specific IP addresses or CIDR ranges. Rules are processed in order.

**Endpoint:** `PATCH /v1/sites/{id}`

**Example: Whitelist specific IP, deny subnet, allow others**
```bash
curl -X PATCH http://localhost:81/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "firewall": {
      "ip_rules": [
        {"action": "allow", "value": "192.168.1.100"},
        {"action": "deny", "value": "192.168.1.0/24"},
        {"action": "allow", "value": "all"}
      ]
    }
  }'
```

**Basic Request Filtering**
Block requests based on User-Agent, HTTP Method, or URL Path.
- **User Agents**: Regex pattern matching (case-insensitive).
- **Methods**: Block specific HTTP methods (e.g., POST, DELETE).
- **Paths**: Block specific URL paths (regex supported).

**Example: Block 'curl' agent, DELETE method, and '/admin' path**
```bash
curl -X PATCH http://localhost:81/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "firewall": {
      "block_rules": {
        "user_agents": ["curl", "wget", "scanner"],
        "methods": ["DELETE", "PUT"],
        "paths": ["/admin", "/private", "/.git"]
      }
    }
  }'
```

**Rate Limiting**
Protect against abuse and DDoS attacks by limiting request rates.
- **Rate**: Number of requests allowed per unit (e.g., 10).
- **Unit**: `r/s` (requests per second) or `r/m` (requests per minute).
- **Burst**: Allow a burst of requests above the limit (nodelay).

**Example: Limit to 10 requests/second with a burst of 20**
```bash
curl -X PATCH http://localhost:81/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "firewall": {
      "rate_limit": {
        "enabled": true,
        "rate": 10,
        "unit": "r/s",
        "burst": 20
      }
    }
  }'
```

**Path-Specific Method Blocking**
Block specific HTTP methods for defined paths (e.g., prevent POST to /admin).

**Example: Block POST and DELETE on /admin**
```bash
curl -X PATCH http://localhost:81/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "firewall": {
      "block_rules": {
        "path_methods": {
          "/admin": ["POST", "DELETE"],
          "/api/readonly": ["PUT", "PATCH", "DELETE"]
        }
      }
    }
  }'
```

**View Firewall Rules**
Get the current firewall configuration for a site.

**Endpoint:** `GET /v1/sites/{id}/firewall`

**Example:**
```bash
curl http://localhost:81/v1/sites/example.local/firewall
```

**Clear Firewall Rules**
Remove specific sections of the firewall configuration or clear everything.

**Endpoint:** `DELETE /v1/sites/{id}/firewall`

**Query Parameters:**
- `section`: The section to clear. Options:
    - `ip_rules`: Clear all IP Allow/Deny rules.
    - `block_rules`: Clear all Request Filtering rules.
    - `rate_limit`: Disable and clear Rate Limiting.
    - `all` (or empty): Clear ALL firewall rules.

**Examples:**
```bash
# Clear only IP rules
curl -X DELETE "http://localhost:81/v1/sites/example.local/firewall?section=ip_rules"

# Clear Rate Limiting
curl -X DELETE "http://localhost:81/v1/sites/example.local/firewall?section=rate_limit"

# Clear Everything
curl -X DELETE "http://localhost:81/v1/sites/example.local/firewall"
```

### 10. Load Balancing (Sites Only)
Hubfly supports HTTP upstream load balancing per site.
If `load_balancing` is omitted, site behavior stays backward compatible with legacy mode (single upstream proxy behavior as before).

- `round_robin` (default)
- `least_conn`
- `ip_hash` (basic sticky routing by client IP)
- weighted balancing via `load_balancing.weights` aligned with `upstreams`

Examples:
```bash
# Switch to least connections
curl -X PATCH http://localhost:81/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "load_balancing": {
      "enabled": true,
      "algorithm": "least_conn",
      "weights": [1, 1]
    }
  }'

# Use IP hash stickiness (weights must be 1)
curl -X PATCH http://localhost:81/v1/sites/example.local \
  -H "Content-Type: application/json" \
  -d '{
    "load_balancing": {
      "enabled": true,
      "algorithm": "ip_hash",
      "weights": [1, 1]
    }
  }'
```

---

### 11. Trigger Manual Certificate Retry
Force an immediate certificate issuance retry for an SSL-enabled site.

**Endpoint:** `POST /v1/sites/{id}/cert/retry`

**Example:**
```bash
curl -X POST http://localhost:81/v1/sites/example.local/cert/retry
```

This endpoint is useful when:
- DNS propagation has completed and you want to retry immediately.
- A previous attempt failed and you do not want to wait for the next scheduled retry.

### 12. Run Fallback/Rollback Integration Checks
Validate fallback certificate behavior, manual retry endpoint wiring, rollback safety on invalid config, runtime-resolved stream upstreams, upstream-crash resilience (reload/restart with missing upstream), and startup repair of stale SSL cert paths.

```bash
chmod +x ./integration_fallback_cert_test.sh
./integration_fallback_cert_test.sh
```

Optional environment flags:
- `RESET_STACK=true`: runs `docker compose down -v` before starting tests.
- `CLEANUP_ON_EXIT=true`: tears the stack down after test completion.
- `API_BASE`: override API URL (default `http://localhost:81`).
- `CONTAINER_NAME`: override container name (default `hubfly-reverse-proxy`).
- `CRASH_UPSTREAM_IMAGE`: local image used for crash-resilience checks (defaults to first available of `nginx:stable-alpine`, `nginx:alpine`, then falls back to proxy container image ID).

---

## Project Structure

- **/cmd/hubfly**: Main entry point.
- **/internal/api**: REST API handlers and routing.
- **/internal/nginx**: NGINX configuration generation, validation, and reloading.
- **/internal/certbot**: Wrapper for Certbot (SSL issuance/revocation).
- **/internal/logmanager**: Log reading, filtering, and parsing logic.
- **/internal/store**: JSON-based persistence for site metadata.
- **/static**: Web frontend assets (Dashboard, Analytics UI).
- **/templates**: NGINX configuration snippets (e.g., caching, security).
