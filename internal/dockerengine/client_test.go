package dockerengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamContainerEventsFiltersAndParsesEvents(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("expected /events path, got %s", r.URL.Path)
		}
		filters := r.URL.Query().Get("filters")
		if filters == "" {
			t.Fatalf("expected filters query parameter")
		}
		var decoded map[string][]string
		if err := json.Unmarshal([]byte(filters), &decoded); err != nil {
			t.Fatalf("expected valid filters json: %v", err)
		}
		events := decoded["event"]
		expected := map[string]bool{"start": true, "restart": true, "unpause": true, "stop": true}
		if len(events) != len(expected) {
			t.Fatalf("expected %d event filters, got %d (%v)", len(expected), len(events), events)
		}
		for _, action := range events {
			if !expected[action] {
				t.Fatalf("unexpected event filter %q", action)
			}
		}
		fmt.Fprintln(w, `{"Type":"container","Action":"start","Actor":{"ID":"abc123","Attributes":{"name":"app"}}}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	received := make([]Event, 0, 1)
	err := client.StreamContainerEvents(ctx, []string{"start", "restart", "unpause", "stop"}, func(event Event) {
		received = append(received, event)
		cancel()
	})
	if err != nil && err != context.Canceled {
		t.Fatalf("StreamContainerEvents returned error: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}
	if received[0].Action != "start" {
		t.Fatalf("expected start action, got %q", received[0].Action)
	}
	if received[0].Actor.Attributes["name"] != "app" {
		t.Fatalf("expected container name app, got %q", received[0].Actor.Attributes["name"])
	}
}
