package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func (s *Server) activeHTTPConfigSet(candidateID string, candidate []byte) (map[string][]byte, error) {
	configs := make(map[string][]byte)

	sites, err := s.Store.ListSites()
	if err != nil {
		return nil, err
	}
	for _, site := range sites {
		if strings.TrimSpace(site.ActiveConfig) == "" {
			continue
		}
		normalized := s.Nginx.NormalizeRenderedHTTPConfig(site.ActiveConfig)
		if normalized != site.ActiveConfig {
			site.ActiveConfig = normalized
			_ = s.Store.SaveSite(&site)
		}
		configs[site.ID] = []byte(normalized)
	}

	redirects, err := s.Store.ListRedirects()
	if err != nil {
		return nil, err
	}
	for _, redirect := range redirects {
		if strings.TrimSpace(redirect.ActiveConfig) == "" {
			continue
		}
		normalized := s.Nginx.NormalizeRenderedHTTPConfig(redirect.ActiveConfig)
		if normalized != redirect.ActiveConfig {
			redirect.ActiveConfig = normalized
			_ = s.Store.SaveRedirect(&redirect)
		}
		configs[redirect.ID] = []byte(normalized)
	}

	if candidate != nil {
		configs[candidateID] = candidate
	}
	return configs, nil
}

func (s *Server) seedActiveHTTPConfigsFromDisk() {
	sites, err := s.Store.ListSites()
	if err == nil {
		for _, site := range sites {
			if strings.TrimSpace(site.ActiveConfig) != "" {
				continue
			}
			path := filepath.Join(s.Nginx.SitesDir, site.ID+".conf")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			site.ActiveConfig = string(data)
			site.ActiveConfig = s.Nginx.NormalizeRenderedHTTPConfig(site.ActiveConfig)
			site.Status = "active"
			site.DeployStatus = "active"
			site.DeployError = ""
			_ = s.Store.SaveSite(&site)
		}
	}

	redirects, err := s.Store.ListRedirects()
	if err == nil {
		for _, redirect := range redirects {
			if strings.TrimSpace(redirect.ActiveConfig) != "" {
				continue
			}
			path := filepath.Join(s.Nginx.SitesDir, redirect.ID+".conf")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			redirect.ActiveConfig = string(data)
			redirect.ActiveConfig = s.Nginx.NormalizeRenderedHTTPConfig(redirect.ActiveConfig)
			redirect.Status = "active"
			redirect.DeployStatus = "active"
			redirect.DeployError = ""
			_ = s.Store.SaveRedirect(&redirect)
		}
	}
}

func (s *Server) restoreActiveHTTPConfigs() {
	s.seedActiveHTTPConfigsFromDisk()
	entries, err := os.ReadDir(s.Nginx.SitesDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".conf" {
				continue
			}
			_ = os.Remove(filepath.Join(s.Nginx.SitesDir, entry.Name()))
		}
	}

	configs := make(map[string][]byte)
	sites, err := s.Store.ListSites()
	if err == nil {
		sort.Slice(sites, func(i, j int) bool { return sites[i].ID < sites[j].ID })
		for _, site := range sites {
			if strings.TrimSpace(site.ActiveConfig) == "" {
				continue
			}
			normalized := s.Nginx.NormalizeRenderedHTTPConfig(site.ActiveConfig)
			if normalized != site.ActiveConfig {
				site.ActiveConfig = normalized
				_ = s.Store.SaveSite(&site)
			}
			configs[site.ID] = []byte(normalized)
		}
	}
	redirects, err := s.Store.ListRedirects()
	if err == nil {
		sort.Slice(redirects, func(i, j int) bool { return redirects[i].ID < redirects[j].ID })
		for _, redirect := range redirects {
			if strings.TrimSpace(redirect.ActiveConfig) == "" {
				continue
			}
			normalized := s.Nginx.NormalizeRenderedHTTPConfig(redirect.ActiveConfig)
			if normalized != redirect.ActiveConfig {
				redirect.ActiveConfig = normalized
				_ = s.Store.SaveRedirect(&redirect)
			}
			configs[redirect.ID] = []byte(normalized)
		}
	}
	for id, config := range configs {
		_ = s.Nginx.ApplyRenderedNoReload(id, config)
	}
	if len(configs) > 0 {
		_ = s.Nginx.Reload()
	}
}

func siteHasLiveConfig(site *models.Site) bool {
	return strings.TrimSpace(site.ActiveConfig) != ""
}

func redirectHasLiveConfig(redirect *models.Redirect) bool {
	return strings.TrimSpace(redirect.ActiveConfig) != ""
}
