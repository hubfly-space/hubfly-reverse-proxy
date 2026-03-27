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
