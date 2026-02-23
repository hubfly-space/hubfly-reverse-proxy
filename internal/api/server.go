package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/certbot"
	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/logmanager"
	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
	"github.com/hubfly/hubfly-reverse-proxy/internal/upstream"
)

var certRetrySchedule = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

const certRetrySweepInterval = 30 * time.Second

type Server struct {
	Store            store.Store
	Nginx            *nginx.Manager
	Certbot          *certbot.Manager
	Docker           *dockerengine.Client
	LogManager       *logmanager.Manager
	UpstreamResolver upstream.Resolver
	BuildInfo        BuildInfo
	startedAt        time.Time
	retryMu          sync.Mutex
	retrying         map[string]bool
}

type BuildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

func NewServer(s store.Store, n *nginx.Manager, c *certbot.Manager, d *dockerengine.Client, l *logmanager.Manager, resolver upstream.Resolver, buildInfo BuildInfo) *Server {
	if resolver == nil {
		resolver = upstream.NewDefaultResolver(d)
	}
	srv := &Server{
		Store:            s,
		Nginx:            n,
		Certbot:          c,
		Docker:           d,
		LogManager:       l,
		UpstreamResolver: resolver,
		BuildInfo:        buildInfo,
		startedAt:        time.Now(),
		retrying:         make(map[string]bool),
	}
	go srv.certRetryLoop()
	return srv
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/sites", s.handleSites)           // GET, POST
	mux.HandleFunc("/v1/sites/", s.handleSiteDetail)     // GET, DELETE, PATCH
	mux.HandleFunc("/v1/streams", s.handleStreams)       // GET, POST
	mux.HandleFunc("/v1/streams/", s.handleStreamDetail) // GET, DELETE

	return s.loggingMiddleware(mux)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Read body for logging if present
		var bodyBytes []byte
		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut) {
			bodyBytes, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // Restore body
		}

		slog.Debug("API Request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"body", string(bodyBytes),
		)

		// Wrap ResponseWriter to capture status code
		rw := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		slog.Info("API Response",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", duration,
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	sites, _ := s.Store.ListSites()
	streams, _ := s.Store.ListStreams()

	nginxHealth := s.Nginx.Health()
	certbotHealth := s.Certbot.Health()
	dockerHealth := dockerengine.Health{Available: false}
	if s.Docker != nil {
		dockerHealth = s.Docker.Health()
	}
	status := "ok"
	if !nginxHealth.Available || !nginxHealth.Running || !certbotHealth.Available || !dockerHealth.Available {
		status = "degraded"
	}

	jsonResponse(w, 200, map[string]interface{}{
		"status": status,
		"time":   time.Now().UTC().Format(time.RFC3339),
		"service": map[string]interface{}{
			"version":    s.BuildInfo.Version,
			"commit":     s.BuildInfo.Commit,
			"build_time": s.BuildInfo.BuildTime,
			"go_version": runtime.Version(),
			"started_at": s.startedAt.UTC().Format(time.RFC3339),
			"uptime":     time.Since(s.startedAt).String(),
		},
		"nginx":   nginxHealth,
		"certbot": certbotHealth,
		"docker":  dockerHealth,
		"store": map[string]interface{}{
			"sites_count":   len(sites),
			"streams_count": len(streams),
		},
	})
}

type createStreamRequest struct {
	ID            string `json:"id"`
	ListenPort    int    `json:"listen_port"`
	Upstream      string `json:"upstream"`
	ContainerPort int    `json:"container_port,omitempty"`
	Protocol      string `json:"protocol"`
	Domain        string `json:"domain,omitempty"`
}

func normalizeStreamUpstream(resolver upstream.Resolver, upstreamAddress string, containerPort int) (string, error) {
	return resolver.Resolve(upstreamAddress, containerPort)
}

func (s *Server) normalizeSiteUpstreams(upstreamsInput []string) ([]string, error) {
	if len(upstreamsInput) == 0 {
		return nil, fmt.Errorf("at least one upstream is required")
	}

	normalized := make([]string, 0, len(upstreamsInput))
	for _, upstreamAddress := range upstreamsInput {
		resolved, err := s.UpstreamResolver.Resolve(upstreamAddress, 0)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, resolved)
	}
	return normalized, nil
}

func (s *Server) ensureResolvedSiteUpstreams(site *models.Site) error {
	original := append([]string(nil), site.Upstreams...)
	normalized, err := s.normalizeSiteUpstreams(site.Upstreams)
	if err != nil {
		return err
	}
	site.Upstreams = normalized

	changed := len(original) != len(normalized)
	if !changed {
		for idx := range original {
			if original[idx] != normalized[idx] {
				changed = true
				break
			}
		}
	}
	if changed {
		site.UpdatedAt = time.Now()
		if err := s.Store.SaveSite(site); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleStreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		streams, err := s.Store.ListStreams()
		if err != nil {
			errorResponse(w, 500, err.Error())
			return
		}
		jsonResponse(w, 200, streams)
	case http.MethodPost:
		var req createStreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, 400, "invalid json")
			return
		}

		upstreamAddress, err := normalizeStreamUpstream(s.UpstreamResolver, req.Upstream, req.ContainerPort)
		if err != nil {
			errorResponse(w, 400, err.Error())
			return
		}

		stream := models.Stream{
			ID:         strings.TrimSpace(req.ID),
			ListenPort: req.ListenPort,
			Upstream:   upstreamAddress,
			Protocol:   strings.ToLower(strings.TrimSpace(req.Protocol)),
			Domain:     strings.TrimSpace(req.Domain),
		}

		if stream.ListenPort == 0 {
			streams, err := s.Store.ListStreams()
			if err != nil {
				errorResponse(w, 500, "failed to list streams: "+err.Error())
				return
			}

			usedPorts := make(map[int]bool)
			for _, str := range streams {
				usedPorts[str.ListenPort] = true
			}

			var candidates []int
			for p := 30000; p <= 30100; p++ {
				if !usedPorts[p] {
					candidates = append(candidates, p)
				}
			}

			if len(candidates) == 0 {
				errorResponse(w, 500, "no available ports in range 30000-30100")
				return
			}

			stream.ListenPort = candidates[rand.Intn(len(candidates))]
		}

		if stream.ID == "" {
			// Generate ID
			stream.ID = fmt.Sprintf("stream-%d", stream.ListenPort)
		}
		if stream.Protocol == "" {
			stream.Protocol = "tcp"
		}

		stream.CreatedAt = time.Now()
		stream.UpdatedAt = time.Now()
		stream.Status = "provisioning"

		if err := s.Store.SaveStream(&stream); err != nil {
			errorResponse(w, 500, err.Error())
			return
		}

		streamCopy := stream
		go s.provisionStream(&streamCopy)

		jsonResponse(w, 201, stream)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleStreamDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/streams/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		stream, err := s.Store.GetStream(id)
		if err != nil {
			errorResponse(w, 404, "stream not found")
			return
		}
		jsonResponse(w, 200, stream)
	case http.MethodDelete:
		// Get stream to know the port
		stream, err := s.Store.GetStream(id)
		if err != nil {
			errorResponse(w, 404, "stream not found")
			return
		}
		port := stream.ListenPort

		if err := s.Store.DeleteStream(id); err != nil {
			errorResponse(w, 500, err.Error())
			return
		}

		// Reconcile Nginx Config for this port
		go s.reconcileStreams(port)

		jsonResponse(w, 200, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) reconcileStreams(port int) {
	slog.Info("Reconciling streams", "port", port)

	// 1. List all streams
	allStreams, err := s.Store.ListStreams()
	if err != nil {
		slog.Error("reconcile error: failed to list streams", "error", err)
		return
	}

	// 2. Filter by port
	var portStreams []models.Stream
	for _, str := range allStreams {
		if str.ListenPort == port {
			resolved, resolveErr := s.UpstreamResolver.Resolve(str.Upstream, 0)
			if resolveErr != nil {
				s.updateStreamStatus(str.ID, "error", "failed to resolve upstream: "+resolveErr.Error())
				continue
			}
			if str.Upstream != resolved {
				str.Upstream = resolved
				str.UpdatedAt = time.Now()
				if saveErr := s.Store.SaveStream(&str); saveErr != nil {
					slog.Warn("failed to persist resolved stream upstream", "id", str.ID, "error", saveErr)
				}
			}
			portStreams = append(portStreams, str)
		}
	}
	slog.Debug("Found streams for port", "port", port, "count", len(portStreams))

	// 3. Rebuild Config
	if err := s.Nginx.RebuildStreamConfig(port, portStreams); err != nil {
		slog.Error("reconcile error: failed to rebuild config", "port", port, "error", err)
		// Update status for all affected streams?
		// For MVP, we log. In production, we should update status of all portStreams to 'error'.
		return
	}

	// Success: Update status of these streams to active
	for _, str := range portStreams {
		if str.Status != "active" {
			s.updateStreamStatus(str.ID, "active", "")
		}
	}
	slog.Info("Stream reconciliation complete", "port", port)
}

func (s *Server) provisionStream(stream *models.Stream) {
	// Deprecated: use reconcileStreams
	s.reconcileStreams(stream.ListenPort)
}

func (s *Server) updateStreamStatus(id, status, msg string) {
	stream, err := s.Store.GetStream(id)
	if err != nil {
		return
	}
	stream.Status = status
	stream.ErrorMessage = msg
	stream.UpdatedAt = time.Now()
	s.Store.SaveStream(stream)
}

func (s *Server) certRetryLoop() {
	s.sweepCertRetries()

	ticker := time.NewTicker(certRetrySweepInterval)
	defer ticker.Stop()

	for range ticker.C {
		s.sweepCertRetries()
	}
}

func (s *Server) sweepCertRetries() {
	sites, err := s.Store.ListSites()
	if err != nil {
		slog.Error("Failed to list sites for cert retry sweep", "error", err)
		return
	}

	now := time.Now()
	for _, site := range sites {
		if !site.SSL || site.CertIssueStatus != "retrying" || site.NextCertRetryAt == nil {
			continue
		}
		if now.Before(*site.NextCertRetryAt) {
			continue
		}
		if !s.tryStartRetry(site.ID) {
			continue
		}

		siteID := site.ID
		go func() {
			defer s.finishRetry(siteID)
			s.retryCertificate(siteID)
		}()
	}
}

func (s *Server) tryStartRetry(siteID string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()

	if s.retrying[siteID] {
		return false
	}
	s.retrying[siteID] = true
	return true
}

func (s *Server) finishRetry(siteID string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	delete(s.retrying, siteID)
}

func nextCertRetryDelay(retryCount int, reason string) time.Duration {
	if retryCount < 1 {
		retryCount = 1
	}

	idx := retryCount - 1
	if idx >= len(certRetrySchedule) {
		idx = len(certRetrySchedule) - 1
	}

	delay := certRetrySchedule[idx]
	lowerReason := strings.ToLower(reason)
	if strings.Contains(lowerReason, "rate limit") || strings.Contains(lowerReason, "too many requests") {
		if delay < 12*time.Hour {
			delay = 12 * time.Hour
		}
	}

	jitter := time.Duration(rand.Intn(30)) * time.Second
	return delay + jitter
}

func (s *Server) markCertRetryNeeded(siteID, reason string) {
	site, err := s.Store.GetSite(siteID)
	if err != nil {
		slog.Error("Failed to load site for cert retry", "site_id", siteID, "error", err)
		return
	}

	site.CertRetryCount++
	site.LastCertError = reason
	site.UpdatedAt = time.Now()

	maxRetries := len(certRetrySchedule)
	if site.CertRetryCount > maxRetries {
		site.CertIssueStatus = "failed"
		site.NextCertRetryAt = nil
		site.Status = "active"
		site.ErrorMessage = "certificate issuance failed permanently; fallback certificate in use: " + reason
		if err := s.Store.SaveSite(site); err != nil {
			slog.Error("Failed to persist terminal certificate failure state", "site_id", siteID, "error", err)
			return
		}
		go s.refreshSiteConfig(site)
		return
	}

	delay := nextCertRetryDelay(site.CertRetryCount, reason)
	nextAttempt := time.Now().Add(delay)
	site.CertIssueStatus = "retrying"
	site.NextCertRetryAt = &nextAttempt
	site.Status = "active"
	site.ErrorMessage = fmt.Sprintf(
		"certificate issuance failed, retry %d/%d at %s: %s",
		site.CertRetryCount,
		maxRetries,
		nextAttempt.UTC().Format(time.RFC3339),
		reason,
	)
	if err := s.Store.SaveSite(site); err != nil {
		slog.Error("Failed to persist certificate retry state", "site_id", siteID, "error", err)
		return
	}

	go s.refreshSiteConfig(site)
}

func (s *Server) clearCertRetryState(siteID string) {
	site, err := s.Store.GetSite(siteID)
	if err != nil {
		return
	}

	site.CertIssueStatus = "valid"
	site.CertRetryCount = 0
	site.NextCertRetryAt = nil
	site.LastCertError = ""
	site.ErrorMessage = ""
	site.UpdatedAt = time.Now()
	if err := s.Store.SaveSite(site); err != nil {
		slog.Error("Failed to clear certificate retry state", "site_id", siteID, "error", err)
	}
}

func (s *Server) issueCertificate(domain string) error {
	if err := s.Certbot.Issue(domain); err != nil {
		return err
	}

	if !s.Nginx.HasDomainCertificate(domain) {
		return fmt.Errorf("certificate files missing after issuance for domain %s", domain)
	}

	return nil
}

func (s *Server) retryCertificate(siteID string) {
	site, err := s.Store.GetSite(siteID)
	if err != nil {
		return
	}
	if !site.SSL {
		return
	}

	slog.Info("Retrying certificate issuance", "site_id", site.ID, "domain", site.Domain, "retry_count", site.CertRetryCount)
	s.updateStatus(site.ID, "provisioning", "retrying certificate issuance")

	if err := s.issueCertificate(site.Domain); err != nil {
		slog.Warn("Certificate retry failed", "site_id", site.ID, "domain", site.Domain, "error", err)
		s.markCertRetryNeeded(site.ID, err.Error())
		return
	}

	s.clearCertRetryState(site.ID)

	updatedSite, err := s.Store.GetSite(site.ID)
	if err != nil {
		return
	}
	s.refreshSiteConfig(updatedSite)
}

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sites, err := s.Store.ListSites()
		if err != nil {
			errorResponse(w, 500, err.Error())
			return
		}
		jsonResponse(w, 200, sites)
	case http.MethodPost:
		var site models.Site
		if err := json.NewDecoder(r.Body).Decode(&site); err != nil {
			errorResponse(w, 400, "invalid json")
			return
		}
		normalizedUpstreams, err := s.normalizeSiteUpstreams(site.Upstreams)
		if err != nil {
			errorResponse(w, 400, err.Error())
			return
		}
		site.Upstreams = normalizedUpstreams
		if site.ID == "" {
			site.ID = site.Domain // Simple ID generation
		}
		site.CreatedAt = time.Now()
		site.UpdatedAt = time.Now()
		site.Status = "provisioning"
		if site.SSL {
			site.CertIssueStatus = "pending"
		}

		// save initial state
		if err := s.Store.SaveSite(&site); err != nil {
			errorResponse(w, 500, err.Error())
			return
		}

		// Apply Nginx Config (async)
		// We pass a copy to avoid race with jsonResponse which reads 'site'
		siteCopy := site
		go s.provisionSite(&siteCopy)

		jsonResponse(w, 201, site)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleSiteDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/sites/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	if strings.HasSuffix(id, "/logs") {
		realID := strings.TrimSuffix(id, "/logs")
		s.handleSiteLogs(w, r, realID)
		return
	}

	if strings.HasSuffix(id, "/cert/retry") {
		realID := strings.TrimSuffix(id, "/cert/retry")
		s.handleSiteCertRetry(w, r, realID)
		return
	}

	if strings.HasSuffix(id, "/firewall") {
		realID := strings.TrimSuffix(id, "/firewall")
		s.handleSiteFirewall(w, r, realID)
		return
	}

	switch r.Method {
	case http.MethodGet:
		site, err := s.Store.GetSite(id)
		if err != nil {
			errorResponse(w, 404, "site not found")
			return
		}
		jsonResponse(w, 200, site)
	case http.MethodDelete:
		// Check if revoke requested
		revoke := r.URL.Query().Get("revoke_cert") == "true"

		site, err := s.Store.GetSite(id)
		if err != nil {
			errorResponse(w, 404, "site not found")
			return
		}

		if revoke && site.SSL {
			if err := s.Certbot.Revoke(site.Domain); err != nil {
				slog.Error("Failed to revoke cert", "domain", site.Domain, "error", err)
				// continue to delete
			}
		}

		if err := s.Nginx.Delete(id); err != nil {
			errorResponse(w, 500, "failed to remove nginx config: "+err.Error())
			return
		}

		if err := s.Store.DeleteSite(id); err != nil {
			errorResponse(w, 500, err.Error())
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "deleted"})
	case http.MethodPatch:
		// Decode partial update
		var input struct {
			Domain          *string                `json:"domain"`
			Upstreams       []string               `json:"upstreams"`
			ForceSSL        *bool                  `json:"force_ssl"`
			SSL             *bool                  `json:"ssl"`
			ExtraConfig     *string                `json:"extra_config"`
			ProxySetHeaders map[string]string      `json:"proxy_set_header"`
			Firewall        *models.FirewallConfig `json:"firewall"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			errorResponse(w, 400, "invalid json")
			return
		}

		site, err := s.Store.GetSite(id)
		if err != nil {
			errorResponse(w, 404, "site not found")
			return
		}

		// Detect if we need full re-provisioning (cert issuance) or just config reload
		needsFullProvision := false

		if input.Domain != nil && *input.Domain != site.Domain {
			site.Domain = *input.Domain
			needsFullProvision = true
		}
		if input.SSL != nil && *input.SSL != site.SSL {
			site.SSL = *input.SSL
			needsFullProvision = true

			if !site.SSL {
				site.CertIssueStatus = ""
				site.CertRetryCount = 0
				site.NextCertRetryAt = nil
				site.LastCertError = ""
				site.ErrorMessage = ""
			}
		}

		// Apply other updates
		if input.Upstreams != nil {
			normalizedUpstreams, resolveErr := s.normalizeSiteUpstreams(input.Upstreams)
			if resolveErr != nil {
				errorResponse(w, 400, resolveErr.Error())
				return
			}
			site.Upstreams = normalizedUpstreams
		}
		if input.ForceSSL != nil {
			site.ForceSSL = *input.ForceSSL
		}
		if input.ExtraConfig != nil {
			site.ExtraConfig = *input.ExtraConfig
		}
		if input.ProxySetHeaders != nil {
			site.ProxySetHeaders = input.ProxySetHeaders
		}
		if input.Firewall != nil {
			site.Firewall = input.Firewall
		}

		if site.SSL && needsFullProvision {
			site.CertIssueStatus = "pending"
			site.CertRetryCount = 0
			site.NextCertRetryAt = nil
			site.LastCertError = ""
			site.ErrorMessage = ""
		}

		site.UpdatedAt = time.Now()

		if err := s.Store.SaveSite(site); err != nil {
			errorResponse(w, 500, err.Error())
			return
		}

		siteCopy := *site
		if needsFullProvision {
			go s.provisionSite(&siteCopy)
		} else {
			go s.refreshSiteConfig(&siteCopy)
		}

		jsonResponse(w, 200, site)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) refreshSiteConfig(site *models.Site) {
	slog.Info("Refreshing site config", "site_id", site.ID, "domain", site.Domain)
	if err := s.ensureResolvedSiteUpstreams(site); err != nil {
		s.updateStatus(site.ID, "error", "failed to resolve upstream(s): "+err.Error())
		return
	}
	preserveMessage := ""
	if currentSite, err := s.Store.GetSite(site.ID); err == nil {
		if currentSite.CertIssueStatus == "retrying" || currentSite.CertIssueStatus == "failed" {
			preserveMessage = currentSite.ErrorMessage
		}
	}

	provisioningMessage := "refreshing config"
	if preserveMessage != "" {
		provisioningMessage = preserveMessage
	}
	s.updateStatus(site.ID, "provisioning", provisioningMessage)

	config, err := s.Nginx.GenerateConfig(site)
	if err != nil {
		slog.Error("Config generation failed", "site_id", site.ID, "error", err)
		s.updateStatus(site.ID, "error", "config gen failed: "+err.Error())
		return
	}

	if err := s.Nginx.Validate(config); err != nil {
		slog.Error("Config validation failed", "site_id", site.ID, "error", err)
		s.updateStatus(site.ID, "error", "config invalid: "+err.Error())
		return
	}

	if err := s.Nginx.Apply(site.ID, config); err != nil {
		slog.Error("Config application failed", "site_id", site.ID, "error", err)
		s.updateStatus(site.ID, "error", "apply failed: "+err.Error())
		return
	}

	slog.Info("Site config refreshed successfully", "site_id", site.ID)
	s.updateStatus(site.ID, "active", preserveMessage)
}

func (s *Server) provisionSite(site *models.Site) {
	slog.Info("Provisioning site", "site_id", site.ID, "domain", site.Domain, "ssl_requested", site.SSL)
	if err := s.ensureResolvedSiteUpstreams(site); err != nil {
		s.updateStatus(site.ID, "error", "failed to resolve upstream(s): "+err.Error())
		return
	}

	// 1. Generate Nginx Config (HTTP)
	// 2. Test & Reload
	// 3. If SSL, Issue Cert -> Regenerate (SSL) -> Reload

	// Initial render (might be HTTP only first if SSL requested but not present)
	// For MVP simplicity, we trust the 'SSL' flag.
	// In real life, we first render HTTP-only to pass challenge, then SSL.

	// Logic:
	// If SSL is requested, we force SSL=false for first pass to ensure Nginx starts and serves challenge.
	// Then we run certbot.
	// Then we set SSL=true and re-render.

	originalSSL := site.SSL
	if originalSSL {
		site.SSL = false // Temporary disable for challenge
	}

	staging, err := s.Nginx.GenerateConfig(site)
	if err != nil {
		slog.Error("Initial config generation failed", "site_id", site.ID, "error", err)
		s.updateStatus(site.ID, "error", "config gen failed: "+err.Error())
		return
	}

	if err := s.Nginx.Validate(staging); err != nil {
		slog.Error("Initial config validation failed", "site_id", site.ID, "error", err)
		s.updateStatus(site.ID, "error", "config invalid: "+err.Error())
		return
	}

	if err := s.Nginx.Apply(site.ID, staging); err != nil {
		slog.Error("Initial config application failed", "site_id", site.ID, "error", err)
		s.updateStatus(site.ID, "error", "apply failed: "+err.Error())
		return
	}

	if !originalSSL {
		slog.Info("Site provisioned (HTTP only)", "site_id", site.ID)
		s.updateStatus(site.ID, "active", "")
		return
	}

	// Handle SSL
	slog.Info("Starting SSL provisioning", "site_id", site.ID, "domain", site.Domain)
	s.updateStatus(site.ID, "provisioning", "issuing certificate")
	trackedSite, err := s.Store.GetSite(site.ID)
	if err != nil {
		slog.Error("Failed to load site before certificate issuance", "site_id", site.ID, "error", err)
		return
	}
	trackedSite.CertIssueStatus = "pending"
	trackedSite.CertRetryCount = 0
	trackedSite.NextCertRetryAt = nil
	trackedSite.LastCertError = ""
	trackedSite.ErrorMessage = ""
	trackedSite.UpdatedAt = time.Now()
	if err := s.Store.SaveSite(trackedSite); err != nil {
		slog.Error(
			"Failed to persist site certificate state reset",
			"site_id", site.ID,
			"cert_issue_status", trackedSite.CertIssueStatus,
			"cert_retry_count", trackedSite.CertRetryCount,
			"next_cert_retry_at", trackedSite.NextCertRetryAt,
			"last_cert_error", trackedSite.LastCertError,
			"error_message", trackedSite.ErrorMessage,
			"updated_at", trackedSite.UpdatedAt,
			"error", err,
		)
		return
	}

	if err := s.issueCertificate(site.Domain); err != nil {
		slog.Error("Certificate issuance failed", "site_id", site.ID, "domain", site.Domain, "error", err)
		s.markCertRetryNeeded(site.ID, err.Error())
		return
	}

	s.clearCertRetryState(site.ID)

	slog.Info("Site provisioned with SSL", "site_id", site.ID)
	updatedSite, err := s.Store.GetSite(site.ID)
	if err != nil {
		slog.Error("Failed to load site after certificate issuance", "site_id", site.ID, "error", err)
		return
	}
	s.refreshSiteConfig(updatedSite)
}

func (s *Server) updateStatus(id, status, msg string) {
	site, err := s.Store.GetSite(id)
	if err != nil {
		return
	}
	site.Status = status
	site.ErrorMessage = msg
	site.UpdatedAt = time.Now()
	s.Store.SaveSite(site)
}

func jsonResponse(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func errorResponse(w http.ResponseWriter, code int, msg string) {
	jsonResponse(w, code, map[string]interface{}{
		"error": msg,
		"code":  code,
	})
}

func (s *Server) handleSiteLogs(w http.ResponseWriter, r *http.Request, siteID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Parse Query Params
	logType := r.URL.Query().Get("type")
	if logType == "" {
		logType = "access"
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}

	search := r.URL.Query().Get("search")

	var since, until time.Time
	if t := r.URL.Query().Get("since"); t != "" {
		since, _ = time.Parse(time.RFC3339, t)
	}
	if t := r.URL.Query().Get("until"); t != "" {
		until, _ = time.Parse(time.RFC3339, t)
	}

	opts := logmanager.LogOptions{
		Limit:  limit,
		Since:  since,
		Until:  until,
		Search: search,
	}

	if logType == "error" {
		logs, err := s.LogManager.GetErrorLogs(siteID, opts)
		if err != nil {
			errorResponse(w, 500, "failed to read error logs: "+err.Error())
			return
		}
		jsonResponse(w, 200, logs)
	} else {
		logs, err := s.LogManager.GetAccessLogs(siteID, opts)
		if err != nil {
			errorResponse(w, 500, "failed to read access logs: "+err.Error())
			return
		}
		jsonResponse(w, 200, logs)
	}
}

func (s *Server) handleSiteCertRetry(w http.ResponseWriter, r *http.Request, siteID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	site, err := s.Store.GetSite(siteID)
	if err != nil {
		errorResponse(w, 404, "site not found")
		return
	}
	if !site.SSL {
		errorResponse(w, 400, "ssl is disabled for this site")
		return
	}

	if !s.tryStartRetry(site.ID) {
		errorResponse(w, 409, "certificate retry already in progress")
		return
	}

	now := time.Now()
	site.CertIssueStatus = "retrying"
	site.NextCertRetryAt = &now
	site.ErrorMessage = "manual certificate retry requested"
	site.UpdatedAt = now
	if err := s.Store.SaveSite(site); err != nil {
		s.finishRetry(site.ID)
		errorResponse(w, 500, "failed to persist retry state: "+err.Error())
		return
	}

	siteIDCopy := site.ID
	go func() {
		defer s.finishRetry(siteIDCopy)
		s.retryCertificate(siteIDCopy)
	}()

	jsonResponse(w, 202, map[string]interface{}{
		"status":             "retry-started",
		"site_id":            site.ID,
		"cert_issue_status":  site.CertIssueStatus,
		"next_cert_retry_at": site.NextCertRetryAt,
	})
}

func (s *Server) handleSiteFirewall(w http.ResponseWriter, r *http.Request, siteID string) {
	site, err := s.Store.GetSite(siteID)
	if err != nil {
		errorResponse(w, 404, "site not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if site.Firewall == nil {
			jsonResponse(w, 200, models.FirewallConfig{})
			return
		}
		jsonResponse(w, 200, site.Firewall)

	case http.MethodDelete:
		if site.Firewall == nil {
			jsonResponse(w, 200, map[string]string{"status": "no firewall rules to clear"})
			return
		}

		section := r.URL.Query().Get("section")
		switch section {
		case "ip_rules":
			site.Firewall.IPRules = nil
		case "rate_limit":
			site.Firewall.RateLimit = nil
		case "block_rules":
			site.Firewall.BlockRules = nil
		case "all", "":
			site.Firewall = nil
		default:
			errorResponse(w, 400, "invalid section: must be ip_rules, rate_limit, block_rules, or all")
			return
		}

		site.UpdatedAt = time.Now()
		if err := s.Store.SaveSite(site); err != nil {
			errorResponse(w, 500, err.Error())
			return
		}

		// Apply changes
		go s.refreshSiteConfig(site)

		jsonResponse(w, 200, map[string]string{"status": "cleared", "section": section})

	default:
		http.Error(w, "method not allowed", 405)
	}
}
