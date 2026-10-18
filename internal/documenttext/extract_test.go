package documenttext

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestSupportsOnlyClassificationFormats(t *testing.T) {
	for _, name := range []string{"note.TXT", "guide.md", "archive.mdown", "scan.pdf", "report.docx", "slides.pptx"} {
		if !Supports(name) {
			t.Errorf("Supports(%q) = false", name)
		}
	}
	for _, name := range []string{"sheet.xlsx", "legacy.doc", "image.png", ""} {
		if Supports(name) {
			t.Errorf("Supports(%q) = true", name)
		}
	}
}

func TestExtractTextAppliesUTF8AndOutputLimits(t *testing.T) {
	got, err := Extract(context.Background(), []byte("中文正文"), ".MD")
	if err != nil || string(got) != "中文正文" {
		t.Fatalf("Extract() = (%q, %v)", got, err)
	}
	for _, body := range [][]byte{{0xff}, {'a', 0}} {
		if _, err := Extract(context.Background(), body, ".txt"); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("invalid text error = %v", err)
		}
	}
	if _, err := Extract(context.Background(), bytes.Repeat([]byte("x"), MaxOutputBytes+1), ".txt"); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("oversized text error = %v", err)
	}
	if _, err := Extract(context.Background(), nil, ".xlsx"); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("unsupported format error = %v", err)
	}
}
