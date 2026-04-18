package nginx

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type upstreamTarget struct {
	Address string
	Weight  int
}

func sanitizeName(input string) string {
	if strings.TrimSpace(input) == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

type Manager struct {
	SitesDir      string
	StreamsDir    string
	StagingDir    string
	TemplatesDir  string
	NginxConf     string
	NginxBin      string
	FallbackCert  string
	FallbackKey   string
	CertsDir      string
	WebrootDir    string
	StaticDir     string
	LogsDir       string
	CacheDir      string
	PIDFile       string
	WildcardCerts []WildcardCertificate
}

type WildcardCertificate struct {
	Domain       string `json:"domain,omitempty"`
	DomainSuffix string `json:"domain_suffix,omitempty"`
	CertPath     string `json:"cert_path"`
	KeyPath      string `json:"key_path"`
}

type Options struct {
	NginxConf     string
	NginxBin      string
	CertsDir      string
	WebrootDir    string
	StaticDir     string
	LogsDir       string
	PIDFile       string
	WildcardCerts []WildcardCertificate
}

type Health struct {
	Available bool   `json:"available"`
	Running   bool   `json:"running"`
	Binary    string `json:"binary,omitempty"`
	Version   string `json:"version,omitempty"`
	Config    string `json:"config"`
	PIDFile   string `json:"pid_file"`
	PID       int    `json:"pid,omitempty"`
	Error     string `json:"error,omitempty"`
}

func NewManager(baseDir string, opts ...Options) *Manager {
	cfg := Options{
		NginxConf:  filepath.Join(baseDir, "nginx", "nginx.conf"),
		CertsDir:   filepath.Join(baseDir, "letsencrypt"),
		WebrootDir: filepath.Join(baseDir, "www"),
		StaticDir:  filepath.Join(baseDir, "static"),
		LogsDir:    filepath.Join(baseDir, "logs"),
		PIDFile:    filepath.Join(baseDir, "run", "nginx.pid"),
	}
	if len(opts) > 0 {
		override := opts[0]
		if strings.TrimSpace(override.NginxConf) != "" {
			cfg.NginxConf = strings.TrimSpace(override.NginxConf)
		}
		cfg.NginxBin = strings.TrimSpace(override.NginxBin)
		if strings.TrimSpace(override.CertsDir) != "" {
			cfg.CertsDir = strings.TrimSpace(override.CertsDir)
		}
		if strings.TrimSpace(override.WebrootDir) != "" {
			cfg.WebrootDir = strings.TrimSpace(override.WebrootDir)
		}
		if strings.TrimSpace(override.StaticDir) != "" {
			cfg.StaticDir = strings.TrimSpace(override.StaticDir)
		}
		if strings.TrimSpace(override.LogsDir) != "" {
			cfg.LogsDir = strings.TrimSpace(override.LogsDir)
		}
		if strings.TrimSpace(override.PIDFile) != "" {
			cfg.PIDFile = strings.TrimSpace(override.PIDFile)
		}
		cfg.WildcardCerts = override.WildcardCerts
	}

	return &Manager{
		SitesDir:      filepath.Join(baseDir, "sites"),
		StreamsDir:    filepath.Join(baseDir, "streams"),
		StagingDir:    filepath.Join(baseDir, "staging"),
		TemplatesDir:  filepath.Join(baseDir, "templates"),
		NginxConf:     cfg.NginxConf,
		NginxBin:      cfg.NginxBin,
		FallbackCert:  filepath.Join(baseDir, "default-certs", "fallback.crt"),
		FallbackKey:   filepath.Join(baseDir, "default-certs", "fallback.key"),
		CertsDir:      cfg.CertsDir,
		WebrootDir:    cfg.WebrootDir,
		StaticDir:     cfg.StaticDir,
		LogsDir:       cfg.LogsDir,
		CacheDir:      filepath.Join(filepath.Dir(cfg.LogsDir), "cache", "nginx"),
		PIDFile:       cfg.PIDFile,
		WildcardCerts: cfg.WildcardCerts,
	}
}

func (m *Manager) EnsureDirs() error {
	dirs := []string{
		m.SitesDir,
		m.StreamsDir,
		m.StagingDir,
		m.TemplatesDir,
		filepath.Dir(m.FallbackCert),
		m.WebrootDir,
		m.StaticDir,
		m.LogsDir,
		m.CacheDir,
		filepath.Dir(m.PIDFile),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	if err := m.ensureFallbackCertificate(); err != nil {
		return err
	}
	return m.ensureMainConfigPaths()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (m *Manager) Validate(stagingFile string) error {
	if _, err := os.Stat(stagingFile); err != nil {
		return fmt.Errorf("staging config missing: %w", err)
	}
	return nil
}

func (m *Manager) runConfigTest() error {
	if err := m.ensureFallbackCertificate(); err != nil {
		return fmt.Errorf("ensure fallback certificate: %w", err)
	}
	path, err := m.resolveBinary()
	if err != nil {
		slog.Warn("Nginx not found, skipping config test")
		return nil
	}
	slog.Info("nginx_config_test_started", "command", path, "nginx_conf", m.NginxConf)
	cmd := exec.Command(path, "-t", "-c", m.NginxConf)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("nginx_config_test_failed", "error", err, "duration", time.Since(start), "output", string(out))
		return fmt.Errorf("nginx config test failed: %s, output: %s", err, string(out))
	}
	slog.Info("nginx_config_test_succeeded", "duration", time.Since(start), "output", string(out))
	return nil
}

func (m *Manager) Apply(siteID, stagingFile string) error {
	return m.apply(siteID, stagingFile, true)
}

func (m *Manager) ApplyNoReload(siteID, stagingFile string) error {
	return m.apply(siteID, stagingFile, false)
}

func (m *Manager) ApplyRendered(siteID string, config []byte) error {
	return m.applyBytes(siteID, config, true)
}

func (m *Manager) ApplyRenderedNoReload(siteID string, config []byte) error {
	return m.applyBytes(siteID, config, false)
}

func (m *Manager) apply(siteID, stagingFile string, reload bool) error {
	stagingData, err := os.ReadFile(stagingFile)
	if err != nil {
		return err
	}
	return m.applyBytes(siteID, stagingData, reload)
}

func (m *Manager) applyBytes(siteID string, stagingData []byte, reload bool) error {
	target := filepath.Join(m.SitesDir, siteID+".conf")
	var previousData []byte
	previousExists := false
	if b, err := os.ReadFile(target); err == nil {
		previousExists = true
		previousData = b
	} else if !os.IsNotExist(err) {
		return err
	}

	restorePrevious := func() {
		if previousExists {
			_ = os.WriteFile(target, previousData, 0644)
			return
		}
		_ = os.Remove(target)
	}

	if err := os.WriteFile(target, stagingData, 0644); err != nil {
		return err
	}
	if err := m.runConfigTest(); err != nil {
		restorePrevious()
		return err
	}
	if reload {
		if err := m.Reload(); err != nil {
			restorePrevious()
			return err
		}
	}
	slog.Info("Applied site config", "site_id", siteID, "target", target)
	return nil
}

func (m *Manager) Reload() error {
	path, err := m.resolveBinary()
	if err != nil {
		slog.Warn("Nginx not found, skipping reload")
		return nil
	}
	slog.Info("nginx_reload_started", "command", path, "pid_file", m.PIDFile)
	cmd := exec.Command(path, "-s", "reload", "-c", m.NginxConf)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("nginx_reload_failed_attempting_restart", "error", err, "duration", time.Since(start), "output", string(out))
		if startErr := m.EnsureRunning(); startErr != nil {
			slog.Error("nginx_restart_failed_after_reload_failure", "reload_error", err, "start_error", startErr)
			return fmt.Errorf("nginx reload failed: %s, output: %s", err, string(out))
		}
		slog.Info("nginx_restart_succeeded_after_reload_failure")
		return nil
	}
	slog.Info("nginx_reload_succeeded", "duration", time.Since(start), "output", string(out))
	return m.EnsureRunning()
}

func (m *Manager) PurgeCache() error {
	dir := strings.TrimSpace(m.CacheDir)
	if dir == "" || dir == "/" || dir == "." {
		return fmt.Errorf("invalid cache dir: %q", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0755)
		}
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) resolveBinary() (string, error) {
	if m.NginxBin != "" {
		if _, err := os.Stat(m.NginxBin); err != nil {
			return "", fmt.Errorf("nginx binary not found at %s", m.NginxBin)
		}
		return m.NginxBin, nil
	}
	return exec.LookPath("nginx")
}

func (m *Manager) Version() (string, error) {
	path, err := m.resolveBinary()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(path, "-v").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get nginx version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) pid() (int, error) {
	pidBytes, err := os.ReadFile(m.PIDFile)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(pidBytes))
	if pidStr == "" {
		return 0, os.ErrNotExist
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("invalid pid in %s: %w", m.PIDFile, err)
	}
	return pid, nil
}

func (m *Manager) IsRunning() (bool, int, error) {
	pid, err := m.pid()
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, pid, err
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return false, pid, nil
	}
	return true, pid, nil
}

func (m *Manager) EnsureRunning() error {
	running, _, err := m.IsRunning()
	if err != nil {
		return err
	}
	if running {
		slog.Debug("nginx_already_running")
		return nil
	}

	path, err := m.resolveBinary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.PIDFile), 0755); err != nil {
		return err
	}
	if err := m.testMainConfig(path); err != nil {
		return err
	}
	if err := m.stopUnmanagedNginx(path); err != nil {
		return err
	}

	args, err := m.startArgs()
	if err != nil {
		return err
	}
	slog.Info("nginx_start_started", "command", path, "args", args)
	cmd := exec.Command(path, args...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		if strings.Contains(output, "address already in use") {
			slog.Warn("nginx_start_conflict_detected_retrying_takeover", "output", output)
			if stopErr := m.stopUnmanagedNginx(path); stopErr != nil {
				slog.Warn("failed_to_stop_unmanaged_nginx_after_conflict", "error", stopErr)
			}
			cmdRetry := exec.Command(path, args...)
			retryOut, retryErr := cmdRetry.CombinedOutput()
			if retryErr == nil {
				slog.Info("nginx_start_retry_succeeded", "output", string(retryOut))
				return m.waitForRunning()
			}
			slog.Error("nginx_start_retry_failed", "error", retryErr, "output", string(retryOut))
		}
		slog.Error("nginx_start_failed", "error", err, "duration", time.Since(start), "output", string(out))
		return fmt.Errorf("nginx start failed: %s, output: %s", err, string(out))
	}
	slog.Info("nginx_start_command_succeeded", "duration", time.Since(start), "output", string(out))
	return m.waitForRunning()
}

func (m *Manager) ensureMainConfigPaths() error {
	content, err := os.ReadFile(m.NginxConf)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read nginx config %s: %w", m.NginxConf, err)
	}
	updated := string(content)
	updated = strings.ReplaceAll(updated, "/etc/hubfly/sites/*.conf", filepath.ToSlash(filepath.Join(m.SitesDir, "*.conf")))
	updated = strings.ReplaceAll(updated, "/etc/hubfly/streams/*.conf", filepath.ToSlash(filepath.Join(m.StreamsDir, "*.conf")))
	updated = strings.ReplaceAll(updated, "/var/www/hubfly/static", filepath.ToSlash(m.StaticDir))
	updated = strings.ReplaceAll(updated, "/var/log/hubfly/access.log", filepath.ToSlash(filepath.Join(m.LogsDir, "access.log")))
	updated = strings.ReplaceAll(updated, "/var/cache/nginx", filepath.ToSlash(m.CacheDir))
	updated = normalizeMainConfigRuntimePaths(updated, m)
	updated = ensureServerNameHashConfig(updated)
	updated = strings.ReplaceAll(updated, "ssl_reject_handshake on;", fmt.Sprintf("ssl_certificate %s;\n        ssl_certificate_key %s;\n        return 444;", filepath.ToSlash(m.FallbackCert), filepath.ToSlash(m.FallbackKey)))
	updated = strings.ReplaceAll(updated, "listen 82;", "listen 10004;")
	updated = strings.ReplaceAll(updated, "proxy_pass http://127.0.0.1:81;", "proxy_pass http://127.0.0.1:10003;")
	updated = strings.ReplaceAll(updated, "proxy_pass http://127.0.0.1:7890;", "proxy_pass http://127.0.0.1:10005;")

	pidPattern := regexp.MustCompile(`(?m)^\s*pid\s+[^;]+;\s*$`)
	if pidPattern.MatchString(updated) {
		updated = pidPattern.ReplaceAllString(updated, fmt.Sprintf("pid %s;", filepath.ToSlash(m.PIDFile)))
	} else {
		updated = fmt.Sprintf("pid %s;\n%s", filepath.ToSlash(m.PIDFile), updated)
	}

	errLogPattern := regexp.MustCompile(`(?m)^\s*error_log\s+[^;]+;\s*$`)
	errorLogPath := filepath.ToSlash(filepath.Join(m.LogsDir, "nginx.error.log"))
	if errLogPattern.MatchString(updated) {
		updated = errLogPattern.ReplaceAllString(updated, fmt.Sprintf("error_log %s notice;", errorLogPath))
	} else {
		updated = fmt.Sprintf("error_log %s notice;\n%s", errorLogPath, updated)
	}

	runtimeUser := discoverRuntimeUser(m.NginxConf)
	userPattern := regexp.MustCompile(`(?m)^\s*user\s+[^;]+;\s*$`)
	if runtimeUser != "" {
		updated = userPattern.ReplaceAllString(updated, fmt.Sprintf("user %s;", runtimeUser))
		if !userPattern.MatchString(updated) {
			updated = fmt.Sprintf("user %s;\n%s", runtimeUser, updated)
		}
	} else {
		updated = userPattern.ReplaceAllString(updated, "# user directive managed by host defaults")
	}

	updated = m.normalizeStreamModuleSupport(updated)
	if updated == string(content) {
		return nil
	}
	return os.WriteFile(m.NginxConf, []byte(updated), 0644)
}

func normalizeMainConfigRuntimePaths(config string, m *Manager) string {
	sitesIncludePattern := regexp.MustCompile(`(?m)^\s*include\s+.+/sites/\*\.conf;\s*$`)
	streamsIncludePattern := regexp.MustCompile(`(?m)^\s*include\s+.+/streams/\*\.conf;\s*$`)
	staticRootPattern := regexp.MustCompile(`(?m)^(\s*root\s+).+/static(\s*;)\s*$`)
	accessLogPattern := regexp.MustCompile(`(?m)^\s*access_log\s+.+/access\.log\s+hubfly;\s*$`)
	cachePathPattern := regexp.MustCompile(`(?m)^\s*proxy_cache_path\s+.+\s+levels=1:2\s+keys_zone=hubfly_cache:10m\s+max_size=10g\s+inactive=60m\s+use_temp_path=off;\s*$`)
	fallbackCertPattern := regexp.MustCompile(`(?m)^\s*ssl_certificate\s+.+/default-certs/fallback\.crt;\s*$`)
	fallbackKeyPattern := regexp.MustCompile(`(?m)^\s*ssl_certificate_key\s+.+/default-certs/fallback\.key;\s*$`)

	config = sitesIncludePattern.ReplaceAllString(config, fmt.Sprintf("    include %s;", filepath.ToSlash(filepath.Join(m.SitesDir, "*.conf"))))
	config = streamsIncludePattern.ReplaceAllString(config, fmt.Sprintf("    include %s;", filepath.ToSlash(filepath.Join(m.StreamsDir, "*.conf"))))
	config = staticRootPattern.ReplaceAllString(config, fmt.Sprintf("${1}%s${2}", filepath.ToSlash(m.StaticDir)))
	config = accessLogPattern.ReplaceAllString(config, fmt.Sprintf("    access_log  %s  hubfly;", filepath.ToSlash(filepath.Join(m.LogsDir, "access.log"))))
	config = cachePathPattern.ReplaceAllString(config, fmt.Sprintf("    proxy_cache_path %s levels=1:2 keys_zone=hubfly_cache:10m max_size=10g inactive=60m use_temp_path=off;", filepath.ToSlash(m.CacheDir)))
	config = fallbackCertPattern.ReplaceAllString(config, fmt.Sprintf("        ssl_certificate %s;", filepath.ToSlash(m.FallbackCert)))
	config = fallbackKeyPattern.ReplaceAllString(config, fmt.Sprintf("        ssl_certificate_key %s;", filepath.ToSlash(m.FallbackKey)))
	return config
}

func ensureServerNameHashConfig(config string) string {
	if strings.Contains(config, "server_names_hash_bucket_size") && strings.Contains(config, "server_names_hash_max_size") {
		return config
	}
	httpBlockPattern := regexp.MustCompile(`http\s*\{\n`)
	if !httpBlockPattern.MatchString(config) {
		return config
	}
	return httpBlockPattern.ReplaceAllString(config, "http {\n    server_names_hash_bucket_size 128;\n    server_names_hash_max_size 4096;\n")
}

func (m *Manager) normalizeStreamModuleSupport(config string) string {
	if !strings.Contains(config, "stream {") {
		return config
	}
	streamModulePath := discoverStreamModulePath()
	if streamModulePath != "" {
		loadLine := fmt.Sprintf("load_module %s;", filepath.ToSlash(streamModulePath))
		if !strings.Contains(config, loadLine) {
			config = loadLine + "\n" + config
		}
	}
	return config
}

func discoverStreamModulePath() string {
	candidates := []string{
		"/usr/lib/nginx/modules/ngx_stream_module.so",
		"/usr/lib64/nginx/modules/ngx_stream_module.so",
		"/usr/share/nginx/modules/ngx_stream_module.so",
	}
	for _, path := range candidates {
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func (m *Manager) ensureFallbackCertificate() error {
	if fileExists(m.FallbackCert) && fileExists(m.FallbackKey) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.FallbackCert), 0755); err != nil {
		return err
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate fallback key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate fallback serial: %w", err)
	}

	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: "hubfly-fallback.local"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"hubfly-fallback.local", "localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create fallback certificate: %w", err)
	}

	certOut, err := os.OpenFile(m.FallbackCert, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open fallback cert file: %w", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		_ = certOut.Close()
		return fmt.Errorf("write fallback cert: %w", err)
	}
	if err := certOut.Close(); err != nil {
		return fmt.Errorf("close fallback cert file: %w", err)
	}

	keyOut, err := os.OpenFile(m.FallbackKey, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open fallback key file: %w", err)
	}
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes}); err != nil {
		_ = keyOut.Close()
		return fmt.Errorf("write fallback key: %w", err)
	}
	return keyOut.Close()
}

func (m *Manager) testMainConfig(path string) error {
	out, err := exec.Command(path, "-t", "-c", m.NginxConf).CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if strings.Contains(output, `unknown directive "stream"`) {
			return fmt.Errorf("nginx config validation failed: %s. stream module is required; install it (for Ubuntu/Debian: apt install libnginx-mod-stream) and restart", output)
		}
		return fmt.Errorf("nginx config validation failed: %s", output)
	}
	return nil
}

func (m *Manager) configHasPIDDirective() (bool, error) {
	content, err := os.ReadFile(m.NginxConf)
	if err != nil {
		return false, fmt.Errorf("failed to read nginx config: %w", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "pid ") {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) startArgs() ([]string, error) {
	args := []string{"-c", m.NginxConf}
	hasPIDDirective, err := m.configHasPIDDirective()
	if err != nil {
		return nil, err
	}
	if !hasPIDDirective {
		args = append(args, "-g", fmt.Sprintf("pid %s;", m.PIDFile))
	}
	return args, nil
}

func (m *Manager) waitForRunning() error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		if runningNow, _, runErr := m.IsRunning(); runErr == nil && runningNow {
			return nil
		}
	}
	slog.Error("nginx_start_health_check_timeout")
	return fmt.Errorf("nginx did not become running after start")
}

func (m *Manager) stopUnmanagedNginx(path string) error {
	running, pid, err := m.IsRunning()
	if err == nil && running && pid > 0 {
		return nil
	}
	pids, err := discoverNginxPIDs()
	if err != nil || len(pids) == 0 {
		return err
	}

	slog.Warn("unmanaged_nginx_detected_attempting_takeover", "pids", pids)
	out, quitErr := exec.Command(path, "-s", "quit").CombinedOutput()
	if quitErr != nil {
		slog.Warn("nginx_quit_command_failed_for_takeover", "error", quitErr, "output", string(out))
	} else {
		slog.Info("nginx_quit_command_succeeded_for_takeover", "output", string(out))
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		remaining, remErr := discoverNginxPIDs()
		if remErr != nil {
			return remErr
		}
		if len(remaining) == 0 {
			return nil
		}
	}

	remaining, err := discoverNginxPIDs()
	if err != nil || len(remaining) == 0 {
		return err
	}
	slog.Warn("forcing_nginx_process_shutdown_for_takeover", "pids", remaining)
	for _, p := range remaining {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}

	finalDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(finalDeadline) {
		time.Sleep(200 * time.Millisecond)
		final, finalErr := discoverNginxPIDs()
		if finalErr != nil {
			return finalErr
		}
		if len(final) == 0 {
			return nil
		}
	}
	if left, _ := discoverNginxPIDs(); len(left) > 0 {
		return fmt.Errorf("unable to stop existing nginx processes: %v", left)
	}
	return nil
}

func discoverNginxPIDs() ([]int, error) {
	if _, err := exec.LookPath("pgrep"); err == nil {
		out, err := exec.Command("pgrep", "-x", "nginx").Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return nil, nil
			}
			return nil, fmt.Errorf("failed to discover nginx pids: %w", err)
		}
		return filterTakeoverCandidates(parsePIDList(string(out))), nil
	}

	out, err := exec.Command("ps", "-eo", "pid=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to discover nginx pids via ps: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	raw := make([]string, 0)
	for _, line := range lines {
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) >= 2 && parts[1] == "nginx" {
			raw = append(raw, parts[0])
		}
	}
	pids := filterTakeoverCandidates(parsePIDList(strings.Join(raw, "\n")))
	sort.Ints(pids)
	return pids, nil
}

func parsePIDList(output string) []int {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	pids := make([]int, 0, len(lines))
	for _, line := range lines {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func filterTakeoverCandidates(pids []int) []int {
	out := make([]int, 0, len(pids))
	for _, pid := range pids {
		if isContainerizedNginxPID(pid) || isNginxWorkerPID(pid) {
			continue
		}
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func isContainerizedNginxPID(pid int) bool {
	cgroupPath := filepath.Join("/proc", strconv.Itoa(pid), "cgroup")
	if cgroupData, err := os.ReadFile(cgroupPath); err == nil {
		cg := strings.ToLower(string(cgroupData))
		if strings.Contains(cg, "docker") || strings.Contains(cg, "containerd") || strings.Contains(cg, "kubepods") || strings.Contains(cg, "libpod") {
			return true
		}
	}

	statPath := filepath.Join("/proc", strconv.Itoa(pid), "status")
	if statusData, err := os.ReadFile(statPath); err == nil {
		ppid := 0
		for _, line := range strings.Split(string(statusData), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					ppid, _ = strconv.Atoi(fields[1])
				}
				break
			}
		}
		if ppid > 0 {
			commPath := filepath.Join("/proc", strconv.Itoa(ppid), "comm")
			if commData, err := os.ReadFile(commPath); err == nil {
				parentComm := strings.TrimSpace(strings.ToLower(string(commData)))
				if strings.Contains(parentComm, "containerd-shim") || strings.Contains(parentComm, "conmon") {
					return true
				}
			}
		}
	}
	return false
}

func isNginxWorkerPID(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	cmdline := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")))
	return strings.Contains(cmdline, "worker process")
}

func discoverRuntimeUser(pathHint string) string {
	if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); sudoUser != "" {
		if _, err := user.Lookup(sudoUser); err == nil {
			return sudoUser
		}
	}

	stat, err := os.Stat(pathHint)
	if err != nil {
		return ""
	}
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(sys.Uid), 10))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Username)
}

func (m *Manager) Restart() error {
	path, err := m.resolveBinary()
	if err != nil {
		return err
	}
	slog.Info("nginx_restart_started", "command", path)
	quitOut, quitErr := exec.Command(path, "-s", "quit", "-c", m.NginxConf).CombinedOutput()
	if quitErr != nil {
		slog.Warn("nginx_quit_before_restart_failed", "error", quitErr, "output", string(quitOut))
	} else {
		slog.Info("nginx_quit_before_restart_succeeded", "output", string(quitOut))
	}
	time.Sleep(500 * time.Millisecond)
	if err := m.EnsureRunning(); err != nil {
		slog.Error("nginx_restart_failed", "error", err)
		return err
	}
	slog.Info("nginx_restart_succeeded")
	return nil
}

func (m *Manager) Health() Health {
	h := Health{Config: m.NginxConf, PIDFile: m.PIDFile}
	path, err := m.resolveBinary()
	if err != nil {
		h.Error = err.Error()
		return h
	}
	h.Binary = path
	h.Available = true
	if version, versionErr := m.Version(); versionErr == nil {
		h.Version = version
	} else {
		h.Error = versionErr.Error()
	}
	running, pid, runErr := m.IsRunning()
	h.Running = running
	h.PID = pid
	if runErr != nil {
		if h.Error == "" {
			h.Error = runErr.Error()
		} else {
			h.Error += "; " + runErr.Error()
		}
	}
	return h
}

func (m *Manager) Delete(siteID string) error {
	target := filepath.Join(m.SitesDir, siteID+".conf")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	slog.Info("Deleted site config", "site_id", siteID, "file", target)
	return m.Reload()
}
