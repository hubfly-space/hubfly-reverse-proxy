package certbot

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Manager struct {
	Webroot    string
	Email      string
	BinaryPath string
	CertsDir   string
	WorkDir    string
	LogsDir    string
}

type Health struct {
	Available bool   `json:"available"`
	Binary    string `json:"binary"`
	Version   string `json:"version,omitempty"`
	Webroot   string `json:"webroot"`
	CertsDir  string `json:"certs_dir"`
	WorkDir   string `json:"work_dir"`
	LogsDir   string `json:"logs_dir"`
	Error     string `json:"error,omitempty"`
}

func NewManager(webroot, email, binaryPath, certsDir, workDir, logsDir string) *Manager {
	return &Manager{
		Webroot:    webroot,
		Email:      email,
		BinaryPath: strings.TrimSpace(binaryPath),
		CertsDir:   certsDir,
		WorkDir:    workDir,
		LogsDir:    logsDir,
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
		WorkDir:  m.WorkDir,
		LogsDir:  m.LogsDir,
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
		"--config-dir", m.CertsDir,
		"--work-dir", m.WorkDir,
		"--logs-dir", m.LogsDir,
		"--webroot",
		"-w", m.Webroot,
		"-d", domain,
		"--non-interactive",
		"--agree-tos",
		"-m", m.Email,
	}

	slog.Info("certbot_issue_started", "domain", domain, "command", path, "args", args, "webroot", m.Webroot)

	cmd := exec.Command(path, args...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)

	slog.Info("certbot_issue_output", "domain", domain, "duration", duration, "output", string(out))

	if err != nil {
		slog.Error("certbot_issue_failed", "domain", domain, "error", err, "duration", duration, "output", string(out))
		return fmt.Errorf("certbot failed: %s, output: %s", err, string(out))
	}
	slog.Info("certbot_issue_succeeded", "domain", domain, "duration", duration)
	return nil
}

func (m *Manager) Revoke(domain string) error {
	certPath := m.certPath(domain)

	path, err := m.resolveBinary()
	if err != nil {
		return err
	}

	slog.Info("certbot_revoke_started", "domain", domain, "cert_path", certPath)

	args := []string{
		"revoke",
		"--cert-path", certPath,
		"--reason", "unspecified",
		"--non-interactive",
		"--config-dir", m.CertsDir,
		"--work-dir", m.WorkDir,
		"--logs-dir", m.LogsDir,
	}
	cmd := exec.Command(path, args...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	duration := time.Since(start)

	slog.Info("certbot_revoke_output", "domain", domain, "duration", duration, "output", string(out))

	if err != nil {
		slog.Error("certbot_revoke_failed", "domain", domain, "error", err, "duration", duration, "output", string(out))
		return fmt.Errorf("certbot revoke failed: %s, output: %s", err, string(out))
	}
	slog.Info("certbot_revoke_succeeded", "domain", domain, "duration", duration)
	return nil
}
