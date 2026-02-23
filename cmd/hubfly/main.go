package main

import (
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/hubfly/hubfly-reverse-proxy/internal/api"
	"github.com/hubfly/hubfly-reverse-proxy/internal/certbot"
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

	runtimeDir := flag.String("runtime-dir", ".runtime", "Project-local runtime directory")
	configDir := flag.String("config-dir", "", "Directory for managed site/stream config and metadata")
	port := flag.String("port", "81", "API listening port")
	nginxConf := flag.String("nginx-conf", "", "Path to nginx.conf")
	nginxBin := flag.String("nginx-bin", "", "Path to nginx binary")
	nginxPid := flag.String("nginx-pid", "", "Path to nginx PID file")
	certbotBin := flag.String("certbot-bin", "", "Path to certbot binary")
	certsDir := flag.String("certs-dir", "", "Certificate base directory (contains live/<domain>/)")
	webrootDir := flag.String("webroot-dir", "", "Webroot directory for ACME HTTP-01")
	logsDir := flag.String("logs-dir", "", "Directory for per-site logs")
	flag.Parse()

	resolvedRuntimeDir := mustAbs(*runtimeDir)
	resolvedConfigDir := defaultedPath(*configDir, filepath.Join(resolvedRuntimeDir, "config"))
	resolvedNginxConf := defaultedPath(*nginxConf, filepath.Join(resolvedRuntimeDir, "nginx", "nginx.conf"))
	resolvedNginxPID := defaultedPath(*nginxPid, filepath.Join(resolvedRuntimeDir, "run", "nginx.pid"))
	resolvedCertsDir := defaultedPath(*certsDir, filepath.Join(resolvedRuntimeDir, "letsencrypt"))
	resolvedWebrootDir := defaultedPath(*webrootDir, filepath.Join(resolvedRuntimeDir, "www"))
	resolvedLogsDir := defaultedPath(*logsDir, filepath.Join(resolvedRuntimeDir, "logs"))
	resolvedStaticDir := filepath.Join(resolvedRuntimeDir, "static")

	if err := os.MkdirAll(resolvedRuntimeDir, 0755); err != nil {
		slog.Error("Failed to create runtime dir", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(resolvedNginxConf), 0755); err != nil {
		slog.Error("Failed to create nginx config dir", "error", err)
		os.Exit(1)
	}

	copyIfMissing(filepath.Join("templates"), filepath.Join(resolvedConfigDir, "templates"))
	copyIfMissing(filepath.Join("static"), resolvedStaticDir)
	if err := writeDefaultNginxConfIfMissing(resolvedNginxConf, resolvedConfigDir, resolvedWebrootDir, resolvedStaticDir, resolvedLogsDir); err != nil {
		slog.Error("Failed to prepare nginx.conf", "error", err)
		os.Exit(1)
	}

	slog.Info("Initializing Hubfly",
		"version", appVersion,
		"config_dir", resolvedConfigDir,
		"runtime_dir", resolvedRuntimeDir,
		"port", *port,
	)

	st, err := store.NewJSONStore(resolvedConfigDir)
	if err != nil {
		slog.Error("Failed to initialize store", "error", err)
		os.Exit(1)
	}

	nm := nginx.NewManager(resolvedConfigDir, nginx.Options{
		NginxConf:  resolvedNginxConf,
		NginxBin:   stringsOrEmpty(defaultBinary(*nginxBin, filepath.Join(resolvedRuntimeDir, "bin", "nginx"))),
		CertsDir:   resolvedCertsDir,
		WebrootDir: resolvedWebrootDir,
		StaticDir:  resolvedStaticDir,
		LogsDir:    resolvedLogsDir,
		PIDFile:    resolvedNginxPID,
	})
	if err := nm.EnsureDirs(); err != nil {
		slog.Error("Failed to create nginx dirs", "error", err)
		os.Exit(1)
	}

	if err := nm.EnsureRunning(); err != nil {
		slog.Warn("Nginx is not running at startup", "error", err)
	}

	cm := certbot.NewManager(
		resolvedWebrootDir,
		"cert-support@hubfly.app",
		stringsOrEmpty(defaultBinary(*certbotBin, filepath.Join(resolvedRuntimeDir, "bin", "certbot"))),
		resolvedCertsDir,
	)

	lm := logmanager.NewManager(resolvedLogsDir)

	srv := api.NewServer(st, nm, cm, lm, upstream.NewDefaultResolver(), api.BuildInfo{
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

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func defaultedPath(input, fallback string) string {
	if input == "" {
		return fallback
	}
	return input
}

func defaultBinary(input, candidate string) string {
	if input != "" {
		return input
	}
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

func stringsOrEmpty(value string) string {
	return value
}

func copyIfMissing(srcRoot, dstRoot string) {
	srcInfo, err := os.Stat(srcRoot)
	if err != nil || !srcInfo.IsDir() {
		return
	}
	if _, err := os.Stat(dstRoot); err == nil {
		return
	}

	_ = filepath.Walk(srcRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstRoot, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		dstFile, err := os.Create(target)
		if err != nil {
			return err
		}
		defer dstFile.Close()

		_, err = io.Copy(dstFile, srcFile)
		return err
	})
}

func writeDefaultNginxConfIfMissing(confPath, configDir, webrootDir, staticDir, logsDir string) error {
	if _, err := os.Stat(confPath); err == nil {
		return nil
	}

	content := "worker_processes auto;\n" +
		"pid " + filepath.Join(filepath.Dir(confPath), "nginx.pid") + ";\n\n" +
		"events { worker_connections 1024; }\n\n" +
		"http {\n" +
		"    default_type application/octet-stream;\n" +
		"    log_format hubfly '$remote_addr - $remote_user [$time_local] \"$request\" '\n" +
		"                      '$status $body_bytes_sent \"$http_referer\" '\n" +
		"                      '\"$http_user_agent\" \"$request_time\"';\n" +
		"    access_log " + filepath.Join(logsDir, "access.log") + " hubfly;\n" +
		"    sendfile on;\n" +
		"    keepalive_timeout 65;\n" +
		"    map $http_upgrade $connection_upgrade { default upgrade; '' close; }\n" +
		"    include " + filepath.Join(configDir, "sites", "*.conf") + ";\n" +
		"    server {\n" +
		"        listen 80 default_server;\n" +
		"        server_name _;\n" +
		"        root " + staticDir + ";\n" +
		"        index index.html;\n" +
		"        location / { try_files $uri $uri/ =404; }\n" +
		"    }\n" +
		"    server {\n" +
		"        listen 82;\n" +
		"        server_name _;\n" +
		"        root " + staticDir + ";\n" +
		"        index index.html;\n" +
		"        location / { try_files $uri $uri/ =404; }\n" +
		"        location /v1/ {\n" +
		"            proxy_pass http://127.0.0.1:81;\n" +
		"            proxy_set_header Host $host;\n" +
		"            proxy_set_header X-Real-IP $remote_addr;\n" +
		"            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n" +
		"            proxy_set_header X-Forwarded-Proto $scheme;\n" +
		"        }\n" +
		"        location /.well-known/acme-challenge/ {\n" +
		"            root " + webrootDir + ";\n" +
		"            try_files $uri =404;\n" +
		"        }\n" +
		"    }\n" +
		"}\n\n" +
		"stream {\n" +
		"    include " + filepath.Join(configDir, "streams", "*.conf") + ";\n" +
		"}\n"

	return os.WriteFile(confPath, []byte(content), 0644)
}
