package certbot

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Manager struct {
	Webroot    string
	Email      string
	BinaryPath string
	CertsDir   string
}

type Health struct {
	Available bool   `json:"available"`
	Binary    string `json:"binary"`
	Version   string `json:"version,omitempty"`
	Webroot   string `json:"webroot"`
	CertsDir  string `json:"certs_dir"`
	Error     string `json:"error,omitempty"`
}

func NewManager(webroot, email, binaryPath, certsDir string) *Manager {
	return &Manager{
		Webroot:    webroot,
		Email:      email,
		BinaryPath: strings.TrimSpace(binaryPath),
		CertsDir:   certsDir,
	}
}

func (m *Manager) certPath(domain string) string {
	return filepath.Join(m.CertsDir, "live", domain, "cert.pem")
}

func (m *Manager) resolveBinary() (string, error) {
	if m.BinaryPath != "" {
		if _, err := os.Stat(m.BinaryPath); err != nil {
			return "", fmt.Errorf("certbot binary not found at %s", m.BinaryPath)
		}
		return m.BinaryPath, nil
	}

	path, err := exec.LookPath("certbot")
	if err != nil {
		return "", fmt.Errorf("certbot not found")
	}
	return path, nil
}

func (m *Manager) Version() (string, error) {
	path, err := m.resolveBinary()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get certbot version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) Health() Health {
	h := Health{
		Webroot:  m.Webroot,
		CertsDir: m.CertsDir,
	}

	path, err := m.resolveBinary()
	if err != nil {
		h.Error = err.Error()
		return h
	}

	h.Binary = path
	h.Available = true
	if version, err := m.Version(); err == nil {
		h.Version = version
	} else {
		h.Error = err.Error()
	}

	return h
}

func (m *Manager) Issue(domain string) error {
	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	args := []string{
		"certonly",
		"--webroot",
		"-w", m.Webroot,
		"-d", domain,
		"--non-interactive",
		"--agree-tos",
		"-m", m.Email,
	}

	slog.Info("Running certbot issue", "domain", domain, "command", path, "args", args)

	cmd := exec.Command(path, args...)
	out, err := cmd.CombinedOutput()

	slog.Debug("Certbot output", "domain", domain, "output", string(out))

	if err != nil {
		slog.Error("Certbot issue failed", "domain", domain, "error", err, "output", string(out))
		return fmt.Errorf("certbot failed: %s, output: %s", err, string(out))
	}
	return nil
}

func (m *Manager) Revoke(domain string) error {
	certPath := m.certPath(domain)

	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	slog.Info("Running certbot revoke", "domain", domain, "cert_path", certPath)

	cmd := exec.Command(path, "revoke", "--cert-path", certPath, "--reason", "unspecified", "--non-interactive")
	out, err := cmd.CombinedOutput()

	slog.Debug("Certbot revoke output", "domain", domain, "output", string(out))

	if err != nil {
		slog.Error("Certbot revoke failed", "domain", domain, "error", err, "output", string(out))
		return fmt.Errorf("certbot revoke failed: %s, output: %s", err, string(out))
	}
	return nil
}
