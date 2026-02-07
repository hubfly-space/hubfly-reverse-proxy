package nginx

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

type Manager struct {
	SitesDir     string
	StreamsDir   string
	StagingDir   string
	TemplatesDir string
	NginxConf    string // Path to main nginx.conf
	FallbackCert string
	FallbackKey  string
}

func NewManager(baseDir string) *Manager {
	return &Manager{
		SitesDir:     filepath.Join(baseDir, "sites"),
		StreamsDir:   filepath.Join(baseDir, "streams"),
		StagingDir:   filepath.Join(baseDir, "staging"),
		TemplatesDir: filepath.Join(baseDir, "templates"),
		NginxConf:    "/etc/nginx/nginx.conf",
		FallbackCert: filepath.Join(baseDir, "default-certs", "fallback.crt"),
		FallbackKey:  filepath.Join(baseDir, "default-certs", "fallback.key"),
	}
}

// EnsureDirs creates necessary directories
func (m *Manager) EnsureDirs() error {
	dirs := []string{
		m.SitesDir,
		m.StreamsDir,
		m.StagingDir,
		m.TemplatesDir,
		filepath.Dir(m.FallbackCert),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (m *Manager) realCertPaths(domain string) (string, string) {
	base := filepath.Join("/etc/letsencrypt/live", domain)
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "privkey.pem")
}

func (m *Manager) resolveCertPaths(domain string) (string, string, bool) {
	certPath, keyPath := m.realCertPaths(domain)
	if fileExists(certPath) && fileExists(keyPath) {
		return certPath, keyPath, false
	}
	return m.FallbackCert, m.FallbackKey, true
}

func (m *Manager) HasDomainCertificate(domain string) bool {
	certPath, keyPath := m.realCertPaths(domain)
	return fileExists(certPath) && fileExists(keyPath)
}

// GenerateConfig renders the site config to a staging file.
func (m *Manager) GenerateConfig(site *models.Site) (string, error) {
	// Load templates
	var templateContent strings.Builder
	for _, tplName := range site.Templates {
		content, err := os.ReadFile(filepath.Join(m.TemplatesDir, tplName+".conf"))
		if err != nil {
			// For MVP, we might log warning but here we fail
			// If template not found, maybe ignore? stricter is better.
			return "", fmt.Errorf("failed to load template %s: %w", tplName, err)
		}
		templateContent.Write(content)
		templateContent.WriteString("\n")
	}

	certPath, keyPath, usingFallbackCert := m.resolveCertPaths(site.Domain)
	effectiveForceSSL := site.ForceSSL
	if site.SSL && usingFallbackCert {
		// Avoid redirecting all HTTP requests to a certificate mismatch while fallback is active.
		effectiveForceSSL = false
	}

	// Wrapper for template data
	data := struct {
		*models.Site
		TemplateSnippets  string
		SSLCertificate    string
		SSLKey            string
		EffectiveForceSSL bool
		UsingFallbackCert bool
	}{
		Site:              site,
		TemplateSnippets:  templateContent.String(),
		SSLCertificate:    certPath,
		SSLKey:            keyPath,
		EffectiveForceSSL: effectiveForceSSL,
		UsingFallbackCert: usingFallbackCert,
	}

	// Basic server block template
	// In a real app, this might be loaded from a file.
	const serverTmpl = `
{{ if .Firewall }}
{{ if .Firewall.RateLimit }}
{{ if .Firewall.RateLimit.Enabled }}
limit_req_zone $binary_remote_addr zone=zone_{{ .ID }}:10m rate={{ .Firewall.RateLimit.Rate }}{{ .Firewall.RateLimit.Unit }};
{{ end }}
{{ end }}
{{ end }}

server {
    listen 80;
    server_name {{ .Domain }};

    access_log /var/log/hubfly/{{ .ID }}.access.log hubfly;
    error_log /var/log/hubfly/{{ .ID }}.error.log notice;

    {{ if .Firewall }}
    {{ if .Firewall.BlockRules }}
    {{ range .Firewall.BlockRules.Paths }}
    location ~ {{ . }} { return 403; }
    {{ end }}
    {{ range $path, $methods := .Firewall.BlockRules.PathMethods }}
    location ~ {{ $path }} {
        if ($request_method ~* "({{ join $methods "|" }})") { return 405; }
        # Fallback to main proxy pass if method allowed? 
        # Note: 'location' blocks capture request. We need to proxy_pass here too if not blocked.
        # But duplication is messy. 
        # Better strategy: strict match location with limit_except or if.
        # If we use location ~ $path, it takes precedence.
        # So we must include proxy logic inside.
        set $upstream_endpoint "http://{{ index $.Upstreams 0 }}";
        proxy_pass $upstream_endpoint;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
        {{ range $k, $v := $.ProxySetHeaders }}
        proxy_set_header {{ $k }} {{ $v }};
        {{ end }}
    }
    {{ end }}
    {{ end }}
    {{ end }}

    {{ if .EffectiveForceSSL }}
    location / {
        return 301 https://$host$request_uri;
    }
    location /ws/ {
        set $upstream_endpoint "http://{{ index .Upstreams 0 }}";
        proxy_pass $upstream_endpoint;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
    }

    {{ else }}
    location / {
        set $upstream_endpoint "http://{{ index .Upstreams 0 }}";

        {{ if .Firewall }}
        {{ range .Firewall.IPRules }}
        {{ .Action }} {{ .Value }};
        {{ end }}
        
        {{ if .Firewall.BlockRules }}
        {{ if .Firewall.BlockRules.UserAgents }}
        if ($http_user_agent ~* "({{ join .Firewall.BlockRules.UserAgents "|" }})") { return 403; }
        {{ end }}
        {{ if .Firewall.BlockRules.Methods }}
        if ($request_method ~* "({{ join .Firewall.BlockRules.Methods "|" }})") { return 405; }
        {{ end }}
        {{ end }}

        {{ if .Firewall.RateLimit }}
        {{ if .Firewall.RateLimit.Enabled }}
        limit_req zone=zone_{{ .ID }} burst={{ .Firewall.RateLimit.Burst }} nodelay;
        {{ end }}
        {{ end }}
        {{ end }}

        proxy_pass $upstream_endpoint;

        # WebSocket Support
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;

        {{ range $k, $v := .ProxySetHeaders }}
        proxy_set_header {{ $k }} {{ $v }};
        {{ end }}

        {{ .TemplateSnippets }}
        {{ .ExtraConfig }}
    }
    {{ end }}

    # Challenge path for Certbot
    location /.well-known/acme-challenge/ {
        root /var/www/hubfly;
        try_files $uri =404;
    }

    error_page 403 /403.html;
    location = /403.html {
        root /var/www/hubfly/static;
        internal;
    }

    error_page 502 504 /502.html;
    location = /502.html {
        root /var/www/hubfly/static;
        internal;
    }
}

{{ if .SSL }}
server {
    listen 443 ssl;
    http2 on;
    server_name {{ .Domain }};

    ssl_certificate {{ .SSLCertificate }};
    ssl_certificate_key {{ .SSLKey }};
    {{ if .UsingFallbackCert }}
    # Fallback certificate is active until the domain certificate is issued.
    {{ end }}

    {{ if .Firewall }}
    {{ if .Firewall.BlockRules }}
    {{ range .Firewall.BlockRules.Paths }}
    location ~ {{ . }} { return 403; }
    {{ end }}
    {{ range $path, $methods := .Firewall.BlockRules.PathMethods }}
    location ~ {{ $path }} {
        if ($request_method ~* "({{ join $methods "|" }})") { return 405; }
        set $upstream_endpoint "http://{{ index $.Upstreams 0 }}";
        proxy_pass $upstream_endpoint;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
        {{ range $k, $v := $.ProxySetHeaders }}
        proxy_set_header {{ $k }} {{ $v }};
        {{ end }}
    }
    {{ end }}
    {{ end }}
    {{ end }}

    location / {
        set $upstream_endpoint "http://{{ index .Upstreams 0 }}";

        {{ if .Firewall }}
        {{ range .Firewall.IPRules }}
        {{ .Action }} {{ .Value }};
        {{ end }}

        {{ if .Firewall.BlockRules }}
        {{ if .Firewall.BlockRules.UserAgents }}
        if ($http_user_agent ~* "({{ join .Firewall.BlockRules.UserAgents "|" }})") { return 403; }
        {{ end }}
        {{ if .Firewall.BlockRules.Methods }}
        if ($request_method ~* "({{ join .Firewall.BlockRules.Methods "|" }})") { return 405; }
        {{ end }}
        {{ end }}

        {{ if .Firewall.RateLimit }}
        {{ if .Firewall.RateLimit.Enabled }}
        limit_req zone=zone_{{ .ID }} burst={{ .Firewall.RateLimit.Burst }} nodelay;
        {{ end }}
        {{ end }}
        {{ end }}

        proxy_pass $upstream_endpoint;

        # WebSocket Support
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;

        {{ range $k, $v := .ProxySetHeaders }}
        proxy_set_header {{ $k }} {{ $v }};
        {{ end }}

        {{ .TemplateSnippets }}
        {{ .ExtraConfig }}
    }

    location /ws/ {
        set $upstream_endpoint "http://{{ index .Upstreams 0 }}";
        proxy_pass $upstream_endpoint;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
    }

    error_page 403 /403.html;
    location = /403.html {
        root /var/www/hubfly/static;
        internal;
    }

    error_page 502 504 /502.html;
    location = /502.html {
        root /var/www/hubfly/static;
        internal;
    }
}
{{ end }}
`
	// Note: This is a simplified template for MVP.
	// Real implementation should load from m.TemplatesDir and handle "Templates" list (caching, etc).

	funcMap := template.FuncMap{
		"join": strings.Join,
	}

	t, err := template.New("site").Funcs(funcMap).Parse(serverTmpl)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}

	stagingFile := filepath.Join(m.StagingDir, site.ID+".conf")
	if err := os.WriteFile(stagingFile, buf.Bytes(), 0644); err != nil {
		return "", err
	}
	slog.Debug("Generated staging config", "file", stagingFile)

	return stagingFile, nil
}

// RebuildStreamConfig generates the config for a specific port, handling multiple SNI streams.
func (m *Manager) RebuildStreamConfig(port int, streams []models.Stream) error {
	if len(streams) == 0 {
		return m.DeleteStreamConfig(port)
	}

	// Check if we need SNI routing
	// If multiple streams, or the single stream has a domain, we use SNI.
	// Exception: UDP cannot use ssl_preread (DTLS is complex, assume TCP for SNI).
	useSNI := false
	if len(streams) > 1 {
		useSNI = true
	} else if streams[0].Domain != "" {
		useSNI = true
	}

	var buf bytes.Buffer

	// Simple Pass-through (No SNI, Single Stream)
	if !useSNI {
		s := streams[0]
		proto := ""
		if s.Protocol == "udp" {
			proto = " udp"
		}
		mapName := fmt.Sprintf("stream_simple_map_%d", s.ListenPort)

		// Plain server block with runtime-resolved upstream to avoid startup failures
		// when Docker DNS name is temporarily unavailable.
		tmpl := `
map $remote_addr ${{ .MapName }} {
    default {{ .Upstream }};
}

server {
    listen {{ .ListenPort }}{{ .Proto }};
    listen [::]:{{ .ListenPort }}{{ .Proto }};
    proxy_pass ${{ .MapName }};
}
`
		data := struct {
			ListenPort int
			Proto      string
			Upstream   string
			MapName    string
		}{
			ListenPort: s.ListenPort,
			Proto:      proto,
			Upstream:   s.Upstream,
			MapName:    mapName,
		}

		t, _ := template.New("simple_stream").Parse(tmpl)
		if err := t.Execute(&buf, data); err != nil {
			return err
		}
	} else {
		// SNI Routing (TCP only usually)
		// 1. Map block
		// 2. Server block with ssl_preread

		// Map name needs to be unique per port
		mapName := fmt.Sprintf("stream_map_%d", port)

		buf.WriteString(fmt.Sprintf("map $ssl_preread_server_name $%s {\n", mapName))
		for _, s := range streams {
			if s.Domain != "" {
				buf.WriteString(fmt.Sprintf("    %s %s;\n", s.Domain, s.Upstream))
			} else {
				// Default/Catch-all if one is missing domain?
				// Or explicit default. For now, let's map "." (if supported) or use default clause
			}
		}
		// If there's a stream with empty domain, make it default?
		var defaultStream *models.Stream
		for _, s := range streams {
			if s.Domain == "" {
				defaultStream = &s
				break
			}
		}
		if defaultStream != nil {
			buf.WriteString(fmt.Sprintf("    default %s;\n", defaultStream.Upstream))
		}
		buf.WriteString("}\n\n")

		buf.WriteString("server {\n")
		buf.WriteString(fmt.Sprintf("    listen %d;\n", port))
		buf.WriteString("    ssl_preread on;\n")
		buf.WriteString(fmt.Sprintf("    proxy_pass $%s;\n", mapName))
		buf.WriteString("}\n")
	}

	configFile := filepath.Join(m.StreamsDir, fmt.Sprintf("port_%d.conf", port))
	if err := os.WriteFile(configFile, buf.Bytes(), 0644); err != nil {
		return err
	}
	slog.Info("Rebuilt stream config", "port", port, "file", configFile)

	return m.Reload()
}

func (m *Manager) DeleteStreamConfig(port int) error {
	target := filepath.Join(m.StreamsDir, fmt.Sprintf("port_%d.conf", port))
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	slog.Info("Deleted stream config", "port", port, "file", target)
	return m.Reload()
}

// Validate runs nginx -t against the staging config
// Note: To validate a single include properly, we usually need to validate the whole nginx tree.
// For MVP, we assume the staging file is valid if it parses.
// A robust way is to create a temp nginx.conf that includes the staging file.
func (m *Manager) Validate(stagingFile string) error {
	if _, err := os.Stat(stagingFile); err != nil {
		return fmt.Errorf("staging config missing: %w", err)
	}
	return nil
}

func (m *Manager) runConfigTest() error {
	path, err := exec.LookPath("nginx")
	if err != nil {
		slog.Warn("Nginx not found, skipping config test")
		return nil
	}

	cmd := exec.Command(path, "-t", "-c", m.NginxConf)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Nginx config test failed", "error", err, "output", string(out))
		return fmt.Errorf("nginx config test failed: %s, output: %s", err, string(out))
	}
	slog.Debug("Nginx config test success", "output", string(out))
	return nil
}

// Apply writes staging config to live sites dir, validates full nginx tree, and reloads.
func (m *Manager) Apply(siteID, stagingFile string) error {
	target := filepath.Join(m.SitesDir, siteID+".conf")
	stagingData, err := os.ReadFile(stagingFile)
	if err != nil {
		return err
	}

	var previousData []byte
	previousExists := false
	if b, err := os.ReadFile(target); err == nil {
		previousExists = true
		previousData = b
	} else if !os.IsNotExist(err) {
		return err
	}

	restorePrevious := func() {
		if previousExists {
			_ = os.WriteFile(target, previousData, 0644)
			return
		}
		_ = os.Remove(target)
	}

	if err := os.WriteFile(target, stagingData, 0644); err != nil {
		return err
	}

	if err := m.runConfigTest(); err != nil {
		restorePrevious()
		return err
	}

	if err := m.Reload(); err != nil {
		restorePrevious()
		return err
	}

	if err := os.Remove(stagingFile); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to remove staging config", "file", stagingFile, "error", err)
	}

	slog.Info("Applied site config", "site_id", siteID, "target", target)
	return nil
}

func (m *Manager) Reload() error {
	path, err := exec.LookPath("nginx")
	if err != nil {
		slog.Warn("Nginx not found, skipping reload")
		return nil // Skip if no nginx
	}
	slog.Info("Reloading Nginx")
	cmd := exec.Command(path, "-s", "reload")
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("Nginx reload failed", "error", err, "output", string(out))
		return fmt.Errorf("nginx reload failed: %s, output: %s", err, string(out))
	}
	slog.Debug("Nginx reload success", "output", string(out))
	return nil
}

func (m *Manager) Delete(siteID string) error {
	target := filepath.Join(m.SitesDir, siteID+".conf")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	slog.Info("Deleted site config", "site_id", siteID, "file", target)
	return m.Reload()
}

// DeleteStream removed from here as we now manage by port via DeleteStreamConfig
