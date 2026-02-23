package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/hubfly/hubfly-reverse-proxy/internal/api"
	"github.com/hubfly/hubfly-reverse-proxy/internal/certbot"
	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/logmanager"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
	"github.com/hubfly/hubfly-reverse-proxy/internal/upstream"
)

var (
	appVersion = "dev"
	gitCommit  = "unknown"
	buildTime  = "unknown"
)

func main() {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	logger := slog.New(slog.NewTextHandler(os.Stdout, opts))
	slog.SetDefault(logger)

	configDir := flag.String("config-dir", "/etc/hubfly", "Directory for config and data")
	port := flag.String("port", "81", "API listening port")
	nginxConf := flag.String("nginx-conf", "/etc/nginx/nginx.conf", "Path to nginx.conf")
	nginxBin := flag.String("nginx-bin", "", "Path to nginx binary (optional)")
	nginxPid := flag.String("nginx-pid", "/var/run/nginx.pid", "Path to nginx PID file")
	certbotBin := flag.String("certbot-bin", "", "Path to certbot binary (optional)")
	certsDir := flag.String("certs-dir", "/etc/letsencrypt", "Certificate base directory")
	webrootDir := flag.String("webroot-dir", "/var/www/hubfly", "Webroot directory for ACME HTTP-01")
	logsDir := flag.String("logs-dir", "/var/log/hubfly", "Directory for per-site logs")
	dockerSock := flag.String("docker-sock", "/var/run/docker.sock", "Path to Docker engine socket")
	flag.Parse()

	slog.Info("Initializing Hubfly",
		"version", appVersion,
		"config_dir", *configDir,
		"port", *port,
		"docker_sock", *dockerSock,
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
	resolver := upstream.NewDefaultResolver(dockerClient)
	lm := logmanager.NewManager(*logsDir)

	srv := api.NewServer(st, nm, cm, dockerClient, lm, resolver, api.BuildInfo{
		Version:   appVersion,
		Commit:    gitCommit,
		BuildTime: buildTime,
	})

	slog.Info("Hubfly API starting", "address", ":"+*port)

	if err := http.ListenAndServe(":"+*port, srv.Routes()); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
