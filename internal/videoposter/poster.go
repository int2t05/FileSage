// Package videoposter 按需从根目录内已安全打开的视频流生成 JPEG 预览图。
// Linux 进程组清理和 ffmpeg 参数限制不构成 CPU、内存或系统调用沙箱。
package videoposter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"filesage/internal/safecmd"
)

const (
	defaultTimeout       = 15 * time.Second
	defaultConcurrency   = 2
	maxConcurrency       = 2
	defaultOutputLimit   = 4 << 20
	defaultStderrLimit   = 64 << 10
	defaultMaxAllocation = 64 << 20
)

var (
	// ErrDisabled 表示系统未安装或无法启动 ffmpeg。
	ErrDisabled = errors.New("video poster disabled")
	// ErrTimeout 表示缩略图生成超过时限。
	ErrTimeout = errors.New("video poster timed out")
	// ErrOutputLimit 表示生成结果超过缓冲上限。
	ErrOutputLimit = errors.New("video poster output limit exceeded")
	// ErrInvalidJPEG 表示 ffmpeg 未返回完整 JPEG。
	ErrInvalidJPEG = errors.New("video poster is not a complete JPEG")
)

// Options 配置缩略图生成进程和资源上限。
type Options struct {
	Binary         string
	Timeout        time.Duration
	MaxConcurrent  int
	MaxOutputBytes int
	MaxStderrBytes int
	MaxAllocation  int
}

// Generator 使用 ffmpeg 从已由调用方安全打开的视频流提取首帧。
type Generator struct {
	binary         string
	disabled       error
	timeout        time.Duration
	maxOutputBytes int
	maxStderrBytes int
	maxAllocation  int
	semaphore      chan struct{}

	// 以下字段仅供同包进程级测试替换命令，不参与生产配置。
	commandPrefix []string
	commandEnv    []string
	commandMu     sync.Mutex
}

// New 创建生成器；ffmpeg 不可用时保留禁用状态，不影响服务启动。
func New(options Options) *Generator {
	binary := options.Binary
	var disabled error
	if binary == "" {
		binary = "ffmpeg"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		disabled = fmt.Errorf("%w: ffmpeg executable not found", ErrDisabled)
	} else {
		binary = resolved
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	concurrency := options.MaxConcurrent
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	if concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}
	outputLimit := options.MaxOutputBytes
	if outputLimit <= 0 {
		outputLimit = defaultOutputLimit
	}
	stderrLimit := options.MaxStderrBytes
	if stderrLimit <= 0 {
		stderrLimit = defaultStderrLimit
	}
	maxAllocation := options.MaxAllocation
	if maxAllocation <= 0 {
		maxAllocation = defaultMaxAllocation
	}
	return &Generator{
		binary:         binary,
		disabled:       disabled,
		timeout:        timeout,
		maxOutputBytes: outputLimit,
		maxStderrBytes: stderrLimit,
		maxAllocation:  maxAllocation,
		semaphore:      make(chan struct{}, concurrency),
	}
}

// Generate 返回 JPEG 字节；视频路径从不传给 ffmpeg，输入只通过 stdin 提供。
func (g *Generator) Generate(ctx context.Context, video io.Reader) ([]byte, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: generator is unavailable", ErrDisabled)
	}
	if g.disabled != nil {
		return nil, g.disabled
	}
	if video == nil {
		return nil, errors.New("video reader is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	runCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	select {
	case g.semaphore <- struct{}{}:
		defer func() { <-g.semaphore }()
	case <-runCtx.Done():
		return nil, contextError(ctx, runCtx)
	}

	args := ffmpegArguments(g.maxAllocation)
	g.commandMu.Lock()
	prefix := append([]string(nil), g.commandPrefix...)
	extraEnv := append([]string(nil), g.commandEnv...)
	g.commandMu.Unlock()
	cmd, err := safecmd.CommandContext(runCtx, g.binary, append(prefix, args...)...)
	if err != nil {
		return nil, fmt.Errorf("prepare ffmpeg isolation: %w", err)
	}
	if len(extraEnv) > 0 {
		cmd.Env = mergeEnvironment(cmd.Env, extraEnv)
	}
	configureProcess(cmd)
	cmd.Stdin = video
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg output: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg diagnostics: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrDisabled, err)
		}
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	out := *bytes.NewBuffer(make([]byte, 0, g.maxOutputBytes))
	diagnostics := *bytes.NewBuffer(make([]byte, 0, g.maxStderrBytes))
	var stderrTruncated bool
	stdoutErr := make(chan error, 1)
	stderrDone := make(chan struct{})
	go func() {
		_, _, copyErr := copyLimited(&out, stdout, g.maxOutputBytes, true)
		stdoutErr <- copyErr
		if errors.Is(copyErr, ErrOutputLimit) && cmd.Process != nil {
			_ = killProcessTree(cmd)
		}
	}()
	go func() {
		_, stderrTruncated, _ = copyLimited(&diagnostics, stderr, g.maxStderrBytes, false)
		close(stderrDone)
	}()

	copyErr := <-stdoutErr
	<-stderrDone
	// 先读完管道再等待进程，避免 Wait 提前关闭仍在读取的文件描述符。
	waitErr := cmd.Wait()
	if copyErr != nil {
		if errors.Is(copyErr, ErrOutputLimit) {
			return nil, ErrOutputLimit
		}
	}
	if runCtx.Err() != nil {
		return nil, contextError(ctx, runCtx)
	}
	if copyErr != nil {
		return nil, fmt.Errorf("read ffmpeg output: %w", copyErr)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(diagnostics.String())
		if stderrTruncated {
			detail += " [stderr truncated]"
		}
		if detail != "" {
			return nil, fmt.Errorf("ffmpeg failed: %s: %w", detail, waitErr)
		}
		return nil, fmt.Errorf("ffmpeg failed: %w", waitErr)
	}
	image := out.Bytes()
	if len(image) < 4 || image[0] != 0xff || image[1] != 0xd8 || image[len(image)-2] != 0xff || image[len(image)-1] != 0xd9 {
		return nil, fmt.Errorf("%w (size %d)", ErrInvalidJPEG, len(image))
	}
	return image, nil
}

func mergeEnvironment(environment, overrides []string) []string {
	merged := append([]string(nil), environment...)
	for _, override := range overrides {
		key, _, ok := strings.Cut(override, "=")
		if !ok {
			continue
		}
		for i := len(merged) - 1; i >= 0; i-- {
			oldKey, _, found := strings.Cut(merged[i], "=")
			if found && strings.EqualFold(oldKey, key) {
				merged = append(merged[:i], merged[i+1:]...)
			}
		}
		merged = append(merged, override)
	}
	return merged
}

func ffmpegArguments(maxAllocation int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-threads", "1", "-filter_threads", "1", "-filter_complex_threads", "1",
		"-max_alloc", fmt.Sprint(maxAllocation),
		"-protocol_whitelist", "pipe",
		"-format_whitelist", "3gp,asf,avi,flv,matroska,mov,mpeg,mpegts,ogg,rm,webm",
		"-probesize", "5M", "-analyzeduration", "5000000",
		"-i", "pipe:0", "-map", "0:v:0", "-an", "-sn", "-dn",
		"-frames:v", "1",
		"-vf", "scale=w='min(480,iw)':h='min(480,ih)':force_original_aspect_ratio=decrease",
		"-threads", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "3", "pipe:1",
	}
}

// copyLimited 限制保留内存；stdout 超限立即终止，stderr 超限后继续排空。
func copyLimited(dst *bytes.Buffer, src io.Reader, limit int, stopOnLimit bool) (int, bool, error) {
	chunk := make([]byte, 32<<10)
	truncated := false
	for {
		n, readErr := src.Read(chunk)
		if n > 0 {
			remaining := limit - dst.Len()
			if remaining > 0 {
				keep := n
				if keep > remaining {
					keep = remaining
				}
				_, _ = dst.Write(chunk[:keep])
			}
			if n > remaining {
				truncated = true
				if stopOnLimit {
					return dst.Len(), true, ErrOutputLimit
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return dst.Len(), truncated, nil
			}
			return dst.Len(), truncated, readErr
		}
	}
}

func contextError(parent, run context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if errors.Is(run.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return run.Err()
}
