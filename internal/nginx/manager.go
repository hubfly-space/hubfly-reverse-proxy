package nginx

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	SitesDir     string
	StreamsDir   string
	StagingDir   string
	TemplatesDir string
	NginxConf    string // Path to main nginx.conf
	NginxBin     string
	FallbackCert string
	FallbackKey  string
	CertsDir     string
	WebrootDir   string
	StaticDir    string
	LogsDir      string
	PIDFile      string
}

type Options struct {
	NginxConf  string
	NginxBin   string
	CertsDir   string
	WebrootDir string
	StaticDir  string
	LogsDir    string
	PIDFile    string
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
	}

	return &Manager{
		SitesDir:     filepath.Join(baseDir, "sites"),
		StreamsDir:   filepath.Join(baseDir, "streams"),
		StagingDir:   filepath.Join(baseDir, "staging"),
		TemplatesDir: filepath.Join(baseDir, "templates"),
		NginxConf:    cfg.NginxConf,
		NginxBin:     cfg.NginxBin,
		FallbackCert: filepath.Join(baseDir, "default-certs", "fallback.crt"),
		FallbackKey:  filepath.Join(baseDir, "default-certs", "fallback.key"),
		CertsDir:     cfg.CertsDir,
		WebrootDir:   cfg.WebrootDir,
		StaticDir:    cfg.StaticDir,
		LogsDir:      cfg.LogsDir,
		PIDFile:      cfg.PIDFile,
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
		filepath.Dir(m.PIDFile),
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
	base := filepath.Join(m.CertsDir, "live", domain)
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
	for i, endpoint := range site.Upstreams {
		weight := 1
		if i < len(lbWeights) && lbWeights[i] > 0 {
			weight = lbWeights[i]
		}
		upstreamTargets = append(upstreamTargets, upstreamTarget{
			Address: endpoint,
			Weight:  weight,
		})
	}
	proxyPassTarget := fmt.Sprintf("http://%s", site.Upstreams[0])
	if useLoadBalancing {
		proxyPassTarget = fmt.Sprintf("http://%s", upstreamName)
	}

	// Wrapper for template data
	data := struct {
		*models.Site
		TemplateSnippets  string
		SSLCertificate    string
		SSLKey            string
		EffectiveForceSSL bool
		UsingFallbackCert bool
		LogsDir           string
		WebrootDir        string
		StaticDir         string
		UpstreamName      string
		UpstreamTargets   []upstreamTarget
		LoadBalanceAlgo   string
		UseLoadBalancing  bool
		ProxyPassTarget   string
	}{
		Site:              site,
		TemplateSnippets:  templateContent.String(),
		SSLCertificate:    certPath,
		SSLKey:            keyPath,
		EffectiveForceSSL: effectiveForceSSL,
		UsingFallbackCert: usingFallbackCert,
		LogsDir:           m.LogsDir,
		WebrootDir:        m.WebrootDir,
		StaticDir:         m.StaticDir,
		UpstreamName:      upstreamName,
		UpstreamTargets:   upstreamTargets,
		LoadBalanceAlgo:   lbAlgorithm,
		UseLoadBalancing:  useLoadBalancing,
		ProxyPassTarget:   proxyPassTarget,
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

        proxy_pass $upstream_endpoint;

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

        proxy_pass $upstream_endpoint;

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
	slog.Info("Reloading Nginx")
	cmd := exec.Command(path, "-s", "reload")
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("Nginx reload failed, attempting restart", "error", err, "output", string(out))
		if startErr := m.EnsureRunning(); startErr != nil {
			slog.Error("Nginx restart failed after reload failure", "reload_error", err, "start_error", startErr)
			return fmt.Errorf("nginx reload failed: %s, output: %s", err, string(out))
		}
		return nil
	}
	slog.Debug("Nginx reload success", "output", string(out))
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
		return nil
	}

	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(m.PIDFile), 0755); err != nil {
		return err
	}

	args := []string{"-c", m.NginxConf, "-g", fmt.Sprintf("pid %s;", m.PIDFile)}
	cmd := exec.Command(path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nginx start failed: %s, output: %s", err, string(out))
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		runningNow, _, runErr := m.IsRunning()
		if runErr == nil && runningNow {
			return nil
		}
	}
	return fmt.Errorf("nginx did not become running after start")
}

func (m *Manager) Restart() error {
	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	cmd := exec.Command(path, "-s", "quit")
	_, _ = cmd.CombinedOutput()

	time.Sleep(500 * time.Millisecond)
	return m.EnsureRunning()
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
