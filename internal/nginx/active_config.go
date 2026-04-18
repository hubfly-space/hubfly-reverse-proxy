package nginx

import (
	"fmt"
	"path/filepath"
	"regexp"
)

var (
	accessLogPattern      = regexp.MustCompile(`(?m)^(\s*access_log\s+)[^;]*/([^/\s;]+\.access\.log)(\s+hubfly;)\s*$`)
	errorLogPattern       = regexp.MustCompile(`(?m)^(\s*error_log\s+)[^;]*/([^/\s;]+\.error\.log)(\s+\w+;)\s*$`)
	fallbackCertPattern   = regexp.MustCompile(`(?m)^(\s*ssl_certificate\s+).*/default-certs/fallback\.crt(\s*;)\s*$`)
	fallbackKeyPattern    = regexp.MustCompile(`(?m)^(\s*ssl_certificate_key\s+).*/default-certs/fallback\.key(\s*;)\s*$`)
	domainCertPattern     = regexp.MustCompile(`(?m)^(\s*ssl_certificate\s+).*/certbot/config/live/([^/\s;]+)/fullchain\.pem(\s*;)\s*$`)
	domainKeyPattern      = regexp.MustCompile(`(?m)^(\s*ssl_certificate_key\s+).*/certbot/config/live/([^/\s;]+)/privkey\.pem(\s*;)\s*$`)
	webrootPattern        = regexp.MustCompile(`(?m)^(\s*root\s+).*/www(\s*;)\s*$`)
	staticRootPattern     = regexp.MustCompile(`(?m)^(\s*root\s+).*/static(\s*;)\s*$`)
)

func (m *Manager) NormalizeRenderedHTTPConfig(config string) string {
	out := accessLogPattern.ReplaceAllStringFunc(config, func(line string) string {
		matches := accessLogPattern.FindStringSubmatch(line)
		return fmt.Sprintf("%s%s%s", matches[1], filepath.ToSlash(filepath.Join(m.LogsDir, matches[2])), matches[3])
	})
	out = errorLogPattern.ReplaceAllStringFunc(out, func(line string) string {
		matches := errorLogPattern.FindStringSubmatch(line)
		return fmt.Sprintf("%s%s%s", matches[1], filepath.ToSlash(filepath.Join(m.LogsDir, matches[2])), matches[3])
	})
	out = fallbackCertPattern.ReplaceAllString(out, fmt.Sprintf("${1}%s${2}", filepath.ToSlash(m.FallbackCert)))
	out = fallbackKeyPattern.ReplaceAllString(out, fmt.Sprintf("${1}%s${2}", filepath.ToSlash(m.FallbackKey)))
	out = domainCertPattern.ReplaceAllStringFunc(out, func(line string) string {
		matches := domainCertPattern.FindStringSubmatch(line)
		certPath, _ := m.realCertPaths(matches[2])
		return fmt.Sprintf("%s%s%s", matches[1], filepath.ToSlash(certPath), matches[3])
	})
	out = domainKeyPattern.ReplaceAllStringFunc(out, func(line string) string {
		matches := domainKeyPattern.FindStringSubmatch(line)
		_, keyPath := m.realCertPaths(matches[2])
		return fmt.Sprintf("%s%s%s", matches[1], filepath.ToSlash(keyPath), matches[3])
	})
	out = webrootPattern.ReplaceAllString(out, fmt.Sprintf("${1}%s${2}", filepath.ToSlash(m.WebrootDir)))
	out = staticRootPattern.ReplaceAllString(out, fmt.Sprintf("${1}%s${2}", filepath.ToSlash(m.StaticDir)))
	return out
}
