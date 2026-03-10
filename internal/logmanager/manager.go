package logmanager

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	filename := filepath.Join(m.LogDir, siteID+".access.log")

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
			return false
		}

		return true
	})

	return entries, err
}

func (m *Manager) GetErrorLogs(siteID string, opts LogOptions) ([]ErrorLogEntry, error) {
	var entries []ErrorLogEntry
	filename := filepath.Join(m.LogDir, siteID+".error.log")

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
			return false
		}

		return true
	})

	return entries, err
}
