//go:build linux

package documenttext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	pdfOCRTimeout       = 60 * time.Second
	pdfOCRAddressSpace  = 1 << 30
	pdfOCRScaleTo       = 1600
	pdfOCRPageBytes     = 4 << 20
	pdfOCRTotalPageSize = 64 << 20
	pdfOCRLanguages     = "chi_sim+eng"
)

var (
	pdfOCRPathsOnce  sync.Once
	pdfOCRRasterizer string
	pdfOCRPageInfo   string
	pdfOCRExecutable string
	pdfOCRPathErr    error
)

type ocrMount struct {
	source   string
	target   string
	readOnly bool
}

// extractScannedPDF 仅对无文字层的 PDF 识别前 20 页。
func extractScannedPDF(ctx context.Context, data []byte) (text []byte, err error) {
	pdfOCRPathsOnce.Do(discoverPDFOCRPaths)
	if pdfOCRPathErr != nil {
		return nil, pdfOCRPathErr
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, pdfOCRTimeout)
	defer cancel()
	if err := ensureOCRLanguages(deadlineCtx); err != nil {
		return nil, err
	}
	temporaryDirectory, err := newPDFOCRDirectory()
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temporaryDirectory); cleanupErr != nil {
			text = nil
			err = errors.Join(err, fmt.Errorf("%w: OCR temporary data cleanup failed", ErrUnavailable))
		}
	}()
	pageCount, err := countOCRPages(deadlineCtx, data)
	if err != nil {
		return nil, err
	}
	if pageCount == 0 {
		return nil, ErrNoText
	}
	output := &boundedOutput{limit: MaxOutputBytes + 1}
	var totalPageBytes int64
	for page := 1; page <= pageCount; page++ {
		pageName, pageBytes, renderErr := renderOCRPage(deadlineCtx, data, temporaryDirectory, page)
		if renderErr != nil {
			return nil, renderErr
		}
		if totalPageBytes > pdfOCRTotalPageSize-pageBytes {
			return nil, ErrOutputTooLarge
		}
		totalPageBytes += pageBytes
		pageOutput, recognizeErr := recognizeOCRPage(deadlineCtx, temporaryDirectory, pageName)
		removeErr := os.Remove(filepath.Join(temporaryDirectory, pageName))
		if removeErr != nil {
			return nil, fmt.Errorf("%w: OCR temporary page cleanup failed", ErrUnavailable)
		}
		if recognizeErr != nil {
			return nil, recognizeErr
		}
		if pageOutput.total > int64(MaxOutputBytes)-output.total {
			return nil, ErrOutputTooLarge
		}
		if pageOutput.total == 0 {
			continue
		}
		_, _ = output.Write(pageOutput.data.Bytes())
		_, _ = output.Write([]byte("\n"))
	}
	text = bytes.TrimSpace(output.data.Bytes())
	if output.total > MaxOutputBytes {
		return nil, ErrOutputTooLarge
	}
	if !validDocumentText(text) {
		return nil, ErrInvalidDocument
	}
	if len(text) == 0 {
		return nil, ErrNoText
	}
	return append([]byte(nil), text...), nil
}

// discoverPDFOCRPaths 只启用系统安装的 Poppler 与 Tesseract。
func discoverPDFOCRPaths() {
	pdfOCRRasterizer, pdfOCRPathErr = trustedSystemExecutable("pdftoppm")
	if pdfOCRPathErr != nil {
		pdfOCRPathErr = fmt.Errorf("%w: PDF rasterizer was not found", ErrUnavailable)
		return
	}
	pdfOCRPageInfo, pdfOCRPathErr = trustedSystemExecutable("pdfinfo")
	if pdfOCRPathErr != nil {
		pdfOCRPathErr = fmt.Errorf("%w: PDF page counter was not found", ErrUnavailable)
		return
	}
	pdfOCRExecutable, pdfOCRPathErr = trustedSystemExecutable("tesseract")
	if pdfOCRPathErr != nil {
		pdfOCRPathErr = fmt.Errorf("%w: OCR engine was not found", ErrUnavailable)
		return
	}
	if err := cleanupStalePDFOCRDirectories(time.Now()); err != nil {
		pdfOCRPathErr = err
	}
}

func cleanupStalePDFOCRDirectories(now time.Time) error {
	parent, err := os.Lstat("/tmp")
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%w: OCR temporary storage cannot be checked", ErrUnavailable)
	}
	entries, err := os.ReadDir("/tmp")
	if err != nil {
		return fmt.Errorf("%w: OCR temporary storage cannot be checked", ErrUnavailable)
	}
	cutoff := now.Add(-24 * time.Hour)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "filesage-documenttext-ocr-") {
			continue
		}
		path := filepath.Join("/tmp", entry.Name())
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: OCR temporary storage cannot be checked", ErrUnavailable)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !info.ModTime().Before(cutoff) {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("%w: stale OCR temporary data could not be removed", ErrUnavailable)
		}
	}
	return nil
}

func countOCRPages(ctx context.Context, pdf []byte) (int, error) {
	command := newPDFOCRCommand(ctx, pdfOCRPageInfo, []string{"-"}, nil, 8, 8<<20)
	command.Stdin = bytes.NewReader(pdf)
	stdout := &boundedOutput{limit: 8 << 10}
	command.Stdout = stdout
	command.Stderr = &boundedOutput{limit: 8 << 10}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("%w: PDF page count failed", ErrInvalidDocument)
	}
	for _, line := range strings.Split(string(stdout.data.Bytes()), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimSuffix(fields[0], ":") != "Pages" {
			continue
		}
		count, err := strconv.Atoi(fields[1])
		if err != nil || count < 1 {
			return 0, ErrInvalidDocument
		}
		if count > maxPDFPages {
			return maxPDFPages, nil
		}
		return count, nil
	}
	return 0, ErrInvalidDocument
}

func renderOCRPage(ctx context.Context, pdf []byte, directory string, page int) (string, int64, error) {
	const name = "page.png"
	command := newPDFOCRCommand(ctx, pdfOCRRasterizer, []string{
		"-q", "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-gray", "-png", "-singlefile",
		"-scale-to", strconv.Itoa(pdfOCRScaleTo), "-", "/work/page",
	}, []ocrMount{{source: directory, target: "/work"}}, 20, pdfOCRPageBytes)
	command.Stdin = bytes.NewReader(pdf)
	command.Stdout = io.Discard
	command.Stderr = &boundedOutput{limit: 8 << 10}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, fmt.Errorf("%w: PDF page rasterization failed", ErrInvalidDocument)
	}
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", 0, fmt.Errorf("%w: OCR page is unavailable", ErrInvalidDocument)
	}
	if info.Size() <= 0 || info.Size() > pdfOCRPageBytes {
		return "", 0, ErrOutputTooLarge
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, ErrInvalidDocument
	}
	config, decodeErr := png.DecodeConfig(file)
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > pdfOCRMaxPixels {
		return "", 0, ErrInvalidDocument
	}
	return name, info.Size(), nil
}

func ensureOCRLanguages(ctx context.Context) error {
	command := newPDFOCRCommand(ctx, pdfOCRExecutable, []string{"--list-langs"}, nil, 8, 8<<20)
	stdout := &boundedOutput{limit: 8 << 10}
	command.Stdout = stdout
	command.Stderr = &boundedOutput{limit: 8 << 10}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: OCR language data is unavailable", ErrUnavailable)
	}
	languages := make(map[string]bool, 2)
	for _, line := range strings.Fields(string(stdout.data.Bytes())) {
		languages[line] = true
	}
	if !languages["chi_sim"] || !languages["eng"] {
		return fmt.Errorf("%w: Simplified Chinese and English OCR data are required", ErrUnavailable)
	}
	return nil
}

const pdfOCRMaxPixels = int64(pdfOCRScaleTo) * pdfOCRScaleTo

func recognizeOCRPage(ctx context.Context, directory, name string) (*boundedOutput, error) {
	command := newPDFOCRCommand(ctx, pdfOCRExecutable, []string{
		filepath.Join("/input", name), "stdout", "-l", pdfOCRLanguages, "--psm", "3",
	}, []ocrMount{{source: directory, target: "/input", readOnly: true}}, 10, 8<<20)
	output := &boundedOutput{limit: MaxOutputBytes + 1}
	command.Stdout = output
	command.Stderr = &boundedOutput{limit: 8 << 10}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: OCR failed", ErrInvalidDocument)
	}
	return output, nil
}

func newPDFOCRCommand(ctx context.Context, executable string, commandArgs []string, mounts []ocrMount, cpuSeconds int, fileSize int64) *exec.Cmd {
	args := []string{
		"--as=" + strconv.Itoa(pdfOCRAddressSpace), "--cpu=" + strconv.Itoa(cpuSeconds),
		"--fsize=" + strconv.FormatInt(fileSize, 10), "--nofile=128", "--nproc=512", "--",
		pdfTextBwrapPath,
		"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session",
		"--cap-drop", "ALL", "--tmpfs", "/", "--dir", "/usr", "--ro-bind", "/usr", "/usr",
		"--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--chdir", "/tmp",
		"--clearenv", "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/tmp",
		"--setenv", "LANG", "C.UTF-8", "--setenv", "LC_ALL", "C.UTF-8", "--setenv", "OMP_THREAD_LIMIT", "1",
	}
	for _, mount := range mounts {
		args = append(args, "--dir", mount.target)
		mode := "--bind"
		if mount.readOnly {
			mode = "--ro-bind"
		}
		args = append(args, mode, mount.source, mount.target)
	}
	args = append(args, "--", executable)
	args = append(args, commandArgs...)
	args = insertPDFLibraryMounts(args)
	command := exec.CommandContext(ctx, pdfTextPrlimitPath, args...)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "OMP_THREAD_LIMIT=1"}
	return command
}

func newPDFOCRDirectory() (string, error) {
	parent, err := os.Lstat("/tmp")
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode()&os.ModeSticky == 0 {
		return "", fmt.Errorf("%w: private OCR temporary storage is unavailable", ErrUnavailable)
	}
	directory, err := os.MkdirTemp("/tmp", "filesage-documenttext-ocr-")
	if err != nil {
		return "", fmt.Errorf("%w: private OCR temporary storage is unavailable", ErrUnavailable)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", fmt.Errorf("%w: private OCR temporary storage is unavailable", ErrUnavailable)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", fmt.Errorf("%w: private OCR temporary storage is unavailable", ErrUnavailable)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		_ = os.RemoveAll(directory)
		return "", fmt.Errorf("%w: private OCR temporary storage is unsafe", ErrUnavailable)
	}
	return directory, nil
}

func validDocumentText(text []byte) bool {
	return utf8.Valid(text) && bytes.IndexByte(text, 0) < 0
}
