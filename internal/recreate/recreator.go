package recreate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
)

type Result struct {
	Sites       int
	Redirects   int
	Streams     int
	StreamPorts int
}

func Run(st store.Store, nm *nginx.Manager) (Result, error) {
	if err := nm.EnsureDirs(); err != nil {
		return Result{}, err
	}
	if err := pruneConfigs(nm.SitesDir); err != nil {
		return Result{}, fmt.Errorf("prune site configs: %w", err)
	}
	if err := pruneConfigs(nm.StreamsDir); err != nil {
		return Result{}, fmt.Errorf("prune stream configs: %w", err)
	}
	if err := pruneConfigs(nm.StagingDir); err != nil {
		return Result{}, fmt.Errorf("prune staging configs: %w", err)
	}

	sites, err := st.ListSites()
	if err != nil {
		return Result{}, fmt.Errorf("list sites: %w", err)
	}
	sort.Slice(sites, func(i, j int) bool {
		return strings.TrimSpace(sites[i].ID) < strings.TrimSpace(sites[j].ID)
	})

	for i := range sites {
		site := sites[i]
		if strings.TrimSpace(site.ActiveConfig) != "" {
			normalized := nm.NormalizeRenderedHTTPConfig(site.ActiveConfig)
			if err := nm.ApplyRenderedNoReload(site.ID, []byte(normalized)); err != nil {
				return Result{}, fmt.Errorf("apply active site config for %s: %w", site.ID, err)
			}
			continue
		}
		configFile, err := nm.GenerateConfig(&site)
		if err != nil {
			return Result{}, fmt.Errorf("generate site config for %s: %w", site.ID, err)
		}
		if err := nm.Validate(configFile); err != nil {
			return Result{}, fmt.Errorf("validate site config for %s: %w", site.ID, err)
		}
		if err := nm.ApplyNoReload(site.ID, configFile); err != nil {
			return Result{}, fmt.Errorf("apply site config for %s: %w", site.ID, err)
		}
	}

	redirects, err := st.ListRedirects()
	if err != nil {
		return Result{}, fmt.Errorf("list redirects: %w", err)
	}
	sort.Slice(redirects, func(i, j int) bool {
		return strings.TrimSpace(redirects[i].ID) < strings.TrimSpace(redirects[j].ID)
	})
	for i := range redirects {
		redirect := redirects[i]
		if strings.TrimSpace(redirect.ActiveConfig) != "" {
			normalized := nm.NormalizeRenderedHTTPConfig(redirect.ActiveConfig)
			if err := nm.ApplyRenderedNoReload(redirect.ID, []byte(normalized)); err != nil {
				return Result{}, fmt.Errorf("apply active redirect config for %s: %w", redirect.ID, err)
			}
			continue
		}
		configFile, err := nm.GenerateRedirectConfig(&redirect)
		if err != nil {
			return Result{}, fmt.Errorf("generate redirect config for %s: %w", redirect.ID, err)
		}
		if err := nm.Validate(configFile); err != nil {
			return Result{}, fmt.Errorf("validate redirect config for %s: %w", redirect.ID, err)
		}
		if err := nm.ApplyNoReload(redirect.ID, configFile); err != nil {
			return Result{}, fmt.Errorf("apply redirect config for %s: %w", redirect.ID, err)
		}
	}

	streams, err := st.ListStreams()
	if err != nil {
		return Result{}, fmt.Errorf("list streams: %w", err)
	}

	portGroups := make(map[int][]models.Stream)
	for _, stream := range streams {
		if stream.Disabled || strings.TrimSpace(stream.Upstream) == "" {
			continue
		}
		portGroups[stream.ListenPort] = append(portGroups[stream.ListenPort], stream)
	}
	ports := make([]int, 0, len(portGroups))
	for port := range portGroups {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	for _, port := range ports {
		if err := nm.RebuildStreamConfigNoReload(port, portGroups[port]); err != nil {
			return Result{}, fmt.Errorf("rebuild stream config for port %d: %w", port, err)
		}
	}

	if err := nm.Reload(); err != nil {
		return Result{}, fmt.Errorf("reload nginx: %w", err)
	}

	return Result{
		Sites:       len(sites),
		Redirects:   len(redirects),
		Streams:     len(streams),
		StreamPorts: len(ports),
	}, nil
}

func pruneConfigs(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".conf" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
