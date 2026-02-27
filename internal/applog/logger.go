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
}

type rotatingFileWriter struct {
	mu       sync.Mutex
	dir      string
	prefix   string
	current  string
	file     *os.File
	location *time.Location
}

func newRotatingFileWriter(dir, prefix string) *rotatingFileWriter {
	return &rotatingFileWriter{
		dir:      dir,
		prefix:   prefix,
		location: time.UTC,
	}
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.rotateIfNeededLocked(time.Now()); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

func (w *rotatingFileWriter) rotateIfNeededLocked(now time.Time) error {
	target := w.filenameFor(now)
	if w.file != nil && target == w.current {
		return nil
	}

	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.current = target
	return nil
}

func (w *rotatingFileWriter) currentFile() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current
}

func (w *rotatingFileWriter) filenameFor(now time.Time) string {
	hourStamp := now.In(w.location).Format("20060102-15")
	return filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.prefix, hourStamp))
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

	writer := newRotatingFileWriter(opts.Dir, opts.Prefix)
	if err := writer.rotateIfNeededLocked(time.Now()); err != nil {
		return nil, fmt.Errorf("failed to initialize log file: %w", err)
	}

	handler := slog.NewJSONHandler(io.MultiWriter(os.Stdout, writer), &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	})
	slog.SetDefault(slog.New(handler))
	cleanupOldFiles(opts.Dir, opts.Prefix, opts.Retention, writer)

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
				cleanupOldFiles(opts.Dir, opts.Prefix, opts.Retention, writer)
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

func cleanupOldFiles(dir, prefix string, retention time.Duration, writer *rotatingFileWriter) {
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
		if path == writer.currentFile() {
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
