package nginx

import (
	"os"
	"strings"
	"testing"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestFirewallIPRules(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-firewall",
		Domain:    "firewall.local",
		Upstreams: []string{"127.0.0.1:8080"},
		Firewall: &models.FirewallConfig{
			IPRules: []models.IPRule{
				{Action: "allow", Value: "192.168.1.100"},
				{Action: "deny", Value: "192.168.1.0/24"},
				{Action: "allow", Value: "all"},
			},
		},
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expectedRules := []string{
		"allow 192.168.1.100;",
		"deny 192.168.1.0/24;",
		"allow all;",
	}

	for _, rule := range expectedRules {
		if !strings.Contains(configStr, rule) {
			t.Errorf("Config missing rule: %s", rule)
		}
	}
}

func TestFirewallBlockingRules(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_block")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-block",
		Domain:    "block.local",
		Upstreams: []string{"127.0.0.1:8080"},
		Firewall: &models.FirewallConfig{
			BlockRules: &models.BlockRules{
				Paths:      []string{"/admin", "/private"},
				UserAgents: []string{"curl", "wget"},
				Methods:    []string{"POST", "DELETE"},
			},
		},
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expectedStrings := []string{
		"location ~ /admin { return 403; }",
		"location ~ /private { return 403; }",
		`if ($http_user_agent ~* "(curl|wget)") { return 403; }`,
		`if ($request_method ~* "(POST|DELETE)") { return 405; }`,
	}

	for _, s := range expectedStrings {
		if !strings.Contains(configStr, s) {
			t.Errorf("Config missing blocking rule: %s", s)
		}
	}
}

func TestFirewallRateLimiting(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_rate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-rate",
		Domain:    "rate.local",
		Upstreams: []string{"127.0.0.1:8080"},
		Firewall: &models.FirewallConfig{
			RateLimit: &models.RateLimitConfig{
				Enabled: true,
				Rate:    10,
				Unit:    "r/s",
				Burst:   20,
			},
		},
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expectedStrings := []string{
		"limit_req_zone $binary_remote_addr zone=zone_test-rate:10m rate=10r/s;",
		"limit_req zone=zone_test-rate burst=20 nodelay;",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(configStr, s) {
			t.Errorf("Config missing rate limit rule: %s", s)
		}
	}
}

func TestFirewallPathMethodBlocking(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_pm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-pm",
		Domain:    "pm.local",
		Upstreams: []string{"127.0.0.1:8080"},
		Firewall: &models.FirewallConfig{
			BlockRules: &models.BlockRules{
				PathMethods: map[string][]string{
					"/admin": {"POST", "DELETE"},
				},
			},
		},
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expectedStrings := []string{
		"location ~ /admin {",
		`if ($request_method ~* "(POST|DELETE)") { return 405; }`,
		"proxy_pass", // Ensure it still proxies
	}

	for _, s := range expectedStrings {
		if !strings.Contains(configStr, s) {
			t.Errorf("Config missing path-method rule: %s", s)
		}
	}
}

func TestSSLConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_ssl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-ssl",
		Domain:    "ssl.local",
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
	configStr := string(content)

	expectedStrings := []string{
		"listen 443 ssl;",
		"ssl_certificate " + mgr.FallbackCert + ";",
		"ssl_certificate_key " + mgr.FallbackKey + ";",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(configStr, s) {
			t.Errorf("Config missing SSL directive: %s", s)
		}
	}
}

func TestForceSSLRemainsEnabledWhileUsingFallbackCert(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_force_ssl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-fallback-force-ssl",
		Domain:    "fallback.local",
		Upstreams: []string{"127.0.0.1:8080"},
		SSL:       true,
		ForceSSL:  true,
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	if !strings.Contains(configStr, "return 301 https://$host$request_uri;") {
		t.Fatalf("expected HTTP->HTTPS redirect to remain enabled while using fallback cert")
	}
}

func TestForceSSLAlwaysRedirectsHTTP(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_force_ssl_redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-force-ssl-redirect",
		Domain:    "force-ssl-redirect.local",
		Upstreams: []string{"127.0.0.1:8080"},
		ForceSSL:  true,
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	if !strings.Contains(configStr, "return 301 https://$host$request_uri;") {
		t.Fatalf("expected ForceSSL block to always redirect HTTP to HTTPS")
	}

	if strings.Contains(configStr, `$http_upgrade`) {
		t.Fatalf("did not expect websocket upgrade-based redirect bypass in ForceSSL block")
	}
}

func TestGeneratedConfigHasNoLegacyWebSocketLocationOrForcedUpgradeHeader(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_ws_cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-ws-cleanup",
		Domain:    "ws-cleanup.local",
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
	configStr := string(content)

	if strings.Contains(configStr, "location /ws/ {") {
		t.Fatalf("did not expect legacy /ws/ location in generated config")
	}

	if strings.Contains(configStr, `proxy_set_header Connection "upgrade";`) {
		t.Fatalf("did not expect forced Connection: upgrade header in general proxy locations")
	}

	if !strings.Contains(configStr, "proxy_set_header Upgrade $http_upgrade;") {
		t.Fatalf("expected websocket upgrade header support in proxied locations")
	}

	if !strings.Contains(configStr, "proxy_set_header Connection $connection_upgrade;") {
		t.Fatalf("expected websocket connection upgrade mapping in proxied locations")
	}
}

func TestProxyForwardsPublicHostHeaders(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_test_forwarded_headers")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:        "test-forwarded-headers",
		Domain:    "forwarded.local",
		Upstreams: []string{"127.0.0.1:8080"},
		SSL:       false,
	}

	configFile, err := mgr.GenerateConfig(site)
	if err != nil {
		t.Fatalf("GenerateConfig failed: %v", err)
	}

	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expected := []string{
		"proxy_set_header Host $host;",
		"proxy_set_header X-Forwarded-Host $host;",
		"proxy_set_header X-Forwarded-Proto $scheme;",
		"proxy_set_header X-Forwarded-Port $server_port;",
		"proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;",
	}

	for _, e := range expected {
		if !strings.Contains(configStr, e) {
			t.Fatalf("expected forwarded header directive not found: %s", e)
		}
	}
}
