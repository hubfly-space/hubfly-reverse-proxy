package api

import (
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"
)

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
		if now.Before(*site.NextCertRetryAt) || !s.tryStartRetry(site.ID) {
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
	return delay + time.Duration(rand.Intn(30))*time.Second
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
		site.DeployStatus = "invalid"
		site.DeployError = "certificate issuance failed permanently: " + reason
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
	site.DeployStatus = "pending"
	site.DeployError = fmt.Sprintf("certificate retry %d/%d pending: %s", site.CertRetryCount, maxRetries, reason)
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
	slog.Info("certificate_issue_started", "domain", domain)

	certPath, keyPath, source, exists := s.Nginx.ResolveDomainCertificate(domain)
	if source == "wildcard" {
		if !exists {
			return fmt.Errorf(
				"wildcard certificate configured for domain %s but files are missing: cert=%s key=%s",
				domain, certPath, keyPath,
			)
		}
		slog.Info("certificate_issue_skipped_using_wildcard", "domain", domain, "cert_path", certPath, "key_path", keyPath)
		return nil
	}

	if err := s.Certbot.Issue(domain); err != nil {
		slog.Error("certificate_issue_failed", "domain", domain, "error", err)
		return err
	}
	if !s.Nginx.HasDomainCertificate(domain) {
		slog.Error("certificate_issue_verification_failed", "domain", domain)
		return fmt.Errorf("certificate files missing after issuance for domain %s", domain)
	}
	slog.Info("certificate_issue_succeeded", "domain", domain)
	return nil
}

func (s *Server) retryCertificate(siteID string) {
	site, err := s.Store.GetSite(siteID)
	if err != nil || !site.SSL {
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
	if err == nil {
		s.refreshSiteConfig(updatedSite)
	}
}
