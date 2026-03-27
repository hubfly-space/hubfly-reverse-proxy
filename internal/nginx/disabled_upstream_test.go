package nginx

import (
	"os"
	"strings"
	"testing"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestGenerateConfigReturns502WhenAllUpstreamsDisabled(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_disabled_upstream")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	site := &models.Site{
		ID:                "disabled-site",
		Domain:            "disabled.local",
		Upstreams:         []string{"172.18.0.10:8080"},
		DisabledUpstreams: []bool{true},
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
	if !strings.Contains(config, "return 502;") {
		t.Fatalf("expected generated config to return 502 when all upstreams are disabled")
	}
}
