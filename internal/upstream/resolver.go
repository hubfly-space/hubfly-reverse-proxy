package upstream

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

type Resolver interface {
	Resolve(upstream string, overridePort int) (string, error)
}

type DefaultResolver struct{}

func NewDefaultResolver() *DefaultResolver {
	return &DefaultResolver{}
}

func (r *DefaultResolver) Resolve(upstream string, overridePort int) (string, error) {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return "", fmt.Errorf("upstream is required")
	}

	host, port, err := splitHostAndPort(upstream)
	if err != nil {
		return "", err
	}

	if overridePort != 0 {
		if overridePort < 1 || overridePort > 65535 {
			return "", fmt.Errorf("container_port must be between 1 and 65535")
		}
		port = overridePort
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("upstream must include a valid port")
	}

	resolvedHost, err := resolveHost(host)
	if err != nil {
		return "", err
	}

	return net.JoinHostPort(resolvedHost, strconv.Itoa(port)), nil
}

func splitHostAndPort(upstream string) (string, int, error) {
	if host, portStr, err := net.SplitHostPort(upstream); err == nil {
		port, convErr := strconv.Atoi(portStr)
		if convErr != nil {
			return "", 0, fmt.Errorf("upstream must include a valid port")
		}
		return host, port, nil
	}

	if strings.HasPrefix(upstream, "[") && strings.HasSuffix(upstream, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(upstream, "["), "]"), 0, nil
	}

	if strings.Count(upstream, ":") > 1 {
		return upstream, 0, nil
	}

	host, portStr, ok := strings.Cut(upstream, ":")
	if !ok {
		return upstream, 0, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("upstream must include a valid port")
	}
	return host, port, nil
}

func resolveHost(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}

	ips, err := net.LookupIP(host)
	if err == nil {
		if selected := pickPreferredIP(ips); selected != "" {
			return selected, nil
		}
	}

	if dockerIP, dockerErr := lookupDockerContainerIP(host); dockerErr == nil && dockerIP != "" {
		return dockerIP, nil
	}

	if err != nil {
		return "", fmt.Errorf("failed to resolve upstream host %q: %w", host, err)
	}
	return "", fmt.Errorf("failed to resolve upstream host %q", host)
}

func pickPreferredIP(ips []net.IP) string {
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	for _, ip := range ips {
		if ip.To16() != nil {
			return ip.String()
		}
	}
	return ""
}

func lookupDockerContainerIP(container string) (string, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return "", err
	}

	cmd := exec.Command(path, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	for _, field := range fields {
		if ip := net.ParseIP(field); ip != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no container IP found")
}
