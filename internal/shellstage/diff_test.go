// diff_test.go 覆盖限长文本差异预览。
package shellstage

import (
	"strings"
	"testing"
)

// TestFormatTextDiffRejectsBinary 验证二进制正文不会进入差异输出。
func TestFormatTextDiffRejectsBinary(t *testing.T) {
	diff, truncated := FormatTextDiff([]byte("safe\x00secret"), []byte("next"), 64)
	if truncated || diff != "[binary content omitted]\n" {
		t.Fatalf("binary diff = %q, truncated = %v", diff, truncated)
	}
	if strings.Contains(diff, "secret") {
		t.Fatal("binary payload leaked into diff")
	}
}

// TestFormatTextDiffHonorsOutputLimit 验证差异输出符合字节预算。
func TestFormatTextDiffHonorsOutputLimit(t *testing.T) {
	diff, truncated := FormatTextDiff([]byte("old\n"), []byte("new\n"), 20)
	if !truncated {
		t.Fatal("expected bounded diff to be marked truncated")
	}
	if len(diff) > 20 {
		t.Fatalf("diff length = %d, limit 20", len(diff))
	}
}

// TestFormatTextDiffCapsInputWork 验证超长输入只处理预算内前缀。
func TestFormatTextDiffCapsInputWork(t *testing.T) {
	input := strings.Repeat("line\n", 1024)
	diff, truncated := FormatTextDiff([]byte(input), []byte(input+"extra\n"), 64)
	if !truncated {
		t.Fatal("expected oversized inputs to be marked truncated")
	}
	if len(diff) > 64 {
		t.Fatalf("diff length = %d, limit 64", len(diff))
	}
}
