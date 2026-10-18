//go:build !linux

package imagepreview

import (
	"bytes"
	"errors"
	"testing"
)

// TestGenerateRequiresLinuxProcessLimits 禁止无资源限制的非 Linux 解码。
func TestGenerateRequiresLinuxProcessLimits(t *testing.T) {
	generator := &Generator{heifDecoder: "decoder", rawDecoder: "decoder"}
	if _, err := generator.Generate(t.Context(), "photo.heic", bytes.NewReader([]byte("image")), Thumbnail); !errors.Is(err, ErrDisabled) {
		t.Fatalf("non-Linux decode error = %v, want ErrDisabled", err)
	}
}
