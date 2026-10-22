package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRequestIDHandlerAddsContextAndDoesNotDuplicateExplicitField(t *testing.T) {
	var output bytes.Buffer
	handler := NewRequestIDHandler(slog.NewTextHandler(&output, nil))
	logger := slog.New(handler).With("service", "filesage").WithGroup("agent")
	ctx := WithRequestID(context.Background(), "request-test-id")
	logger.InfoContext(ctx, "tool step completed", "request_id", "request-test-id", "action", "rename")

	line := output.String()
	if strings.Count(line, "request_id=") != 1 || !strings.Contains(line, "request_id=request-test-id") {
		t.Fatalf("request ID field = %q", line)
	}
	if !strings.Contains(line, "agent.action=rename") || !strings.Contains(line, "service=filesage") {
		t.Fatalf("wrapped attributes were lost: %q", line)
	}
}

func TestRequestIDHandlerOmitsMissingContextValue(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRequestIDHandler(slog.NewTextHandler(&output, nil)))
	logger.InfoContext(context.Background(), "background task")
	if strings.Contains(output.String(), "request_id=") {
		t.Fatalf("background log has an empty request ID: %q", output.String())
	}
}
