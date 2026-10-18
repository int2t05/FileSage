//go:build linux

package documenttext

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/office"
	"filesage/internal/testutil/imagepdf"
)

// TestExtractOfficeAndPDFText 使用无敏感样本验证真实沙箱抽取。
func TestExtractOfficeAndPDFText(t *testing.T) {
	if os.Getenv("FILESAGE_DOCUMENTTEXT_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_DOCUMENTTEXT_INTEGRATION=1 to test real sandboxed extraction")
	}
	ctx := context.Background()
	docx, err := os.ReadFile(filepath.Join("..", "office", "testdata", "preview.docx"))
	if err != nil {
		t.Fatal(err)
	}
	docx = fixtureWithChineseText(t, docx)
	pdf, err := office.Convert(ctx, docx, ".docx")
	if err != nil {
		t.Fatalf("create PDF fixture: %v", err)
	}
	for name, input := range map[string]struct {
		data      []byte
		extension string
		marker    string
	}{
		"pdf":  {data: pdf, extension: ".pdf", marker: "中文合同分类测试文本"},
		"docx": {data: docx, extension: ".docx", marker: "中文合同分类测试文本"},
		"pptx": {data: mustReadFixture(t, "preview.pptx"), extension: ".pptx", marker: "FileSage preview smoke"},
	} {
		t.Run(name, func(t *testing.T) {
			text, err := Extract(ctx, input.data, input.extension)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(text), input.marker) {
				t.Fatalf("extracted text omitted fixture marker: %q", text)
			}
		})
	}
}

func TestExtractPDFRejectsWhenProcessingSlotsAreFull(t *testing.T) {
	for range cap(pdfTextSlots) {
		pdfTextSlots <- struct{}{}
	}
	defer func() {
		for range cap(pdfTextSlots) {
			<-pdfTextSlots
		}
	}()
	if _, err := Extract(context.Background(), []byte("%PDF-1.4"), ".pdf"); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy extraction error = %v, want ErrBusy", err)
	}
}

// TestCleanupStaleTemporaryDataOnlyRemovesOldPrivateDirectories 验证清理范围仅限旧私有目录。
func TestCleanupStaleTemporaryDataOnlyRemovesOldPrivateDirectories(t *testing.T) {
	makeDirectory := func(mode os.FileMode, age time.Duration) string {
		t.Helper()
		directory, err := os.MkdirTemp("/tmp", "filesage-documenttext-ocr-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(directory) })
		if err := os.Chmod(directory, mode); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-age)
		if err := os.Chtimes(directory, past, past); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	oldPrivate := makeDirectory(0o700, 48*time.Hour)
	recentPrivate := makeDirectory(0o700, time.Hour)
	oldPublic := makeDirectory(0o755, 48*time.Hour)

	if err := CleanupStaleTemporaryData(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(oldPrivate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old private OCR directory was not removed: %v", err)
	}
	for _, directory := range []string{recentPrivate, oldPublic} {
		if _, err := os.Lstat(directory); err != nil {
			t.Fatalf("non-stale or non-private directory was removed: %v", err)
		}
	}
}

// TestExtractScannedPDFRequiresExplicitOCR 验证扫描件只有主动启用后才执行 OCR。
func TestExtractScannedPDFRequiresExplicitOCR(t *testing.T) {
	if os.Getenv("FILESAGE_DOCUMENTTEXT_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_DOCUMENTTEXT_INTEGRATION=1 to test real OCR")
	}
	ctx := context.Background()
	docx, err := os.ReadFile(filepath.Join("..", "office", "testdata", "preview.docx"))
	if err != nil {
		t.Fatal(err)
	}
	docx = fixtureWithChineseText(t, docx)
	pdf, err := office.Convert(ctx, docx, ".docx")
	if err != nil {
		t.Fatalf("create PDF fixture: %v", err)
	}
	scannedPDF, err := imagepdf.FromPDF(ctx, pdf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(ctx, scannedPDF, ".pdf"); !errors.Is(err, ErrNoText) {
		t.Fatalf("OCR-off extraction error = %v, want ErrNoText", err)
	}
	text, err := ExtractWithOptions(ctx, scannedPDF, ".pdf", Options{EnableOCR: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "中文合同分类测试文本") {
		t.Fatalf("OCR omitted fixture marker: %q", text)
	}
}

func fixtureWithChineseText(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	writer := zip.NewWriter(&result)
	replaced := false
	for _, entry := range reader.File {
		file, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			t.Fatal(err)
		}
		if entry.Name == "word/document.xml" {
			if !bytes.Contains(body, []byte("DOCX preview test fixture.")) {
				t.Fatal("DOCX fixture text changed")
			}
			body = bytes.Replace(body, []byte("DOCX preview test fixture."), []byte("中文合同分类测试文本。"), 1)
			replaced = true
		}
		output, err := writer.Create(entry.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if !replaced {
		t.Fatal("DOCX fixture text was not replaced")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "office", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
