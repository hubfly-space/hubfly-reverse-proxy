package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/api"
	"github.com/hubfly/hubfly-reverse-proxy/internal/applog"
	"github.com/hubfly/hubfly-reverse-proxy/internal/certbot"
	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/logmanager"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
)

var (
	appVersion = "dev"
	gitCommit  = "unknown"
	buildTime  = "unknown"
)

func main() {
	configDir := flag.String("config-dir", ".", "Directory for hubfly runtime data")
	port := flag.String("port", "81", "API listening port")
	nginxConf := flag.String("nginx-conf", "", "Path to nginx.conf (default: <config-dir>/nginx/nginx.conf)")
	nginxBin := flag.String("nginx-bin", "", "Path to nginx binary (optional)")
	nginxPid := flag.String("nginx-pid", "", "Path to nginx PID file (default: <config-dir>/run/nginx.pid)")
	certbotBin := flag.String("certbot-bin", "", "Path to certbot binary (optional)")
	certsDir := flag.String("certs-dir", "", "Certificate config directory (default: <config-dir>/certbot/config)")
	certbotWorkDir := flag.String("certbot-work-dir", "", "Certbot work directory (default: <config-dir>/certbot/work)")
	certbotLogsDir := flag.String("certbot-logs-dir", "", "Certbot logs directory (default: <config-dir>/certbot/logs)")
	wildcardCertsConfig := flag.String("wildcard-certs-config", "", "Path to wildcard certificate JSON config (optional, defaults to <certs-dir>/wildcards/config.json when present)")
	webrootDir := flag.String("webroot-dir", "", "Webroot directory for ACME HTTP-01 (default: <config-dir>/www)")
	logsDir := flag.String("logs-dir", "", "Directory for per-site nginx logs (default: <config-dir>/logs/nginx)")
	appLogDir := flag.String("app-log-dir", "", "Directory for Hubfly runtime logs (default: <config-dir>/logs/runtime)")
	appLogRetention := flag.String("app-log-retention", "168h", "Retention for Hubfly runtime logs (Go duration, e.g. 168h)")
	appLogCleanupInterval := flag.String("app-log-cleanup-interval", "1h", "Cleanup interval for Hubfly runtime logs (Go duration)")
	dockerSock := flag.String("docker-sock", "/var/run/docker.sock", "Path to Docker engine socket")
	enableDockerSync := flag.Bool("enable-docker-sync", false, "Enable Docker-based upstream resolution/sync")
	flag.Parse()

	baseDir, err := filepath.Abs(strings.TrimSpace(*configDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -config-dir value %q: %v\n", *configDir, err)
		os.Exit(1)
	}
	if strings.TrimSpace(*nginxConf) == "" {
		*nginxConf = filepath.Join(baseDir, "nginx", "nginx.conf")
	}
	if strings.TrimSpace(*nginxPid) == "" {
		*nginxPid = filepath.Join(baseDir, "run", "nginx.pid")
	}
	if strings.TrimSpace(*certsDir) == "" {
		*certsDir = filepath.Join(baseDir, "certbot", "config")
	}
	if strings.TrimSpace(*certbotWorkDir) == "" {
		*certbotWorkDir = filepath.Join(baseDir, "certbot", "work")
	}
	if strings.TrimSpace(*certbotLogsDir) == "" {
		*certbotLogsDir = filepath.Join(baseDir, "certbot", "logs")
	}
	if strings.TrimSpace(*webrootDir) == "" {
		*webrootDir = filepath.Join(baseDir, "www")
	}
	if strings.TrimSpace(*logsDir) == "" {
		*logsDir = filepath.Join(baseDir, "logs", "nginx")
	}
	if strings.TrimSpace(*appLogDir) == "" {
		*appLogDir = filepath.Join(baseDir, "logs", "runtime")
	}

	retention, err := time.ParseDuration(strings.TrimSpace(*appLogRetention))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -app-log-retention value %q: %v\n", *appLogRetention, err)
		os.Exit(1)
	}
	cleanupInterval, err := time.ParseDuration(strings.TrimSpace(*appLogCleanupInterval))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -app-log-cleanup-interval value %q: %v\n", *appLogCleanupInterval, err)
		os.Exit(1)
	}
	stopAppLogger, err := applog.Setup(applog.Options{
		Dir:             *appLogDir,
		Retention:       retention,
		CleanupInterval: cleanupInterval,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize app logger: %v\n", err)
		os.Exit(1)
	}
	defer stopAppLogger()

	slog.Info("Initializing Hubfly",
		"version", appVersion,
		"config_dir", baseDir,
		"port", *port,
		"docker_sock", *dockerSock,
		"docker_sync_enabled", *enableDockerSync,
		"app_log_dir", *appLogDir,
		"app_log_retention", retention.String(),
		"app_log_cleanup_interval", cleanupInterval.String(),
	)

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		slog.Error("Failed to create config dir", "error", err)
		os.Exit(1)
	}
	for _, dir := range []string{*certsDir, *certbotWorkDir, *certbotLogsDir, *webrootDir, *logsDir, *appLogDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("Failed to create runtime dir", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	wildcardConfigPath := strings.TrimSpace(*wildcardCertsConfig)
	if wildcardConfigPath == "" {
		wildcardConfigPath = filepath.Join(*certsDir, "wildcards", "config.json")
	}
	wildcardCerts, err := loadWildcardCertsConfig(wildcardConfigPath)
	if err != nil {
		slog.Error("Failed to load wildcard certificates config", "path", wildcardConfigPath, "error", err)
		os.Exit(1)
	}
	if len(wildcardCerts) > 0 {
		slog.Info("Loaded wildcard certificate mappings", "path", wildcardConfigPath, "count", len(wildcardCerts))
	}

	st, err := store.NewSQLiteStore(baseDir)
	if err != nil {
		slog.Error("Failed to initialize store", "error", err)
		os.Exit(1)
	}

	nm := nginx.NewManager(baseDir, nginx.Options{
		NginxConf:     *nginxConf,
		NginxBin:      *nginxBin,
		CertsDir:      *certsDir,
		WebrootDir:    *webrootDir,
		StaticDir:     *webrootDir + "/static",
		LogsDir:       *logsDir,
		PIDFile:       *nginxPid,
		WildcardCerts: wildcardCerts,
	})
	if err := nm.EnsureDirs(); err != nil {
		slog.Error("Failed to create nginx dirs", "error", err)
		os.Exit(1)
	}

	if err := nm.EnsureRunning(); err != nil {
		slog.Warn("Nginx is not running at startup", "error", err)
	}

	cm := certbot.NewManager(*webrootDir, "cert-support@hubfly.app", *certbotBin, *certsDir, *certbotWorkDir, *certbotLogsDir)
	logStartupHealth(appVersion, gitCommit, buildTime, nm, cm)

	var dockerClient *dockerengine.Client
	if *enableDockerSync {
		dockerClient = dockerengine.NewClient(*dockerSock)
	}
	lm := logmanager.NewManager(*logsDir)

	srv := api.NewServer(st, nm, cm, dockerClient, lm, api.BuildInfo{
		Version:   appVersion,
		Commit:    gitCommit,
		BuildTime: buildTime,
	})
	srv.Bootstrap()

	slog.Info("Hubfly API starting", "address", ":"+*port)

	if err := http.ListenAndServe(":"+*port, srv.Routes()); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func logStartupHealth(version, commit, built string, nm *nginx.Manager, cm *certbot.Manager) {
	nginxHealth := nm.Health()
	certbotHealth := cm.Health()
	slog.Info("startup_runtime_check",
		"service_version", version,
		"service_commit", commit,
		"service_build_time", built,
		"go_version", runtime.Version(),
		"nginx_available", nginxHealth.Available,
		"nginx_running", nginxHealth.Running,
		"nginx_binary", nginxHealth.Binary,
		"nginx_version", nginxHealth.Version,
		"nginx_error", nginxHealth.Error,
		"certbot_available", certbotHealth.Available,
		"certbot_binary", certbotHealth.Binary,
		"certbot_version", certbotHealth.Version,
		"certbot_error", certbotHealth.Error,
	)
}

type wildcardCertConfig struct {
	Wildcards []nginx.WildcardCertificate `json:"wildcards"`
}

func loadWildcardCertsConfig(path string) ([]nginx.WildcardCertificate, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(content)) == "" {
		return nil, nil
	}

	var wrapped wildcardCertConfig
	if err := json.Unmarshal(content, &wrapped); err == nil && len(wrapped.Wildcards) > 0 {
		return validateWildcardCerts(wrapped.Wildcards)
	}

	var direct []nginx.WildcardCertificate
	if err := json.Unmarshal(content, &direct); err == nil {
		return validateWildcardCerts(direct)
	}

	return nil, fmt.Errorf("invalid wildcard certificate config JSON in %s", path)
}

func validateWildcardCerts(entries []nginx.WildcardCertificate) ([]nginx.WildcardCertificate, error) {
	valid := make([]nginx.WildcardCertificate, 0, len(entries))
	for i, entry := range entries {
		if strings.TrimSpace(entry.Domain) == "" && strings.TrimSpace(entry.DomainSuffix) == "" {
			return nil, fmt.Errorf("wildcards[%d]: domain or domain_suffix is required", i)
		}
		if strings.TrimSpace(entry.CertPath) == "" {
			return nil, fmt.Errorf("wildcards[%d]: cert_path is required", i)
		}
		if strings.TrimSpace(entry.KeyPath) == "" {
			return nil, fmt.Errorf("wildcards[%d]: key_path is required", i)
		}
		valid = append(valid, entry)
	}
	return valid, nil
}
