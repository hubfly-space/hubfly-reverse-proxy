package nginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestResolveDomainCertificateUsesWildcardForSubdomain(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_wildcard_cert")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir, Options{
		WildcardCerts: []WildcardCertificate{
			{
				Domain:   "eu1.hubfly.app",
				CertPath: "wildcards/eu1/fullchain.pem",
				KeyPath:  "wildcards/eu1/privkey.pem",
			},
		},
	})

	certPath := filepath.Join(mgr.CertsDir, "wildcards", "eu1", "fullchain.pem")
	keyPath := filepath.Join(mgr.CertsDir, "wildcards", "eu1", "privkey.pem")
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}

	gotCert, gotKey, source, found := mgr.ResolveDomainCertificate("testing.eu1.hubfly.app")
	if !found {
		t.Fatalf("expected wildcard certificate to be resolved")
	}
	if source != "wildcard" {
		t.Fatalf("expected source wildcard, got %q", source)
	}
	if gotCert != certPath {
		t.Fatalf("expected cert path %q, got %q", certPath, gotCert)
	}
	if gotKey != keyPath {
		t.Fatalf("expected key path %q, got %q", keyPath, gotKey)
	}
}

func TestGenerateConfigUsesWildcardCertificatePaths(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_wildcard_cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir, Options{
		WildcardCerts: []WildcardCertificate{
			{
				Domain:   "eu1.hubfly.app",
				CertPath: "wildcards/eu1/fullchain.pem",
				KeyPath:  "wildcards/eu1/privkey.pem",
			},
		},
	})
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	certPath := filepath.Join(mgr.CertsDir, "wildcards", "eu1", "fullchain.pem")
	keyPath := filepath.Join(mgr.CertsDir, "wildcards", "eu1", "privkey.pem")
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "wildcard-site",
		Domain:    "api.eu1.hubfly.app",
		Upstreams: []string{"127.0.0.1:8080"},
		SSL:       true,
	}
	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	config := string(content)
	if !strings.Contains(config, "ssl_certificate "+certPath+";") {
		t.Fatalf("expected generated config to use wildcard cert path")
	}
	if !strings.Contains(config, "ssl_certificate_key "+keyPath+";") {
		t.Fatalf("expected generated config to use wildcard key path")
	}
}
