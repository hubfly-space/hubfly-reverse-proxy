package applog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultPrefix          = "hubfly-go"
	defaultCleanupInterval = time.Minute
)

type Options struct {
	Dir             string
	Prefix          string
	Retention       time.Duration
	CleanupInterval time.Duration
	Level           slog.Leveler
}

type bootFileWriter struct {
	mu   sync.Mutex
	path string
	file *os.File
}

func newBootFileWriter(path string) *bootFileWriter {
	return &bootFileWriter{path: path}
}

func (w *bootFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
		w.file = f
	}
	return w.file.Write(p)
}

func (w *bootFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

func Setup(opts Options) (func(), error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, fmt.Errorf("log dir is required")
	}
	if strings.TrimSpace(opts.Prefix) == "" {
		opts.Prefix = defaultPrefix
	}
	if opts.Retention <= 0 {
		opts.Retention = time.Hour
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = defaultCleanupInterval
	}
	if err := os.MkdirAll(opts.Dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log dir: %w", err)
	}

	bootStamp := time.Now().UTC().Format("20060102-150405")
	bootLogFile := filepath.Join(opts.Dir, fmt.Sprintf("%s-%s.log", opts.Prefix, bootStamp))
	writer := newBootFileWriter(bootLogFile)

	level := opts.Level
	if level == nil {
		level = slog.LevelWarn
	}
	handler := slog.NewJSONHandler(io.MultiWriter(os.Stdout, writer), &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
	})
	slog.SetDefault(slog.New(handler))
	cleanupOldFiles(opts.Dir, opts.Prefix, opts.Retention, bootLogFile)

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(opts.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cleanupOldFiles(opts.Dir, opts.Prefix, opts.Retention, bootLogFile)
			case <-stopCh:
				return
			}
		}
	}()

	cleanup := func() {
		close(stopCh)
		wg.Wait()
		if err := writer.Close(); err != nil {
			slog.Warn("failed to close app log writer", "error", err)
		}
	}
	return cleanup, nil
}

func cleanupOldFiles(dir, prefix string, retention time.Duration, currentFile string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("app log cleanup failed to read dir", "dir", dir, "error", err)
		return
	}

	cutoff := time.Now().Add(-retention)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, prefix+"-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		path := filepath.Join(dir, name)
		if path == currentFile {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			slog.Warn("app log cleanup failed to stat file", "file", path, "error", err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("app log cleanup failed to remove file", "file", path, "error", err)
			continue
		}
		slog.Info("removed old app log file", "file", path, "retention", retention.String())
	}
}

// ParseLevel converts a string log level to slog.Level.
func ParseLevel(input string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelWarn, fmt.Errorf("invalid log level %q", input)
	}
}
