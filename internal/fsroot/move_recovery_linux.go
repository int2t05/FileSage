//go:build linux

// 本文件持久记录跨卷移动和隔离删除，并在重启时安全恢复来源。
package fsroot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	moveRecoveryFile        = "file"
	moveRecoveryTree        = "tree"
	moveRecoveryDelete      = "delete"
	moveDeletePrepared      = "prepared"
	moveDeleteIsolated      = "isolated"
	moveDeleteCommitted     = "committed"
	moveRecoveryDirPrefix   = ".filesage-move-quarantine-"
	moveRecoveryRecordLimit = 4096
)

type moveObjectIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type moveRecoveryRecord struct {
	Version                int                `json:"version"`
	ID                     string             `json:"id"`
	Kind                   string             `json:"kind"`
	Phase                  string             `json:"phase,omitempty"`
	SourcePath             string             `json:"source_path"`
	QuarantinePath         string             `json:"quarantine_path"`
	DestinationPath        string             `json:"destination_path"`
	SourceIdentity         moveObjectIdentity `json:"source_identity"`
	QuarantineIdentity     moveObjectIdentity `json:"quarantine_identity"`
	DestinationIdentity    moveObjectIdentity `json:"destination_identity"`
	DestinationFingerprint Fingerprint        `json:"destination_fingerprint"`
	DestinationDigest      string             `json:"destination_digest"`
}

// ConfigureMoveRecoveryDirectory 设置服务数据目录中的私有移动日志目录。
func (r *Root) ConfigureMoveRecoveryDirectory(directory string) error {
	if r == nil || r.root == nil || strings.TrimSpace(directory) == "" {
		return ErrMoveRecoveryUnavailable
	}
	if r.moveRecoveryRoot != nil {
		return errors.New("move recovery directory already configured")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve move recovery directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return fmt.Errorf("create move recovery directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("inspect move recovery directory: %w", ErrMoveRecoveryUnavailable)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return fmt.Errorf("secure move recovery directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return fmt.Errorf("resolve move recovery directory: %w", err)
	}
	journalRoot, err := os.OpenRoot(resolved)
	if err != nil {
		return fmt.Errorf("open move recovery directory: %w", err)
	}
	directoryFile, err := journalRoot.Open(".")
	if err != nil {
		_ = journalRoot.Close()
		return fmt.Errorf("inspect open move recovery directory: %w", err)
	}
	var stat unix.Stat_t
	statErr := unix.Fstat(int(directoryFile.Fd()), &stat)
	syncErr := directoryFile.Sync()
	closeErr := directoryFile.Close()
	if statErr != nil || syncErr != nil || closeErr != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o777 != 0o700 || stat.Uid != uint32(unix.Geteuid()) {
		_ = journalRoot.Close()
		return errors.Join(ErrMoveRecoveryUnavailable, statErr, syncErr, closeErr)
	}
	if err := syncMoveRecoveryPath(filepath.Dir(resolved)); err != nil {
		_ = journalRoot.Close()
		return fmt.Errorf("persist move recovery directory: %w", err)
	}
	r.moveRecoveryRoot = journalRoot
	return nil
}

func platformRequireMoveRecovery(root *Root) error {
	if root == nil || root.moveRecoveryRoot == nil {
		return ErrMoveRecoveryUnavailable
	}
	return nil
}

func moveRecoveryID(quarantinePath string) string {
	name := path.Base(quarantinePath)
	if !strings.HasPrefix(name, moveRecoveryDirPrefix) {
		return ""
	}
	id := strings.TrimPrefix(name, moveRecoveryDirPrefix)
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 12 {
		return ""
	}
	return id
}

func validateMoveRecoveryRecord(record moveRecoveryRecord) error {
	if record.Version != 1 || record.ID == "" || moveRecoveryID(record.QuarantinePath) != record.ID {
		return ErrInvalidPath
	}
	if record.Kind != moveRecoveryFile && record.Kind != moveRecoveryTree && record.Kind != moveRecoveryDelete {
		return ErrInvalidPath
	}
	if validateMutationPath(record.SourcePath) != nil || validateMutationPath(record.QuarantinePath) != nil || hasTrashComponent(record.SourcePath) || hasTrashComponent(record.QuarantinePath) {
		return ErrInvalidPath
	}
	if path.Dir(record.SourcePath) != path.Dir(record.QuarantinePath) || record.SourcePath == record.QuarantinePath {
		return ErrInvalidPath
	}
	if record.SourceIdentity.Inode == 0 || record.QuarantineIdentity.Inode == 0 {
		return ErrIdentityUnavailable
	}
	if record.Kind == moveRecoveryDelete {
		if record.Phase != moveDeletePrepared && record.Phase != moveDeleteIsolated && record.Phase != moveDeleteCommitted {
			return ErrConflict
		}
		if record.DestinationPath != "" || record.DestinationIdentity.Inode != 0 || record.DestinationFingerprint.Exists || record.DestinationDigest != "" {
			return ErrConflict
		}
		return nil
	}
	if record.Phase != "" {
		return ErrConflict
	}
	if validateMutationPath(record.DestinationPath) != nil || hasTrashComponent(record.DestinationPath) || record.DestinationPath == record.SourcePath || record.DestinationPath == record.QuarantinePath {
		return ErrInvalidPath
	}
	if record.DestinationIdentity.Inode == 0 {
		return ErrIdentityUnavailable
	}
	if !record.DestinationFingerprint.Exists || !record.DestinationFingerprint.IdentityAvailable || record.DestinationFingerprint.Device != record.DestinationIdentity.Device || record.DestinationFingerprint.Inode != record.DestinationIdentity.Inode {
		return ErrIdentityUnavailable
	}
	digest, err := hex.DecodeString(record.DestinationDigest)
	if err != nil || len(digest) != 32 {
		return ErrConflict
	}
	if record.Kind == moveRecoveryTree && record.SourceIdentity != record.QuarantineIdentity {
		return ErrConflict
	}
	if record.Kind == moveRecoveryFile && (record.DestinationFingerprint.Kind != KindFile || record.DestinationFingerprint.Size < 0) {
		return ErrConflict
	}
	if record.Kind == moveRecoveryTree && record.DestinationFingerprint.Kind != KindDirectory {
		return ErrConflict
	}
	return nil
}

func (r *Root) writeMoveRecoveryRecord(record moveRecoveryRecord) error {
	if err := platformRequireMoveRecovery(r); err != nil {
		return err
	}
	if err := validateMoveRecoveryRecord(record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporaryName := "." + record.ID + ".tmp"
	finalName := record.ID + ".json"
	file, err := r.moveRecoveryRoot.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = r.moveRecoveryRoot.Remove(temporaryName)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = r.moveRecoveryRoot.Remove(temporaryName)
		return err
	}
	if err := file.Close(); err != nil {
		_ = r.moveRecoveryRoot.Remove(temporaryName)
		return err
	}
	if err := r.moveRecoveryRoot.Rename(temporaryName, finalName); err != nil {
		_ = r.moveRecoveryRoot.Remove(temporaryName)
		return err
	}
	return syncMoveRecoveryDirectory(r.moveRecoveryRoot)
}

func (r *Root) removeMoveRecoveryRecord(id string) error {
	if err := platformRequireMoveRecovery(r); err != nil {
		return err
	}
	if moveRecoveryID(moveRecoveryDirPrefix+id) != id {
		return ErrInvalidPath
	}
	err := r.moveRecoveryRoot.Remove(id + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncMoveRecoveryDirectory(r.moveRecoveryRoot)
}

func (r *Root) RecoverInterruptedMoves(ctx context.Context) (MoveRecoverySummary, error) {
	var summary MoveRecoverySummary
	if ctx == nil {
		return summary, fmt.Errorf("move recovery requires a context: %w", os.ErrInvalid)
	}
	if err := platformRequireMoveRecovery(r); err != nil {
		return summary, err
	}
	entries, err := readMoveRecoveryEntries(r.moveRecoveryRoot)
	if err != nil {
		return summary, fmt.Errorf("read move recovery journal: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
			if err := r.removeMoveRecoveryTemporary(name); err != nil {
				summary.Pending++
				summary.PendingPaths = append(summary.PendingPaths, name)
			} else {
				summary.Cleaned++
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		record, err := r.readMoveRecoveryRecord(name)
		if err != nil {
			summary.Pending++
			summary.PendingPaths = append(summary.PendingPaths, name)
			continue
		}
		status, err := r.recoverMoveRecord(ctx, record)
		if err != nil {
			summary.Pending++
			summary.PendingPaths = append(summary.PendingPaths, record.SourcePath, recoveryEntryPath(record))
			continue
		}
		switch status {
		case moveRecoveryRestored:
			summary.Restored++
		case moveRecoveryCleaned:
			summary.Cleaned++
		default:
			summary.Pending++
			summary.PendingPaths = append(summary.PendingPaths, record.SourcePath, recoveryEntryPath(record))
		}
	}
	return summary, nil
}

// recoveryEntryPath 返回需要人工核查的隔离项路径。
func recoveryEntryPath(record moveRecoveryRecord) string {
	if record.Kind == moveRecoveryFile || record.Kind == moveRecoveryDelete {
		return path.Join(record.QuarantinePath, "source")
	}
	return record.QuarantinePath
}

func readMoveRecoveryEntries(root *os.Root) ([]os.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.ReadDir(-1)
}

type moveRecoveryStatus int

const (
	moveRecoveryNeedsReview moveRecoveryStatus = iota
	moveRecoveryRestored
	moveRecoveryCleaned
)

func (r *Root) readMoveRecoveryRecord(name string) (moveRecoveryRecord, error) {
	var record moveRecoveryRecord
	if !strings.HasSuffix(name, ".json") {
		return record, ErrInvalidPath
	}
	id := strings.TrimSuffix(name, ".json")
	if moveRecoveryID(moveRecoveryDirPrefix+id) != id {
		return record, ErrInvalidPath
	}
	file, err := r.moveRecoveryRoot.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return record, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > moveRecoveryRecordLimit {
		return record, ErrConflict
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Uid != uint32(unix.Geteuid()) {
		return record, errors.Join(ErrConflict, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, moveRecoveryRecordLimit+1))
	if err != nil || len(data) > moveRecoveryRecordLimit {
		return record, errors.Join(ErrConflict, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return record, ErrConflict
	}
	if record.ID != id {
		return record, ErrConflict
	}
	return record, validateMoveRecoveryRecord(record)
}

func (r *Root) recoverMoveRecord(ctx context.Context, record moveRecoveryRecord) (moveRecoveryStatus, error) {
	if err := validateMoveRecoveryRecord(record); err != nil {
		return moveRecoveryNeedsReview, err
	}
	if record.Kind == moveRecoveryDelete {
		return r.recoverDeleteRecord(record)
	}
	sourceEntry, sourceErr := r.LstatEntry(record.SourcePath)
	sourceExists := sourceErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return moveRecoveryNeedsReview, sourceErr
	}
	quarantineEntry, quarantineErr := r.LstatEntry(record.QuarantinePath)
	quarantineExists := quarantineErr == nil
	if quarantineErr != nil && !errors.Is(quarantineErr, os.ErrNotExist) {
		return moveRecoveryNeedsReview, quarantineErr
	}
	if sourceExists {
		if !sameMoveObject(sourceEntry, record.SourceIdentity) {
			return moveRecoveryNeedsReview, ErrConflict
		}
		if !quarantineExists {
			return moveRecoveryCleaned, r.removeMoveRecoveryRecord(record.ID)
		}
		if record.Kind == moveRecoveryFile && quarantineEntry.Kind == KindDirectory && sameMoveObject(quarantineEntry, record.QuarantineIdentity) {
			entries, err := r.List(record.QuarantinePath)
			if err == nil && len(entries) == 0 {
				if err := removeRecoveryQuarantineDirectory(r, record.QuarantinePath, record.QuarantineIdentity); err == nil {
					return moveRecoveryCleaned, r.removeMoveRecoveryRecord(record.ID)
				}
			}
		}
		return moveRecoveryNeedsReview, nil
	}
	if err := r.verifyMoveRecoveryDestination(ctx, record); err != nil {
		return moveRecoveryNeedsReview, err
	}
	if !quarantineExists {
		return moveRecoveryCleaned, r.removeMoveRecoveryRecord(record.ID)
	}
	if record.Kind == moveRecoveryTree {
		if quarantineEntry.Kind != KindDirectory || !sameMoveObject(quarantineEntry, record.QuarantineIdentity) {
			return moveRecoveryNeedsReview, ErrConflict
		}
		if err := r.RenameNoReplace(record.QuarantinePath, record.SourcePath); err != nil {
			return moveRecoveryNeedsReview, err
		}
		if err := syncMoveParents(r, record.QuarantinePath, record.SourcePath); err != nil {
			return moveRecoveryNeedsReview, err
		}
		return moveRecoveryRestored, r.removeMoveRecoveryRecord(record.ID)
	}
	if quarantineEntry.Kind != KindDirectory || !sameMoveObject(quarantineEntry, record.QuarantineIdentity) {
		return moveRecoveryNeedsReview, ErrConflict
	}
	quarantinedSource := path.Join(record.QuarantinePath, "source")
	entry, err := r.LstatEntry(quarantinedSource)
	if errors.Is(err, os.ErrNotExist) {
		entries, listErr := r.List(record.QuarantinePath)
		if listErr != nil || len(entries) != 0 {
			return moveRecoveryNeedsReview, errors.Join(ErrConflict, listErr)
		}
		if err := removeRecoveryQuarantineDirectory(r, record.QuarantinePath, record.QuarantineIdentity); err != nil {
			return moveRecoveryNeedsReview, err
		}
		return moveRecoveryCleaned, r.removeMoveRecoveryRecord(record.ID)
	}
	if err != nil || entry.Kind != KindFile || entry.Symlink || !sameMoveObject(entry, record.SourceIdentity) {
		return moveRecoveryNeedsReview, errors.Join(ErrConflict, err)
	}
	if err := r.RenameNoReplace(quarantinedSource, record.SourcePath); err != nil {
		return moveRecoveryNeedsReview, err
	}
	if err := syncMoveParent(r, quarantinedSource); err != nil {
		return moveRecoveryNeedsReview, err
	}
	if err := removeRecoveryQuarantineDirectory(r, record.QuarantinePath, record.QuarantineIdentity); err != nil {
		return moveRecoveryNeedsReview, err
	}
	return moveRecoveryRestored, r.removeMoveRecoveryRecord(record.ID)
}

// recoverDeleteRecord 恢复中断时隔离的目录项，绝不覆盖原路径的新内容。
func (r *Root) recoverDeleteRecord(record moveRecoveryRecord) (moveRecoveryStatus, error) {
	sourceEntry, sourceErr := r.LstatEntry(record.SourcePath)
	sourceExists := sourceErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return moveRecoveryNeedsReview, sourceErr
	}
	quarantineEntry, quarantineErr := r.LstatEntry(record.QuarantinePath)
	quarantineExists := quarantineErr == nil
	if quarantineErr != nil && !errors.Is(quarantineErr, os.ErrNotExist) {
		return moveRecoveryNeedsReview, quarantineErr
	}
	if !quarantineExists {
		if (sourceExists && sameRemovalIdentity(sourceEntry, record.SourceIdentity)) || record.Phase == moveDeleteCommitted {
			return moveRecoveryCleaned, r.removeMoveRecoveryRecord(record.ID)
		}
		return moveRecoveryNeedsReview, ErrConflict
	}
	if quarantineEntry.Kind != KindDirectory || !sameMoveObject(quarantineEntry, record.QuarantineIdentity) {
		return moveRecoveryNeedsReview, ErrConflict
	}

	stagedPath := path.Join(record.QuarantinePath, "source")
	stagedEntry, stagedErr := r.LstatEntry(stagedPath)
	stagedExists := stagedErr == nil
	if stagedErr != nil && !errors.Is(stagedErr, os.ErrNotExist) {
		return moveRecoveryNeedsReview, stagedErr
	}
	if stagedExists {
		if sourceExists || record.Phase == moveDeleteCommitted || !sameRemovalIdentity(stagedEntry, record.SourceIdentity) {
			return moveRecoveryNeedsReview, ErrConflict
		}
		if err := r.RenameNoReplace(stagedPath, record.SourcePath); err != nil {
			return moveRecoveryNeedsReview, err
		}
		if err := syncMoveParents(r, stagedPath, record.SourcePath); err != nil {
			return moveRecoveryNeedsReview, err
		}
		if err := removeRecoveryQuarantineDirectory(r, record.QuarantinePath, record.QuarantineIdentity); err != nil {
			return moveRecoveryNeedsReview, err
		}
		return moveRecoveryRestored, r.removeMoveRecoveryRecord(record.ID)
	}

	entries, err := r.List(record.QuarantinePath)
	if err != nil || len(entries) != 0 {
		return moveRecoveryNeedsReview, errors.Join(ErrConflict, err)
	}
	if err := removeRecoveryQuarantineDirectory(r, record.QuarantinePath, record.QuarantineIdentity); err != nil {
		return moveRecoveryNeedsReview, err
	}
	return moveRecoveryCleaned, r.removeMoveRecoveryRecord(record.ID)
}

// sameRemovalIdentity 比较目录项与恢复日志中的对象身份。
func sameRemovalIdentity(entry Entry, expected moveObjectIdentity) bool {
	if entry.Symlink {
		return entry.LinkIdentityAvailable && entry.LinkDevice == expected.Device && entry.LinkInode == expected.Inode
	}
	return entry.IdentityAvailable && entry.Device == expected.Device && entry.Inode == expected.Inode
}

// verifyMoveRecoveryDestination 确认目标仍是移动完成时发布的副本。
func (r *Root) verifyMoveRecoveryDestination(ctx context.Context, record moveRecoveryRecord) error {
	entry, err := r.LstatEntry(record.DestinationPath)
	if err != nil {
		return errors.Join(ErrConflict, err)
	}
	if entry.Symlink || !sameMoveObject(entry, record.DestinationIdentity) || !sameFingerprint(entry.Fingerprint(), record.DestinationFingerprint) {
		return ErrConflict
	}
	if record.Kind == moveRecoveryFile {
		if entry.Kind != KindFile {
			return ErrConflict
		}
		return r.verifyMoveRecoveryFile(ctx, record)
	}
	if entry.Kind != KindDirectory {
		return ErrConflict
	}
	snapshot, err := r.SnapshotTree(ctx, record.DestinationPath)
	if err != nil {
		return err
	}
	if snapshot.Digest != record.DestinationDigest {
		return ErrConflict
	}
	return nil
}

// verifyMoveRecoveryFile 复核目标状态与发布摘要，避免误清未完成记录。
func (r *Root) verifyMoveRecoveryFile(ctx context.Context, record moveRecoveryRecord) error {
	parent, name, err := openRenameParent(r, record.DestinationPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentPath := parentPathForOperation(record.DestinationPath)
	if err := verifyOpenedDirectory(r, parentPath, parent); err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || !sameFingerprint(fingerprintFromInfo(before), record.DestinationFingerprint) {
		return errors.Join(ErrConflict, err)
	}
	digest := sha256.New()
	count, err := io.Copy(digest, contextReader{ctx: ctx, reader: file})
	if err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil || count != record.DestinationFingerprint.Size || !sameFingerprint(fingerprintFromInfo(after), record.DestinationFingerprint) {
		return errors.Join(ErrConflict, err)
	}
	expectedDigest, err := hex.DecodeString(record.DestinationDigest)
	if err != nil || subtle.ConstantTimeCompare(digest.Sum(nil), expectedDigest) != 1 {
		return ErrConflict
	}
	return verifyOpenedDirectory(r, parentPath, parent)
}

func (r *Root) removeMoveRecoveryTemporary(name string) error {
	if len(name) != 1+24+4 || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmp") {
		return ErrInvalidPath
	}
	if _, err := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp")); err != nil {
		return ErrInvalidPath
	}
	info, err := r.moveRecoveryRoot.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return ErrConflict
	}
	if err := r.moveRecoveryRoot.Remove(name); err != nil {
		return err
	}
	return syncMoveRecoveryDirectory(r.moveRecoveryRoot)
}

func removeRecoveryQuarantineDirectory(root *Root, relativePath string, expected moveObjectIdentity) error {
	parentPath, name, err := openRenameParent(root, relativePath)
	if err != nil {
		return err
	}
	defer parentPath.Close()
	if err := verifyOpenedDirectory(root, parentPathForOperation(relativePath), parentPath); err != nil {
		return err
	}
	fd, err := unix.Openat(int(parentPath.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), name)
	defer directory.Close()
	if err := verifyMoveQuarantineDirectory(parentPath, name, directory); err != nil {
		return err
	}
	stat, err := statxFD(int(directory.Fd()))
	if err != nil || statxDevice(stat) != expected.Device || stat.Ino != expected.Inode {
		return errors.Join(ErrPathChanged, err)
	}
	entries, err := directory.ReadDir(-1)
	if err != nil || len(entries) != 0 {
		return errors.Join(ErrConflict, err)
	}
	if err := verifyOpenedDirectory(root, parentPathForOperation(relativePath), parentPath); err != nil {
		return err
	}
	if err := verifyMoveQuarantineDirectory(parentPath, name, directory); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parentPath.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parentPath.Sync()
}

func sameMoveObject(entry Entry, expected moveObjectIdentity) bool {
	return entry.IdentityAvailable && entry.Device == expected.Device && entry.Inode == expected.Inode
}

func syncMoveRecoveryDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func syncMoveRecoveryPath(directoryPath string) error {
	directory, err := os.Open(directoryPath)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
