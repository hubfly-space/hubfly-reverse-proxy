package nginx

import (
	"path/filepath"
	"strings"
)

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
