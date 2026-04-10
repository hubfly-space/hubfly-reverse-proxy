package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/recreate"
)

func (s *Server) handleManualReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	slog.Info("manual_reload_requested", "request_id", requestIDFromContext(r.Context()))

	if err := s.Nginx.Reload(); err != nil {
		errorResponse(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status": "reloaded",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleManualFullCheckReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Docker == nil {
		errorResponse(w, http.StatusServiceUnavailable, "docker engine is unavailable")
		return
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	slog.Info("manual_full_check_requested", "request_id", requestIDFromContext(r.Context()))

	reloaded, siteCount, streamCount, err := s.syncUpstreamsAndRefreshLocked(false)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "full check failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":                "checked",
		"reloaded":              reloaded,
		"sites_changed":         siteCount,
		"stream_ports_changed":  streamCount,
		"next_scheduled_check":  time.Now().Add(dockerFullCheckInterval).UTC().Format(time.RFC3339),
		"requested_at_utc_time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleManualRecreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	slog.Info("manual_recreate_requested", "request_id", requestIDFromContext(r.Context()))

	result, err := recreate.Run(s.Store, s.Nginx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "recreate failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":                "recreated",
		"sites_recreated":       result.Sites,
		"streams_recreated":     result.Streams,
		"stream_ports_rebuilt":  result.StreamPorts,
		"requested_at_utc_time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleManualCachePurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	slog.Info("manual_cache_purge_requested", "request_id", requestIDFromContext(r.Context()))

	if err := s.Nginx.PurgeCache(); err != nil {
		errorResponse(w, http.StatusInternalServerError, "cache purge failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status": "cache_purged",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func jsonResponse(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func errorResponse(w http.ResponseWriter, code int, msg string) {
	jsonResponse(w, code, map[string]interface{}{
		"error": msg,
		"code":  code,
	})
}
