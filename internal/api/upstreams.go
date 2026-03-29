package api

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/upstream"
)

func normalizeStreamUpstream(upstreamAddress string, containerPort int) (string, error) {
	return upstream.NormalizeEndpoint(upstreamAddress, containerPort)
}

func (s *Server) mapEndpointToContainerIP(endpoint string, overridePort int, preferredNetwork string) (string, string, string, error) {
	normalized, err := upstream.NormalizeEndpoint(endpoint, overridePort)
	if err != nil {
		return "", "", "", err
	}
	host, port, err := upstream.ParseEndpoint(normalized)
	if err != nil {
		return "", "", "", err
	}
	if upstream.IsIPHost(host) {
		return normalized, "", "", nil
	}
	if s.Docker == nil {
		return "", "", "", fmt.Errorf("docker engine is unavailable")
	}

	networkIPs, err := s.Docker.ResolveContainerIPs(host)
	if err != nil {
		return "", "", "", err
	}
	ip, network := dockerengine.PickIPFromNetworks(networkIPs, preferredNetwork, "")
	if ip == "" {
		return "", "", "", fmt.Errorf("no container ip found for %s", host)
	}

	return net.JoinHostPort(ip, strconv.Itoa(port)), host, network, nil
}

func (s *Server) normalizeSiteUpstreams(upstreamsInput []string) ([]string, []string, []string, error) {
	if len(upstreamsInput) == 0 {
		return nil, nil, nil, fmt.Errorf("at least one upstream is required")
	}

	normalized := make([]string, 0, len(upstreamsInput))
	containers := make([]string, 0, len(upstreamsInput))
	networks := make([]string, 0, len(upstreamsInput))
	for _, upstreamAddress := range upstreamsInput {
		mapped, containerName, networkName, err := s.mapEndpointToContainerIP(upstreamAddress, 0, "")
		if err != nil {
			return nil, nil, nil, err
		}
		normalized = append(normalized, mapped)
		containers = append(containers, containerName)
		networks = append(networks, networkName)
	}
	return normalized, containers, networks, nil
}

func normalizeSiteLoadBalancing(lb *models.LoadBalancing, upstreams []string) (*models.LoadBalancing, error) {
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("at least one upstream is required")
	}
	if lb == nil {
		return nil, nil
	}

	normalized := &models.LoadBalancing{}
	*normalized = *lb

	algorithmInput := strings.TrimSpace(normalized.Algorithm)
	hasWeights := len(normalized.Weights) > 0
	if !normalized.Enabled && algorithmInput == "" && !hasWeights {
		return nil, nil
	}
	normalized.Enabled = true

	algorithm := strings.ToLower(algorithmInput)
	switch algorithm {
	case "", "round_robin":
		algorithm = "round_robin"
	case "least_conn", "ip_hash":
	default:
		return nil, fmt.Errorf("invalid load balancing algorithm: %s", normalized.Algorithm)
	}
	normalized.Algorithm = algorithm

	if len(normalized.Weights) == 0 {
		normalized.Weights = make([]int, len(upstreams))
		for i := range normalized.Weights {
			normalized.Weights[i] = 1
		}
	} else {
		if len(normalized.Weights) != len(upstreams) {
			return nil, fmt.Errorf("load balancing weights must match upstream count")
		}
		for i, weight := range normalized.Weights {
			if weight < 1 {
				return nil, fmt.Errorf("load balancing weight at index %d must be >= 1", i)
			}
		}
	}

	if normalized.Algorithm == "ip_hash" {
		for i, weight := range normalized.Weights {
			if weight != 1 {
				return nil, fmt.Errorf("ip_hash does not support custom weights, weight at index %d must be 1", i)
			}
		}
	}

	return normalized, nil
}

func normalizePortScanRequest(req containerPortsRequest) ([]int, error) {
	if len(req.Ports) > 0 {
		seen := make(map[int]bool, len(req.Ports))
		out := make([]int, 0, len(req.Ports))
		for _, port := range req.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("ports must be between 1 and 65535")
			}
			if !seen[port] {
				seen[port] = true
				out = append(out, port)
			}
		}
		sort.Ints(out)
		return out, nil
	}

	fromPort := req.FromPort
	toPort := req.ToPort
	if fromPort == 0 && toPort == 0 {
		fromPort = 1
		toPort = 65535
	}
	if fromPort < 1 || toPort < 1 || fromPort > 65535 || toPort > 65535 {
		return nil, fmt.Errorf("from_port and to_port must be between 1 and 65535")
	}
	if fromPort > toPort {
		return nil, fmt.Errorf("from_port must be less than or equal to to_port")
	}

	out := make([]int, 0, toPort-fromPort+1)
	for port := fromPort; port <= toPort; port++ {
		out = append(out, port)
	}
	return out, nil
}

func scanOpenTCPPorts(ip string, ports []int, timeout time.Duration, concurrency int) []int {
	if len(ports) == 0 {
		return nil
	}

	jobs := make(chan int, len(ports))
	results := make(chan int, len(ports))
	var wg sync.WaitGroup

	workerCount := concurrency
	if workerCount > len(ports) {
		workerCount = len(ports)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for port := range jobs {
				address := net.JoinHostPort(ip, strconv.Itoa(port))
				conn, err := net.DialTimeout("tcp", address, timeout)
				if err == nil {
					_ = conn.Close()
					results <- port
				}
			}
		}()
	}

	for _, port := range ports {
		jobs <- port
	}
	close(jobs)
	wg.Wait()
	close(results)

	openPorts := make([]int, 0)
	for port := range results {
		openPorts = append(openPorts, port)
	}
	sort.Ints(openPorts)
	return openPorts
}
