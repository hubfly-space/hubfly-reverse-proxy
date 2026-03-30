package api

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/upstream"
)

func (s *Server) dockerSyncLoop() {
	if s.Docker == nil || !s.DockerSync {
		return
	}

	ticker := time.NewTicker(dockerFullCheckInterval)
	defer ticker.Stop()

	s.triggerDockerFullCheck("startup")
	for {
		select {
		case reason := <-s.fullCheckRequests:
			s.runDockerFullCheck(reason)
		case <-ticker.C:
			s.runDockerFullCheck("interval")
		}
	}
}

func (s *Server) triggerDockerFullCheck(reason string) {
	if s.Docker == nil || !s.DockerSync {
		return
	}
	select {
	case s.fullCheckRequests <- reason:
	default:
	}
}

func (s *Server) runDockerFullCheck(reason string) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	reloaded, siteCount, streamCount, err := s.syncUpstreamsAndRefreshLocked(false)
	if err != nil {
		slog.Warn("background_full_check_failed", "reason", reason, "error", err)
		return
	}
	slog.Info(
		"background_full_check_completed",
		"reason", reason,
		"reloaded", reloaded,
		"sites_changed", siteCount,
		"stream_ports_changed", streamCount,
	)
}

func (s *Server) dockerEventLoop() {
	if s.Docker == nil || !s.DockerSync {
		return
	}

	actions := []string{"start", "restart", "unpause", "stop"}
	lastTrigger := time.Time{}
	for {
		err := s.Docker.StreamContainerEvents(context.Background(), actions, func(event dockerengine.Event) {
			now := time.Now()
			if now.Sub(lastTrigger) < 2*time.Second {
				return
			}
			lastTrigger = now
			slog.Info(
				"docker_event_full_check_queued",
				"action", event.Action,
				"container_id", event.Actor.ID,
				"container_name", event.Actor.Attributes["name"],
			)
			s.triggerDockerFullCheck("docker_event:" + event.Action)
		})
		if err != nil {
			slog.Warn("docker_event_stream_stopped", "error", err)
			time.Sleep(2 * time.Second)
			continue
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *Server) syncUpstreamsAndRefreshLocked(forceReload bool) (bool, int, int, error) {
	siteChanged, siteIDs, err := s.syncSitesFromContainers()
	if err != nil {
		slog.Debug("site upstream sync skipped", "error", err)
		return false, 0, 0, err
	}
	streamChanged, ports, err := s.syncStreamsFromContainers()
	if err != nil {
		slog.Debug("stream upstream sync skipped", "error", err)
		return false, 0, 0, err
	}

	shouldReload := forceReload || siteChanged || streamChanged
	if !shouldReload {
		return false, 0, 0, nil
	}

	for _, siteID := range siteIDs {
		site, err := s.Store.GetSite(siteID)
		if err != nil {
			continue
		}
		siteCopy := *site
		s.refreshSiteConfigWithReload(&siteCopy, false)
	}
	for _, port := range ports {
		s.reconcileStreamsWithReload(port, false)
	}

	if err := s.Nginx.Reload(); err != nil {
		slog.Warn("upstream sync reload failed", "error", err)
		return false, len(siteIDs), len(ports), err
	}

	slog.Info("upstream sync applied",
		"sites_changed", len(siteIDs),
		"stream_ports_changed", len(ports),
		"force_reload", forceReload,
	)
	return true, len(siteIDs), len(ports), nil
}

func (s *Server) syncSitesFromContainers() (bool, []string, error) {
	sites, err := s.Store.ListSites()
	if err != nil {
		return false, nil, err
	}

	changed := false
	changedIDs := make(map[string]bool)
	for _, site := range sites {
		siteChanged, updatedSite, err := s.refreshSiteUpstreamsFromContainer(site)
		if err != nil {
			slog.Debug("site upstream sync error", "site_id", site.ID, "error", err)
			continue
		}
		if !siteChanged {
			continue
		}
		if err := s.Store.SaveSite(&updatedSite); err != nil {
			slog.Warn("failed to persist refreshed site upstreams", "site_id", site.ID, "error", err)
			continue
		}
		changed = true
		changedIDs[site.ID] = true
	}

	siteIDs := make([]string, 0, len(changedIDs))
	for id := range changedIDs {
		siteIDs = append(siteIDs, id)
	}
	sort.Strings(siteIDs)
	return changed, siteIDs, nil
}

func (s *Server) syncStreamsFromContainers() (bool, []int, error) {
	streams, err := s.Store.ListStreams()
	if err != nil {
		return false, nil, err
	}

	changed := false
	changedPorts := make(map[int]bool)
	for _, stream := range streams {
		changedStream, updated, err := s.refreshStreamUpstreamFromContainer(stream)
		if err != nil {
			slog.Debug("stream upstream sync error", "stream_id", stream.ID, "error", err)
			continue
		}
		if !changedStream {
			continue
		}
		if err := s.Store.SaveStream(&updated); err != nil {
			slog.Warn("failed to persist refreshed stream upstream", "stream_id", stream.ID, "error", err)
			continue
		}
		changed = true
		changedPorts[stream.ListenPort] = true
	}

	ports := make([]int, 0, len(changedPorts))
	for p := range changedPorts {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return changed, ports, nil
}

func isContainerNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status=404") || strings.Contains(strings.ToLower(msg), "no such container")
}

func disableExpiredMissingUpstream(missingSince *time.Time, now time.Time) bool {
	return missingSince != nil && now.Sub(*missingSince) >= missingContainerGracePeriod
}

func (s *Server) refreshSiteUpstreamsFromContainer(site models.Site) (bool, models.Site, error) {
	changed := false
	now := time.Now()

	if len(site.UpstreamContainers) < len(site.Upstreams) {
		padded := make([]string, len(site.Upstreams))
		copy(padded, site.UpstreamContainers)
		site.UpstreamContainers = padded
	}
	if len(site.UpstreamNetworks) < len(site.Upstreams) {
		padded := make([]string, len(site.Upstreams))
		copy(padded, site.UpstreamNetworks)
		site.UpstreamNetworks = padded
	}
	if len(site.UpstreamMissingSince) < len(site.Upstreams) {
		padded := make([]*time.Time, len(site.Upstreams))
		copy(padded, site.UpstreamMissingSince)
		site.UpstreamMissingSince = padded
	}
	if len(site.DisabledUpstreams) < len(site.Upstreams) {
		padded := make([]bool, len(site.Upstreams))
		copy(padded, site.DisabledUpstreams)
		site.DisabledUpstreams = padded
	}

	for i := range site.Upstreams {
		container := strings.TrimSpace(site.UpstreamContainers[i])
		if container == "" {
			host, _, parseErr := upstream.ParseEndpoint(site.Upstreams[i])
			if parseErr == nil && !upstream.IsIPHost(host) {
				container = host
				site.UpstreamContainers[i] = host
				changed = true
			}
		}
		if container == "" {
			continue
		}

		currentHost, currentPort, err := upstream.ParseEndpoint(site.Upstreams[i])
		if err != nil {
			continue
		}
		networkIPs, err := s.Docker.ResolveContainerIPs(container)
		if err != nil {
			if isContainerNotFoundErr(err) {
				if site.UpstreamMissingSince[i] == nil {
					missingSince := now
					site.UpstreamMissingSince[i] = &missingSince
					changed = true
				}
				if disableExpiredMissingUpstream(site.UpstreamMissingSince[i], now) && !site.DisabledUpstreams[i] {
					site.DisabledUpstreams[i] = true
					changed = true
				}
			}
			continue
		}
		if site.UpstreamMissingSince[i] != nil {
			site.UpstreamMissingSince[i] = nil
			changed = true
		}
		if site.DisabledUpstreams[i] {
			site.DisabledUpstreams[i] = false
			changed = true
		}

		newIP, newNetwork := dockerengine.PickIPFromNetworks(networkIPs, site.UpstreamNetworks[i], currentHost)
		if newIP == "" {
			continue
		}
		newEndpoint := net.JoinHostPort(newIP, strconv.Itoa(currentPort))
		if newEndpoint != site.Upstreams[i] || newNetwork != site.UpstreamNetworks[i] {
			site.Upstreams[i] = newEndpoint
			site.UpstreamNetworks[i] = newNetwork
			changed = true
		}
	}

	if changed {
		site.UpdatedAt = now
	}
	return changed, site, nil
}

func (s *Server) refreshStreamUpstreamFromContainer(stream models.Stream) (bool, models.Stream, error) {
	metadataChanged := false
	now := time.Now()
	container := strings.TrimSpace(stream.ContainerName)
	if container == "" {
		host, _, parseErr := upstream.ParseEndpoint(stream.Upstream)
		if parseErr == nil && !upstream.IsIPHost(host) {
			container = host
			stream.ContainerName = host
			metadataChanged = true
		}
	}
	if container == "" {
		return false, stream, nil
	}

	currentHost, currentPort, err := upstream.ParseEndpoint(stream.Upstream)
	if err != nil {
		return false, stream, err
	}
	networkIPs, err := s.Docker.ResolveContainerIPs(container)
	if err != nil {
		if isContainerNotFoundErr(err) {
			if stream.MissingSince == nil {
				missingSince := now
				stream.MissingSince = &missingSince
				stream.UpdatedAt = now
				return true, stream, nil
			}
			if disableExpiredMissingUpstream(stream.MissingSince, now) && !stream.Disabled {
				stream.Disabled = true
				stream.Status = "error"
				stream.ErrorMessage = "tracked container missing for over 5 minutes; stream disabled to avoid stale IP routing"
				stream.UpdatedAt = now
				return true, stream, nil
			}
			return false, stream, nil
		}
		return false, stream, err
	}
	if stream.MissingSince != nil {
		stream.MissingSince = nil
		metadataChanged = true
	}
	if stream.Disabled {
		stream.Disabled = false
		stream.Status = "provisioning"
		stream.ErrorMessage = ""
		metadataChanged = true
	}

	newIP, newNetwork := dockerengine.PickIPFromNetworks(networkIPs, stream.ContainerNetwork, currentHost)
	if newIP == "" {
		return metadataChanged, stream, nil
	}
	newEndpoint := net.JoinHostPort(newIP, strconv.Itoa(currentPort))
	if newEndpoint == stream.Upstream && newNetwork == stream.ContainerNetwork {
		if metadataChanged {
			stream.UpdatedAt = now
		}
		return metadataChanged, stream, nil
	}

	stream.Upstream = newEndpoint
	stream.ContainerNetwork = newNetwork
	stream.UpdatedAt = now
	return true, stream, nil
}
