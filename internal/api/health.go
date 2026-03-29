package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	sites, _ := s.Store.ListSites()
	streams, _ := s.Store.ListStreams()

	nginxHealth := s.Nginx.Health()
	certbotHealth := s.Certbot.Health()
	dockerHealth := dockerengine.Health{Available: false, Error: "docker sync disabled"}
	dockerRequired := s.Docker != nil
	if s.Docker != nil {
		dockerHealth = s.Docker.Health()
		if !s.DockerSync && dockerHealth.Error == "" {
			dockerHealth.Error = "docker sync disabled"
		}
	}

	status := "ok"
	if !nginxHealth.Available || !nginxHealth.Running || !certbotHealth.Available || (dockerRequired && !dockerHealth.Available) {
		status = "degraded"
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
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

func (s *Server) handleManagementVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	nginxHealth := s.Nginx.Health()
	certbotHealth := s.Certbot.Health()

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"service": map[string]interface{}{
			"version":    s.BuildInfo.Version,
			"commit":     s.BuildInfo.Commit,
			"build_time": s.BuildInfo.BuildTime,
			"go_version": runtime.Version(),
		},
		"tools": map[string]interface{}{
			"nginx": map[string]interface{}{
				"available": nginxHealth.Available,
				"binary":    nginxHealth.Binary,
				"version":   nginxHealth.Version,
			},
			"certbot": map[string]interface{}{
				"available": certbotHealth.Available,
				"binary":    certbotHealth.Binary,
				"version":   certbotHealth.Version,
			},
		},
	})
}

type containerPortsRequest struct {
	Container   string `json:"container"`
	Network     string `json:"network,omitempty"`
	FromPort    int    `json:"from_port,omitempty"`
	ToPort      int    `json:"to_port,omitempty"`
	Ports       []int  `json:"ports,omitempty"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

type containerPortsResult struct {
	IP        string `json:"ip"`
	Network   string `json:"network"`
	OpenPorts []int  `json:"open_ports"`
}

func (s *Server) handleContainerPorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Docker == nil {
		errorResponse(w, http.StatusServiceUnavailable, "docker engine is unavailable")
		return
	}

	var req containerPortsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Container = strings.TrimSpace(req.Container)
	if req.Container == "" {
		errorResponse(w, http.StatusBadRequest, "container is required")
		return
	}

	ports, err := normalizePortScanRequest(req)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if req.TimeoutMS <= 0 {
		timeout = 150 * time.Millisecond
	}
	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = 512
	}
	if concurrency > 2048 {
		concurrency = 2048
	}

	networkIPs, err := s.Docker.ResolveContainerIPs(req.Container)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	networkNames := make([]string, 0, len(networkIPs))
	for networkName := range networkIPs {
		if req.Network != "" && networkName != req.Network {
			continue
		}
		networkNames = append(networkNames, networkName)
	}
	sort.Strings(networkNames)
	if len(networkNames) == 0 {
		errorResponse(w, http.StatusBadRequest, "no matching container network found")
		return
	}

	started := time.Now()
	results := make([]containerPortsResult, 0, len(networkNames))
	for _, networkName := range networkNames {
		ip := strings.TrimSpace(networkIPs[networkName])
		openPorts := scanOpenTCPPorts(ip, ports, timeout, concurrency)
		results = append(results, containerPortsResult{
			IP:        ip,
			Network:   networkName,
			OpenPorts: openPorts,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"container":     req.Container,
		"ports_scanned": len(ports),
		"timeout_ms":    timeout.Milliseconds(),
		"concurrency":   concurrency,
		"duration_ms":   time.Since(started).Milliseconds(),
		"results":       results,
	})
}
