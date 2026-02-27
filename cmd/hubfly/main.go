package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	configDir := flag.String("config-dir", "/etc/hubfly", "Directory for config and data")
	port := flag.String("port", "81", "API listening port")
	nginxConf := flag.String("nginx-conf", "/etc/nginx/nginx.conf", "Path to nginx.conf")
	nginxBin := flag.String("nginx-bin", "", "Path to nginx binary (optional)")
	nginxPid := flag.String("nginx-pid", "/var/run/nginx.pid", "Path to nginx PID file")
	certbotBin := flag.String("certbot-bin", "", "Path to certbot binary (optional)")
	certsDir := flag.String("certs-dir", "/etc/letsencrypt", "Certificate base directory")
	webrootDir := flag.String("webroot-dir", "/var/www/hubfly", "Webroot directory for ACME HTTP-01")
	logsDir := flag.String("logs-dir", "/var/log/hubfly", "Directory for per-site logs")
	appLogDir := flag.String("app-log-dir", "/var/log/hubfly-go", "Directory for Hubfly Go application logs")
	appLogRetention := flag.String("app-log-retention", "1h", "Retention for Hubfly Go application logs (Go duration, e.g. 1h)")
	appLogCleanupInterval := flag.String("app-log-cleanup-interval", "1m", "Cleanup interval for Hubfly Go application logs (Go duration)")
	dockerSock := flag.String("docker-sock", "/var/run/docker.sock", "Path to Docker engine socket")
	flag.Parse()

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
		"config_dir", *configDir,
		"port", *port,
		"docker_sock", *dockerSock,
		"app_log_dir", *appLogDir,
		"app_log_retention", retention.String(),
		"app_log_cleanup_interval", cleanupInterval.String(),
	)

	if err := os.MkdirAll(*configDir, 0755); err != nil {
		slog.Error("Failed to create config dir", "error", err)
		os.Exit(1)
	}

	st, err := store.NewJSONStore(*configDir)
	if err != nil {
		slog.Error("Failed to initialize store", "error", err)
		os.Exit(1)
	}

	nm := nginx.NewManager(*configDir, nginx.Options{
		NginxConf:  *nginxConf,
		NginxBin:   *nginxBin,
		CertsDir:   *certsDir,
		WebrootDir: *webrootDir,
		StaticDir:  *webrootDir + "/static",
		LogsDir:    *logsDir,
		PIDFile:    *nginxPid,
	})
	if err := nm.EnsureDirs(); err != nil {
		slog.Error("Failed to create nginx dirs", "error", err)
		os.Exit(1)
	}

	if err := nm.EnsureRunning(); err != nil {
		slog.Warn("Nginx is not running at startup", "error", err)
	}

	cm := certbot.NewManager(*webrootDir, "cert-support@hubfly.app", *certbotBin, *certsDir)
	dockerClient := dockerengine.NewClient(*dockerSock)
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
