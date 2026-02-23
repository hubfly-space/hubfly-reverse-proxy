# hubfly-reverse-proxy — Design document

## 1. Overview
**hubfly-reverse-proxy** is a single Docker image appliance that exposes a REST API (written in Go) to manage an embedded NGINX instance and Certbot for SSL automation. The goal is to allow programmatic, safe, and auditable creation, update, deletion and inspection of HTTP(S) virtual hosts with convenience templates (caching, security headers, compression, etc.). The service validates NGINX configuration before applying it, performs graceful reloads to avoid downtime, supports SSL issuance and revocation (with retries and backoff), and keeps configuration templates so complex setups are easy to enable.

Key principles:
- **Safe-by-default**: always validate before apply (`nginx -t`) and never replace the live config if the test fails.
- **Atomic changes & graceful reloads**: apply only validated config and do `nginx -s reload` for zero-downtime.
- **Programmable templates**: allow enabling features (caching, rate-limiting, compression) via short API calls that expand to NGINX snippets.
- **Idempotence**: API calls are idempotent where feasible (re-applying same config is safe).
- **Observability**: expose health, logs, and config dump endpoints.


## 2. High-level architecture

Components inside the container:
- **Go REST API** (main program) — single binary: `/usr/local/bin/hubfly`.
- **NGINX** — official upstream image used as runtime webserver.
- **Certbot** — official certbot image or package available inside container for certificate provisioning.
- **Local config store** — filesystem layout under `/etc/hubfly/` (configs, templates, metadata, cert files symlinked to `/etc/letsencrypt/`). Optionally an embedded sqlite DB for metadata.

External mounts/binds required (recommended):
- `/etc/letsencrypt` (persist certificates)
- `/var/www/hubfly` (webroot for HTTP challenge)
- `/etc/nginx/conf.d` (nginx site configs; hubfly writes into its own namespace and manages includes)
- optional: `/var/log/hubfly` (app logs)

Traffic flow:
1. User calls API to add `domain -> upstream` mapping.
2. Go app renders NGINX site file (from template + enabled features) to a staging file.
3. Go app runs `nginx -t -c <staging>` or `nginx -t -q -c /etc/nginx/nginx.conf` after placing staging file into include dir.
4. If `nginx -t` succeeds, app atomically moves staging into place and triggers `nginx -s reload`.
5. If `ssl=true` requested, the app triggers Certbot using webroot (or DNS challenge), waits for result, retries on transient errors, and if successful updates site to listen 443 and enables `ssl` blocks.


## 3. File layout (inside container)
```
/etc/hubfly/
  sites/                # generated site files (nginx conf snippets) named <id>.conf
  staging/              # staging area for tests
  templates/            # reusable template snippets (caching, security headers...)
  metadata.db           # sqlite metadata (sites, history, status) or JSON files
  certbot/              # local certbot working dir (if not using /etc/letsencrypt directly)
/var/www/hubfly/        # webroot used for HTTP-01 challenge
/etc/nginx/nginx.conf   # main nginx conf with `include /etc/hubfly/sites/*.conf;`
/var/log/hubfly/        # logs
```


## 4. Core REST API (v1) — resources & examples
Base path: `POST /v1` for create; `GET /v1` for reads, etc. All endpoints return JSON and consistent error format `{ "error": "message", "code": 400 }`.

### 4.1 Health & status
- `GET /v1/health` — returns basic service health (API, nginx, certbot availability, DB)
- `GET /v1/nginx/config` — returns effective nginx configuration summary (list of sites managed)

### 4.2 Sites management
- `POST /v1/sites` — create or update a site (idempotent).
  - Body:
    ```json
    {
      "id": "optional-slug-or-generated",
      "domain": "example.com",
      "upstreams": ["10.0.0.5:8080"],
      "force_ssl": true,
      "ssl": true,               
      "templates": ["basic-caching","security-headers"],
      "extra_config": "optional raw nginx snippet",
      "http_to_https": true,
      "proxy_set_header": {"X-Forwarded-For": "$proxy_add_x_forwarded_for"}
    }
    ```
  - Response: created/updated site metadata, current status (applied/staged), certificate info if any.

- `GET /v1/sites` — list managed sites with status (active, pending-cert, cert-failed, error)
- `GET /v1/sites/{id}` — details & effective config text
- `DELETE /v1/sites/{id}` — remove site: removes nginx config file, triggers reload, and optionally revokes certificate (query param `?revoke_cert=true`).

### 4.3 Partial updates & feature toggles
- `PATCH /v1/sites/{id}` — update part of configuration (e.g., enable caching, change upstream, tweak timeouts). Body contains only changed fields. The server will re-render template + test + apply.

- `POST /v1/sites/{id}/templates` — add/remove named templates (body: {"add": ["caching"], "remove": ["old-template"]})

### 4.4 SSL management
- `POST /v1/sites/{id}/ssl/issue` — triggers certificate issuance for the site's domain(s).
  - Body: `{ "domains": ["example.com", "www.example.com"], "method": "http01|dns01", "email": "admin@example.com", "staging": false }`
  - Response: accepted (202) with operation id for tracking.
- `POST /v1/sites/{id}/ssl/revoke` — revoke certificate.
  - Body: `{ "reason": "key_compromise" }`
- `GET /v1/sites/{id}/ssl/status` — shows certificate status (valid, expiry date, last_attempt, failure_reason)

### 4.5 Admin / management
- `GET /v1/ops/logs` — tail or search hubfly logs (with pagination)
- `POST /v1/ops/nginx/test` — tests current nginx configuration and returns detailed output
- `POST /v1/ops/nginx/reload` — force reload (rarely needed, hubfly will do it automatically)


## 5. API behavior details
- **Validation-first**: All updates render into `staging/` then run `nginx -t` (pointing to nginx.conf that includes `staging/` path or using a temporary `-c` file). If `nginx -t` fails, hubfly returns `400` and includes the `nginx -t` stderr output.
- **Atomic switch**: When successful, move staging file(s) into `sites/` atomically (rename) and run `nginx -s reload`.
- **Rollback**: hubfly keeps a 5-deep history of previous config files to rollback if reload fails after an apply.
- **Idempotent create**: `POST /v1/sites` with same `id` updates existing resource.
- **Operations**: long-running tasks (certificate issuance) return an operation id and are executed asynchronously in the app process with status polling available at `/v1/ops/{opid}`.


## 6. Certificate lifecycle & retries

### Strategy
- Use **HTTP-01** by default with webroot pointing at `/var/www/hubfly/.well-known/acme-challenge/` because it is straightforward inside a single-container setup.
- If the domain uses DNS-only or HTTP fails, support **DNS-01** via provider plugins (optional) — the user must supply provider credentials.

### Workflows
1. Receive `issue` request. Render nginx config so challenge requests are served from webroot (temporary server block added if needed).
2. Ensure port 80 is listening on host and reachable (document requirement: container must be able to bind 80/443 and ports forwarded on host).
3. Run `certbot certonly --webroot -w /var/www/hubfly -d example.com -m admin@... --agree-tos --non-interactive`.
4. On failure due to rate-limits, return clear error and advise on `staging=true` to bypass production limits for testing.
5. On transient failures (timeouts, connection refused), retry with exponential backoff up to configurable N attempts (default 5) and jitter.
6. On success, update site file to enable `listen 443 ssl;` and include cert paths from `/etc/letsencrypt/live/example.com/fullchain.pem` and `privkey.pem`.

### Revocation
- `POST /v1/sites/{id}/ssl/revoke` will call `certbot revoke --cert-path /etc/letsencrypt/live/example.com/cert.pem --reason unspecified` (or use ACME APIs). After revocation, hubfly removes SSL blocks from the site config and triggers reload. It will also optionally delete cert files.


## 7. Template system & common templates
Templates are stored as parametrized NGINX snippets. Each template is a small text file with variables inserted (golang `text/template` or equivalent). Example templates:

### 7.1 `basic-caching`
```
proxy_cache_path /var/cache/nginx/{{ .site_id }} levels=1:2 keys_zone={{ .site_id }}:10m max_size=1g inactive=60m;

server {
  location / {
    proxy_cache {{ .site_id }};
    proxy_cache_key "$scheme$request_method$host$request_uri";
    proxy_cache_valid 200 302 10m;
    proxy_cache_valid 404 1m;
  }
}
```

### 7.2 `security-headers`
```
add_header X-Frame-Options "SAMEORIGIN" always;
add_header X-Content-Type-Options "nosniff" always;
add_header Referrer-Policy "same-origin" always;
add_header Strict-Transport-Security "max-age=31536000; includeSubDomains; preload" always;
```

### 7.3 `gzip-and-compression`
```
gzip on;
gzip_types text/plain text/css application/json application/javascript text/xml application/xml application/xml+rss text/javascript;
```

The API can merge templates into the generated site file. Users may pass `extra_config` to inject raw NGINX snippets (with caution). Hubfly will sanitize/limit the size of `extra_config`.


## 8. NGINX config generation rules (examples)
A generated server block (simplified):

```
server {
  listen 80;
  server_name example.com www.example.com;

  # Redirect to https if http_to_https true
  if ($http_x_forwarded_proto != "https") {
    return 301 https://$host$request_uri;
  }

  location /.well-known/acme-challenge/ {
    root /var/www/hubfly;
    try_files $uri =404;
  }

  location / {
    proxy_pass http://10.0.0.5:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # included template snippets such as caching, security headers
  }
}

# SSL server (added after cert issuance)
server {
  listen 443 ssl http2;
  server_name example.com www.example.com;
  ssl_certificate /etc/letsencrypt/live/example.com/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/example.com/privkey.pem;
  include /etc/hubfly/templates/ssl-hardening.conf; # e.g. ciphers, protocols
  ...
}
```


## 9. Docker image & deployment
**Multi-stage Dockerfile (outline)**

1. Build stage: compile Go binary
```Dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/hubfly ./cmd/hubfly
```

2. Runtime: base on nginx:stable-alpine and install certbot
```Dockerfile
FROM nginx:stable-alpine
RUN apk add --no-cache certbot openssl bash sqlite
COPY --from=builder /out/hubfly /usr/local/bin/hubfly
COPY ./nginx/nginx.conf /etc/nginx/nginx.conf
# create dirs
RUN mkdir -p /etc/hubfly/sites /etc/hubfly/staging /var/www/hubfly /var/log/hubfly /var/cache/nginx
VOLUME ["/etc/letsencrypt","/var/www/hubfly","/etc/hubfly/sites"]
EXPOSE 80 443
CMD ["/usr/local/bin/hubfly", "--config", "/etc/hubfly/config.yaml"]
```

Notes:
- You may prefer separate containers (nginx + hubfly + certbot) for security and upgrade isolation; the single-image approach trades separation for convenience. Consider Docker Compose mode where hubfly talks to nginx via shared filesystem (the design still fits).
- Running certbot inside the same container simplifies webroot access but requires the container to be given the ability to bind 80/443 on the host or have host forward ports.


## 10. Concurrency, locking and race conditions
- **File locks**: Use file-level locking when writing site files and when certbot processes run to avoid concurrent cert issuance for same domain.
- **Operation queue**: Use a small in-process worker queue for long-running ops (cert issuance, revocation). Record operation state in metadata DB.
- **Retries & idempotency**: Re-try cert issuance on transient errors only; rate-limit retries to avoid hitting Let's Encrypt limits.


## 11. Security considerations
- Run the Go binary as a non-root user where possible, but certbot/NGINX require binding to 80/443 — either run as root in container with careful separation or advise to run with host ports forwarded.
- Protect the REST API with authentication (JWT or mTLS). Default: enable an admin token and require it in `Authorization: Bearer ...` header.
- Validate/limit `extra_config` to avoid injection of destructive directives.
- Rate-limit API endpoints to avoid mass issuance attempts.
- Keep `/etc/letsencrypt` persisted and backed up. Watch let's encrypt rate limits; include a `staging` flag to use LE staging environment for tests.


## 12. Observability & logging
- API logs: structured JSON logs rotated to `/var/log/hubfly/hubfly.log`.
- NGINX access & error logs remain in `/var/log/nginx/` but you can add per-site access logs under `/var/log/hubfly/sites/<id>.access.log`.
- Expose Prometheus metrics endpoint `/metrics` with metrics like `hubfly_nginx_reload_total`, `hubfly_cert_issuance_attempt_total`, `hubfly_site_count`.
- Expose `GET /v1/ops/{opid}/log` for viewing long-running operation logs.


## 13. Rate-limits & safety with Let's Encrypt
- Respect LE rate limits: keep retries conservative and expose `staging=true` option for testing.
- Provide clear error messages about `too many certificates` or `rate limited` and include the Let's Encrypt docs link in logs (user-readable explanation).


## 14. Example API flows

### Create site + issue cert (simple)
1. `POST /v1/sites` with `domain=example.com`, `upstreams=["10.0.0.5:8080"]`, `ssl=true`, `force_ssl=true`.
2. Server renders config to staging and tests.
3. Server applies config (listen 80) and reloads.
4. Server runs certbot with webroot; on success, updates site to add SSL block and reloads.

### Enable caching on existing domain (patch)
1. `PATCH /v1/sites/{id}` with `{ "templates": { "add": ["basic-caching"] } }`.
2. Server re-renders, tests, applies.

### Remove site + revoke cert
1. `DELETE /v1/sites/{id}?revoke_cert=true`
2. Server calls certbot revoke and deletes site files, reloads nginx.


## 15. Edge cases & operational notes
- If `nginx -s reload` fails (rare), keep previous config and attempt automatic rollback. Notify via operation status and logs.
- If the host blocks port 80 externally, HTTP-01 will fail. Document requirement that port 80 be reachable for issuance unless DNS-01 is used.
- If multiple domains share a certificate (SAN), generate cert for all names in single cert issuance operation.
- If user wants wildcard certs, require DNS-01 and plugin support.


## 16. Implementation roadmap (suggested phases)
**Phase 0 (MVP)**
- Basic Go API: create/list/delete sites
- Render static server block + `proxy_pass`
- `nginx -t` + `nginx -s reload` flow (no SSL)

**Phase 1**
- Integrate Certbot with HTTP-01 (webroot)
- Add SSL issue, status, revoke endpoints
- Add operation queue & status endpoint

**Phase 2**
- Template engine: caching, security headers, gzip
- Partial updates/PATCH endpoints
- Rollback history
- Metrics `/metrics`

**Phase 3**
- DNS-01 provider integrations
- Advanced templates (rate-limiting, WAF rules)
- UI/dashboard
- Multi-node coordination (etcd/consul) for HA


## 17. Sample NGINX snippet templates (put under `/etc/hubfly/templates/`)
- `ssl-hardening.conf`
```
ssl_protocols TLSv1.2 TLSv1.3;
ssl_prefer_server_ciphers on;
ssl_ciphers "EECDH+AESGCM:EDH+AESGCM";
ssl_session_cache shared:SSL:10m;
ssl_session_timeout 10m;
```

- `proxy-headers.conf`
```
proxy_set_header Host $host;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
```


## 18. Example: sample `POST /v1/sites` request
```
POST /v1/sites
Content-Type: application/json
Authorization: Bearer <token>

{
  "id": "example-com",
  "domain": "example.com",
  "upstreams": ["10.10.0.4:3000"],
  "ssl": true,
  "force_ssl": true,
  "templates": ["proxy-headers","basic-caching"],
  "email": "admin@example.com"
}
```

Response (201):
```
{
  "id": "example-com",
  "domain": "example.com",
  "status": "provisioning",
  "message": "site applied; certificate issuance in progress (operation id 7)"
}
```


## 19. Testing & CI
- Unit tests for template rendering and config generation.
- Integration tests that spin up container with exposed 80/443 and uses Let's Encrypt staging to issue certs.
- Use `nginx -t` checks in tests and capture stdout/stderr.
- E2E tests for API idempotency and rollback scenarios.


## 20. Notes and tradeoffs
- Combining NGINX + Certbot + API in one image is convenient but increases blast radius; consider splitting into `hubfly-api` + `nginx` container with a shared volume for configs as an alternative when scaling or applying security hardening.
- Let's Encrypt rate limits are a real operational constraint; provide clear UX about `staging` vs `production`.
- To support high-scale dynamic routing (lots of small ephemeral hostnames), consider using a dynamic upstream manager and `lua` or `nginx-plus` features; but that increases complexity and licensing.


---

If you want, I can:
- generate a `docker-compose.yml` and detailed `Dockerfile` for the single-image approach,
- provide a Go project scaffold (main.go, handlers, template renderer, sqlite metadata),
- create example NGINX templates and a sample `config.yaml` for hubfly.

Tell me which of those to add next and I will put it into the document.
