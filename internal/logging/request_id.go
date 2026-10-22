// 本包在日志上下文中传递 HTTP 请求标识。
package logging

import (
	"context"
	"log/slog"
)

type requestIDContextKey struct{}

// WithRequestID 将请求标识加入日志上下文。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

// RequestID 返回上下文中的请求标识。
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

// NewRequestIDHandler 为请求上下文中的日志添加请求标识。
func NewRequestIDHandler(next slog.Handler) slog.Handler {
	return requestIDHandler{next: next}
}

type requestIDHandler struct {
	next slog.Handler
}

func (h requestIDHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h requestIDHandler) Handle(ctx context.Context, record slog.Record) error {
	if requestID := RequestID(ctx); requestID != "" && !hasRequestID(record) {
		record.AddAttrs(slog.String("request_id", requestID))
	}
	return h.next.Handle(ctx, record)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{next: h.next.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{next: h.next.WithGroup(name)}
}

func hasRequestID(record slog.Record) bool {
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "request_id" {
			found = true
			return false
		}
		return true
	})
	return found
}
