package nginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestRebuildStreamConfigUsesRuntimeVariableForSimpleTCP(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")

	tmpDir, err := os.MkdirTemp("", "nginx_stream_test_tcp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	stream := models.Stream{
		ID:         "tcp-stream",
		ListenPort: 30051,
		Upstream:   "missing-host:5432",
		Protocol:   "tcp",
	}

	if err := mgr.RebuildStreamConfig(stream.ListenPort, []models.Stream{stream}); err != nil {
		t.Fatalf("RebuildStreamConfig failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(mgr.StreamsDir, "port_30051.conf"))
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expected := []string{
		"map $remote_addr $stream_simple_map_30051 {",
		"default missing-host:5432;",
		"proxy_pass $stream_simple_map_30051;",
	}
	for _, s := range expected {
		if !strings.Contains(configStr, s) {
			t.Fatalf("expected stream config to contain %q", s)
		}
	}
}

func TestRebuildStreamConfigUsesRuntimeVariableForSimpleUDP(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")

	tmpDir, err := os.MkdirTemp("", "nginx_stream_test_udp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	stream := models.Stream{
		ID:         "udp-stream",
		ListenPort: 30052,
		Upstream:   "dns-missing:9999",
		Protocol:   "udp",
	}

	if err := mgr.RebuildStreamConfig(stream.ListenPort, []models.Stream{stream}); err != nil {
		t.Fatalf("RebuildStreamConfig failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(mgr.StreamsDir, "port_30052.conf"))
	if err != nil {
		t.Fatal(err)
	}
	configStr := string(content)

	expected := []string{
		"listen 30052 udp;",
		"listen [::]:30052 udp;",
		"map $remote_addr $stream_simple_map_30052 {",
		"default dns-missing:9999;",
		"proxy_pass $stream_simple_map_30052;",
	}
	for _, s := range expected {
		if !strings.Contains(configStr, s) {
			t.Fatalf("expected stream config to contain %q", s)
		}
	}
}
