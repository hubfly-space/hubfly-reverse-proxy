package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
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
		site.CreatedAt = time.Now()
		site.UpdatedAt = time.Now()
		site.Status = "provisioning"
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

	config, err := s.Nginx.GenerateConfig(&runtimeSite)
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

	var applyErr error
	if reload {
		applyErr = s.Nginx.Apply(site.ID, config)
	} else {
		applyErr = s.Nginx.ApplyNoReload(site.ID, config)
	}
	if applyErr != nil {
		slog.Error("Config application failed", "site_id", site.ID, "error", applyErr)
		s.updateStatus(site.ID, "error", "apply failed: "+applyErr.Error())
		return
	}

	slog.Info("Site config refreshed successfully", "site_id", site.ID)
	s.updateStatus(site.ID, "active", preserveMessage)
}

func (s *Server) provisionSite(site *models.Site) {
	slog.Info("Provisioning site", "site_id", site.ID, "domain", site.Domain, "ssl_requested", site.SSL)
	runtimeSite := *site
	originalSSL := runtimeSite.SSL
	if originalSSL {
		runtimeSite.SSL = false
	}

	staging, err := s.Nginx.GenerateConfig(&runtimeSite)
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
		s.markCertRetryNeeded(site.ID, err.Error())
		return
	}

	s.clearCertRetryState(site.ID)
	slog.Info("Site provisioned with SSL", "site_id", site.ID)
	updatedSite, err := s.Store.GetSite(site.ID)
	if err == nil {
		s.refreshSiteConfig(updatedSite)
	}
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
