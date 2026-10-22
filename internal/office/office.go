// Package office 在受限子进程中生成 Office 文档的只读 PDF 预览。
package office

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// MaxInputBytes 限制单个 Office 源文件的大小。
	MaxInputBytes int64 = 64 << 20
	// MaxExpandedBytes 限制 OOXML 包声明的展开总量。
	MaxExpandedBytes uint64 = 256 << 20
	// MaxOutputBytes 限制 PDF 预览大小。
	MaxOutputBytes int64 = 128 << 20
	// MaxConcurrentPreviews 限制同时读取和转换的 Office 请求数。
	MaxConcurrentPreviews  = 2
	maxOfficeEntryBytes    = 128 << 20
	maxConcurrentJobs      = MaxConcurrentPreviews
	defaultConversionLimit = 45 * time.Second
)

var (
	ErrUnavailable       = errors.New("office preview dependencies are unavailable")
	ErrUnsupportedFormat = errors.New("office preview format is unsupported")
	ErrInputTooLarge     = errors.New("office preview input exceeds the size limit")
	ErrExpandedTooLarge  = errors.New("office package exceeds the expansion limit")
	ErrInvalidDocument   = errors.New("office document is invalid or unsupported")
	ErrConversionFailed  = errors.New("office document conversion failed")
	ErrOutputTooLarge    = errors.New("office preview output exceeds the size limit")
)

type bindMount struct {
	source string
	target string
}

type executionPaths struct {
	oosplash     string
	programDir   string
	bwrap        string
	prlimit      string
	setpriv      string
	readOnlyDir  []bindMount
	readOnlyFile []bindMount
	loaderPath   string
}

type Converter struct {
	paths   executionPaths
	slots   chan struct{}
	timeout time.Duration
	tempDir string
}

type converterOptions struct {
	timeout time.Duration
	tempDir string
}

var (
	defaultMu        sync.Mutex
	defaultConverter *Converter
	defaultErr       error
)

// New 查找 Office 转换与隔离工具；缺少依赖时不启用转换。
func New() (*Converter, error) {
	paths, err := discoverPaths()
	if err != nil {
		return nil, err
	}
	tempDir, err := privateOfficeTempRoot()
	if err != nil {
		return nil, err
	}
	return newConverter(paths, converterOptions{tempDir: tempDir})
}

// Convert 使用服务默认转换器处理只读预览。
func Convert(ctx context.Context, data []byte, extension string) ([]byte, error) {
	defaultMu.Lock()
	if defaultConverter == nil && defaultErr == nil {
		defaultConverter, defaultErr = New()
	}
	converter, err := defaultConverter, defaultErr
	defaultMu.Unlock()
	if err != nil {
		return nil, err
	}
	return converter.Convert(ctx, data, extension)
}

func newConverter(paths executionPaths, options converterOptions) (*Converter, error) {
	if paths.oosplash == "" || paths.programDir == "" || paths.bwrap == "" || paths.prlimit == "" || paths.setpriv == "" {
		return nil, ErrUnavailable
	}
	timeout := options.timeout
	if timeout <= 0 {
		timeout = defaultConversionLimit
	}
	tempDir := options.tempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Converter{paths: paths, slots: make(chan struct{}, maxConcurrentJobs), timeout: timeout, tempDir: tempDir}, nil
}

// Convert 先校验 OOXML 包，再在独立临时目录和隔离进程中生成 PDF。
func (c *Converter) Convert(ctx context.Context, data []byte, extension string) (pdf []byte, resultErr error) {
	format, ok := officeFormat(extension)
	if !ok {
		return nil, ErrUnsupportedFormat
	}
	if int64(len(data)) > MaxInputBytes {
		return nil, ErrInputTooLarge
	}
	workCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-workCtx.Done():
		return nil, workCtx.Err()
	}
	if err := validateOfficePackageContext(workCtx, data, format); err != nil {
		return nil, err
	}

	workDir, err := os.MkdirTemp(c.tempDir, "job-")
	if err != nil {
		return nil, fmt.Errorf("create private office directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove private office directory: %w", err))
			pdf = nil
		}
	}()
	if err := prepareWorkDir(workDir, data, format.inputName); err != nil {
		return nil, err
	}
	if err := runConverter(workCtx, c.paths, workDir, format); err != nil {
		if errors.Is(workCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("office conversion timed out: %w", context.DeadlineExceeded)
		}
		if errors.Is(workCtx.Err(), context.Canceled) {
			return nil, context.Canceled
		}
		return nil, fmt.Errorf("%w: %v", ErrConversionFailed, err)
	}
	return readPDF(workDir)
}

type officeFormatInfo struct {
	inputName string
	manifest  string
	content   string
	export    string
}

func officeFormat(extension string) (officeFormatInfo, bool) {
	switch strings.ToLower(extension) {
	case ".docx":
		return officeFormatInfo{inputName: "source.docx", manifest: "[Content_Types].xml", content: "word/document.xml", export: "writer_pdf_Export"}, true
	case ".pptx":
		return officeFormatInfo{inputName: "source.pptx", manifest: "[Content_Types].xml", content: "ppt/presentation.xml", export: "impress_pdf_Export"}, true
	default:
		return officeFormatInfo{}, false
	}
}

func validateOfficePackage(data []byte, format officeFormatInfo) error {
	return validateOfficePackageContext(context.Background(), data, format)
}

func validateOfficePackageContext(ctx context.Context, data []byte, format officeFormatInfo) error {
	if int64(len(data)) > MaxInputBytes {
		return ErrInputTooLarge
	}
	if err := preflightOfficeArchive(data); err != nil {
		return err
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxOfficeEntries {
		return ErrInvalidDocument
	}
	var expanded uint64
	manifest, content := false, false
	seen := make(map[string]struct{}, len(reader.File))
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name
		mode := entry.Mode()
		if !safeOfficeEntry(name) || entry.Flags&1 != 0 || mode&fs.ModeSymlink != 0 || !mode.IsRegular() && !mode.IsDir() {
			return ErrInvalidDocument
		}
		if _, exists := seen[name]; exists {
			return ErrInvalidDocument
		}
		seen[name] = struct{}{}
		if entry.UncompressedSize64 > maxOfficeEntryBytes || entry.UncompressedSize64 > MaxExpandedBytes-expanded {
			return ErrExpandedTooLarge
		}
		unpacked, err := entry.Open()
		if err != nil {
			return ErrInvalidDocument
		}
		remaining := MaxExpandedBytes - expanded
		if remaining > maxOfficeEntryBytes {
			remaining = maxOfficeEntryBytes
		}
		actual, readErr := readOfficeEntry(ctx, unpacked, remaining)
		closeErr := unpacked.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrExpandedTooLarge) {
				return ErrExpandedTooLarge
			}
			return ErrInvalidDocument
		}
		if actual != entry.UncompressedSize64 {
			return ErrInvalidDocument
		}
		expanded += actual
		manifest = manifest || name == format.manifest
		content = content || name == format.content
	}
	if !manifest || !content {
		return ErrInvalidDocument
	}
	return nil
}

func readOfficeEntry(ctx context.Context, reader io.Reader, limit uint64) (uint64, error) {
	buffer := make([]byte, 32<<10)
	var total uint64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		readBuffer := buffer
		remaining := limit + 1 - total
		if uint64(len(readBuffer)) > remaining {
			readBuffer = readBuffer[:remaining]
		}
		count, err := reader.Read(readBuffer)
		total += uint64(count)
		if total > limit {
			return total, ErrExpandedTooLarge
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		if count == 0 {
			continue
		}
	}
}

func safeOfficeEntry(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || strings.Contains(strings.SplitN(name, "/", 2)[0], ":") {
		return false
	}
	clean := path.Clean(name)
	return clean == name || clean+"/" == name
}

func prepareWorkDir(workDir string, data []byte, inputName string) error {
	for _, name := range []string{"home", "tmp", "cache", "config", "data", "runtime", "profile", "out"} {
		if err := os.Mkdir(filepath.Join(workDir, name), 0o700); err != nil {
			return fmt.Errorf("prepare private office directory: %w", err)
		}
	}
	input, err := os.OpenFile(filepath.Join(workDir, inputName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private office input: %w", err)
	}
	_, writeErr := input.Write(data)
	closeErr := input.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write private office input: %w", err)
	}
	return nil
}

func readPDF(workDir string) ([]byte, error) {
	pdfPath := filepath.Join(workDir, "out", "source.pdf")
	info, err := os.Lstat(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("%w: converted PDF is missing", ErrConversionFailed)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, ErrInvalidDocument
	}
	if info.Size() > MaxOutputBytes {
		return nil, ErrOutputTooLarge
	}
	file, err := os.Open(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("%w: converted PDF is unavailable", ErrConversionFailed)
	}
	defer file.Close()
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, fmt.Errorf("%w: converted PDF is unreadable", ErrConversionFailed)
	}
	if len(data) < 5 || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, ErrInvalidDocument
	}
	return data, nil
}
