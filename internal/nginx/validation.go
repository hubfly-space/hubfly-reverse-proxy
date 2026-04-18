package nginx

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (m *Manager) ValidateHTTPConfigSet(configs map[string][]byte) error {
	if err := m.ensureFallbackCertificate(); err != nil {
		return fmt.Errorf("ensure fallback certificate: %w", err)
	}
	tempRoot, err := os.MkdirTemp("", "hubfly-nginx-validate")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempRoot)

	testSitesDir := filepath.Join(tempRoot, "sites")
	testStreamsDir := filepath.Join(tempRoot, "streams")
	testLogsDir := filepath.Join(tempRoot, "logs")
	testCacheDir := filepath.Join(tempRoot, "cache", "nginx")
	testPIDFile := filepath.Join(tempRoot, "run", "nginx.pid")
	if err := os.MkdirAll(testSitesDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(testStreamsDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(testLogsDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(testPIDFile), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(testCacheDir, 0755); err != nil {
		return err
	}

	for id, config := range configs {
		rewrittenConfig := rewriteValidationPorts(string(config))
		if err := os.WriteFile(filepath.Join(testSitesDir, id+".conf"), []byte(rewrittenConfig), 0644); err != nil {
			return err
		}
	}

	streamEntries, err := os.ReadDir(m.StreamsDir)
	if err == nil {
		for _, entry := range streamEntries {
			if entry.IsDir() {
				continue
			}
			src := filepath.Join(m.StreamsDir, entry.Name())
			data, readErr := os.ReadFile(src)
			if readErr != nil {
				return readErr
			}
			rewrittenStream := rewriteValidationPorts(string(data))
			if writeErr := os.WriteFile(filepath.Join(testStreamsDir, entry.Name()), []byte(rewrittenStream), 0644); writeErr != nil {
				return writeErr
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	mainConfig, err := os.ReadFile(m.NginxConf)
	if err != nil {
		return fmt.Errorf("read nginx config: %w", err)
	}
	updatedConfig := string(mainConfig)
	updatedConfig = strings.ReplaceAll(updatedConfig, filepath.ToSlash(filepath.Join(m.SitesDir, "*.conf")), filepath.ToSlash(filepath.Join(testSitesDir, "*.conf")))
	updatedConfig = strings.ReplaceAll(updatedConfig, filepath.ToSlash(filepath.Join(m.StreamsDir, "*.conf")), filepath.ToSlash(filepath.Join(testStreamsDir, "*.conf")))
	testNginxConf := filepath.Join(tempRoot, "nginx.conf")
	if err := os.WriteFile(testNginxConf, []byte(updatedConfig), 0644); err != nil {
		return err
	}

	testManager := *m
	testManager.SitesDir = testSitesDir
	testManager.StreamsDir = testStreamsDir
	testManager.NginxConf = testNginxConf
	testManager.LogsDir = testLogsDir
	testManager.CacheDir = testCacheDir
	testManager.PIDFile = testPIDFile

	if err := testManager.ensureMainConfigPaths(); err != nil {
		return err
	}
	testConfigBytes, err := os.ReadFile(testNginxConf)
	if err != nil {
		return err
	}
	testConfig := rewriteValidationPorts(string(testConfigBytes))
	testConfig = injectValidationErrorLog(testConfig)
	if err := os.WriteFile(testNginxConf, []byte(testConfig), 0644); err != nil {
		return err
	}
	return testManager.runConfigTest()
}

func rewriteValidationPorts(config string) string {
	replacements := []struct {
		pattern *regexp.Regexp
		value   string
	}{
		{regexp.MustCompile(`(?m)(listen\s+)\[::\]:443(\s+ssl\b)`), `${1}[::]:18443$2`},
		{regexp.MustCompile(`(?m)(listen\s+)443(\s+ssl\b)`), `${1}18443$2`},
		{regexp.MustCompile(`(?m)(listen\s+)\[::\]:80(\b)`), `${1}[::]:18080$2`},
		{regexp.MustCompile(`(?m)(listen\s+)80(\b)`), `${1}18080$2`},
		{regexp.MustCompile(`(?m)(listen\s+)10004(\b)`), `${1}11004$2`},
	}
	out := config
	for _, replacement := range replacements {
		out = replacement.pattern.ReplaceAllString(out, replacement.value)
	}
	return out
}

func injectValidationErrorLog(config string) string {
	errorLogPattern := regexp.MustCompile(`(?m)^\s*error_log\s+[^;]+;\s*$`)
	if errorLogPattern.MatchString(config) {
		return errorLogPattern.ReplaceAllString(config, "error_log stderr notice;")
	}
	return "error_log stderr notice;\n" + config
}
