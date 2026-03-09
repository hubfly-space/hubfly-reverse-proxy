package dockerengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

type Client struct {
	Endpoint   string
	httpClient *http.Client
	baseURL    string
}

type Health struct {
	Available     bool   `json:"available"`
	Endpoint      string `json:"endpoint"`
	APIVersion    string `json:"api_version,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`
	Error         string `json:"error,omitempty"`
}

type versionResponse struct {
	APIVersion string `json:"ApiVersion"`
	Version    string `json:"Version"`
}

type inspectResponse struct {
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func NewClient(endpoint string) *Client {
	endpoint = normalizeEndpoint(endpoint)

	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{}
	baseURL := endpoint
	if isUnixSocketEndpoint(endpoint) {
		socketPath := strings.TrimPrefix(endpoint, "unix://")
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		}
		baseURL = "http://docker"
	}

	return &Client{
		Endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 4 * time.Second, Transport: transport},
		baseURL:    strings.TrimRight(baseURL, "/"),
	}
}

func (c *Client) Health() Health {
	h := Health{Endpoint: c.Endpoint}
	version, err := c.Version()
	if err != nil {
		h.Error = err.Error()
		return h
	}
	h.Available = true
	h.APIVersion = version.APIVersion
	h.EngineVersion = version.Version
	return h
}

func (c *Client) Version() (*versionResponse, error) {
	slog.Debug("docker_version_request_started", "endpoint", c.Endpoint)
	body, err := c.get("/version")
	if err != nil {
		slog.Warn("docker_version_request_failed", "endpoint", c.Endpoint, "error", err)
		return nil, err
	}
	defer body.Close()

	var v versionResponse
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		return nil, fmt.Errorf("failed to parse docker version response: %w", err)
	}
	slog.Debug("docker_version_request_succeeded", "api_version", v.APIVersion, "engine_version", v.Version)
	return &v, nil
}

func (c *Client) ResolveContainerIP(container string) (string, error) {
	ips, err := c.ResolveContainerIPs(container)
	if err != nil {
		return "", err
	}
	ip, _ := PickIPFromNetworks(ips, "", "")
	if ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no container IP found for %s", container)
}

func (c *Client) ResolveContainerIPs(container string) (map[string]string, error) {
	slog.Info("docker_resolve_container_ips_started", "container", container)
	body, err := c.get(path.Join("/containers", container, "json"))
	if err != nil {
		slog.Warn("docker_resolve_container_ips_failed", "container", container, "error", err)
		return nil, err
	}
	defer body.Close()

	var inspect inspectResponse
	if err := json.NewDecoder(body).Decode(&inspect); err != nil {
		return nil, fmt.Errorf("failed to parse docker inspect response: %w", err)
	}

	out := make(map[string]string)
	for networkName, netCfg := range inspect.NetworkSettings.Networks {
		ip := strings.TrimSpace(netCfg.IPAddress)
		if ip != "" {
			out[networkName] = ip
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no container IP found for %s", container)
	}
	slog.Info("docker_resolve_container_ips_succeeded", "container", container, "networks", out)
	return out, nil
}

func PickIPFromNetworks(networkIPs map[string]string, preferredNetwork string, fallbackIP string) (string, string) {
	if preferredNetwork != "" {
		if ip := strings.TrimSpace(networkIPs[preferredNetwork]); ip != "" {
			return ip, preferredNetwork
		}
	}
	for networkName, ip := range networkIPs {
		if strings.TrimSpace(ip) == strings.TrimSpace(fallbackIP) && ip != "" {
			return ip, networkName
		}
	}
	networkNames := make([]string, 0, len(networkIPs))
	for networkName := range networkIPs {
		networkNames = append(networkNames, networkName)
	}
	sort.Strings(networkNames)
	for _, networkName := range networkNames {
		ip := networkIPs[networkName]
		if ip != "" {
			return ip, networkName
		}
	}
	return "", ""
}

func (c *Client) get(endpoint string) (io.ReadCloser, error) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Warn("docker_request_failed", "endpoint", endpoint, "duration", time.Since(start), "error", err)
		return nil, fmt.Errorf("docker engine request failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		slog.Warn("docker_request_non_success", "endpoint", endpoint, "status", resp.StatusCode, "duration", time.Since(start), "body", strings.TrimSpace(string(body)))
		return nil, fmt.Errorf("docker engine request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	slog.Debug("docker_request_succeeded", "endpoint", endpoint, "status", resp.StatusCode, "duration", time.Since(start))

	return resp.Body, nil
}

func normalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "http://127.0.0.1:10010"
	}
	if strings.HasPrefix(endpoint, "/") {
		return "unix://" + endpoint
	}
	if strings.HasPrefix(endpoint, "unix://") || strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	return "http://" + endpoint
}

func isUnixSocketEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "unix://")
}
