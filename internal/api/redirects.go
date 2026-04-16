package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func (s *Server) handleRedirects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		redirects, err := s.Store.ListRedirects()
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, redirects)
	case http.MethodPost:
		var redirect models.Redirect
		if err := json.NewDecoder(r.Body).Decode(&redirect); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := normalizeRedirect(&redirect); err != nil {
			errorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.ensureRedirectIDAvailable(redirect.ID, ""); err != nil {
			errorResponse(w, http.StatusConflict, err.Error())
			return
		}
		if err := s.ensureSourceDomainAvailable(redirect.SourceDomain, ""); err != nil {
			errorResponse(w, http.StatusConflict, err.Error())
			return
		}
		now := time.Now()
		redirect.CreatedAt = now
		redirect.UpdatedAt = now
		redirect.Status = "provisioning"
		redirect.DeployStatus = "pending"
		if err := s.Store.SaveRedirect(&redirect); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		redirectCopy := redirect
		go s.provisionRedirect(&redirectCopy)
		jsonResponse(w, http.StatusCreated, redirect)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRedirectDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/redirects/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		redirect, err := s.Store.GetRedirect(id)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "redirect not found")
			return
		}
		jsonResponse(w, http.StatusOK, redirect)
	case http.MethodDelete:
		if _, err := s.Store.GetRedirect(id); err != nil {
			errorResponse(w, http.StatusNotFound, "redirect not found")
			return
		}
		if err := s.Nginx.Delete(id); err != nil {
			errorResponse(w, http.StatusInternalServerError, "failed to remove nginx config: "+err.Error())
			return
		}
		if s.LogManager != nil {
			if err := s.LogManager.DeleteSiteLogs(id); err != nil {
				errorResponse(w, http.StatusInternalServerError, "failed to remove redirect logs: "+err.Error())
				return
			}
		}
		if err := s.Store.DeleteRedirect(id); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSiteConvertToRedirect(w http.ResponseWriter, r *http.Request, siteID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input struct {
		TargetDomain string `json:"target_domain"`
		SSL          *bool  `json:"ssl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	site, err := s.Store.GetSite(siteID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "site not found")
		return
	}
	redirect := models.Redirect{
		ID:           site.ID,
		SourceDomain: site.Domain,
		TargetDomain: input.TargetDomain,
		SSL:          site.SSL,
		Status:       "provisioning",
		CreatedAt:    site.CreatedAt,
		UpdatedAt:    time.Now(),
	}
	if input.SSL != nil {
		redirect.SSL = *input.SSL
	}
	if err := normalizeRedirect(&redirect); err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.ensureRedirectIDAvailable(redirect.ID, site.ID); err != nil {
		errorResponse(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.ensureSourceDomainAvailable(redirect.SourceDomain, site.ID); err != nil {
		errorResponse(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.Store.SaveRedirect(&redirect); err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if err := s.applyRedirect(&redirect, false); err != nil {
		_ = s.Store.DeleteRedirect(redirect.ID)
		errorResponse(w, http.StatusInternalServerError, "failed to convert site to redirect: "+err.Error())
		return
	}
	if redirect.SSL && !site.SSL {
		if err := s.issueCertificate(redirect.SourceDomain); err != nil {
			_ = s.Store.DeleteRedirect(redirect.ID)
			errorResponse(w, http.StatusInternalServerError, "failed to issue redirect certificate: "+err.Error())
			return
		}
	}
	if redirect.SSL {
		if err := s.applyRedirect(&redirect, true); err != nil {
			_ = s.Store.DeleteRedirect(redirect.ID)
			errorResponse(w, http.StatusInternalServerError, "failed to enable redirect ssl: "+err.Error())
			return
		}
	}
	if err := s.Store.DeleteSite(site.ID); err != nil {
		errorResponse(w, http.StatusInternalServerError, "redirect applied but failed to remove original site metadata: "+err.Error())
		return
	}
	redirect.Status = "active"
	redirect.ErrorMessage = ""
	redirect.UpdatedAt = time.Now()
	if err := s.Store.SaveRedirect(&redirect); err != nil {
		errorResponse(w, http.StatusInternalServerError, "redirect applied but failed to persist redirect status: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, redirect)
}

func (s *Server) provisionRedirect(redirect *models.Redirect) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	slog.Info("Provisioning redirect", "redirect_id", redirect.ID, "source_domain", redirect.SourceDomain, "target_domain", redirect.TargetDomain, "ssl_requested", redirect.SSL)
	s.updateRedirectDeployState(redirect.ID, "pending", "")
	if err := s.applyRedirect(redirect, false); err != nil {
		slog.Error("Redirect application failed", "redirect_id", redirect.ID, "error", err)
		s.updateRedirectDeployFailure(redirect.ID, "apply failed: "+err.Error())
		s.updateRedirectStatus(redirect.ID, "error", "apply failed: "+err.Error())
		return
	}
	if !redirect.SSL {
		s.updateRedirectStatus(redirect.ID, "active", "")
		s.updateRedirectDeployState(redirect.ID, "active", "")
		return
	}
	s.updateRedirectStatus(redirect.ID, "provisioning", "issuing certificate")
	if err := s.issueCertificate(redirect.SourceDomain); err != nil {
		slog.Error("Redirect certificate issuance failed", "redirect_id", redirect.ID, "source_domain", redirect.SourceDomain, "error", err)
		s.updateRedirectDeployFailure(redirect.ID, "certificate issue failed: "+err.Error())
		s.updateRedirectStatus(redirect.ID, "error", "certificate issue failed: "+err.Error())
		return
	}
	if err := s.applyRedirect(redirect, true); err != nil {
		slog.Error("Redirect SSL application failed", "redirect_id", redirect.ID, "error", err)
		s.updateRedirectDeployFailure(redirect.ID, "apply failed: "+err.Error())
		s.updateRedirectStatus(redirect.ID, "error", "apply failed: "+err.Error())
		return
	}
	s.updateRedirectStatus(redirect.ID, "active", "")
	s.updateRedirectDeployState(redirect.ID, "active", "")
}

func (s *Server) refreshRedirectConfig(redirect *models.Redirect) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if err := s.applyRedirect(redirect, true); err != nil {
		s.updateRedirectDeployFailure(redirect.ID, "apply failed: "+err.Error())
		s.updateRedirectStatus(redirect.ID, "error", "apply failed: "+err.Error())
		return
	}
	s.updateRedirectStatus(redirect.ID, "active", "")
	s.updateRedirectDeployState(redirect.ID, "active", "")
}

func (s *Server) applyRedirect(redirect *models.Redirect, withSSL bool) error {
	runtimeRedirect := *redirect
	if !withSSL {
		runtimeRedirect.SSL = false
	}
	config, err := s.Nginx.GenerateRedirectConfig(&runtimeRedirect)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(config); removeErr != nil && !os.IsNotExist(removeErr) {
			slog.Warn("failed_to_remove_redirect_staging_config", "redirect_id", redirect.ID, "file", config, "error", removeErr)
		}
	}()
	configBytes, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	if err := s.validateHTTPConfigCandidate(redirect.ID, configBytes); err != nil {
		return err
	}
	if err := s.Nginx.ApplyRendered(redirect.ID, configBytes); err != nil {
		return err
	}
	s.updateRedirectActiveConfig(redirect.ID, configBytes)
	return nil
}

func (s *Server) updateRedirectStatus(id, status, msg string) {
	redirect, err := s.Store.GetRedirect(id)
	if err != nil {
		return
	}
	redirect.Status = status
	redirect.ErrorMessage = msg
	redirect.UpdatedAt = time.Now()
	if err := s.Store.SaveRedirect(redirect); err != nil {
		slog.Error("redirect_status_update_failed", "redirect_id", id, "status", status, "error", err)
	}
}

func (s *Server) updateRedirectDeployState(id, deployStatus, deployError string) {
	redirect, err := s.Store.GetRedirect(id)
	if err != nil {
		return
	}
	redirect.DeployStatus = deployStatus
	redirect.DeployError = deployError
	redirect.UpdatedAt = time.Now()
	_ = s.Store.SaveRedirect(redirect)
}

func (s *Server) updateRedirectDeployFailure(id, message string) {
	redirect, err := s.Store.GetRedirect(id)
	if err != nil {
		return
	}
	redirect.DeployStatus = "invalid"
	redirect.DeployError = message
	redirect.UpdatedAt = time.Now()
	if !redirectHasLiveConfig(redirect) {
		redirect.Status = "error"
		redirect.ErrorMessage = message
	}
	_ = s.Store.SaveRedirect(redirect)
}

func (s *Server) updateRedirectActiveConfig(id string, config []byte) {
	redirect, err := s.Store.GetRedirect(id)
	if err != nil {
		return
	}
	redirect.ActiveConfig = string(config)
	redirect.DeployStatus = "active"
	redirect.DeployError = ""
	redirect.UpdatedAt = time.Now()
	_ = s.Store.SaveRedirect(redirect)
}

func normalizeRedirect(redirect *models.Redirect) error {
	redirect.ID = strings.TrimSpace(redirect.ID)
	redirect.SourceDomain = strings.ToLower(strings.TrimSpace(redirect.SourceDomain))
	redirect.TargetDomain = strings.ToLower(strings.TrimSpace(redirect.TargetDomain))
	if redirect.SourceDomain == "" {
		return fmt.Errorf("source_domain is required")
	}
	if redirect.TargetDomain == "" {
		return fmt.Errorf("target_domain is required")
	}
	if strings.Contains(redirect.SourceDomain, "/") || strings.Contains(redirect.TargetDomain, "/") {
		return fmt.Errorf("domains must be hostnames only")
	}
	if redirect.SourceDomain == redirect.TargetDomain {
		return fmt.Errorf("source_domain and target_domain must differ")
	}
	if redirect.ID == "" {
		redirect.ID = redirect.SourceDomain
	}
	return nil
}

func (s *Server) ensureSourceDomainAvailable(domain, skipSiteID string) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	for _, site := range sites {
		if site.ID == skipSiteID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(site.Domain), domain) {
			return fmt.Errorf("domain %s is already used by site %s", domain, site.ID)
		}
	}
	redirects, err := s.Store.ListRedirects()
	if err != nil {
		return err
	}
	for _, redirect := range redirects {
		if redirect.ID == skipSiteID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(redirect.SourceDomain), domain) {
			return fmt.Errorf("domain %s is already used by redirect %s", domain, redirect.ID)
		}
	}
	return nil
}

func (s *Server) ensureRedirectIDAvailable(id, skipID string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("id is required")
	}
	if site, err := s.Store.GetSite(id); err == nil && site != nil && id != skipID {
		return fmt.Errorf("id %s is already used by site %s", id, site.ID)
	}
	if redirect, err := s.Store.GetRedirect(id); err == nil && redirect != nil && id != skipID {
		return fmt.Errorf("id %s is already used by redirect %s", id, redirect.ID)
	}
	return nil
}
