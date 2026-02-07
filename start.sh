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

ensure_fallback_cert
rewrite_missing_ssl_paths

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
