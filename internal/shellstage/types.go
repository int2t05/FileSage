// Package shellstage 在 Linux Bubblewrap 沙箱中运行 Shell 并生成暂存差异。
package shellstage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

var (
	ErrUnavailable    = errors.New("shellstage: sandbox unavailable")
	ErrInvalidRequest = errors.New("shellstage: invalid request")
	ErrUnsafeStaging  = errors.New("shellstage: unsafe staging tree")
	ErrInputChanged   = errors.New("shellstage: read-only input changed")
	ErrWallTimeLimit  = errors.New("shellstage: wall-time limit exceeded")
	ErrCPUTimeLimit   = errors.New("shellstage: CPU-time limit exceeded")
	ErrMemoryLimit    = errors.New("shellstage: aggregate memory limit exceeded")
	ErrOutputLimit    = errors.New("shellstage: output limit exceeded")
	ErrStagingLimit   = errors.New("shellstage: staging limit exceeded")
	ErrTemporaryLimit = errors.New("shellstage: temporary-space limit exceeded")
	ErrFileSizeLimit  = errors.New("shellstage: file-size limit exceeded")
	ErrProcessLimit   = errors.New("shellstage: process-count limit exceeded")
)

// Operation 描述工作区中的一项文件变化。
type Operation string

const (
	Added    Operation = "added"
	Modified Operation = "modified"
	Deleted  Operation = "deleted"
)

// Request 只接受已隔离的基线目录和独立工作区副本。
type Request struct {
	BaselineDir  string
	WorkspaceDir string
	WorkingDir   string
	Command      string
	Limits       Limits
}

// Limits 是每次运行必须明确提供的资源上限。
type Limits struct {
	WallTime            time.Duration
	CPUSeconds          uint64
	AddressSpaceBytes   uint64
	MemoryBytes         uint64
	Processes           uint64
	FileSizeBytes       int64
	StagingBytes        int64
	TemporaryBytes      int64
	OutputBytes         int64
	MaxFiles            int
	MaxDepth            int
	MaxDiffBytesPerFile int64
}

// DefaultLimits 返回适合单次本地文件任务的保守上限。
func DefaultLimits() Limits {
	return Limits{
		WallTime:            2 * time.Minute,
		CPUSeconds:          30,
		AddressSpaceBytes:   512 << 20,
		MemoryBytes:         1 << 30,
		Processes:           128,
		FileSizeBytes:       256 << 20,
		StagingBytes:        1 << 30,
		TemporaryBytes:      64 << 20,
		OutputBytes:         256 << 10,
		MaxFiles:            20_000,
		MaxDepth:            64,
		MaxDiffBytesPerFile: 64 << 10,
	}
}

// FileState 汇总一条路径的内容与权限状态。
type FileState struct {
	SHA256      string
	Mode        uint32
	IsDirectory bool
	Size        int64
}

// Change 是路径排序后的完整差异项。
type Change struct {
	Path          string
	Operation     Operation
	Before        *FileState
	After         *FileState
	Binary        bool
	TextDiff      string
	DiffTruncated bool
}

// Result 返回命令状态、限长输出和完整路径差异清单。
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	Changes  []Change
}

// CheckAvailability 验证当前 Linux 主机能否建立完整沙箱。
func CheckAvailability(ctx context.Context) error {
	return platformCheckAvailability(ctx)
}

// Execute 在隔离副本中运行命令并返回可审阅差异，不提交任何文件。
func Execute(ctx context.Context, req Request) (Result, error) {
	return platformExecute(ctx, req)
}

// validateRequest 拒绝不完整、越界或未设置硬上限的请求。
func validateRequest(req Request) error {
	if strings.TrimSpace(req.Command) == "" || len(req.Command) > 64<<10 {
		return fmt.Errorf("%w: command must contain 1 to 65536 bytes", ErrInvalidRequest)
	}
	if req.WorkingDir == "" || req.WorkingDir == "." {
		req.WorkingDir = "."
	} else if path.IsAbs(req.WorkingDir) || path.Clean(req.WorkingDir) != req.WorkingDir || !fs.ValidPath(req.WorkingDir) || strings.Contains(req.WorkingDir, `\`) {
		return fmt.Errorf("%w: working directory must be a clean relative path", ErrInvalidRequest)
	}
	if req.BaselineDir == "" || req.WorkspaceDir == "" {
		return fmt.Errorf("%w: both staging directories are required", ErrInvalidRequest)
	}
	return validateLimits(req.Limits)
}

// validateLimits 限制请求方可配置的最大资源预算。
func validateLimits(l Limits) error {
	if l.WallTime < time.Second || l.WallTime > 15*time.Minute ||
		l.CPUSeconds == 0 || l.CPUSeconds > 300 ||
		l.AddressSpaceBytes < 32<<20 || l.AddressSpaceBytes > 8<<30 ||
		l.MemoryBytes < 32<<20 || l.MemoryBytes > 8<<30 ||
		l.Processes < 2 || l.Processes > 4096 ||
		l.FileSizeBytes < 1 || l.FileSizeBytes > 4<<30 ||
		l.StagingBytes < 1<<20 || l.StagingBytes > 16<<30 ||
		l.TemporaryBytes < 1<<20 || l.TemporaryBytes > 8<<30 || l.TemporaryBytes > l.StagingBytes ||
		l.OutputBytes < 1 || l.OutputBytes > 8<<20 ||
		l.MaxFiles < 2 || l.MaxFiles > 100_000 ||
		l.MaxDepth < 1 || l.MaxDepth > 256 ||
		l.MaxDiffBytesPerFile < 128 || l.MaxDiffBytesPerFile > 1<<20 {
		return fmt.Errorf("%w: resource limits are missing or exceed supported bounds", ErrInvalidRequest)
	}
	return nil
}
