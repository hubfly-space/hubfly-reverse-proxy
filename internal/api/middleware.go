package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = generateRequestID()
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
		w.Header().Set("X-Request-ID", requestID)

		var bodyBytes []byte
		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut) {
			bodyBytes, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		slog.Info("api_request_received",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"remote", r.RemoteAddr,
			"user_agent", r.UserAgent(),
			"body", truncateForLog(bodyBytes, maxLoggedBodyBytes),
		)

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK, maxBodyBytes: maxLoggedBodyBytes}
		next.ServeHTTP(rw, r)

		slog.Info("api_request_completed",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"response_body", rw.body.String(),
			"response_bytes", rw.bytes,
			"duration", time.Since(start),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status       int
	bytes        int
	body         bytes.Buffer
	maxBodyBytes int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	remaining := rw.maxBodyBytes - rw.body.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			rw.body.Write(p)
		} else {
			rw.body.Write(p[:remaining])
		}
	}
	n, err := rw.ResponseWriter.Write(p)
	rw.bytes += n
	return n, err
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func requestIDFromContext(ctx context.Context) string {
	v := ctx.Value(requestIDContextKey{})
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func generateRequestID() string {
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), rand.Int63())
}

func truncateForLog(data []byte, max int) string {
	if len(data) <= max {
		return string(data)
	}
	return fmt.Sprintf("%s...[truncated %d bytes]", string(data[:max]), len(data)-max)
}
