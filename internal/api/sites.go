package api

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/logmanager"
	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sites, err := s.Store.ListSites()
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, sites)
	case http.MethodPost:
		var site models.Site
		if err := json.NewDecoder(r.Body).Decode(&site); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid json")
			return
		}
		slog.Info("site_create_requested",
			"request_id", requestIDFromContext(r.Context()),
			"site_id", site.ID,
			"domain", site.Domain,
			"ssl", site.SSL,
			"force_ssl", site.ForceSSL,
			"upstreams", site.Upstreams,
		)

		normalizedUpstreams, containers, networks, err := s.normalizeSiteUpstreams(site.Upstreams)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		site.Upstreams = normalizedUpstreams
		site.UpstreamContainers = containers
		site.UpstreamNetworks = networks
		site.LoadBalancing, err = normalizeSiteLoadBalancing(site.LoadBalancing, site.Upstreams)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		if site.ID == "" {
			site.ID = site.Domain
		}
		if err := s.ensureRedirectIDAvailable(site.ID, ""); err != nil {
			errorResponse(w, http.StatusConflict, err.Error())
			return
		}
		if err := s.ensureSourceDomainAvailable(site.Domain, ""); err != nil {
			errorResponse(w, http.StatusConflict, err.Error())
			return
		}
		site.CreatedAt = time.Now()
		site.UpdatedAt = time.Now()
		site.Status = "provisioning"
		site.DeployStatus = "pending"
		if site.SSL {
			site.CertIssueStatus = "pending"
		}

		if err := s.Store.SaveSite(&site); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("site_create_accepted",
			"request_id", requestIDFromContext(r.Context()),
			"site_id", site.ID,
			"domain", site.Domain,
			"ssl", site.SSL,
			"upstreams", site.Upstreams,
			"upstream_containers", site.UpstreamContainers,
			"upstream_networks", site.UpstreamNetworks,
		)

		siteCopy := site
		go s.provisionSite(&siteCopy)
		jsonResponse(w, http.StatusCreated, site)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSiteDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/sites/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(id, "/logs/stream") {
		s.handleSiteLogsStream(w, r, strings.TrimSuffix(id, "/logs/stream"))
		return
	}
	if strings.HasSuffix(id, "/logs") {
		s.handleSiteLogs(w, r, strings.TrimSuffix(id, "/logs"))
		return
	}
	if strings.HasSuffix(id, "/cert/retry") {
		s.handleSiteCertRetry(w, r, strings.TrimSuffix(id, "/cert/retry"))
		return
	}
	if strings.HasSuffix(id, "/firewall") {
		s.handleSiteFirewall(w, r, strings.TrimSuffix(id, "/firewall"))
		return
	}
	if strings.HasSuffix(id, "/convert-to-redirect") {
		s.handleSiteConvertToRedirect(w, r, strings.TrimSuffix(id, "/convert-to-redirect"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		site, err := s.Store.GetSite(id)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "site not found")
			return
		}
		jsonResponse(w, http.StatusOK, site)
	case http.MethodDelete:
		revoke := r.URL.Query().Get("revoke_cert") == "true"
		site, err := s.Store.GetSite(id)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "site not found")
			return
		}
		if revoke && site.SSL {
			if s.Nginx.IsWildcardDomain(site.Domain) {
				slog.Info("Skipping cert revocation for wildcard-mapped domain", "domain", site.Domain)
			} else if err := s.Certbot.Revoke(site.Domain); err != nil {
				slog.Error("Failed to revoke cert", "domain", site.Domain, "error", err)
			}
		}
		if err := s.Nginx.Delete(id); err != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to remove nginx config: "+err.Error())
			return
		}
		if s.LogManager != nil {
			if err := s.LogManager.DeleteSiteLogs(id); err != nil {
				errorResponse(w, http.StatusInternalServerError, "failed to remove site logs: "+err.Error())
				return
			}
		}
		if err := s.Store.DeleteSite(id); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("site_delete_completed",
			"request_id", requestIDFromContext(r.Context()),
			"site_id", id,
			"domain", site.Domain,
			"revoke_cert", revoke,
		)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
	case http.MethodPatch:
		var input struct {
			Domain          *string                `json:"domain"`
			Upstreams       []string               `json:"upstreams"`
			LoadBalancing   *models.LoadBalancing  `json:"load_balancing"`
			ForceSSL        *bool                  `json:"force_ssl"`
			SSL             *bool                  `json:"ssl"`
			ExtraConfig     *string                `json:"extra_config"`
			ProxySetHeaders map[string]string      `json:"proxy_set_header"`
			Firewall        *models.FirewallConfig `json:"firewall"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid json")
			return
		}

		site, err := s.Store.GetSite(id)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "site not found")
			return
		}

		needsFullProvision := false
		if input.Domain != nil && *input.Domain != site.Domain {
			if err := s.ensureSourceDomainAvailable(*input.Domain, site.ID); err != nil {
				errorResponse(w, http.StatusConflict, err.Error())
				return
			}
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
		if input.Upstreams != nil {
			normalizedUpstreams, containers, networks, resolveErr := s.normalizeSiteUpstreams(input.Upstreams)
			if resolveErr != nil {
				errorResponse(w, http.StatusBadRequest, resolveErr.Error())
				return
			}
			site.Upstreams = normalizedUpstreams
			site.UpstreamContainers = containers
			site.UpstreamNetworks = networks
		}
		if input.LoadBalancing != nil {
			site.LoadBalancing = input.LoadBalancing
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

		site.LoadBalancing, err = normalizeSiteLoadBalancing(site.LoadBalancing, site.Upstreams)
		if err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		site.DeployStatus = "pending"
		site.DeployError = ""
		site.UpdatedAt = time.Now()

		if err := s.Store.SaveSite(site); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		slog.Info("site_patch_accepted",
			"request_id", requestIDFromContext(r.Context()),
			"site_id", site.ID,
			"domain", site.Domain,
			"ssl", site.SSL,
			"force_ssl", site.ForceSSL,
			"needs_full_provision", needsFullProvision,
			"upstreams", site.Upstreams,
			"upstream_containers", site.UpstreamContainers,
			"upstream_networks", site.UpstreamNetworks,
		)

		siteCopy := *site
		if needsFullProvision {
			go s.provisionSite(&siteCopy)
		} else {
			go s.refreshSiteConfig(&siteCopy)
		}
		jsonResponse(w, http.StatusOK, site)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) refreshSiteConfig(site *models.Site) {
	s.refreshSiteConfigWithReload(site, true)
}

func (s *Server) refreshSiteConfigWithReload(site *models.Site, reload bool) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	slog.Info("Refreshing site config", "site_id", site.ID, "domain", site.Domain)
	runtimeSite := *site
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
	s.updateSiteDeployState(site.ID, "pending", "")

	config, err := s.Nginx.GenerateConfig(&runtimeSite)
	if err != nil {
		slog.Error("Config generation failed", "site_id", site.ID, "error", err)
		s.updateSiteDeployFailure(site.ID, "config gen failed: "+err.Error())
		s.updateStatus(site.ID, "error", "config gen failed: "+err.Error())
		return
	}
	defer func() {
		if removeErr := os.Remove(config); removeErr != nil && !os.IsNotExist(removeErr) {
			slog.Warn("failed_to_remove_site_staging_config", "site_id", site.ID, "file", config, "error", removeErr)
		}
	}()
	configBytes, err := os.ReadFile(config)
	if err != nil {
		s.updateSiteDeployFailure(site.ID, "config read failed: "+err.Error())
		s.updateStatus(site.ID, "error", "config read failed: "+err.Error())
		return
	}
	if err := s.validateHTTPConfigCandidate(site.ID, configBytes); err != nil {
		slog.Error("Config validation failed", "site_id", site.ID, "error", err)
		s.updateSiteDeployFailure(site.ID, "config invalid: "+err.Error())
		if currentSite, getErr := s.Store.GetSite(site.ID); getErr == nil && siteHasLiveConfig(currentSite) {
			s.updateStatus(site.ID, "active", preserveMessage)
			return
		}
		s.updateStatus(site.ID, "error", "config invalid: "+err.Error())
		return
	}

	var applyErr error
	if reload {
		applyErr = s.Nginx.ApplyRendered(site.ID, configBytes)
	} else {
		applyErr = s.Nginx.ApplyRenderedNoReload(site.ID, configBytes)
	}
	if applyErr != nil {
		slog.Error("Config application failed", "site_id", site.ID, "error", applyErr)
		s.updateSiteDeployFailure(site.ID, "apply failed: "+applyErr.Error())
		s.updateStatus(site.ID, "error", "apply failed: "+applyErr.Error())
		return
	}
	s.updateSiteActiveConfig(site.ID, configBytes)

	slog.Info("Site config refreshed successfully", "site_id", site.ID)
	s.updateStatus(site.ID, "active", preserveMessage)
	s.updateSiteDeployState(site.ID, "active", "")
}

func (s *Server) provisionSite(site *models.Site) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	slog.Info("Provisioning site", "site_id", site.ID, "domain", site.Domain, "ssl_requested", site.SSL)
	runtimeSite := *site
	originalSSL := runtimeSite.SSL
	if originalSSL {
		runtimeSite.SSL = false
	}
	s.updateSiteDeployState(site.ID, "pending", "")

	staging, err := s.Nginx.GenerateConfig(&runtimeSite)
	if err != nil {
		slog.Error("Initial config generation failed", "site_id", site.ID, "error", err)
		s.updateSiteDeployFailure(site.ID, "config gen failed: "+err.Error())
		s.updateStatus(site.ID, "error", "config gen failed: "+err.Error())
		return
	}
	defer func() {
		if removeErr := os.Remove(staging); removeErr != nil && !os.IsNotExist(removeErr) {
			slog.Warn("failed_to_remove_site_staging_config", "site_id", site.ID, "file", staging, "error", removeErr)
		}
	}()
	stagingBytes, err := os.ReadFile(staging)
	if err != nil {
		s.updateSiteDeployFailure(site.ID, "config read failed: "+err.Error())
		s.updateStatus(site.ID, "error", "config read failed: "+err.Error())
		return
	}
	if err := s.validateHTTPConfigCandidate(site.ID, stagingBytes); err != nil {
		slog.Error("Initial config validation failed", "site_id", site.ID, "error", err)
		s.updateSiteDeployFailure(site.ID, "config invalid: "+err.Error())
		s.updateStatus(site.ID, "error", "config invalid: "+err.Error())
		return
	}
	if err := s.Nginx.ApplyRendered(site.ID, stagingBytes); err != nil {
		slog.Error("Initial config application failed", "site_id", site.ID, "error", err)
		s.updateSiteDeployFailure(site.ID, "apply failed: "+err.Error())
		s.updateStatus(site.ID, "error", "apply failed: "+err.Error())
		return
	}
	s.updateSiteActiveConfig(site.ID, stagingBytes)
	if !originalSSL {
		slog.Info("Site provisioned (HTTP only)", "site_id", site.ID)
		s.updateStatus(site.ID, "active", "")
		s.updateSiteDeployState(site.ID, "active", "")
		return
	}

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
		slog.Error("Failed to persist site certificate state reset",
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
		s.updateSiteDeployFailure(site.ID, "certificate issue failed: "+err.Error())
		s.markCertRetryNeeded(site.ID, err.Error())
		return
	}

	s.clearCertRetryState(site.ID)
	slog.Info("Site provisioned with SSL", "site_id", site.ID)
	updatedSite, err := s.Store.GetSite(site.ID)
	if err != nil {
		return
	}
	sslConfigFile, err := s.Nginx.GenerateConfig(updatedSite)
	if err != nil {
		s.updateSiteDeployFailure(site.ID, "config gen failed: "+err.Error())
		s.updateStatus(site.ID, "error", "config gen failed: "+err.Error())
		return
	}
	defer func() {
		if removeErr := os.Remove(sslConfigFile); removeErr != nil && !os.IsNotExist(removeErr) {
			slog.Warn("failed_to_remove_site_staging_config", "site_id", site.ID, "file", sslConfigFile, "error", removeErr)
		}
	}()
	sslConfigBytes, err := os.ReadFile(sslConfigFile)
	if err != nil {
		s.updateSiteDeployFailure(site.ID, "config read failed: "+err.Error())
		s.updateStatus(site.ID, "error", "config read failed: "+err.Error())
		return
	}
	if err := s.validateHTTPConfigCandidate(site.ID, sslConfigBytes); err != nil {
		s.updateSiteDeployFailure(site.ID, "config invalid: "+err.Error())
		s.updateStatus(site.ID, "active", "certificate issued but ssl config invalid: "+err.Error())
		return
	}
	if err := s.Nginx.ApplyRendered(site.ID, sslConfigBytes); err != nil {
		s.updateSiteDeployFailure(site.ID, "apply failed: "+err.Error())
		s.updateStatus(site.ID, "error", "apply failed: "+err.Error())
		return
	}
	s.updateSiteActiveConfig(site.ID, sslConfigBytes)
	s.updateStatus(site.ID, "active", "")
	s.updateSiteDeployState(site.ID, "active", "")
}

func (s *Server) updateStatus(id, status, msg string) {
	site, err := s.Store.GetSite(id)
	if err != nil {
		slog.Warn("site_status_update_skipped_site_missing", "site_id", id, "status", status, "error", err)
		return
	}
	prevStatus := site.Status
	prevMsg := site.ErrorMessage
	site.Status = status
	site.ErrorMessage = msg
	site.UpdatedAt = time.Now()
	if err := s.Store.SaveSite(site); err != nil {
		slog.Error("site_status_update_failed", "site_id", id, "status", status, "error", err)
		return
	}
	slog.Info("site_status_updated",
		"site_id", id,
		"previous_status", prevStatus,
		"new_status", status,
		"previous_error_message", prevMsg,
		"new_error_message", msg,
	)
}

func (s *Server) updateSiteDeployState(id, deployStatus, deployError string) {
	site, err := s.Store.GetSite(id)
	if err != nil {
		return
	}
	site.DeployStatus = deployStatus
	site.DeployError = deployError
	site.UpdatedAt = time.Now()
	_ = s.Store.SaveSite(site)
}

func (s *Server) updateSiteDeployFailure(id, message string) {
	site, err := s.Store.GetSite(id)
	if err != nil {
		return
	}
	site.DeployStatus = "invalid"
	site.DeployError = message
	site.UpdatedAt = time.Now()
	if !siteHasLiveConfig(site) {
		site.Status = "error"
		site.ErrorMessage = message
	}
	_ = s.Store.SaveSite(site)
}

func (s *Server) updateSiteActiveConfig(id string, config []byte) {
	site, err := s.Store.GetSite(id)
	if err != nil {
		return
	}
	site.ActiveConfig = string(config)
	site.DeployStatus = "active"
	site.DeployError = ""
	site.UpdatedAt = time.Now()
	_ = s.Store.SaveSite(site)
}

func (s *Server) validateHTTPConfigCandidate(id string, candidate []byte) error {
	configs, err := s.activeHTTPConfigSet(id, candidate)
	if err != nil {
		return err
	}
	return s.Nginx.ValidateHTTPConfigSet(configs)
}

func (s *Server) handleSiteLogs(w http.ResponseWriter, r *http.Request, siteID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logType := r.URL.Query().Get("type")
	if logType == "" {
		logType = "access"
	}

	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
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

	opts := logmanager.LogOptions{Limit: limit, Since: since, Until: until, Search: search}
	if logType == "error" {
		logs, err := s.LogManager.GetErrorLogs(siteID, opts)
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to read error logs: "+err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, logs)
		return
	}

	logs, err := s.LogManager.GetAccessLogs(siteID, opts)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to read access logs: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, logs)
}

func (s *Server) handleSiteLogsStream(w http.ResponseWriter, r *http.Request, siteID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.LogManager == nil {
		errorResponse(w, http.StatusServiceUnavailable, "log manager not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	logType := r.URL.Query().Get("type")
	if logType == "" {
		logType = "access"
	}
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}
	search := r.URL.Query().Get("search")
	var since time.Time
	if t := r.URL.Query().Get("since"); t != "" {
		since, _ = time.Parse(time.RFC3339, t)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(data interface{}) bool {
		payload, err := json.Marshal(data)
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(payload); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Optional backfill
	if !since.IsZero() {
		opts := logmanager.LogOptions{Limit: limit, Since: since, Search: search}
		if logType == "error" {
			logs, err := s.LogManager.GetErrorLogs(siteID, opts)
			if err != nil {
				errorResponse(w, http.StatusInternalServerError, "failed to read error logs: "+err.Error())
				return
			}
			for i := len(logs) - 1; i >= 0; i-- {
				if !send(logs[i]) {
					return
				}
			}
		} else {
			logs, err := s.LogManager.GetAccessLogs(siteID, opts)
			if err != nil {
				errorResponse(w, http.StatusInternalServerError, "failed to read access logs: "+err.Error())
				return
			}
			for i := len(logs) - 1; i >= 0; i-- {
				if !send(logs[i]) {
					return
				}
			}
		}
	}

	logPath := s.LogManager.AccessLogPath(siteID)
	if logType == "error" {
		logPath = s.LogManager.ErrorLogPath(siteID)
	}

	ctx := r.Context()
	var file *os.File
	openFile := func() (*os.File, error) {
		for {
			f, err := os.Open(logPath)
			if err == nil {
				return f, nil
			}
			if !os.IsNotExist(err) {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	var err error
	file, err = openFile()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to open log file: "+err.Error())
		return
	}
	defer file.Close()

	if since.IsZero() {
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to seek log file: "+err.Error())
			return
		}
	}

	reader := bufio.NewReader(file)
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(":\n\n")); err != nil {
				return
			}
			flusher.Flush()
		default:
		}

		line, err := reader.ReadString('\n')
		if err == io.EOF {
			if info, statErr := file.Stat(); statErr == nil {
				pos, _ := file.Seek(0, io.SeekCurrent)
				if info.Size() < pos {
					_, _ = file.Seek(0, io.SeekStart)
					reader.Reset(file)
				}
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}

		line = strings.TrimRight(line, "\n")
		if line == "" {
			continue
		}
		if search != "" && !strings.Contains(line, search) {
			continue
		}

		if logType == "error" {
			entry, ok := logmanager.ParseErrorLogLine(line)
			if ok {
				if !since.IsZero() && !entry.TimeLocal.IsZero() && entry.TimeLocal.Before(since) {
					continue
				}
				if !send(entry) {
					return
				}
				continue
			}
			if !send(map[string]string{"raw": line}) {
				return
			}
			continue
		}

		entry, ok := logmanager.ParseAccessLogLine(line)
		if ok {
			if !since.IsZero() && entry.TimeLocal.Before(since) {
				continue
			}
			if !send(entry) {
				return
			}
			continue
		}
		if !send(map[string]string{"raw": line}) {
			return
		}
	}
}

func (s *Server) handleSiteCertRetry(w http.ResponseWriter, r *http.Request, siteID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	site, err := s.Store.GetSite(siteID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "site not found")
		return
	}
	if !site.SSL {
		errorResponse(w, http.StatusBadRequest, "ssl is disabled for this site")
		return
	}
	if !s.tryStartRetry(site.ID) {
		errorResponse(w, http.StatusConflict, "certificate retry already in progress")
		return
	}

	now := time.Now()
	site.CertIssueStatus = "retrying"
	site.NextCertRetryAt = &now
	site.ErrorMessage = "manual certificate retry requested"
	site.UpdatedAt = now
	if err := s.Store.SaveSite(site); err != nil {
		s.finishRetry(site.ID)
		errorResponse(w, http.StatusInternalServerError, "failed to persist retry state: "+err.Error())
		return
	}
	slog.Info("manual_cert_retry_requested",
		"request_id", requestIDFromContext(r.Context()),
		"site_id", site.ID,
		"domain", site.Domain,
	)

	siteIDCopy := site.ID
	go func() {
		defer s.finishRetry(siteIDCopy)
		s.retryCertificate(siteIDCopy)
	}()

	jsonResponse(w, http.StatusAccepted, map[string]interface{}{
		"status":             "retry-started",
		"site_id":            site.ID,
		"cert_issue_status":  site.CertIssueStatus,
		"next_cert_retry_at": site.NextCertRetryAt,
	})
}

func (s *Server) handleSiteFirewall(w http.ResponseWriter, r *http.Request, siteID string) {
	site, err := s.Store.GetSite(siteID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "site not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if site.Firewall == nil {
			jsonResponse(w, http.StatusOK, models.FirewallConfig{})
			return
		}
		jsonResponse(w, http.StatusOK, site.Firewall)
	case http.MethodDelete:
		if site.Firewall == nil {
			jsonResponse(w, http.StatusOK, map[string]string{"status": "no firewall rules to clear"})
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
			errorResponse(w, http.StatusBadRequest, "invalid section: must be ip_rules, rate_limit, block_rules, or all")
			return
		}

		site.UpdatedAt = time.Now()
		if err := s.Store.SaveSite(site); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		go s.refreshSiteConfig(site)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "cleared", "section": section})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
