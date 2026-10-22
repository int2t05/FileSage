// 本文件为可续传暂存区限制总容量，并保留有效上传的恢复能力。
package upload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

const (
	// 限制空文件及小文件资源数，避免耗尽暂存区 inode。
	maxStagedUploadCount = 128
)

type stagingQuotaStore struct {
	filestore.FileStore
	quota *stagingQuota
}

type stagingQuota struct {
	dir           string
	maxBytes      int64
	mu            sync.Mutex
	reservedBytes int64
	reservations  map[string]int64
	blocked       bool
}

type stagingQuotaUpload struct {
	tusd.Upload
}

type stagingQuotaTerminator struct {
	delegate tusd.TerminatableUpload
	upload   tusd.Upload
	quota    *stagingQuota
}

// newStagingQuotaStore 从现有暂存元数据恢复容量预留。
func newStagingQuotaStore(store filestore.FileStore, dir string, maxBytes int64) (*stagingQuotaStore, error) {
	if maxBytes <= 0 {
		return nil, errors.New("maximum upload staging size must be positive")
	}
	quota := &stagingQuota{dir: dir, maxBytes: maxBytes}
	if err := quota.reload(); err != nil {
		return nil, err
	}
	return &stagingQuotaStore{FileStore: store, quota: quota}, nil
}

// NewUpload 预留总容量后再创建 TUS 资源。
func (store *stagingQuotaStore) NewUpload(ctx context.Context, info tusd.FileInfo) (tusd.Upload, error) {
	store.quota.mu.Lock()
	defer store.quota.mu.Unlock()
	if err := store.quota.refreshBlocked(); err != nil {
		return nil, err
	}
	if store.quota.blocked {
		return nil, tusd.NewError("UPLOAD_STAGING_UNAVAILABLE", "upload staging state cannot be safely accounted", http.StatusInsufficientStorage)
	}
	if info.Size < 0 || info.SizeIsDeferred || info.IsPartial || info.IsFinal {
		return nil, tusd.NewError("INVALID_UPLOAD_SIZE", "upload size must be known and non-negative", http.StatusBadRequest)
	}
	if len(store.quota.reservations) >= maxStagedUploadCount {
		return nil, tusd.NewError("UPLOAD_STAGING_COUNT_LIMIT", "too many unfinished uploads", http.StatusTooManyRequests)
	}
	if info.Size > store.quota.maxBytes-store.quota.reservedBytes {
		return nil, tusd.NewError("UPLOAD_STAGING_LIMIT", "upload staging capacity is full", http.StatusRequestEntityTooLarge)
	}

	upload, err := store.FileStore.NewUpload(ctx, info)
	if err != nil {
		_ = store.quota.reloadLocked()
		return nil, err
	}
	created, err := upload.GetInfo(ctx)
	if err != nil || !validTusUploadID(created.ID) {
		_ = store.FileStore.AsTerminatableUpload(upload).Terminate(ctx)
		_ = store.quota.reloadLocked()
		if err != nil {
			return nil, fmt.Errorf("read new upload state: %w", err)
		}
		return nil, errors.New("new upload has an invalid ID")
	}
	store.quota.reservations[created.ID] = info.Size
	store.quota.reservedBytes += info.Size
	return stagingQuotaUpload{Upload: upload}, nil
}

// GetUpload 返回受容量账本管理的上传资源。
func (store *stagingQuotaStore) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	upload, err := store.FileStore.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	return stagingQuotaUpload{Upload: upload}, nil
}

// AsTerminatableUpload 统一处理客户端取消与发布后清理。
func (store *stagingQuotaStore) AsTerminatableUpload(upload tusd.Upload) tusd.TerminatableUpload {
	if wrapped, ok := upload.(stagingQuotaUpload); ok {
		upload = wrapped.Upload
	}
	return stagingQuotaTerminator{
		delegate: store.FileStore.AsTerminatableUpload(upload),
		upload:   upload,
		quota:    store.quota,
	}
}

// Terminate 仅在暂存文件删除成功后释放预留。
func (terminator stagingQuotaTerminator) Terminate(ctx context.Context) error {
	info, err := terminator.upload.GetInfo(ctx)
	if err != nil {
		return fmt.Errorf("read upload state before termination: %w", err)
	}
	if err := terminator.delegate.Terminate(ctx); err != nil {
		return err
	}
	terminator.quota.release(info.ID)
	return nil
}

// reload 在持锁期间重建容量账本。
func (quota *stagingQuota) reload() error {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	return quota.reloadLocked()
}

// refreshBlocked 在状态异常时重扫，恢复后才允许创建资源。
func (quota *stagingQuota) refreshBlocked() error {
	if !quota.blocked {
		return nil
	}
	return quota.reloadLocked()
}

// reloadLocked 核验恢复状态并重新累计声明容量，调用方须持锁。
func (quota *stagingQuota) reloadLocked() error {
	entries, err := os.ReadDir(quota.dir)
	if err != nil {
		return fmt.Errorf("inspect upload staging quota: %w", err)
	}
	wasBlocked := quota.blocked
	reservations := make(map[string]int64)
	dataFiles := make(map[string]struct{})
	var reserved int64
	blocked := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			blocked = true
			continue
		}
		if strings.HasSuffix(name, ".info") {
			id := strings.TrimSuffix(name, ".info")
			if !validTusUploadID(id) {
				blocked = true
				continue
			}
			infoPath := filepath.Join(quota.dir, name)
			fileInfo, statErr := os.Lstat(infoPath)
			if statErr != nil || !fileInfo.Mode().IsRegular() {
				blocked = true
				continue
			}
			data, readErr := os.ReadFile(infoPath)
			var info tusd.FileInfo
			if readErr != nil || json.Unmarshal(data, &info) != nil || info.ID != id || info.Size < 0 || info.SizeIsDeferred || info.IsPartial || info.IsFinal || len(info.PartialUploads) != 0 {
				blocked = true
				continue
			}
			dataInfo, dataErr := os.Lstat(filepath.Join(quota.dir, id))
			if dataErr != nil || !dataInfo.Mode().IsRegular() || dataInfo.Size() > info.Size || info.Size > int64(^uint64(0)>>1)-reserved {
				blocked = true
				continue
			}
			reservations[id] = info.Size
			dataFiles[id] = struct{}{}
			reserved += info.Size
			continue
		}
		if validTusUploadID(name) {
			dataFiles[name] = struct{}{}
			continue
		}
		if strings.HasSuffix(name, ".lock") && validTusUploadID(strings.TrimSuffix(name, ".lock")) ||
			strings.HasSuffix(name, ".stop") && validTusUploadID(strings.TrimSuffix(name, ".stop")) {
			continue
		}
		blocked = true
	}
	for id := range dataFiles {
		if _, exists := reservations[id]; !exists {
			blocked = true
		}
	}
	quota.reservations = reservations
	quota.reservedBytes = reserved
	quota.blocked = blocked
	if blocked && !wasBlocked {
		slog.Warn("upload staging quota is fail-closed until invalid state is resolved")
	}
	return nil
}

// release 删除已终止上传的容量预留。
func (quota *stagingQuota) release(id string) {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	if size, exists := quota.reservations[id]; exists {
		delete(quota.reservations, id)
		quota.reservedBytes -= size
	}
}
