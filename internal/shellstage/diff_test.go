// diff_test.go 覆盖限长文本差异预览。
package shellstage

import (
	"strings"
	"testing"
)

// TestFormatTextDiffRejectsBinary 验证二进制正文不会进入差异输出。
func TestFormatTextDiffRejectsBinary(t *testing.T) {
	diff, truncated := FormatTextDiff([]byte("safe\x00secret"), []byte("next"), "file.txt", 64)
	if truncated || diff != "[binary content omitted]\n" {
		t.Fatalf("binary diff = %q, truncated = %v", diff, truncated)
	}
	if strings.Contains(diff, "secret") {
		t.Fatal("binary payload leaked into diff")
	}
}

// TestFormatTextDiffHonorsOutputLimit 验证差异输出符合字节预算。
func TestFormatTextDiffHonorsOutputLimit(t *testing.T) {
	diff, truncated := FormatTextDiff([]byte("old\n"), []byte("new\n"), "file.txt", 20)
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
	diff, truncated := FormatTextDiff([]byte(input), []byte(input+"extra\n"), "large.txt", 64)
	if !truncated {
		t.Fatal("expected oversized inputs to be marked truncated")
	}
	if len(diff) > 64 {
		t.Fatalf("diff length = %d, limit 64", len(diff))
	}
}

// TestFormatTextDiffUsesUnifiedRanges 验证路径和行号符合常见 unified diff 格式。
func TestFormatTextDiffUsesUnifiedRanges(t *testing.T) {
	tests := []struct {
		name   string
		before []byte
		after  []byte
		want   string
	}{
		{
			name:  "add",
			after: []byte("new\n"),
			want:  "--- a/docs/new.txt\n+++ b/docs/new.txt\n@@ -0,0 +1 @@\n+new\n",
		},
		{
			name:   "modify",
			before: []byte("old\nsame\n"),
			after:  []byte("new\nsame\n"),
			want:   "--- a/docs/file.txt\n+++ b/docs/file.txt\n@@ -1,2 +1,2 @@\n-old\n+new\n same\n",
		},
		{
			name:   "delete",
			before: []byte("old\n"),
			want:   "--- a/docs/file.txt\n+++ b/docs/file.txt\n@@ -1 +0,0 @@\n-old\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := "docs/file.txt"
			if test.name == "add" {
				path = "docs/new.txt"
			}
			got, truncated := FormatTextDiff(test.before, test.after, path, 1024)
			if truncated || got != test.want {
				t.Fatalf("diff = %q, truncated = %v; want %q", got, truncated, test.want)
			}
		})
	}
}
