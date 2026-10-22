// 本文件验证 Shell 输出持久化前的编码与大小边界。
package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"filesage/internal/store"
)

func TestBoundedShellOutputLimitsEachStreamAndKeepsValidUTF8(t *testing.T) {
	value := strings.Repeat("文", store.MaxShellOutputBytes)
	got, truncated := boundedShellOutput(value)
	if !truncated || len(got) > store.MaxShellOutputBytes || !utf8.ValidString(got) {
		t.Fatalf("bounded output = (bytes=%d, truncated=%t, validUTF8=%t)", len(got), truncated, utf8.ValidString(got))
	}

	got, truncated = boundedShellOutput("short")
	if got != "short" || truncated {
		t.Fatalf("short output = (%q, %t)", got, truncated)
	}

	got, truncated = boundedShellOutput(string([]byte{0xff}))
	if got != "�" || truncated || !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 output = (%q, %t)", got, truncated)
	}
}
