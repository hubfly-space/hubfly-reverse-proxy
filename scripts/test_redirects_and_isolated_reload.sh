#!/usr/bin/env bash
set -euo pipefail

API_BASE="${API_BASE:-http://localhost:10003}"
DOMAIN_SUFFIX="${DOMAIN_SUFFIX:-test.local}"
UPSTREAM_HOST="${UPSTREAM_HOST:-127.0.0.1}"
POLL_SECONDS="${POLL_SECONDS:-30}"
POLL_INTERVAL="${POLL_INTERVAL:-1}"
WORKDIR="$(mktemp -d)"

SERVER_PIDS=()

cleanup() {
  for pid in "${SERVER_PIDS[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

require_cmd curl
require_cmd python3

preflight_live_apply() {
  local probe_domain="preflight-$(date +%s).$DOMAIN_SUFFIX"
  local payload
  payload="$(post_site "$probe_domain" 19081 || true)"
  sleep 2
  local site_payload
  site_payload="$(http_json GET "/v1/sites/$probe_domain" || true)"
  local deploy_error
  deploy_error="$(printf '%s' "$site_payload" | json_get "deploy_error" || true)"
  http_json DELETE "/v1/sites/$probe_domain" >/dev/null 2>&1 || true
  if [[ "$deploy_error" == *"bind() to 0.0.0.0:80 failed"* ]] || [[ "$deploy_error" == *"could not open error log file"* ]]; then
    cat >&2 <<EOF
preflight failed: the Hubfly instance at $API_BASE cannot perform live nginx apply/reload from this context.
The validator passed, but the real apply step is running against privileged ports/log paths without sufficient privileges.

Run this script against the actual root-capable Hubfly node/service, not an unprivileged local dev process.
EOF
    exit 1
  fi
}

json_get() {
  local expr="$1"
  local payload
  payload="$(cat)"
  python3 - "$expr" "$payload" <<'PY'
import json, sys
expr = sys.argv[1]
data = json.loads(sys.argv[2])
value = data
for part in expr.split("."):
    if part == "":
        continue
    if isinstance(value, list):
        value = value[int(part)]
    else:
        value = value.get(part)
if isinstance(value, bool):
    print("true" if value else "false")
elif value is None:
    print("")
else:
    print(value)
PY
}

http_json() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -fsS -X "$method" "$API_BASE$path" -H 'Content-Type: application/json' -d "$body"
  else
    curl -fsS -X "$method" "$API_BASE$path"
  fi
}

wait_for_field() {
  local path="$1"
  local field="$2"
  local expected="$3"
  local started
  started="$(date +%s)"
  while true; do
    local payload
    payload="$(http_json GET "$path")"
    local current
    current="$(printf '%s' "$payload" | json_get "$field")"
    if [[ "$current" == "$expected" ]]; then
      printf '%s\n' "$payload"
      return 0
    fi
    if (( "$(date +%s)" - started >= POLL_SECONDS )); then
      echo "timed out waiting for $path field $field=$expected; last payload: $payload" >&2
      return 1
    fi
    sleep "$POLL_INTERVAL"
  done
}

assert_field() {
  local payload="$1"
  local field="$2"
  local expected="$3"
  local current
  current="$(printf '%s' "$payload" | json_get "$field")"
  if [[ "$current" != "$expected" ]]; then
    echo "assertion failed for field $field: expected $expected got $current; payload: $payload" >&2
    exit 1
  fi
}

start_upstream() {
  local name="$1"
  local port="$2"
  local dir="$WORKDIR/$name"
  mkdir -p "$dir"
  printf '%s\n' "$name" > "$dir/index.html"
  python3 -m http.server "$port" --bind "$UPSTREAM_HOST" --directory "$dir" >/dev/null 2>&1 &
  SERVER_PIDS+=("$!")
}

post_site() {
  local domain="$1"
  local port="$2"
  http_json POST /v1/sites "$(cat <<JSON
{"id":"$domain","domain":"$domain","upstreams":["$UPSTREAM_HOST:$port"],"force_ssl":false,"ssl":false,"templates":[]}
JSON
)"
}

patch_site() {
  local domain="$1"
  local body="$2"
  http_json PATCH "/v1/sites/$domain" "$body"
}

post_redirect() {
  local source_domain="$1"
  local target_domain="$2"
  http_json POST /v1/redirects "$(cat <<JSON
{"id":"$source_domain","source_domain":"$source_domain","target_domain":"$target_domain","ssl":false}
JSON
)"
}

convert_site_to_redirect() {
  local site_id="$1"
  local target_domain="$2"
  http_json POST "/v1/sites/$site_id/convert-to-redirect" "$(cat <<JSON
{"target_domain":"$target_domain","ssl":false}
JSON
)"
}

stamp="$(date +%s)"

WWW_CANONICAL="www-$stamp.$DOMAIN_SUFFIX"
APEX_REDIRECT="$stamp.$DOMAIN_SUFFIX"
NEW_DOMAIN="new-$stamp.$DOMAIN_SUFFIX"
OLD_DOMAIN="old-$stamp.$DOMAIN_SUFFIX"
SITE_A="site-a-$stamp.$DOMAIN_SUFFIX"
SITE_B="site-b-$stamp.$DOMAIN_SUFFIX"
SITE_C="site-c-$stamp.$DOMAIN_SUFFIX"
SITE_D="site-d-$stamp.$DOMAIN_SUFFIX"
SITE_E="site-e-$stamp.$DOMAIN_SUFFIX"
SITE_F="site-f-$stamp.$DOMAIN_SUFFIX"

start_upstream "www" 19081
start_upstream "new" 19082
start_upstream "a" 19083
start_upstream "b" 19084
start_upstream "c" 19085
start_upstream "d" 19086
start_upstream "e" 19087
start_upstream "f" 19088

preflight_live_apply

echo "creating canonical site for www redirect scenario"
post_site "$WWW_CANONICAL" 19081 >/dev/null
wait_for_field "/v1/sites/$WWW_CANONICAL" "status" "active" >/dev/null

echo "creating redirect resource: apex -> www"
post_redirect "$APEX_REDIRECT" "$WWW_CANONICAL" >/dev/null
redirect_payload="$(wait_for_field "/v1/redirects/$APEX_REDIRECT" "status" "active")"
assert_field "$redirect_payload" "target_domain" "$WWW_CANONICAL"

echo "creating old and new sites for domain migration scenario"
post_site "$NEW_DOMAIN" 19082 >/dev/null
wait_for_field "/v1/sites/$NEW_DOMAIN" "status" "active" >/dev/null
post_site "$OLD_DOMAIN" 19082 >/dev/null
wait_for_field "/v1/sites/$OLD_DOMAIN" "status" "active" >/dev/null

echo "converting old site into redirect -> new domain"
convert_site_to_redirect "$OLD_DOMAIN" "$NEW_DOMAIN" >/dev/null
redirect_payload="$(wait_for_field "/v1/redirects/$OLD_DOMAIN" "status" "active")"
assert_field "$redirect_payload" "target_domain" "$NEW_DOMAIN"
if http_json GET "/v1/sites/$OLD_DOMAIN" >/dev/null 2>&1; then
  echo "expected converted site $OLD_DOMAIN to be removed from /v1/sites" >&2
  exit 1
fi

echo "creating three normal sites"
post_site "$SITE_A" 19083 >/dev/null
wait_for_field "/v1/sites/$SITE_A" "status" "active" >/dev/null
post_site "$SITE_B" 19084 >/dev/null
wait_for_field "/v1/sites/$SITE_B" "status" "active" >/dev/null
post_site "$SITE_C" 19085 >/dev/null
wait_for_field "/v1/sites/$SITE_C" "status" "active" >/dev/null

echo "breaking one site config intentionally"
patch_site "$SITE_B" '{"extra_config":"this_directive_does_not_exist on;"}' >/dev/null
site_b_payload="$(wait_for_field "/v1/sites/$SITE_B" "deploy_status" "invalid")"
assert_field "$site_b_payload" "status" "active"

echo "creating a fourth valid site after one site became invalid"
post_site "$SITE_D" 19086 >/dev/null
site_d_payload="$(wait_for_field "/v1/sites/$SITE_D" "status" "active")"
assert_field "$site_d_payload" "deploy_status" "active"

echo "creating a fifth site and breaking it intentionally"
post_site "$SITE_E" 19087 >/dev/null
wait_for_field "/v1/sites/$SITE_E" "status" "active" >/dev/null
patch_site "$SITE_E" '{"extra_config":"this_directive_does_not_exist on;"}' >/dev/null
site_e_payload="$(wait_for_field "/v1/sites/$SITE_E" "deploy_status" "invalid")"
assert_field "$site_e_payload" "status" "active"

echo "creating a sixth valid site after two sites became invalid"
post_site "$SITE_F" 19088 >/dev/null
site_f_payload="$(wait_for_field "/v1/sites/$SITE_F" "status" "active")"
assert_field "$site_f_payload" "deploy_status" "active"

echo "re-checking the fourth valid site is still healthy"
site_d_recheck="$(http_json GET "/v1/sites/$SITE_D")"
assert_field "$site_d_recheck" "status" "active"
assert_field "$site_d_recheck" "deploy_status" "active"

echo
echo "success"
echo "redirect resource scenario: $APEX_REDIRECT -> $WWW_CANONICAL"
echo "convert existing site scenario: $OLD_DOMAIN -> $NEW_DOMAIN"
echo "isolated reload scenario:"
echo "  invalid sites: $SITE_B, $SITE_E"
echo "  active sites after failures: $SITE_D, $SITE_F"
