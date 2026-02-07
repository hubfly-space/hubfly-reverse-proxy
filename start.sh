#!/bin/bash
set -e

FALLBACK_DIR="/etc/hubfly/default-certs"
FALLBACK_CERT="${FALLBACK_DIR}/fallback.crt"
FALLBACK_KEY="${FALLBACK_DIR}/fallback.key"

ensure_fallback_cert() {
  mkdir -p "${FALLBACK_DIR}"

  if [[ -s "${FALLBACK_CERT}" && -s "${FALLBACK_KEY}" ]]; then
    return
  fi

  echo "Generating fallback TLS certificate..."
  openssl req \
    -x509 \
    -nodes \
    -newkey rsa:2048 \
    -days 3650 \
    -keyout "${FALLBACK_KEY}" \
    -out "${FALLBACK_CERT}" \
    -subj "/CN=hubfly-fallback.local" >/dev/null 2>&1
}

rewrite_missing_ssl_paths() {
  shopt -s nullglob
  for conf in /etc/hubfly/sites/*.conf; do
    cert_path=$(sed -n 's|^[[:space:]]*ssl_certificate[[:space:]]\+\([^;]\+\);$|\1|p' "${conf}" | head -n 1)
    key_path=$(sed -n 's|^[[:space:]]*ssl_certificate_key[[:space:]]\+\([^;]\+\);$|\1|p' "${conf}" | head -n 1)

    if [[ -n "${cert_path}" && ! -f "${cert_path}" ]]; then
      sed -i "s|^[[:space:]]*ssl_certificate[[:space:]]\+.*;|    ssl_certificate ${FALLBACK_CERT};|g" "${conf}"
      echo "Patched missing certificate path in ${conf}"
    fi

    if [[ -n "${key_path}" && ! -f "${key_path}" ]]; then
      sed -i "s|^[[:space:]]*ssl_certificate_key[[:space:]]\+.*;|    ssl_certificate_key ${FALLBACK_KEY};|g" "${conf}"
      echo "Patched missing key path in ${conf}"
    fi
  done
}

rewrite_legacy_stream_proxy_pass() {
  shopt -s nullglob
  for conf in /etc/hubfly/streams/port_*.conf; do
    if grep -Eq '^[[:space:]]*proxy_pass[[:space:]]+\$' "${conf}"; then
      continue
    fi

    upstream=$(sed -n 's|^[[:space:]]*proxy_pass[[:space:]]\+\([^;[:space:]]\+\);$|\1|p' "${conf}" | head -n 1)
    if [[ -z "${upstream}" ]]; then
      continue
    fi

    port=$(basename "${conf}" | sed -n 's/^port_\([0-9]\+\)\.conf$/\1/p')
    if [[ -z "${port}" ]]; then
      continue
    fi

    map_var="stream_simple_map_${port}"
    tmp_file=$(mktemp)

    awk -v map_var="${map_var}" -v upstream="${upstream}" '
      BEGIN {
        print "map $remote_addr $" map_var " {"
        print "    default " upstream ";"
        print "}"
        print ""
      }
      {
        if ($1 == "proxy_pass" && substr($2, 1, 1) != "$") {
          print "    proxy_pass $" map_var ";"
          next
        }
        print $0
      }
    ' "${conf}" > "${tmp_file}"

    mv "${tmp_file}" "${conf}"
    echo "Migrated legacy stream upstream to runtime variable in ${conf}"
  done
}

ensure_fallback_cert
rewrite_missing_ssl_paths
rewrite_legacy_stream_proxy_pass

# Start NGINX
# We rely on standard /etc/nginx/nginx.conf. 
# Ensure 'daemon on;' is used or implied so it backgrounds itself, 
# OR we run it in background if 'daemon off;' is set.
# The nginx Docker image defaults to "daemon off;" usually in its CMD, 
# but since we replaced CMD, we control it.
# Our custom nginx.conf does not specify 'daemon', so it defaults to 'on' (daemonize).
echo "Starting NGINX..."
nginx

# Ensure log file exists for GoAccess
touch /var/log/hubfly/access.log

# Start GoAccess
echo "Starting GoAccess..."
goaccess /var/log/hubfly/access.log --config-file=/etc/goaccess.conf --daemon

# Start Hubfly
echo "Starting Hubfly..."
exec /usr/local/bin/hubfly --config-dir /etc/hubfly
