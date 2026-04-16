package nginx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (m *Manager) ValidateHTTPConfigSet(configs map[string][]byte) error {
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
		if err := os.WriteFile(filepath.Join(testSitesDir, id+".conf"), config, 0644); err != nil {
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
			if writeErr := os.WriteFile(filepath.Join(testStreamsDir, entry.Name()), data, 0644); writeErr != nil {
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
	return testManager.runConfigTest()
}
