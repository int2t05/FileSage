// Package documenttext 提取经用户确认后用于 Agent 分析的文档正文。
package documenttext

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"filesage/internal/office"
)

// MaxInputBytes 限制单份 PDF 或 Office 文件。
const MaxInputBytes = 16 << 20

// MaxOutputBytes 限制单份文档实际发送给模型的正文。
const MaxOutputBytes = 64 << 10

// Options 记录用户明确选择的正文提取能力。
type Options struct {
	EnableOCR bool
}

// CleanupStaleTemporaryData 清理上次异常退出遗留的私有 OCR 临时目录。
func CleanupStaleTemporaryData() error {
	return cleanupStaleTemporaryData()
}

var (
	ErrUnavailable       = errors.New("document text extraction is unavailable")
	ErrUnsupportedFormat = errors.New("document text format is unsupported")
	ErrInputTooLarge     = errors.New("document text input exceeds the size limit")
	ErrOutputTooLarge    = errors.New("document text output exceeds the size limit")
	ErrInvalidDocument   = errors.New("document text input is invalid")
	ErrNoText            = errors.New("document contains no extractable text")
	ErrBusy              = errors.New("document text extraction is busy")
)

// Supports 判断 Agent 分类支持的正文格式。
func Supports(filePath string) bool {
	return supportsExtension(filepath.Ext(filePath))
}

func supportsExtension(extension string) bool {
	switch strings.ToLower(extension) {
	case ".txt", ".md", ".markdown", ".mdown", ".pdf", ".docx", ".pptx":
		return true
	default:
		return false
	}
}

// Extract 在确认后抽取文本或隔离转换文档；调用方负责复核文件版本。
func Extract(ctx context.Context, data []byte, extension string) ([]byte, error) {
	return ExtractWithOptions(ctx, data, extension, Options{})
}

// ExtractWithOptions 仅在用户开启时对无文字层的文档执行 OCR。
func ExtractWithOptions(ctx context.Context, data []byte, extension string, options Options) ([]byte, error) {
	extension = strings.ToLower(extension)
	if !supportsExtension(extension) {
		return nil, ErrUnsupportedFormat
	}
	if len(data) > MaxInputBytes {
		return nil, ErrInputTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch extension {
	case ".txt", ".md", ".markdown", ".mdown":
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil, ErrInvalidDocument
		}
		if len(data) > MaxOutputBytes {
			return nil, ErrOutputTooLarge
		}
		return append([]byte(nil), data...), nil
	case ".pdf":
		return extractPDF(ctx, data, options.EnableOCR)
	case ".docx", ".pptx":
		pdf, err := office.Convert(ctx, data, extension)
		if err != nil {
			if errors.Is(err, office.ErrUnavailable) {
				return nil, ErrUnavailable
			}
			return nil, err
		}
		return extractPDF(ctx, pdf, options.EnableOCR)
	default:
		return nil, ErrUnsupportedFormat
	}
}
