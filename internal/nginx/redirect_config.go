package nginx

import (
	"bytes"
	"os"
	"path/filepath"
	"text/template"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func (m *Manager) GenerateRedirectConfig(redirect *models.Redirect) (string, error) {
	certPath, keyPath, usingFallbackCert := m.resolveCertPaths(redirect.SourceDomain)

	data := struct {
		*models.Redirect
		SSLCertificate    string
		SSLKey            string
		UsingFallbackCert bool
		LogsDir           string
	}{
		Redirect:          redirect,
		SSLCertificate:    certPath,
		SSLKey:            keyPath,
		UsingFallbackCert: usingFallbackCert,
		LogsDir:           m.LogsDir,
	}

	t, err := template.New("redirect").Parse(redirectTemplate)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}

	stagingFile := filepath.Join(m.StagingDir, redirect.ID+".conf")
	if err := os.WriteFile(stagingFile, buf.Bytes(), 0644); err != nil {
		return "", err
	}
	return stagingFile, nil
}

const redirectTemplate = `
server {
    listen 80;
    server_name {{ .SourceDomain }};

    access_log {{ .LogsDir }}/{{ .ID }}.access.log hubfly;
    error_log {{ .LogsDir }}/{{ .ID }}.error.log notice;

    {{ if .SSL }}
    return 301 https://{{ .TargetDomain }}$request_uri;
    {{ else }}
    return 301 http://{{ .TargetDomain }}$request_uri;
    {{ end }}
}

{{ if .SSL }}
server {
    listen 443 ssl;
    http2 on;
    server_name {{ .SourceDomain }};

    access_log {{ .LogsDir }}/{{ .ID }}.access.log hubfly;
    error_log {{ .LogsDir }}/{{ .ID }}.error.log notice;

    ssl_certificate {{ .SSLCertificate }};
    ssl_certificate_key {{ .SSLKey }};
    {{ if .UsingFallbackCert }}
    # Fallback certificate is active until the domain certificate is issued.
    {{ end }}

    return 301 https://{{ .TargetDomain }}$request_uri;
}
{{ end }}
`
