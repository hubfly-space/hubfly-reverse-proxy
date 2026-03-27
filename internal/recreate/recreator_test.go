package recreate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
)

func TestRunRecreatesSitesAndStreamsAndPrunesStaleConfigs(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")

	tmpDir, err := os.MkdirTemp("", "hubfly_recreate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	st, err := store.NewJSONStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	site := models.Site{
		ID:        "app-1",
		Domain:    "example.local",
		Upstreams: []string{"127.0.0.1:8080", "127.0.0.1:8081"},
		LoadBalancing: &models.LoadBalancing{
			Enabled:   true,
			Algorithm: "least_conn",
			Weights:   []int{1, 2},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := st.SaveSite(&site); err != nil {
		t.Fatal(err)
	}

	stream := models.Stream{
		ID:         "db-1",
		ListenPort: 15432,
		Upstream:   "127.0.0.1:5432",
		Protocol:   "tcp",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := st.SaveStream(&stream); err != nil {
		t.Fatal(err)
	}

	mgr := nginx.NewManager(tmpDir)
	if err := mgr.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	staleSite := filepath.Join(mgr.SitesDir, "stale.conf")
	staleStream := filepath.Join(mgr.StreamsDir, "port_9999.conf")
	if err := os.WriteFile(staleSite, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleStream, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := Run(st, mgr)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.Sites != 1 || result.Streams != 1 || result.StreamPorts != 1 {
		t.Fatalf("unexpected recreate result: %+v", result)
	}

	if _, err := os.Stat(staleSite); !os.IsNotExist(err) {
		t.Fatalf("expected stale site config to be pruned")
	}
	if _, err := os.Stat(staleStream); !os.IsNotExist(err) {
		t.Fatalf("expected stale stream config to be pruned")
	}

	if _, err := os.Stat(filepath.Join(mgr.SitesDir, "app-1.conf")); err != nil {
		t.Fatalf("expected recreated site config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mgr.StreamsDir, "port_15432.conf")); err != nil {
		t.Fatalf("expected recreated stream config: %v", err)
	}
}
