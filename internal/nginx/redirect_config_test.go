package nginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestGenerateRedirectConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nginx_redirect_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	configFile, err := mgr.GenerateRedirectConfig(&models.Redirect{
		ID:           "example.com",
		SourceDomain: "example.com",
		TargetDomain: "www.example.com",
		SSL:          true,
	})
	if err != nil {
		t.Fatalf("GenerateRedirectConfig failed: %v", err)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	expected := []string{
		"server_name example.com;",
		"return 301 https://www.example.com$request_uri;",
		"listen 443 ssl;",
	}
	for _, item := range expected {
		if !strings.Contains(config, item) {
			t.Fatalf("expected config to contain %q, got:\n%s", item, config)
		}
	}
	if filepath.Dir(configFile) != mgr.StagingDir {
		t.Fatalf("expected staging file in %s, got %s", mgr.StagingDir, configFile)
	}
}
