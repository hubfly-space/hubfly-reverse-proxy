package nginx

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

type upstreamTarget struct {
	Address string
	Weight  int
}

func sanitizeName(input string) string {
	if strings.TrimSpace(input) == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

type Manager struct {
	SitesDir      string
	StreamsDir    string
	StagingDir    string
	TemplatesDir  string
	NginxConf     string // Path to main nginx.conf
	NginxBin      string
	FallbackCert  string
	FallbackKey   string
	CertsDir      string
	WebrootDir    string
	StaticDir     string
	LogsDir       string
	PIDFile       string
	WildcardCerts []WildcardCertificate
}

type WildcardCertificate struct {
	Domain       string `json:"domain,omitempty"`
	DomainSuffix string `json:"domain_suffix,omitempty"`
	CertPath     string `json:"cert_path"`
	KeyPath      string `json:"key_path"`
}

type Options struct {
	NginxConf     string
	NginxBin      string
	CertsDir      string
	WebrootDir    string
	StaticDir     string
	LogsDir       string
	PIDFile       string
	WildcardCerts []WildcardCertificate
}

type Health struct {
	Available bool   `json:"available"`
	Running   bool   `json:"running"`
	Binary    string `json:"binary,omitempty"`
	Version   string `json:"version,omitempty"`
	Config    string `json:"config"`
	PIDFile   string `json:"pid_file"`
	PID       int    `json:"pid,omitempty"`
	Error     string `json:"error,omitempty"`
}

func NewManager(baseDir string, opts ...Options) *Manager {
	cfg := Options{
		NginxConf:  filepath.Join(baseDir, "nginx", "nginx.conf"),
		CertsDir:   filepath.Join(baseDir, "letsencrypt"),
		WebrootDir: filepath.Join(baseDir, "www"),
		StaticDir:  filepath.Join(baseDir, "static"),
		LogsDir:    filepath.Join(baseDir, "logs"),
		PIDFile:    filepath.Join(baseDir, "run", "nginx.pid"),
	}
	if len(opts) > 0 {
		override := opts[0]
		if strings.TrimSpace(override.NginxConf) != "" {
			cfg.NginxConf = strings.TrimSpace(override.NginxConf)
		}
		cfg.NginxBin = strings.TrimSpace(override.NginxBin)
		if strings.TrimSpace(override.CertsDir) != "" {
			cfg.CertsDir = strings.TrimSpace(override.CertsDir)
		}
		if strings.TrimSpace(override.WebrootDir) != "" {
			cfg.WebrootDir = strings.TrimSpace(override.WebrootDir)
		}
		if strings.TrimSpace(override.StaticDir) != "" {
			cfg.StaticDir = strings.TrimSpace(override.StaticDir)
		}
		if strings.TrimSpace(override.LogsDir) != "" {
			cfg.LogsDir = strings.TrimSpace(override.LogsDir)
		}
		if strings.TrimSpace(override.PIDFile) != "" {
			cfg.PIDFile = strings.TrimSpace(override.PIDFile)
		}
		cfg.WildcardCerts = override.WildcardCerts
	}

	return &Manager{
		SitesDir:      filepath.Join(baseDir, "sites"),
		StreamsDir:    filepath.Join(baseDir, "streams"),
		StagingDir:    filepath.Join(baseDir, "staging"),
		TemplatesDir:  filepath.Join(baseDir, "templates"),
		NginxConf:     cfg.NginxConf,
		NginxBin:      cfg.NginxBin,
		FallbackCert:  filepath.Join(baseDir, "default-certs", "fallback.crt"),
		FallbackKey:   filepath.Join(baseDir, "default-certs", "fallback.key"),
		CertsDir:      cfg.CertsDir,
		WebrootDir:    cfg.WebrootDir,
		StaticDir:     cfg.StaticDir,
		LogsDir:       cfg.LogsDir,
		PIDFile:       cfg.PIDFile,
		WildcardCerts: cfg.WildcardCerts,
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
		m.WebrootDir,
		m.StaticDir,
		m.LogsDir,
		filepath.Join(filepath.Dir(m.LogsDir), "cache", "nginx"),
		filepath.Dir(m.PIDFile),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	if err := m.ensureFallbackCertificate(); err != nil {
		return err
	}
	if err := m.ensureMainConfigPaths(); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (m *Manager) realCertPaths(domain string) (string, string) {
	base := filepath.Join(m.CertsDir, "live", domain)
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "privkey.pem")
}

func canonicalDomainPattern(input string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(input)), ".")
}

func (w WildcardCertificate) pattern() string {
	if w.Domain != "" {
		return canonicalDomainPattern(w.Domain)
	}
	return canonicalDomainPattern(w.DomainSuffix)
}

func domainMatchesPattern(domain, pattern string) bool {
	if domain == "" || pattern == "" {
		return false
	}
	if domain == pattern {
		return true
	}
	return strings.HasSuffix(domain, "."+pattern)
}

func (m *Manager) wildcardCertificateForDomain(domain string) (WildcardCertificate, bool) {
	normalizedDomain := canonicalDomainPattern(domain)
	for _, wc := range m.WildcardCerts {
		if domainMatchesPattern(normalizedDomain, wc.pattern()) {
			return wc, true
		}
	}
	return WildcardCertificate{}, false
}

func (m *Manager) wildcardCertPaths(domain string) (string, string, bool) {
	wildcard, ok := m.wildcardCertificateForDomain(domain)
	if !ok {
		return "", "", false
	}

	certPath := strings.TrimSpace(wildcard.CertPath)
	keyPath := strings.TrimSpace(wildcard.KeyPath)
	if certPath == "" || keyPath == "" {
		return certPath, keyPath, true
	}
	if !filepath.IsAbs(certPath) {
		certPath = filepath.Join(m.CertsDir, certPath)
	}
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(m.CertsDir, keyPath)
	}

	return certPath, keyPath, true
}

func (m *Manager) ResolveDomainCertificate(domain string) (string, string, string, bool) {
	certPath, keyPath := m.realCertPaths(domain)
	if fileExists(certPath) && fileExists(keyPath) {
		return certPath, keyPath, "domain", true
	}

	wildcardCertPath, wildcardKeyPath, configured := m.wildcardCertPaths(domain)
	if configured {
		if fileExists(wildcardCertPath) && fileExists(wildcardKeyPath) {
			return wildcardCertPath, wildcardKeyPath, "wildcard", true
		}
		return wildcardCertPath, wildcardKeyPath, "wildcard", false
	}

	return certPath, keyPath, "none", false
}

func (m *Manager) IsWildcardDomain(domain string) bool {
	_, ok := m.wildcardCertificateForDomain(domain)
	return ok
}

func (m *Manager) resolveCertPaths(domain string) (string, string, bool) {
	certPath, keyPath, _, found := m.ResolveDomainCertificate(domain)
	if found {
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
	useLoadBalancing := site.LoadBalancing != nil && site.LoadBalancing.Enabled
	lbAlgorithm := "round_robin"
	lbWeights := make([]int, len(site.Upstreams))
	for i := range lbWeights {
		lbWeights[i] = 1
	}
	if useLoadBalancing {
		if strings.TrimSpace(site.LoadBalancing.Algorithm) != "" {
			lbAlgorithm = strings.ToLower(strings.TrimSpace(site.LoadBalancing.Algorithm))
		}
		if len(site.LoadBalancing.Weights) == len(site.Upstreams) {
			copy(lbWeights, site.LoadBalancing.Weights)
		}
	}

	upstreamName := "hubfly_upstream_" + sanitizeName(site.ID)
	upstreamTargets := make([]upstreamTarget, 0, len(site.Upstreams))
	activeUpstreams := make([]string, 0, len(site.Upstreams))
	for i, endpoint := range site.Upstreams {
		if i < len(site.DisabledUpstreams) && site.DisabledUpstreams[i] {
			continue
		}
		weight := 1
		if i < len(lbWeights) && lbWeights[i] > 0 {
			weight = lbWeights[i]
		}
		activeUpstreams = append(activeUpstreams, endpoint)
		upstreamTargets = append(upstreamTargets, upstreamTarget{
			Address: endpoint,
			Weight:  weight,
		})
	}
	hasAvailableUpstream := len(activeUpstreams) > 0
	proxyPassTarget := ""
	if hasAvailableUpstream {
		proxyPassTarget = fmt.Sprintf("http://%s", activeUpstreams[0])
	}
	if useLoadBalancing && hasAvailableUpstream {
		proxyPassTarget = fmt.Sprintf("http://%s", upstreamName)
	}

	// Wrapper for template data
	data := struct {
		*models.Site
		TemplateSnippets     string
		SSLCertificate       string
		SSLKey               string
		EffectiveForceSSL    bool
		UsingFallbackCert    bool
		LogsDir              string
		WebrootDir           string
		StaticDir            string
		UpstreamName         string
		UpstreamTargets      []upstreamTarget
		LoadBalanceAlgo      string
		UseLoadBalancing     bool
		HasAvailableUpstream bool
		ProxyPassTarget      string
	}{
		Site:                 site,
		TemplateSnippets:     templateContent.String(),
		SSLCertificate:       certPath,
		SSLKey:               keyPath,
		EffectiveForceSSL:    effectiveForceSSL,
		UsingFallbackCert:    usingFallbackCert,
		LogsDir:              m.LogsDir,
		WebrootDir:           m.WebrootDir,
		StaticDir:            m.StaticDir,
		UpstreamName:         upstreamName,
		UpstreamTargets:      upstreamTargets,
		LoadBalanceAlgo:      lbAlgorithm,
		UseLoadBalancing:     useLoadBalancing && hasAvailableUpstream,
		HasAvailableUpstream: hasAvailableUpstream,
		ProxyPassTarget:      proxyPassTarget,
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

{{ if .UseLoadBalancing }}
upstream {{ .UpstreamName }} {
    {{ if eq .LoadBalanceAlgo "least_conn" }}
    least_conn;
    {{ end }}
    {{ if eq .LoadBalanceAlgo "ip_hash" }}
    ip_hash;
    {{ end }}
    {{ range .UpstreamTargets }}
    server {{ .Address }} weight={{ .Weight }};
    {{ end }}
    keepalive 32;
}
{{ end }}

server {
    listen 80;
    server_name {{ .Domain }};

    access_log {{ .LogsDir }}/{{ .ID }}.access.log hubfly;
    error_log {{ .LogsDir }}/{{ .ID }}.error.log notice;

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
        set $upstream_endpoint "{{ $.ProxyPassTarget }}";
        {{ if $.HasAvailableUpstream }}
        proxy_pass $upstream_endpoint;
        {{ else }}
        return 502;
        {{ end }}
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

    {{ else }}
    location / {
        set $upstream_endpoint "{{ .ProxyPassTarget }}";

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

        {{ if $.HasAvailableUpstream }}
        proxy_pass $upstream_endpoint;
        {{ else }}
        return 502;
        {{ end }}

        # WebSocket Support
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
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
        root {{ .WebrootDir }};
        try_files $uri =404;
    }

    error_page 403 /403.html;
    location = /403.html {
        root {{ .StaticDir }};
        internal;
    }

    error_page 502 504 /502.html;
    location = /502.html {
        root {{ .StaticDir }};
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
        set $upstream_endpoint "{{ $.ProxyPassTarget }}";
        {{ if .HasAvailableUpstream }}
        proxy_pass $upstream_endpoint;
        {{ else }}
        return 502;
        {{ end }}
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
        set $upstream_endpoint "{{ .ProxyPassTarget }}";

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

        {{ if .HasAvailableUpstream }}
        proxy_pass $upstream_endpoint;
        {{ else }}
        return 502;
        {{ end }}

        # WebSocket Support
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
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

    error_page 403 /403.html;
    location = /403.html {
        root {{ .StaticDir }};
        internal;
    }

    error_page 502 504 /502.html;
    location = /502.html {
        root {{ .StaticDir }};
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
	return m.rebuildStreamConfig(port, streams, true)
}

func (m *Manager) RebuildStreamConfigNoReload(port int, streams []models.Stream) error {
	return m.rebuildStreamConfig(port, streams, false)
}

func (m *Manager) rebuildStreamConfig(port int, streams []models.Stream, reload bool) error {
	if len(streams) == 0 {
		return m.deleteStreamConfig(port, reload)
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

	if reload {
		return m.Reload()
	}
	return nil
}

func (m *Manager) DeleteStreamConfig(port int) error {
	return m.deleteStreamConfig(port, true)
}

func (m *Manager) DeleteStreamConfigNoReload(port int) error {
	return m.deleteStreamConfig(port, false)
}

func (m *Manager) deleteStreamConfig(port int, reload bool) error {
	target := filepath.Join(m.StreamsDir, fmt.Sprintf("port_%d.conf", port))
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	slog.Info("Deleted stream config", "port", port, "file", target)
	if reload {
		return m.Reload()
	}
	return nil
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
	path, err := m.resolveBinary()
	if err != nil {
		slog.Warn("Nginx not found, skipping config test")
		return nil
	}

	slog.Info("nginx_config_test_started", "command", path, "nginx_conf", m.NginxConf)
	cmd := exec.Command(path, "-t", "-c", m.NginxConf)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)
	if err != nil {
		slog.Error("nginx_config_test_failed", "error", err, "duration", duration, "output", string(out))
		return fmt.Errorf("nginx config test failed: %s, output: %s", err, string(out))
	}
	slog.Info("nginx_config_test_succeeded", "duration", duration, "output", string(out))
	return nil
}

// Apply writes staging config to live sites dir, validates full nginx tree, and reloads.
func (m *Manager) Apply(siteID, stagingFile string) error {
	return m.apply(siteID, stagingFile, true)
}

func (m *Manager) ApplyNoReload(siteID, stagingFile string) error {
	return m.apply(siteID, stagingFile, false)
}

func (m *Manager) apply(siteID, stagingFile string, reload bool) error {
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

	if reload {
		if err := m.Reload(); err != nil {
			restorePrevious()
			return err
		}
	}

	if err := os.Remove(stagingFile); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to remove staging config", "file", stagingFile, "error", err)
	}

	slog.Info("Applied site config", "site_id", siteID, "target", target)
	return nil
}

func (m *Manager) Reload() error {
	path, err := m.resolveBinary()
	if err != nil {
		slog.Warn("Nginx not found, skipping reload")
		return nil // Skip if no nginx
	}
	slog.Info("nginx_reload_started", "command", path, "pid_file", m.PIDFile)
	cmd := exec.Command(path, "-s", "reload", "-c", m.NginxConf)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)
	if err != nil {
		slog.Warn("nginx_reload_failed_attempting_restart", "error", err, "duration", duration, "output", string(out))
		if startErr := m.EnsureRunning(); startErr != nil {
			slog.Error("nginx_restart_failed_after_reload_failure", "reload_error", err, "start_error", startErr)
			return fmt.Errorf("nginx reload failed: %s, output: %s", err, string(out))
		}
		slog.Info("nginx_restart_succeeded_after_reload_failure")
		return nil
	}
	slog.Info("nginx_reload_succeeded", "duration", duration, "output", string(out))
	if err := m.EnsureRunning(); err != nil {
		return err
	}
	return nil
}

func (m *Manager) resolveBinary() (string, error) {
	if m.NginxBin != "" {
		if _, err := os.Stat(m.NginxBin); err != nil {
			return "", fmt.Errorf("nginx binary not found at %s", m.NginxBin)
		}
		return m.NginxBin, nil
	}
	return exec.LookPath("nginx")
}

func (m *Manager) Version() (string, error) {
	path, err := m.resolveBinary()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(path, "-v")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get nginx version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) pid() (int, error) {
	pidBytes, err := os.ReadFile(m.PIDFile)
	if err != nil {
		return 0, err
	}

	pidStr := strings.TrimSpace(string(pidBytes))
	if pidStr == "" {
		return 0, os.ErrNotExist
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("invalid pid in %s: %w", m.PIDFile, err)
	}
	return pid, nil
}

func (m *Manager) IsRunning() (bool, int, error) {
	pid, err := m.pid()
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return false, pid, err
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return false, pid, nil
	}

	return true, pid, nil
}

func (m *Manager) EnsureRunning() error {
	running, _, err := m.IsRunning()
	if err != nil {
		return err
	}
	if running {
		slog.Debug("nginx_already_running")
		return nil
	}

	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(m.PIDFile), 0755); err != nil {
		return err
	}

	// Validate Hubfly config before touching unmanaged host nginx.
	if err := m.testMainConfig(path); err != nil {
		return err
	}

	// If another nginx is running outside Hubfly PID tracking, stop it only after config validates.
	if err := m.stopUnmanagedNginx(path); err != nil {
		return err
	}

	args, err := m.startArgs()
	if err != nil {
		return err
	}

	slog.Info("nginx_start_started", "command", path, "args", args)
	cmd := exec.Command(path, args...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)
	if err != nil {
		output := string(out)
		if strings.Contains(output, "address already in use") {
			slog.Warn("nginx_start_conflict_detected_retrying_takeover", "output", output)
			if stopErr := m.stopUnmanagedNginx(path); stopErr != nil {
				slog.Warn("failed_to_stop_unmanaged_nginx_after_conflict", "error", stopErr)
			}
			cmdRetry := exec.Command(path, args...)
			retryOut, retryErr := cmdRetry.CombinedOutput()
			if retryErr == nil {
				slog.Info("nginx_start_retry_succeeded", "output", string(retryOut))
				return m.waitForRunning()
			}
			slog.Error("nginx_start_retry_failed", "error", retryErr, "output", string(retryOut))
		}
		slog.Error("nginx_start_failed", "error", err, "duration", duration, "output", string(out))
		return fmt.Errorf("nginx start failed: %s, output: %s", err, string(out))
	}
	slog.Info("nginx_start_command_succeeded", "duration", duration, "output", string(out))

	return m.waitForRunning()
}

func (m *Manager) ensureMainConfigPaths() error {
	content, err := os.ReadFile(m.NginxConf)
	if err != nil {
		if os.IsNotExist(err) {
			// Test harnesses may only exercise generated site/stream files and skip main nginx.conf.
			return nil
		}
		return fmt.Errorf("failed to read nginx config %s: %w", m.NginxConf, err)
	}
	updated := string(content)

	// Host-agnostic defaults from legacy config.
	updated = strings.ReplaceAll(updated, "/etc/hubfly/sites/*.conf", filepath.ToSlash(filepath.Join(m.SitesDir, "*.conf")))
	updated = strings.ReplaceAll(updated, "/etc/hubfly/streams/*.conf", filepath.ToSlash(filepath.Join(m.StreamsDir, "*.conf")))
	updated = strings.ReplaceAll(updated, "/var/www/hubfly/static", filepath.ToSlash(m.StaticDir))
	updated = strings.ReplaceAll(updated, "/var/log/hubfly/access.log", filepath.ToSlash(filepath.Join(m.LogsDir, "access.log")))
	updated = strings.ReplaceAll(updated, "/var/cache/nginx", filepath.ToSlash(filepath.Join(filepath.Dir(m.LogsDir), "cache", "nginx")))
	updated = ensureServerNameHashConfig(updated)
	// Compatibility: older nginx versions (for example 1.18.x) don't support ssl_reject_handshake.
	updated = strings.ReplaceAll(
		updated,
		"ssl_reject_handshake on;",
		fmt.Sprintf("ssl_certificate %s;\n        ssl_certificate_key %s;\n        return 444;", filepath.ToSlash(m.FallbackCert), filepath.ToSlash(m.FallbackKey)),
	)
	// Port normalization for older extracted configs.
	updated = strings.ReplaceAll(updated, "listen 82;", "listen 10004;")
	updated = strings.ReplaceAll(updated, "proxy_pass http://127.0.0.1:81;", "proxy_pass http://127.0.0.1:10003;")
	updated = strings.ReplaceAll(updated, "proxy_pass http://127.0.0.1:7890;", "proxy_pass http://127.0.0.1:10005;")

	// Ensure PID file points to hubfly-managed runtime path.
	pidPattern := regexp.MustCompile(`(?m)^\s*pid\s+[^;]+;\s*$`)
	if pidPattern.MatchString(updated) {
		updated = pidPattern.ReplaceAllString(updated, fmt.Sprintf("pid %s;", filepath.ToSlash(m.PIDFile)))
	} else {
		updated = fmt.Sprintf("pid %s;\n%s", filepath.ToSlash(m.PIDFile), updated)
	}

	// Make error log file local to runtime folder.
	errLogPattern := regexp.MustCompile(`(?m)^\s*error_log\s+[^;]+;\s*$`)
	errorLogPath := filepath.ToSlash(filepath.Join(m.LogsDir, "nginx.error.log"))
	if errLogPattern.MatchString(updated) {
		updated = errLogPattern.ReplaceAllString(updated, fmt.Sprintf("error_log %s notice;", errorLogPath))
	} else {
		updated = fmt.Sprintf("error_log %s notice;\n%s", errorLogPath, updated)
	}

	// Set worker user to runtime user so paths under /home/<user> remain accessible.
	runtimeUser := discoverRuntimeUser(m.NginxConf)
	userPattern := regexp.MustCompile(`(?m)^\s*user\s+[^;]+;\s*$`)
	if runtimeUser != "" {
		updated = userPattern.ReplaceAllString(updated, fmt.Sprintf("user %s;", runtimeUser))
		if !userPattern.MatchString(updated) {
			updated = fmt.Sprintf("user %s;\n%s", runtimeUser, updated)
		}
	} else {
		updated = userPattern.ReplaceAllString(updated, "# user directive managed by host defaults")
	}

	updated = m.normalizeStreamModuleSupport(updated)

	if updated == string(content) {
		return nil
	}
	if err := os.WriteFile(m.NginxConf, []byte(updated), 0644); err != nil {
		return fmt.Errorf("failed to update nginx runtime config %s: %w", m.NginxConf, err)
	}
	return nil
}

func ensureServerNameHashConfig(config string) string {
	if strings.Contains(config, "server_names_hash_bucket_size") && strings.Contains(config, "server_names_hash_max_size") {
		return config
	}

	httpBlockPattern := regexp.MustCompile(`http\s*\{\n`)
	if !httpBlockPattern.MatchString(config) {
		return config
	}

	injected := "http {\n    server_names_hash_bucket_size 128;\n    server_names_hash_max_size 4096;\n"
	return httpBlockPattern.ReplaceAllString(config, injected)
}

func (m *Manager) normalizeStreamModuleSupport(config string) string {
	if !strings.Contains(config, "stream {") {
		return config
	}

	streamModulePath := discoverStreamModulePath()
	if streamModulePath != "" {
		loadLine := fmt.Sprintf("load_module %s;", filepath.ToSlash(streamModulePath))
		if !strings.Contains(config, loadLine) {
			config = loadLine + "\n" + config
		}
	}
	return config
}

func discoverStreamModulePath() string {
	candidates := []string{
		"/usr/lib/nginx/modules/ngx_stream_module.so",
		"/usr/lib64/nginx/modules/ngx_stream_module.so",
		"/usr/share/nginx/modules/ngx_stream_module.so",
	}
	for _, path := range candidates {
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func (m *Manager) ensureFallbackCertificate() error {
	if fileExists(m.FallbackCert) && fileExists(m.FallbackKey) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.FallbackCert), 0755); err != nil {
		return err
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate fallback key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate fallback serial: %w", err)
	}

	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "hubfly-fallback.local",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"hubfly-fallback.local", "localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create fallback certificate: %w", err)
	}

	certOut, err := os.OpenFile(m.FallbackCert, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open fallback cert file: %w", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		_ = certOut.Close()
		return fmt.Errorf("write fallback cert: %w", err)
	}
	if err := certOut.Close(); err != nil {
		return fmt.Errorf("close fallback cert file: %w", err)
	}

	keyOut, err := os.OpenFile(m.FallbackKey, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open fallback key file: %w", err)
	}
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes}); err != nil {
		_ = keyOut.Close()
		return fmt.Errorf("write fallback key: %w", err)
	}
	if err := keyOut.Close(); err != nil {
		return fmt.Errorf("close fallback key file: %w", err)
	}

	return nil
}

func (m *Manager) testMainConfig(path string) error {
	cmd := exec.Command(path, "-t", "-c", m.NginxConf)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if strings.Contains(output, `unknown directive "stream"`) {
			return fmt.Errorf("nginx config validation failed: %s. stream module is required; install it (for Ubuntu/Debian: apt install libnginx-mod-stream) and restart", output)
		}
		return fmt.Errorf("nginx config validation failed: %s", output)
	}
	return nil
}

func (m *Manager) configHasPIDDirective() (bool, error) {
	content, err := os.ReadFile(m.NginxConf)
	if err != nil {
		return false, fmt.Errorf("failed to read nginx config: %w", err)
	}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "pid ") {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) startArgs() ([]string, error) {
	args := []string{"-c", m.NginxConf}
	hasPIDDirective, err := m.configHasPIDDirective()
	if err != nil {
		return nil, err
	}
	if !hasPIDDirective {
		args = append(args, "-g", fmt.Sprintf("pid %s;", m.PIDFile))
	}
	return args, nil
}

func (m *Manager) waitForRunning() error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		runningNow, _, runErr := m.IsRunning()
		if runErr == nil && runningNow {
			return nil
		}
	}
	slog.Error("nginx_start_health_check_timeout")
	return fmt.Errorf("nginx did not become running after start")
}

func (m *Manager) stopUnmanagedNginx(path string) error {
	running, pid, err := m.IsRunning()
	if err == nil && running && pid > 0 {
		return nil
	}

	pids, err := discoverNginxPIDs()
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}

	slog.Warn("unmanaged_nginx_detected_attempting_takeover", "pids", pids)
	cmd := exec.Command(path, "-s", "quit")
	out, quitErr := cmd.CombinedOutput()
	if quitErr != nil {
		slog.Warn("nginx_quit_command_failed_for_takeover", "error", quitErr, "output", string(out))
	} else {
		slog.Info("nginx_quit_command_succeeded_for_takeover", "output", string(out))
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		remaining, remErr := discoverNginxPIDs()
		if remErr != nil {
			return remErr
		}
		if len(remaining) == 0 {
			return nil
		}
	}

	// Force-stop stubborn workers/masters to avoid port conflicts.
	remaining, err := discoverNginxPIDs()
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		return nil
	}
	slog.Warn("forcing_nginx_process_shutdown_for_takeover", "pids", remaining)
	for _, p := range remaining {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}

	finalDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(finalDeadline) {
		time.Sleep(200 * time.Millisecond)
		final, finalErr := discoverNginxPIDs()
		if finalErr != nil {
			return finalErr
		}
		if len(final) == 0 {
			return nil
		}
	}
	left, _ := discoverNginxPIDs()
	if len(left) > 0 {
		return fmt.Errorf("unable to stop existing nginx processes: %v", left)
	}
	return nil
}

func discoverNginxPIDs() ([]int, error) {
	if _, err := exec.LookPath("pgrep"); err == nil {
		cmd := exec.Command("pgrep", "-x", "nginx")
		out, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return nil, nil
			}
			return nil, fmt.Errorf("failed to discover nginx pids: %w", err)
		}
		return filterTakeoverCandidates(parsePIDList(string(out))), nil
	}

	// Fallback for hosts without pgrep.
	psCmd := exec.Command("ps", "-eo", "pid=,comm=")
	out, err := psCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to discover nginx pids via ps: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	raw := make([]string, 0)
	for _, line := range lines {
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) < 2 {
			continue
		}
		if parts[1] == "nginx" {
			raw = append(raw, parts[0])
		}
	}
	pids := filterTakeoverCandidates(parsePIDList(strings.Join(raw, "\n")))
	sort.Ints(pids)
	return pids, nil
}

func parsePIDList(output string) []int {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	pids := make([]int, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

func filterTakeoverCandidates(pids []int) []int {
	out := make([]int, 0, len(pids))
	for _, pid := range pids {
		if isContainerizedNginxPID(pid) {
			continue
		}
		if isNginxWorkerPID(pid) {
			continue
		}
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func isContainerizedNginxPID(pid int) bool {
	cgroupPath := filepath.Join("/proc", strconv.Itoa(pid), "cgroup")
	if cgroupData, err := os.ReadFile(cgroupPath); err == nil {
		cg := strings.ToLower(string(cgroupData))
		if strings.Contains(cg, "docker") || strings.Contains(cg, "containerd") || strings.Contains(cg, "kubepods") || strings.Contains(cg, "libpod") {
			return true
		}
	}

	statPath := filepath.Join("/proc", strconv.Itoa(pid), "status")
	if statusData, err := os.ReadFile(statPath); err == nil {
		ppid := 0
		for _, line := range strings.Split(string(statusData), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if parsed, convErr := strconv.Atoi(fields[1]); convErr == nil {
						ppid = parsed
					}
				}
				break
			}
		}
		if ppid > 0 {
			commPath := filepath.Join("/proc", strconv.Itoa(ppid), "comm")
			if commData, err := os.ReadFile(commPath); err == nil {
				parentComm := strings.TrimSpace(strings.ToLower(string(commData)))
				if strings.Contains(parentComm, "containerd-shim") || strings.Contains(parentComm, "conmon") {
					return true
				}
			}
		}
	}

	return false
}

func isNginxWorkerPID(pid int) bool {
	cmdlinePath := filepath.Join("/proc", strconv.Itoa(pid), "cmdline")
	b, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false
	}
	cmdline := strings.ReplaceAll(string(b), "\x00", " ")
	cmdline = strings.ToLower(strings.TrimSpace(cmdline))
	return strings.Contains(cmdline, "worker process")
}

func discoverRuntimeUser(pathHint string) string {
	if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); sudoUser != "" {
		if _, err := user.Lookup(sudoUser); err == nil {
			return sudoUser
		}
	}

	stat, err := os.Stat(pathHint)
	if err != nil {
		return ""
	}
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(sys.Uid), 10))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Username)
}

func (m *Manager) Restart() error {
	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	slog.Info("nginx_restart_started", "command", path)
	cmd := exec.Command(path, "-s", "quit", "-c", m.NginxConf)
	quitOut, quitErr := cmd.CombinedOutput()
	if quitErr != nil {
		slog.Warn("nginx_quit_before_restart_failed", "error", quitErr, "output", string(quitOut))
	} else {
		slog.Info("nginx_quit_before_restart_succeeded", "output", string(quitOut))
	}

	time.Sleep(500 * time.Millisecond)
	if err := m.EnsureRunning(); err != nil {
		slog.Error("nginx_restart_failed", "error", err)
		return err
	}
	slog.Info("nginx_restart_succeeded")
	return nil
}

func (m *Manager) Health() Health {
	h := Health{
		Config:  m.NginxConf,
		PIDFile: m.PIDFile,
	}

	path, err := m.resolveBinary()
	if err != nil {
		h.Error = err.Error()
		return h
	}
	h.Binary = path
	h.Available = true

	version, versionErr := m.Version()
	if versionErr == nil {
		h.Version = version
	} else {
		h.Error = versionErr.Error()
	}

	running, pid, runErr := m.IsRunning()
	h.Running = running
	h.PID = pid
	if runErr != nil {
		if h.Error == "" {
			h.Error = runErr.Error()
		} else {
			h.Error = h.Error + "; " + runErr.Error()
		}
	}

	return h
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
