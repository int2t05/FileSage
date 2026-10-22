//go:build linux

package documenttext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxPDFPages = 20
const pdfTextTimeout = 15 * time.Second
const pdfTextAddressSpace = 1 << 30

var pdfTextSlots = make(chan struct{}, 2)
var pdfTextPathOnce sync.Once
var pdfTextPath string
var pdfTextBwrapPath string
var pdfTextPrlimitPath string
var pdfTextSetprivPath string
var pdfTextPathErr error

// extractPDF 使用无网络沙箱从 PDF 提取限定页数的 UTF-8 正文。
func extractPDF(ctx context.Context, data []byte, enableOCR bool) ([]byte, error) {
	if len(data) < 5 || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, ErrInvalidDocument
	}
	if err := ctx.Err(); err != nil {
		return nil, ctx.Err()
	}
	select {
	case pdfTextSlots <- struct{}{}:
		defer func() { <-pdfTextSlots }()
	default:
		return nil, ErrBusy
	}
	pdfTextPathOnce.Do(discoverPDFTextPaths)
	if pdfTextPathErr != nil {
		return nil, pdfTextPathErr
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, pdfTextTimeout)
	defer cancel()
	args := []string{
		"--as=" + strconv.Itoa(pdfTextAddressSpace), "--cpu=15", "--fsize=67108864", "--nofile=128", "--nproc=512", "--",
		pdfTextSetprivPath, "--ambient-caps=-all", "--",
		pdfTextBwrapPath,
		"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session",
		"--cap-drop", "ALL", "--tmpfs", "/", "--dir", "/usr", "--ro-bind", "/usr", "/usr",
		"--dev", "/dev", "--proc", "/proc", "--dir", "/tmp", "--tmpfs", "/tmp", "--chdir", "/tmp",
		"--clearenv", "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/tmp",
		"--setenv", "LANG", "C.UTF-8", "--setenv", "LC_ALL", "C.UTF-8", "--",
		pdfTextPath, "-q", "-f", "1", "-l", strconv.Itoa(maxPDFPages), "-enc", "UTF-8", "-", "-",
	}
	args = insertPDFLibraryMounts(args)
	command := exec.CommandContext(deadlineCtx, pdfTextPrlimitPath, args...)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	command.Stdin = bytes.NewReader(data)
	stdout := &boundedOutput{limit: MaxOutputBytes + 1}
	stderr := &boundedOutput{limit: 8 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if deadlineCtx.Err() != nil {
			return nil, deadlineCtx.Err()
		}
		if stdout.total == 0 {
			return nil, fmt.Errorf("%w: PDF text extraction failed", ErrInvalidDocument)
		}
		return nil, fmt.Errorf("%w: PDF text extraction process failed", ErrInvalidDocument)
	}
	if stdout.total > MaxOutputBytes {
		return nil, ErrOutputTooLarge
	}
	text := bytes.TrimSpace(stdout.data.Bytes())
	if !utf8.Valid(text) || bytes.IndexByte(text, 0) >= 0 {
		return nil, ErrInvalidDocument
	}
	if len(text) == 0 {
		if !enableOCR {
			return nil, ErrNoText
		}
		return extractScannedPDF(ctx, data)
	}
	return append([]byte(nil), text...), nil
}

// insertPDFLibraryMounts 仅挂载主机现有的只读动态库目录。
func insertPDFLibraryMounts(args []string) []string {
	mounts := make([]string, 0, 12)
	for _, target := range []string{"/lib", "/lib64"} {
		source, err := filepath.EvalSymlinks(target)
		if err != nil {
			continue
		}
		info, err := os.Stat(source)
		if err != nil || !info.IsDir() {
			continue
		}
		mounts = append(mounts, "--dir", target, "--ro-bind", source, target)
	}
	for index, argument := range args {
		if argument == "--dev" {
			result := make([]string, 0, len(args)+len(mounts))
			result = append(result, args[:index]...)
			result = append(result, mounts...)
			result = append(result, args[index:]...)
			return result
		}
	}
	return args
}

// discoverPDFTextPaths 只接受系统安装的固定解析器和沙箱工具。
func discoverPDFTextPaths() {
	pdfTextPath, pdfTextPathErr = trustedSystemExecutable("pdftotext")
	if pdfTextPathErr != nil {
		pdfTextPathErr = fmt.Errorf("%w: pdftotext was not found", ErrUnavailable)
		return
	}
	pdfTextBwrapPath, pdfTextPathErr = trustedSystemExecutable("bwrap")
	if pdfTextPathErr != nil {
		pdfTextPathErr = fmt.Errorf("%w: bubblewrap was not found", ErrUnavailable)
		return
	}
	pdfTextPrlimitPath, pdfTextPathErr = trustedSystemExecutable("prlimit")
	if pdfTextPathErr != nil {
		pdfTextPathErr = fmt.Errorf("%w: prlimit was not found", ErrUnavailable)
		return
	}
	pdfTextSetprivPath, pdfTextPathErr = trustedSystemExecutable("setpriv")
	if pdfTextPathErr != nil {
		pdfTextPathErr = fmt.Errorf("%w: setpriv was not found", ErrUnavailable)
	}
}

func cleanupStaleTemporaryData() error {
	return cleanupStalePDFOCRDirectories(time.Now())
}

func trustedSystemExecutable(name string) (string, error) {
	found, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(found)
	if err != nil || !filepath.IsAbs(resolved) || !strings.HasPrefix(resolved, "/usr/") {
		return "", errors.New("system executable path is invalid")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("system executable is invalid")
	}
	return resolved, nil
}

type boundedOutput struct {
	data  bytes.Buffer
	limit int64
	total int64
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	output.total += int64(len(data))
	if remaining := output.limit - int64(output.data.Len()); remaining > 0 {
		if int64(len(data)) > remaining {
			_, _ = output.data.Write(data[:remaining])
		} else {
			_, _ = output.data.Write(data)
		}
	}
	return len(data), nil
}
