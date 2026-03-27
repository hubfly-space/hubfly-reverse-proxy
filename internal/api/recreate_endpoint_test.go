package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
)

func TestHandleManualRecreate(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")

	tmpDir, err := os.MkdirTemp("", "hubfly_recreate_endpoint")
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
		Upstreams: []string{"127.0.0.1:8080"},
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

	nm := nginx.NewManager(tmpDir)
	if err := nm.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm.SitesDir, "stale.conf"), []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		Store:     st,
		Nginx:     nm,
		retrying:  make(map[string]bool),
		startedAt: time.Now(),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/control/recreate", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload["status"] != "recreated" {
		t.Fatalf("expected recreated status, got %#v", payload["status"])
	}
	if payload["sites_recreated"] != float64(1) {
		t.Fatalf("expected sites_recreated=1, got %#v", payload["sites_recreated"])
	}
	if payload["streams_recreated"] != float64(1) {
		t.Fatalf("expected streams_recreated=1, got %#v", payload["streams_recreated"])
	}

	if _, err := os.Stat(filepath.Join(nm.SitesDir, "app-1.conf")); err != nil {
		t.Fatalf("expected recreated site config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nm.StreamsDir, "port_15432.conf")); err != nil {
		t.Fatalf("expected recreated stream config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nm.SitesDir, "stale.conf")); !os.IsNotExist(err) {
		t.Fatalf("expected stale config to be pruned")
	}
}
