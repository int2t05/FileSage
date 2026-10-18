//go:build linux

// 本文件在私有暂存目录中创建 7z，避免归档工具直接读取原件。
package fsroot

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	sevenZipArchiveOverhead = 2 << 20
	sevenZipEntryOverhead   = 1024
)

// writeSevenZipArchive 先安全暂存原件，再调用 7-Zip 生成归档。
func writeSevenZipArchive(ctx context.Context, root *Root, selectedPaths []string, dst io.Writer, limits ArchiveLimits) error {
	binary, err := sevenZipBinary()
	if err != nil {
		return fmt.Errorf("locate 7-Zip: %w: %w", ErrArchiveDependency, err)
	}
	release, err := acquireSevenZip(ctx)
	if err != nil {
		return err
	}
	defer release()

	maxOutput, err := sevenZipArchiveOutputLimit(limits)
	if err != nil {
		return err
	}
	workDir, err := os.MkdirTemp("", "filesage-7z-")
	if err != nil {
		return fmt.Errorf("create 7-Zip work directory: %w: %w", ErrArchiveStorage, err)
	}
	defer os.RemoveAll(workDir)
	if err := os.Chmod(workDir, 0o700); err != nil {
		return fmt.Errorf("secure 7-Zip work directory: %w: %w", ErrArchiveStorage, err)
	}
	stageRoot := filepath.Join(workDir, "source")
	if err := os.Mkdir(stageRoot, 0o700); err != nil {
		return fmt.Errorf("create 7-Zip source directory: %w: %w", ErrArchiveStorage, err)
	}
	if err := stageSevenZipSource(ctx, root, selectedPaths, stageRoot, limits); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	archivePath := filepath.Join(workDir, "archive.7z")
	if err := createSevenZipFile(ctx, binary, stageRoot, archivePath, workDir, selectedPaths, maxOutput); err != nil {
		return err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open 7-Zip result: %w: %w", ErrArchiveStorage, err)
	}
	info, statErr := archive.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxOutput {
		_ = archive.Close()
		if statErr != nil {
			return fmt.Errorf("inspect 7-Zip result: %w: %w", ErrArchiveStorage, statErr)
		}
		return ErrArchiveLimitExceeded
	}
	_, copyErr := io.Copy(dst, &archiveContextReader{ctx: ctx, reader: io.LimitReader(archive, maxOutput+1)})
	closeErr := archive.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if copyErr != nil {
		return fmt.Errorf("copy 7-Zip result: %w: %w", ErrArchiveStorage, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close 7-Zip result: %w: %w", ErrArchiveStorage, closeErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return root.verifyRootPath()
}

// sevenZipArchiveOutputLimit 为文件数据和归档头部计算输出上限。
func sevenZipArchiveOutputLimit(limits ArchiveLimits) (int64, error) {
	if limits.MaxEntries < 1 || limits.MaxBytes < 1 {
		return 0, fsErrArchiveRequest()
	}
	if int64(limits.MaxEntries) > (int64(^uint64(0)>>1)-sevenZipArchiveOverhead)/sevenZipEntryOverhead {
		return int64(^uint64(0) >> 1), nil
	}
	overhead := int64(limits.MaxEntries)*sevenZipEntryOverhead + sevenZipArchiveOverhead
	if limits.MaxBytes > int64(^uint64(0)>>1)-overhead {
		return int64(^uint64(0) >> 1), nil
	}
	return limits.MaxBytes + overhead, nil
}

// stageSevenZipSource 通过受限 TAR 流暂存原件，并等待来源状态复核完成。
func stageSevenZipSource(ctx context.Context, root *Root, selectedPaths []string, stageRoot string, limits ArchiveLimits) error {
	reader, writer := io.Pipe()
	producerDone := make(chan error, 1)
	go func() {
		writeErr := root.WriteArchiveFormat(ctx, selectedPaths, writer, limits, ArchiveFormatTAR)
		if writeErr != nil {
			_ = writer.CloseWithError(writeErr)
		} else {
			_ = writer.Close()
		}
		producerDone <- writeErr
	}()

	stageErr := extractSevenZipStage(ctx, reader, stageRoot, limits)
	if stageErr != nil {
		_ = reader.CloseWithError(stageErr)
	} else {
		_ = reader.Close()
	}
	producerErr := <-producerDone
	if stageErr != nil {
		return stageErr
	}
	return producerErr
}

// extractSevenZipStage 只接收生成器输出的普通文件和目录。
func extractSevenZipStage(ctx context.Context, source io.Reader, stageRoot string, limits ArchiveLimits) error {
	reader := tar.NewReader(source)
	seen := make(map[string]struct{})
	entries := 0
	var totalBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header == nil {
			return ErrArchiveUnsafeEntry
		}
		isDirectory := header.Typeflag == tar.TypeDir
		name := header.Name
		if isDirectory {
			name = strings.TrimSuffix(name, "/")
		} else if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return ErrArchiveUnsafeEntry
		}
		if name == "" || validateRelativePath(name) != nil || hasTrashComponent(name) {
			return ErrArchiveUnsafeEntry
		}
		components := strings.Split(name, "/")
		if len(components) > maxArchiveDepth {
			return ErrArchiveLimitExceeded
		}
		for _, component := range components {
			if !validArchiveComponent(component) {
				return ErrArchiveUnsafeEntry
			}
		}
		if _, exists := seen[name]; exists {
			return ErrArchiveUnsafeEntry
		}
		seen[name] = struct{}{}
		entries++
		if entries > limits.MaxEntries {
			return ErrArchiveLimitExceeded
		}
		target := filepath.Join(stageRoot, filepath.FromSlash(name))
		if isDirectory {
			if header.Size != 0 {
				return ErrArchiveCorrupt
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				return fmt.Errorf("stage directory: %w: %w", ErrArchiveStorage, err)
			}
			continue
		}
		if header.Size < 0 || header.Size > limits.MaxBytes-totalBytes {
			return ErrArchiveLimitExceeded
		}
		parent := filepath.Dir(target)
		if info, err := os.Stat(parent); err != nil || !info.IsDir() {
			if err != nil {
				return fmt.Errorf("inspect staged parent: %w: %w", ErrArchiveStorage, err)
			}
			return ErrArchiveUnsafeEntry
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create staged file: %w: %w", ErrArchiveStorage, err)
		}
		copied, copyErr := io.CopyN(file, &archiveContextReader{ctx: ctx, reader: reader}, header.Size)
		if copyErr == nil && copied != header.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr == nil {
			copyErr = file.Sync()
		}
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("write staged file: %w: %w", ErrArchiveStorage, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close staged file: %w: %w", ErrArchiveStorage, closeErr)
		}
		totalBytes += copied
	}
}

// createSevenZipFile 限制命令参数并在生成期间监控归档大小。
func createSevenZipFile(ctx context.Context, binary, stageRoot, archivePath, workDir string, selectedPaths []string, maxOutput int64) error {
	arguments := []string{"a", "-t7z", "-mx=1", "-mmt=2", "-bd", "-y", "-spd", archivePath}
	for _, selectedPath := range selectedPaths {
		arguments = append(arguments, "./"+selectedPath)
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = stageRoot
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + workDir, "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	command.Stdout = io.Discard
	stderr := &boundedBuffer{limit: 64 << 10}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start 7-Zip: %w", ErrArchiveCommand)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isArchiveStorageMessage(stderr.String()) {
				return ErrArchiveStorage
			}
			return fmt.Errorf("7-Zip archive command failed: %w", ErrArchiveCommand)
		case <-ctx.Done():
			_ = command.Process.Kill()
			<-done
			return ctx.Err()
		case <-ticker.C:
			info, err := os.Stat(archivePath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				_ = command.Process.Kill()
				<-done
				return fmt.Errorf("inspect 7-Zip output: %w: %w", ErrArchiveStorage, err)
			}
			if !info.Mode().IsRegular() || info.Size() > maxOutput {
				_ = command.Process.Kill()
				<-done
				return ErrArchiveLimitExceeded
			}
		}
	}
}

// isArchiveStorageMessage 识别外部工具报告的磁盘空间错误。
func isArchiveStorageMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "no space left on device") || strings.Contains(lower, "disk quota exceeded") || strings.Contains(lower, "not enough space")
}
