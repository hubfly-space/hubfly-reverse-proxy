# Stage 1: Builder
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod ./
# COPY go.sum ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/hubfly ./cmd/hubfly

# Stage 2: Runtime
FROM nginx:stable-alpine
# Install Certbot, GoAccess and dependencies
RUN apk add --no-cache certbot openssl bash ca-certificates goaccess

# Copy binary
COPY --from=builder /out/hubfly /usr/local/bin/hubfly

# Copy default nginx config
COPY ./nginx/nginx.conf /etc/nginx/nginx.conf

# Copy goaccess config
COPY goaccess.conf /etc/goaccess.conf

# Copy startup script
COPY start.sh /start.sh
RUN chmod +x /start.sh

# Create necessary directories
RUN mkdir -p /etc/hubfly/sites /etc/hubfly/streams /etc/hubfly/staging /etc/hubfly/templates \
    /var/www/hubfly /var/log/hubfly /var/log/hubfly-go /var/cache/nginx

# Copy templates
COPY ./templates /etc/hubfly/templates
COPY ./static /var/www/hubfly/static

# Expose ports
EXPOSE 80 443 81 82 30000-30100

# Volume for persistence
VOLUME ["/etc/letsencrypt", "/etc/hubfly", "/var/www/hubfly", "/var/log/hubfly-go"]

# Entrypoint
CMD ["/start.sh"]
