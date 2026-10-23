// 本文件为 HTTP 请求提供隐私保护的错误与慢请求日志。
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"filesage/internal/logging"
)

const slowRequestThreshold = 2 * time.Second
const tusUploadRoute = "/api/v1/uploads/"

// RequestLogger 记录错误和慢请求，不写入请求路径、查询参数或正文。
func RequestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestIDBytes [12]byte
		requestID := ""
		if _, err := rand.Read(requestIDBytes[:]); err == nil {
			requestID = hex.EncodeToString(requestIDBytes[:])
			w.Header().Set("X-Request-ID", requestID)
		}
		r = r.WithContext(logging.WithRequestID(r.Context(), requestID))
		startedAt := time.Now()
		response := &requestLogResponse{ResponseWriter: w}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(startedAt)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		level, shouldLog := requestLogLevel(r.Method, route, status, duration)
		if !shouldLog {
			return
		}
		logger.LogAttrs(r.Context(), level, "http request completed",
			slog.String("request_id", requestID), slog.String("method", r.Method),
			slog.String("route", route), slog.Int("status", status),
			slog.Int64("duration_ms", duration.Milliseconds()))
	})
}

// requestLogLevel 避免把正常的大文件上传记为慢请求告警。
func requestLogLevel(method, route string, status int, duration time.Duration) (slog.Level, bool) {
	if status >= http.StatusInternalServerError {
		return slog.LevelError, true
	}
	if status >= http.StatusBadRequest {
		return slog.LevelWarn, true
	}
	if duration < slowRequestThreshold {
		return 0, false
	}
	if method == http.MethodPatch && route == tusUploadRoute {
		return slog.LevelInfo, true
	}
	return slog.LevelWarn, true
}

type requestLogResponse struct {
	http.ResponseWriter
	status int
}

func (w *requestLogResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestLogResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// Unwrap 保留 ResponseController 对底层 writer 的访问。
func (w *requestLogResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
