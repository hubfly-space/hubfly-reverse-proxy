package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestRefreshSiteUpstreamsFromContainerDisablesAfterGracePeriod(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"No such container: app"}`, http.StatusNotFound)
	}))
	defer ts.Close()

	srv := &Server{Docker: dockerengine.NewClient(ts.URL)}
	missingSince := time.Now().Add(-6 * time.Minute)
	site := models.Site{
		ID:                   "site-1",
		Upstreams:            []string{"172.18.0.10:8080"},
		UpstreamContainers:   []string{"app"},
		UpstreamNetworks:     []string{"bridge"},
		UpstreamMissingSince: []*time.Time{&missingSince},
		DisabledUpstreams:    []bool{false},
	}

	changed, updated, err := srv.refreshSiteUpstreamsFromContainer(site)
	if err != nil {
		t.Fatalf("refreshSiteUpstreamsFromContainer returned error: %v", err)
	}
	if !changed {
		t.Fatalf("expected site to change")
	}
	if !updated.DisabledUpstreams[0] {
		t.Fatalf("expected upstream to be disabled after grace period")
	}
	if updated.Upstreams[0] != "172.18.0.10:8080" {
		t.Fatalf("expected cached endpoint to be retained for later recovery, got %q", updated.Upstreams[0])
	}
}

func TestRefreshStreamUpstreamFromContainerDisablesAfterGracePeriod(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"No such container: app"}`, http.StatusNotFound)
	}))
	defer ts.Close()

	srv := &Server{Docker: dockerengine.NewClient(ts.URL)}
	missingSince := time.Now().Add(-6 * time.Minute)
	stream := models.Stream{
		ID:            "stream-1",
		ListenPort:    15432,
		Upstream:      "172.18.0.10:5432",
		ContainerName: "app",
		MissingSince:  &missingSince,
	}

	changed, updated, err := srv.refreshStreamUpstreamFromContainer(stream)
	if err != nil {
		t.Fatalf("refreshStreamUpstreamFromContainer returned error: %v", err)
	}
	if !changed {
		t.Fatalf("expected stream to change")
	}
	if !updated.Disabled {
		t.Fatalf("expected stream to be disabled after grace period")
	}
}

func TestRefreshStreamUpstreamFromContainerReEnablesWhenContainerReturns(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.18.0.22"}}}}`)
	}))
	defer ts.Close()

	srv := &Server{Docker: dockerengine.NewClient(ts.URL)}
	missingSince := time.Now().Add(-6 * time.Minute)
	stream := models.Stream{
		ID:               "stream-1",
		ListenPort:       15432,
		Upstream:         "172.18.0.10:5432",
		ContainerName:    "app",
		ContainerNetwork: "bridge",
		MissingSince:     &missingSince,
		Disabled:         true,
		Status:           "error",
		ErrorMessage:     "tracked container missing for over 5 minutes; stream disabled to avoid stale IP routing",
	}

	changed, updated, err := srv.refreshStreamUpstreamFromContainer(stream)
	if err != nil {
		t.Fatalf("refreshStreamUpstreamFromContainer returned error: %v", err)
	}
	if !changed {
		t.Fatalf("expected stream to change")
	}
	if updated.Disabled {
		t.Fatalf("expected stream to be re-enabled")
	}
	if updated.Upstream != "172.18.0.22:5432" {
		t.Fatalf("expected updated upstream, got %q", updated.Upstream)
	}
	if updated.MissingSince != nil {
		t.Fatalf("expected missing_since to be cleared")
	}
}
