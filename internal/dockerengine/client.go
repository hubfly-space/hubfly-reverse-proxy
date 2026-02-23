package dockerengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

type Client struct {
	SocketPath string
	httpClient *http.Client
}

type Health struct {
	Available     bool   `json:"available"`
	SocketPath    string `json:"socket_path"`
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

func NewClient(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}

	return &Client{
		SocketPath: socketPath,
		httpClient: &http.Client{Timeout: 4 * time.Second, Transport: transport},
	}
}

func (c *Client) Health() Health {
	h := Health{SocketPath: c.SocketPath}
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
	body, err := c.get("/version")
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var v versionResponse
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		return nil, fmt.Errorf("failed to parse docker version response: %w", err)
	}
	return &v, nil
}

func (c *Client) ResolveContainerIP(container string) (string, error) {
	body, err := c.get(path.Join("/containers", container, "json"))
	if err != nil {
		return "", err
	}
	defer body.Close()

	var inspect inspectResponse
	if err := json.NewDecoder(body).Decode(&inspect); err != nil {
		return "", fmt.Errorf("failed to parse docker inspect response: %w", err)
	}

	for _, netCfg := range inspect.NetworkSettings.Networks {
		ip := strings.TrimSpace(netCfg.IPAddress)
		if ip != "" {
			return ip, nil
		}
	}

	return "", fmt.Errorf("no container IP found for %s", container)
}

func (c *Client) get(endpoint string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, "http://docker"+endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker engine request failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("docker engine request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return resp.Body, nil
}
