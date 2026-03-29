# Hubfly API Reference

Base API URL: `http://localhost:10003`

Management UI URL: `http://localhost:10004`

## Conventions

- Content type: JSON for request and response bodies unless noted otherwise.
- Request ID: every response includes `X-Request-ID`.
- Error response format:

```json
{
  "error": "message",
  "code": 400
}
```

- Many write operations are asynchronous. The API usually returns `201` or `200` after state is stored, then nginx/certbot work continues in the background.

## Data Models

### Site

```json
{
  "id": "example.local",
  "domain": "example.local",
  "upstreams": ["127.0.0.1:8080"],
  "upstream_containers": ["example_container"],
  "upstream_networks": ["bridge"],
  "load_balancing": {
    "enabled": true,
    "algorithm": "round_robin",
    "weights": [1, 1]
  },
  "force_ssl": false,
  "ssl": false,
  "templates": ["security-headers"],
  "extra_config": "",
  "proxy_set_header": {
    "X-Forwarded-Host": "$host"
  },
  "firewall": {
    "ip_rules": [
      { "value": "192.168.1.100", "action": "allow" }
    ],
    "block_rules": {
      "user_agents": ["curl"],
      "methods": ["DELETE"],
      "paths": ["/admin"],
      "path_methods": {
        "/admin": ["POST", "DELETE"]
      }
    },
    "rate_limit": {
      "enabled": true,
      "rate": 10,
      "unit": "r/s",
      "burst": 20,
      "zone_name": ""
    }
  },
  "status": "active",
  "error_message": "",
  "created_at": "2026-03-12T10:00:00Z",
  "updated_at": "2026-03-12T10:00:00Z",
  "cert_issue_status": "valid",
  "cert_retry_count": 0,
  "next_cert_retry_at": null,
  "last_cert_error": ""
}
```

Notes:
- `upstreams` is required on create.
- Each upstream can be an IP endpoint like `127.0.0.1:8080` or a container endpoint like `my_container:8080`.
- Container upstreams require Docker connectivity.
- `load_balancing.algorithm` supports `round_robin`, `least_conn`, `ip_hash`.
- `ip_hash` requires all weights to be `1`.

### Stream

```json
{
  "id": "stream-30001",
  "listen_port": 30001,
  "upstream": "127.0.0.1:5432",
  "container_name": "postgres",
  "container_network": "bridge",
  "protocol": "tcp",
  "domain": "",
  "status": "active",
  "error_message": "",
  "created_at": "2026-03-12T10:00:00Z",
  "updated_at": "2026-03-12T10:00:00Z"
}
```

Notes:
- `protocol` defaults to `tcp`.
- If `listen_port` is omitted, Hubfly auto-picks an unused port in `30000-30100`.
- `domain` is used for SNI-based TCP routing.

## Endpoints

### GET `/v1/health`

Returns overall service health, build info, nginx/certbot/docker health, and store counts.

Smart behavior:
- upstreams are re-resolved from Docker
- nginx reload happens only if site or stream config changed

Response: `200`

### GET `/v1/management/version`

Returns build metadata and sub-tool versions.

Response: `200`

### POST `/v1/management/container-ports`

Scans listening TCP ports on a container IP.

Request body:

```json
{
  "container": "determined_bassi",
  "network": "bridge",
  "from_port": 1,
  "to_port": 65535,
  "ports": [80, 443, 8000],
  "timeout_ms": 150,
  "concurrency": 512
}
```

Rules:
- `container` is required.
- Use either `ports` or `from_port`/`to_port`.
- If no ports are provided, Hubfly scans `1-65535`.
- `timeout_ms` defaults to `150`.
- `concurrency` defaults to `512`, max `2048`.
- `network` filters to one resolved Docker network.

Response: `200`

```json
{
  "container": "determined_bassi",
  "ports_scanned": 3,
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

Errors:
- `400` invalid json, invalid port range, unknown container, no matching network
- `503` Docker unavailable

### GET `/v1/sites`

Returns all sites.

Response: `200` with `[]Site`

### POST `/v1/sites`

Creates a site and starts background provisioning.

Request body:

```json
{
  "id": "example.local",
  "domain": "example.local",
  "upstreams": ["127.0.0.1:8080"],
  "load_balancing": {
    "enabled": true,
    "algorithm": "round_robin",
    "weights": [1]
  },
  "force_ssl": false,
  "ssl": false,
  "templates": ["security-headers"],
  "extra_config": "",
  "proxy_set_header": {
    "X-Forwarded-Host": "$host"
  },
  "firewall": {
    "ip_rules": [],
    "block_rules": null,
    "rate_limit": null
  }
}
```

Notes:
- `domain`, `upstreams`, `ssl`, `force_ssl`, and `templates` are the practical core fields.
- If `id` is empty, it defaults to `domain`.
- If `ssl` is true, certificate issuance starts after initial HTTP config is applied.

Response: `201` with created `Site`

Errors:
- `400` invalid json, bad upstreams, invalid load balancing config
- `500` persistence failure

### GET `/v1/sites/{id}`

Returns one site.

Response: `200` with `Site`

Errors:
- `404` site not found

### PATCH `/v1/sites/{id}`

Partially updates a site.

Allowed request fields:

```json
{
  "domain": "new.example.local",
  "upstreams": ["127.0.0.1:8081"],
  "load_balancing": {
    "enabled": true,
    "algorithm": "least_conn",
    "weights": [1]
  },
  "force_ssl": true,
  "ssl": true,
  "extra_config": "client_max_body_size 100m;",
  "proxy_set_header": {
    "X-Forwarded-Host": "$host"
  },
  "firewall": {
    "ip_rules": [
      { "value": "10.0.0.0/8", "action": "deny" }
    ]
  }
}
```

Behavior:
- Changing `domain` or `ssl` triggers full reprovisioning.
- Changing other config triggers background config refresh.
- Disabling `ssl` clears certificate retry state.

Response: `200` with updated `Site`

Errors:
- `400` invalid json, invalid upstreams, invalid load balancing config
- `404` site not found
- `500` persistence failure

### DELETE `/v1/sites/{id}`

Deletes a site, removes nginx config, deletes site logs, and optionally revokes certificate.

Query parameters:
- `revoke_cert=true`

Response: `200`

```json
{
  "status": "deleted"
}
```

Errors:
- `404` site not found
- `500` nginx deletion failure, log deletion failure, store deletion failure

### GET `/v1/sites/{id}/logs`

Reads site logs.

Query parameters:
- `type`: `access` or `error`, default `access`
- `limit`: integer, default `100`
- `search`: substring filter
- `since`: RFC3339 timestamp
- `until`: RFC3339 timestamp

Response:
- `200` with `[]LogEntry` for `type=access`
- `200` with `[]ErrorLogEntry` for `type=error`

Errors:
- `500` log read failure

### POST `/v1/sites/{id}/cert/retry`

Triggers immediate certificate retry for an SSL-enabled site.

Response: `202`

```json
{
  "status": "retry-started",
  "site_id": "example.local",
  "cert_issue_status": "retrying",
  "next_cert_retry_at": "2026-03-12T10:30:00Z"
}
```

Errors:
- `400` SSL disabled
- `404` site not found
- `409` retry already in progress
- `500` persistence failure

### GET `/v1/sites/{id}/firewall`

Returns firewall config for a site.

Response:
- `200` with `FirewallConfig`
- `200` with `{}` if no firewall is set

### DELETE `/v1/sites/{id}/firewall`

Clears all or part of firewall config.

Query parameters:
- `section=ip_rules`
- `section=block_rules`
- `section=rate_limit`
- `section=all`
- empty `section` is treated as `all`

Response: `200`

```json
{
  "status": "cleared",
  "section": "ip_rules"
}
```

Special case:
- if no firewall exists, response is `{"status":"no firewall rules to clear"}`

Errors:
- `400` invalid section
- `404` site not found
- `500` persistence failure

### GET `/v1/streams`

Returns all streams.

Response: `200` with `[]Stream`

### POST `/v1/streams`

Creates a stream and starts background reconciliation.

Request body:

```json
{
  "id": "db-1",
  "listen_port": 30001,
  "upstream": "127.0.0.1:5432",
  "container_port": 5432,
  "protocol": "tcp",
  "domain": ""
}
```

Rules:
- `upstream` is required.
- `protocol` supports `tcp` and `udp`.
- If `listen_port` is omitted, Hubfly assigns an unused port in `30000-30100`.
- If `id` is omitted, Hubfly sets `stream-{listen_port}`.

Response: `201` with created `Stream`

Errors:
- `400` invalid json, bad upstream, Docker resolution failure
- `500` persistence failure, no free port in `30000-30100`

### GET `/v1/streams/{id}`

Returns one stream.

Response: `200` with `Stream`

Errors:
- `404` stream not found

### DELETE `/v1/streams/{id}`

Deletes a stream and reconciles nginx for the affected listen port.

Response: `200`

```json
{
  "status": "deleted"
}
```

Errors:
- `404` stream not found
- `500` store deletion failure

### POST `/v1/control/reload`

Forces nginx reload.

Response: `200`

```json
{
  "status": "reloaded",
  "time": "2026-03-12T10:00:00Z"
}
```

Errors:
- `500` reload failed

### POST `/v1/control/full-check`

Runs manual Docker upstream refresh and one reload.

Requirements:
- Docker must be reachable
- `-enable-docker-sync=true`

Background behavior in Docker sync mode:
- Hubfly runs the same full-check flow every 5 minutes
- Hubfly also queues the same full-check flow on Docker container `start`, `restart`, and `unpause` events

Response: `200`

```json
{
  "status": "checked",
  "reloaded": true,
  "sites_changed": 1,
  "stream_ports_changed": 1,
  "next_scheduled_check": "2026-03-12T11:00:00Z",
  "requested_at_utc_time": "2026-03-12T10:00:00Z"
}
```

Errors:
- `503` Docker unavailable
- `500` full check failed

### POST `/v1/control/recreate`

Prunes generated Hubfly nginx config files and recreates them from stored site/stream state.

This endpoint does not need a config directory in the request. It uses the runtime store and paths of the running Hubfly process.

Response: `200`

```json
{
  "status": "recreated",
  "sites_recreated": 3,
  "streams_recreated": 2,
  "stream_ports_rebuilt": 2,
  "requested_at_utc_time": "2026-03-12T10:00:00Z"
}
```

Errors:
- `500` recreate failed

## Validation Rules Summary

- Ports must be `1-65535`.
- `from_port` must be less than or equal to `to_port`.
- Site upstream list must not be empty.
- Load balancing weights must match upstream count.
- Load balancing weights must be `>= 1`.
- `ip_hash` requires all weights to be `1`.
- Unknown route suffixes under `/v1/sites/{id}` and `/v1/streams/{id}` return standard not-found or method errors from the Go HTTP mux/handler flow.

## Operational Notes

- Site creation with `ssl=true` is two-step:
  - initial HTTP config
  - certificate issuance
  - final SSL refresh
- Container upstreams are resolved immediately when the site/stream is created or patched.
- Site deletion also deletes `<site>.access.log` and `<site>.error.log`.
- Docker full-check endpoint matches the background Docker sync behavior: with Docker sync enabled, the same full-check flow is run every 5 minutes and on Docker container `start`, `restart`, and `unpause` events.
- Recreate endpoint rebuilds from the running service's configured store and runtime directories, so it is the API equivalent of `hubfly-reverse-proxy recreate`.
