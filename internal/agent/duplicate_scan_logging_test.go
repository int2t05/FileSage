// 本文件验证重复扫描日志不会把正常取消记录为故障。
package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"filesage/internal/store"
)

func TestDuplicateScanStartLogClassifiesCancellationWithoutRawError(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logDuplicateScanStartFailure(logger, "scan-test", errors.Join(context.Canceled, errors.New("private path /srv/private")))
	if got := output.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "reason=cancelled") || strings.Contains(got, "/srv/private") {
		t.Fatalf("cancel log = %q", got)
	}

	output.Reset()
	logDuplicateScanStartFailure(logger, "scan-test", store.ErrNotFound)
	if got := output.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "reason=scan_start_failed") {
		t.Fatalf("storage failure log = %q", got)
	}
}
