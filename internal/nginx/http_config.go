package nginx

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

const (
	defaultUpstreamMaxFails         = 3
	defaultUpstreamFailTimeout      = "20s"
	defaultProxyNextUpstream        = "error timeout invalid_header http_502 http_503 http_504"
	defaultProxyNextUpstreamTries   = 2
	defaultProxyConnectTimeout      = "10s"
	defaultProxySendTimeout         = "300s"
	defaultProxyReadTimeout         = "300s"
)

func (m *Manager) GenerateConfig(site *models.Site) (string, error) {
	var templateContent strings.Builder
	for _, tplName := range site.Templates {
		content, err := os.ReadFile(filepath.Join(m.TemplatesDir, tplName+".conf"))
		if err != nil {
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
		upstreamTargets = append(upstreamTargets, upstreamTarget{Address: endpoint, Weight: weight})
	}

	hasAvailableUpstream := len(activeUpstreams) > 0
	proxyPassTarget := ""
	if hasAvailableUpstream {
		proxyPassTarget = fmt.Sprintf("http://%s", activeUpstreams[0])
	}
	if useLoadBalancing && hasAvailableUpstream {
		proxyPassTarget = fmt.Sprintf("http://%s", upstreamName)
	}
	usePassiveFailover := useLoadBalancing && len(upstreamTargets) > 1

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
		UsePassiveFailover   bool
		HasAvailableUpstream bool
		ProxyPassTarget      string
		UpstreamMaxFails     int
		UpstreamFailTimeout  string
		ProxyNextUpstream    string
		ProxyNextUpstreamTry int
		ProxyConnectTimeout  string
		ProxySendTimeout     string
		ProxyReadTimeout     string
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
		UsePassiveFailover:   usePassiveFailover,
		HasAvailableUpstream: hasAvailableUpstream,
		ProxyPassTarget:      proxyPassTarget,
		UpstreamMaxFails:     defaultUpstreamMaxFails,
		UpstreamFailTimeout:  defaultUpstreamFailTimeout,
		ProxyNextUpstream:    defaultProxyNextUpstream,
		ProxyNextUpstreamTry: defaultProxyNextUpstreamTries,
		ProxyConnectTimeout:  defaultProxyConnectTimeout,
		ProxySendTimeout:     defaultProxySendTimeout,
		ProxyReadTimeout:     defaultProxyReadTimeout,
	}

	t, err := template.New("site").Funcs(template.FuncMap{"join": strings.Join}).Parse(serverTemplate)
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

const serverTemplate = `
{{ define "proxy_common" }}
        {{ if .HasAvailableUpstream }}
        proxy_pass $upstream_endpoint;
        {{ else }}
        return 502;
        {{ end }}
        {{ if .UsePassiveFailover }}
        proxy_next_upstream {{ .ProxyNextUpstream }};
        proxy_next_upstream_tries {{ .ProxyNextUpstreamTry }};
        proxy_connect_timeout {{ .ProxyConnectTimeout }};
        proxy_send_timeout {{ .ProxySendTimeout }};
        proxy_read_timeout {{ .ProxyReadTimeout }};
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
        {{ range $k, $v := .ProxySetHeaders }}
        proxy_set_header {{ $k }} {{ $v }};
        {{ end }}
{{ end }}

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
    server {{ .Address }} weight={{ .Weight }} max_fails={{ $.UpstreamMaxFails }} fail_timeout={{ $.UpstreamFailTimeout }};
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
        set $upstream_endpoint "{{ $.ProxyPassTarget }}";
        {{ template "proxy_common" $ }}
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
        {{ template "proxy_common" . }}
        {{ .TemplateSnippets }}
        {{ .ExtraConfig }}
    }
    {{ end }}

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

    access_log {{ .LogsDir }}/{{ .ID }}.access.log hubfly;
    error_log {{ .LogsDir }}/{{ .ID }}.error.log notice;

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
        {{ template "proxy_common" $ }}
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
        {{ template "proxy_common" . }}
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
