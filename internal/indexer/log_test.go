// 本文件验证索引扫描日志不泄露用户路径。
package indexer

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"filesage/internal/fsroot"
)

func TestLogScanFailureUsesSafeReasonAndExpectedLevel(t *testing.T) {
	for _, test := range []struct {
		err   error
		level string
		text  string
	}{
		{err: fsroot.ErrWalkUnsupported, level: "level=INFO", text: "safe_walk_unsupported"},
		{err: fmt.Errorf("private/path: %w", errors.New("unexpected")), level: "level=ERROR", text: "filesystem_scan_failed"},
	} {
		var output bytes.Buffer
		logScanFailure(slog.New(slog.NewTextHandler(&output, nil)), test.err)
		if !strings.Contains(output.String(), test.level) || !strings.Contains(output.String(), test.text) {
			t.Errorf("log = %q, want %s and %s", output.String(), test.level, test.text)
		}
		if strings.Contains(output.String(), "private/path") || strings.Contains(output.String(), "unexpected") {
			t.Errorf("log exposed filesystem error details: %q", output.String())
		}
	}
}
