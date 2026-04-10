package logmanager

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LogEntry struct {
	Raw           string    `json:"raw"`
	RemoteAddr    string    `json:"remote_addr,omitempty"`
	RemoteUser    string    `json:"remote_user,omitempty"`
	TimeLocal     time.Time `json:"time_local,omitempty"`
	Request       string    `json:"request,omitempty"`
	Status        int       `json:"status,omitempty"`
	BodyBytesSent int64     `json:"body_bytes_sent,omitempty"`
	Referer       string    `json:"referer,omitempty"`
	UserAgent     string    `json:"user_agent,omitempty"`
	RequestTime   float64   `json:"request_time,omitempty"`
}

type ErrorLogEntry struct {
	Raw       string    `json:"raw"`
	TimeLocal time.Time `json:"time_local,omitempty"`
	Level     string    `json:"level,omitempty"`
	Message   string    `json:"message,omitempty"`
}

type LogOptions struct {
	Limit  int       `json:"limit"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	Search string    `json:"search"`
}

type Manager struct {
	LogDir string
}

func NewManager(logDir string) *Manager {
	return &Manager{LogDir: logDir}
}

func (m *Manager) AccessLogPath(siteID string) string {
	return filepath.Join(m.LogDir, siteID+".access.log")
}

func (m *Manager) ErrorLogPath(siteID string) string {
	return filepath.Join(m.LogDir, siteID+".error.log")
}

func (m *Manager) DeleteSiteLogs(siteID string) error {
	paths := []string{
		filepath.Join(m.LogDir, siteID+".access.log"),
		filepath.Join(m.LogDir, siteID+".error.log"),
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Access Log Regex
// $remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" "$request_time"
// Example: 127.0.0.1 - - [26/Dec/2025:10:00:00 +0000] "GET / HTTP/1.1" 200 612 "-" "Mozilla/5.0" "0.001"
var accessLogRegex = regexp.MustCompile(`^(\S+) - (\S+) \[([^\]]+)\] "([^"]+)" (\d+) (\d+) "([^"]*)" "([^"]*)" "([^"]*)"$`)

const nginxTimeLayout = "02/Jan/2006:15:04:05 -0700"
const errorLogTimeLayout = "2006/01/02 15:04:05"

func ParseAccessLogLine(line string) (LogEntry, bool) {
	matches := accessLogRegex.FindStringSubmatch(line)
	if len(matches) != 10 {
		return LogEntry{}, false
	}

	t, err := time.Parse(nginxTimeLayout, matches[3])
	if err != nil {
		return LogEntry{}, false
	}

	status, _ := strconv.Atoi(matches[5])
	bytesSent, _ := strconv.ParseInt(matches[6], 10, 64)
	reqTime, _ := strconv.ParseFloat(matches[9], 64)

	return LogEntry{
		Raw:           line,
		RemoteAddr:    matches[1],
		RemoteUser:    matches[2],
		TimeLocal:     t,
		Request:       matches[4],
		Status:        status,
		BodyBytesSent: bytesSent,
		Referer:       matches[7],
		UserAgent:     matches[8],
		RequestTime:   reqTime,
	}, true
}

func ParseErrorLogLine(line string) (ErrorLogEntry, bool) {
	var t time.Time
	var err error
	if len(line) >= 19 {
		t, err = time.Parse(errorLogTimeLayout, line[:19])
	}

	level := "unknown"
	startBracket := strings.Index(line, "[")
	endBracket := strings.Index(line, "]")
	if startBracket != -1 && endBracket != -1 && endBracket > startBracket {
		level = line[startBracket+1 : endBracket]
	}

	msg := ""
	if endBracket != -1 && len(line) > endBracket+1 {
		msg = strings.TrimSpace(line[endBracket+1:])
	} else {
		msg = line
	}

	entry := ErrorLogEntry{
		Raw:       line,
		TimeLocal: t,
		Level:     level,
		Message:   msg,
	}

	if err != nil && t.IsZero() {
		return entry, false
	}
	return entry, true
}

// scanFileBackwards reads the file from the end to the beginning.
// callback returns false to stop scanning.
func (m *Manager) scanFileBackwards(filename string, callback func(string) bool) error {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return nil
	}

	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}
	filesize := stat.Size()
	offset := filesize
	const bufferSize = 4096

	var tail []byte

	for offset > 0 {
		readSize := int64(bufferSize)
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize

		if _, err := file.Seek(offset, 0); err != nil {
			return err
		}

		chunk := make([]byte, readSize)
		if _, err := file.Read(chunk); err != nil {
			return err
		}

		p := len(chunk)
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				line := string(chunk[i+1:p]) + string(tail)
				if len(line) > 0 {
					if !callback(line) {
						return nil
					}
				}
				tail = nil
				p = i
			}
		}
		tail = append(chunk[:p], tail...)
	}

	if len(tail) > 0 {
		callback(string(tail))
	}

	return nil
}

func (m *Manager) GetAccessLogs(siteID string, opts LogOptions) ([]LogEntry, error) {
	var entries []LogEntry
	files, err := m.listLogFiles(siteID, ".access.log")
	if err != nil {
		return nil, err
	}

	stopAll := false
	for _, filename := range files {
		err := m.scanFileBackwards(filename, func(line string) bool {
			// 1. Basic Search Filter
			if opts.Search != "" && !strings.Contains(line, opts.Search) {
				return true // continue
			}

			// 2. Parse
			matches := accessLogRegex.FindStringSubmatch(line)
			if len(matches) != 10 {
				// Skip malformed lines
				return true
			}

			t, err := time.Parse(nginxTimeLayout, matches[3])
			if err != nil {
				return true
			}

			// 3. Time Filter
			// Reading backwards: Time decreases.
			// If Time < Since, then all remaining logs are older than Since. Stop.
			if !opts.Since.IsZero() && t.Before(opts.Since) {
				stopAll = true
				return false
			}
			// If Time > Until, this log is too new. Skip it, but older ones might match.
			if !opts.Until.IsZero() && t.After(opts.Until) {
				return true
			}

			status, _ := strconv.Atoi(matches[5])
			bytesSent, _ := strconv.ParseInt(matches[6], 10, 64)
			reqTime, _ := strconv.ParseFloat(matches[9], 64)

			entries = append(entries, LogEntry{
				Raw:           line,
				RemoteAddr:    matches[1],
				RemoteUser:    matches[2],
				TimeLocal:     t,
				Request:       matches[4],
				Status:        status,
				BodyBytesSent: bytesSent,
				Referer:       matches[7],
				UserAgent:     matches[8],
				RequestTime:   reqTime,
			})

			// Limit
			if opts.Limit > 0 && len(entries) >= opts.Limit {
				stopAll = true
				return false
			}

			return true
		})
		if err != nil {
			return entries, err
		}
		if stopAll {
			break
		}
	}

	return entries, nil
}

func (m *Manager) GetErrorLogs(siteID string, opts LogOptions) ([]ErrorLogEntry, error) {
	var entries []ErrorLogEntry
	files, err := m.listLogFiles(siteID, ".error.log")
	if err != nil {
		return nil, err
	}

	stopAll := false
	for _, filename := range files {
		err := m.scanFileBackwards(filename, func(line string) bool {
			if opts.Search != "" && !strings.Contains(line, opts.Search) {
				return true
			}

			// Parse timestamp
			// Format: YYYY/MM/DD HH:MM:SS (first 19 chars)
			var t time.Time
			var err error

			if len(line) >= 19 {
				t, err = time.Parse(errorLogTimeLayout, line[:19])
			}
			// If line is too short, we can't parse time.
			// Treat as "unknown time".
			// If filtering strictly by time, maybe skip?
			// For now, if we can't parse time, we include it unless strict checks fail.

			if err == nil && !t.IsZero() {
				if !opts.Since.IsZero() && t.Before(opts.Since) {
					stopAll = true
					return false
				}
				if !opts.Until.IsZero() && t.After(opts.Until) {
					return true
				}
			} else if !opts.Since.IsZero() || !opts.Until.IsZero() {
				// If we have time filters but can't parse time, safe default is to skip?
				// Or maybe the line is a continuation of a previous error?
				// Complex error logs (stack traces) might be multi-line.
				// Backward scanning multi-line logs is hard.
				// Assumption: One line per log entry or we just treat lines individually.
				// We'll skip if time is required but missing.
				return true
			}

			level := "unknown"
			startBracket := strings.Index(line, "[")
			endBracket := strings.Index(line, "]")
			if startBracket != -1 && endBracket != -1 && endBracket > startBracket {
				level = line[startBracket+1 : endBracket]
			}

			msg := ""
			if endBracket != -1 && len(line) > endBracket+1 {
				msg = strings.TrimSpace(line[endBracket+1:])
			} else {
				msg = line
			}

			entries = append(entries, ErrorLogEntry{
				Raw:       line,
				TimeLocal: t,
				Level:     level,
				Message:   msg,
			})

			if opts.Limit > 0 && len(entries) >= opts.Limit {
				stopAll = true
				return false
			}

			return true
		})
		if err != nil {
			return entries, err
		}
		if stopAll {
			break
		}
	}

	return entries, nil
}

type RetentionOptions struct {
	Retention      time.Duration
	RotateInterval time.Duration
}

const rotatedDateLayout = "20060102"

func (m *Manager) StartRetention(opts RetentionOptions) (func(), error) {
	retention := opts.Retention
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	rotateInterval := opts.RotateInterval
	if rotateInterval <= 0 {
		rotateInterval = time.Hour
	}

	now := time.Now
	lastDate := now().Format(rotatedDateLayout)

	if err := m.rotateIfStale(now()); err != nil {
		return nil, err
	}
	if err := m.cleanupOld(now(), retention); err != nil {
		return nil, err
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(rotateInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				current := now()
				currentDate := current.Format(rotatedDateLayout)
				if currentDate != lastDate {
					_ = m.rotateAll(current.Add(-time.Second))
					lastDate = currentDate
				}
				_ = m.cleanupOld(current, retention)
			case <-stopCh:
				return
			}
		}
	}()

	return func() {
		close(stopCh)
		wg.Wait()
	}, nil
}

func (m *Manager) rotateIfStale(now time.Time) error {
	today := now.Format(rotatedDateLayout)
	files, err := m.listActiveLogFiles()
	if err != nil {
		return err
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Size() == 0 {
			continue
		}
		modDate := info.ModTime().Format(rotatedDateLayout)
		if modDate != today {
			_ = m.rotateFile(path, modDate)
		}
	}
	return nil
}

func (m *Manager) rotateAll(now time.Time) error {
	dateStr := now.Format(rotatedDateLayout)
	files, err := m.listActiveLogFiles()
	if err != nil {
		return err
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		_ = m.rotateFile(path, dateStr)
	}
	return nil
}

func (m *Manager) rotateFile(path, dateStr string) error {
	if dateStr == "" {
		return nil
	}
	target := path + "." + dateStr
	if _, err := os.Stat(target); err == nil {
		for i := 1; i < 1000; i++ {
			candidate := fmt.Sprintf("%s.%d", target, i)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				target = candidate
				break
			}
		}
	}

	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}

	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

func (m *Manager) cleanupOld(now time.Time, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	cutoff := now.Add(-retention)
	entries, err := os.ReadDir(m.LogDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		dateStr, ok := extractRotatedDate(name)
		if !ok {
			continue
		}
		t, err := time.Parse(rotatedDateLayout, dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(m.LogDir, name))
		}
	}
	return nil
}

func (m *Manager) listActiveLogFiles() ([]string, error) {
	entries, err := os.ReadDir(m.LogDir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "access.log" || name == "nginx.error.log" {
			continue
		}
		if strings.HasSuffix(name, ".access.log") || strings.HasSuffix(name, ".error.log") {
			if strings.Contains(name, ".log.") {
				continue
			}
			files = append(files, filepath.Join(m.LogDir, name))
		}
	}
	return files, nil
}

func (m *Manager) listLogFiles(siteID, suffix string) ([]string, error) {
	entries, err := os.ReadDir(m.LogDir)
	if err != nil {
		return nil, err
	}
	baseName := siteID + suffix
	type logFile struct {
		path string
		date int
	}
	var files []logFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == baseName {
			files = append(files, logFile{path: filepath.Join(m.LogDir, name), date: 99999999})
			continue
		}
		if !strings.HasPrefix(name, baseName+".") {
			continue
		}
		rest := strings.TrimPrefix(name, baseName+".")
		dateStr := rest
		if idx := strings.Index(rest, "."); idx != -1 {
			dateStr = rest[:idx]
		}
		if len(dateStr) != 8 || !isDigits(dateStr) {
			continue
		}
		dateInt, err := strconv.Atoi(dateStr)
		if err != nil {
			continue
		}
		files = append(files, logFile{path: filepath.Join(m.LogDir, name), date: dateInt})
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].date == files[j].date {
			return files[i].path > files[j].path
		}
		return files[i].date > files[j].date
	})

	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.path)
	}
	return out, nil
}

func extractRotatedDate(name string) (string, bool) {
	base := ""
	if strings.Contains(name, ".access.log.") {
		base = ".access.log."
	} else if strings.Contains(name, ".error.log.") {
		base = ".error.log."
	} else {
		return "", false
	}

	idx := strings.LastIndex(name, base)
	if idx == -1 {
		return "", false
	}
	rest := name[idx+len(base):]
	if len(rest) < 8 {
		return "", false
	}
	dateStr := rest
	if dot := strings.Index(rest, "."); dot != -1 {
		dateStr = rest[:dot]
	}
	if len(dateStr) != 8 || !isDigits(dateStr) {
		return "", false
	}
	return dateStr, true
}

func isDigits(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
