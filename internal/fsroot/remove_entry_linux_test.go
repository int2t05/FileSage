//go:build linux

// 本文件验证永久删除的隔离、恢复及竞态处理。

package fsroot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestRemoveEntryPreservesReplacementAtIsolationBoundary 验证提交边界替换不会丢文件。
func TestRemoveEntryPreservesReplacementAtIsolationBoundary(t *testing.T) {
	rootPath := t.TempDir()
	targetPath := filepath.Join(rootPath, "target.txt")
	replacementPath := filepath.Join(rootPath, "replacement.txt")
	if err := os.WriteFile(targetPath, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, filepath.Join(t.TempDir(), "move-recovery"))
	approved, err := root.LstatEntry("target.txt")
	if err != nil {
		t.Fatal(err)
	}

	isolated := false
	rename := func(oldDir int, oldPath string, newDir int, newPath string, flags uint) error {
		if !isolated {
			isolated = true
			if err := unix.Renameat2(unix.AT_FDCWD, targetPath, unix.AT_FDCWD, replacementPath, unix.RENAME_EXCHANGE); err != nil {
				return err
			}
		}
		return unix.Renameat2(oldDir, oldPath, newDir, newPath, flags)
	}
	err = removeEntryIfUnchanged(root, approved, rename)
	if !isolated || !errors.Is(err, ErrConflict) {
		t.Fatalf("remove result = (%t, %v), want replacement race rejected", isolated, err)
	}
	if contents, err := os.ReadFile(targetPath); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement at original path = (%q, %v)", contents, err)
	}
	if contents, err := os.ReadFile(replacementPath); err != nil || string(contents) != "approved" {
		t.Fatalf("approved file after race = (%q, %v)", contents, err)
	}
	if entries, err := os.ReadDir(rootPath); err != nil || len(entries) != 2 {
		t.Fatalf("managed directory after conflict = (%v, %v), want only original entries", entries, err)
	}
	if records, err := readMoveRecoveryEntries(root.moveRecoveryRoot); err != nil || len(records) != 0 {
		t.Fatalf("delete recovery records after conflict = (%v, %v), want empty", records, err)
	}
}

// TestRecoverInterruptedDeleteRestoresQuarantinedEntry 验证中断后恢复原文件。
func TestRecoverInterruptedDeleteRestoresQuarantinedEntry(t *testing.T) {
	rootPath, journalPath := t.TempDir(), filepath.Join(t.TempDir(), "move-recovery")
	targetPath := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(targetPath, []byte("keep until delete commits"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, journalPath)
	record := stageDeleteForRecoveryTest(t, root, "note.txt")
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = openRemovalTestRoot(t, rootPath, journalPath)
	defer root.Close()
	summary, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || summary.Restored != 1 || summary.Pending != 0 {
		t.Fatalf("delete recovery = (%+v, %v), want one restored entry", summary, err)
	}
	if contents, err := os.ReadFile(targetPath); err != nil || string(contents) != "keep until delete commits" {
		t.Fatalf("restored file = (%q, %v)", contents, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete quarantine remains: %v", err)
	}
	if records, err := os.ReadDir(journalPath); err != nil || len(records) != 0 {
		t.Fatalf("delete journal after recovery = (%v, %v), want empty", records, err)
	}
}

// TestRecoverInterruptedDeletePreservesOccupiedSource 验证恢复不会覆盖新文件。
func TestRecoverInterruptedDeletePreservesOccupiedSource(t *testing.T) {
	rootPath, journalPath := t.TempDir(), filepath.Join(t.TempDir(), "move-recovery")
	targetPath := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(targetPath, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, journalPath)
	record := stageDeleteForRecoveryTest(t, root, "note.txt")
	if err := os.WriteFile(targetPath, []byte("new user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = openRemovalTestRoot(t, rootPath, journalPath)
	defer root.Close()
	summary, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || summary.Pending != 1 || summary.Restored != 0 {
		t.Fatalf("delete recovery = (%+v, %v), want manual review", summary, err)
	}
	if len(summary.PendingPaths) != 2 || summary.PendingPaths[0] != "note.txt" || summary.PendingPaths[1] != filepath.ToSlash(filepath.Join(record.QuarantinePath, "source")) {
		t.Fatalf("pending delete paths = %v, want source and retained entry", summary.PendingPaths)
	}
	if contents, err := os.ReadFile(targetPath); err != nil || string(contents) != "new user file" {
		t.Fatalf("occupied source = (%q, %v), want unchanged", contents, err)
	}
	stagedPath := filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath), "source")
	if contents, err := os.ReadFile(stagedPath); err != nil || string(contents) != "approved" {
		t.Fatalf("retained approved file = (%q, %v)", contents, err)
	}
	if records, err := os.ReadDir(journalPath); err != nil || len(records) != 1 {
		t.Fatalf("delete recovery journal = (%v, %v), want retained record", records, err)
	}
}

// TestRecoverInterruptedDeleteCleansCommittedRemoval 验证已提交删除的日志清理。
func TestRecoverInterruptedDeleteCleansCommittedRemoval(t *testing.T) {
	rootPath, journalPath := t.TempDir(), filepath.Join(t.TempDir(), "move-recovery")
	targetPath := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(targetPath, []byte("delete committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, journalPath)
	record := stageDeleteForRecoveryTest(t, root, "note.txt")
	quarantine, err := openRootDirectoryNoFollow(root.root, record.QuarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Unlinkat(int(quarantine.Fd()), "source", 0); err != nil {
		t.Fatal(err)
	}
	if err := quarantine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := quarantine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = openRemovalTestRoot(t, rootPath, journalPath)
	defer root.Close()
	summary, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || summary.Cleaned != 1 || summary.Pending != 0 {
		t.Fatalf("delete recovery = (%+v, %v), want committed removal cleaned", summary, err)
	}
	if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed deletion restored source: %v", err)
	}
	if records, err := os.ReadDir(journalPath); err != nil || len(records) != 0 {
		t.Fatalf("delete journal after commit recovery = (%v, %v), want empty", records, err)
	}
}

// TestRecoverInterruptedDeleteKeepsRecordWhenQuarantineMoved 验证隔离项移走后保留日志。
func TestRecoverInterruptedDeleteKeepsRecordWhenQuarantineMoved(t *testing.T) {
	rootPath, journalPath := t.TempDir(), filepath.Join(t.TempDir(), "move-recovery")
	if err := os.WriteFile(filepath.Join(rootPath, "note.txt"), []byte("preserve for review"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, journalPath)
	record := stageDeleteForRecoveryTest(t, root, "note.txt")
	orphanPath := filepath.Join(rootPath, "manual-review")
	quarantinePath := filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath))
	if err := os.Rename(quarantinePath, orphanPath); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = openRemovalTestRoot(t, rootPath, journalPath)
	defer root.Close()
	summary, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || summary.Pending != 1 || len(summary.PendingPaths) != 2 {
		t.Fatalf("delete recovery = (%+v, %v), want manual review", summary, err)
	}
	if contents, err := os.ReadFile(filepath.Join(orphanPath, "source")); err != nil || string(contents) != "preserve for review" {
		t.Fatalf("moved quarantine data = (%q, %v), want unchanged", contents, err)
	}
	if records, err := os.ReadDir(journalPath); err != nil || len(records) != 1 {
		t.Fatalf("delete recovery journal = (%v, %v), want retained record", records, err)
	}
}

// TestRecoverInterruptedDeleteRejectsUnexpectedQuarantineEntry 验证恢复前核对对象身份。
func TestRecoverInterruptedDeleteRejectsUnexpectedQuarantineEntry(t *testing.T) {
	rootPath, journalPath := t.TempDir(), filepath.Join(t.TempDir(), "move-recovery")
	targetPath := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(targetPath, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, journalPath)
	record := stageDeleteForRecoveryTest(t, root, "note.txt")
	stagedPath := filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath), "source")
	originalElsewhere := filepath.Join(rootPath, "original-for-review")
	if err := os.Rename(stagedPath, originalElsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, []byte("unexpected occupant"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = openRemovalTestRoot(t, rootPath, journalPath)
	defer root.Close()
	summary, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || summary.Pending != 1 || summary.Restored != 0 {
		t.Fatalf("delete recovery = (%+v, %v), want unexpected entry preserved for review", summary, err)
	}
	if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery restored an unverified entry: %v", err)
	}
	if contents, err := os.ReadFile(stagedPath); err != nil || string(contents) != "unexpected occupant" {
		t.Fatalf("unexpected quarantine entry = (%q, %v)", contents, err)
	}
	if contents, err := os.ReadFile(originalElsewhere); err != nil || string(contents) != "approved" {
		t.Fatalf("approved file = (%q, %v)", contents, err)
	}
	if records, err := os.ReadDir(journalPath); err != nil || len(records) != 1 {
		t.Fatalf("delete recovery journal = (%v, %v), want retained record", records, err)
	}
}

// TestRecoverInterruptedDeleteDoesNotRestoreEntryAfterCommit 验证提交后不恢复异常条目。
func TestRecoverInterruptedDeleteDoesNotRestoreEntryAfterCommit(t *testing.T) {
	rootPath, journalPath := t.TempDir(), filepath.Join(t.TempDir(), "move-recovery")
	if err := os.WriteFile(filepath.Join(rootPath, "note.txt"), []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, journalPath)
	record := stageDeleteForRecoveryTest(t, root, "note.txt")
	stagedPath := filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath), "source")
	if err := os.Remove(stagedPath); err != nil {
		t.Fatal(err)
	}
	record.Phase = moveDeleteCommitted
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, []byte("unexpected after commit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root = openRemovalTestRoot(t, rootPath, journalPath)
	defer root.Close()
	summary, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || summary.Pending != 1 || summary.Restored != 0 || summary.Cleaned != 0 {
		t.Fatalf("delete recovery = (%+v, %v), want committed-path anomaly preserved for review", summary, err)
	}
	if contents, err := os.ReadFile(stagedPath); err != nil || string(contents) != "unexpected after commit" {
		t.Fatalf("unexpected quarantine entry = (%q, %v)", contents, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "note.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed delete was restored: %v", err)
	}
	if records, err := os.ReadDir(journalPath); err != nil || len(records) != 1 {
		t.Fatalf("delete recovery journal = (%v, %v), want retained record", records, err)
	}
}

// openRemovalTestRoot 创建带删除日志目录的测试根。
func openRemovalTestRoot(t *testing.T, rootPath, journalPath string) *Root {
	t.Helper()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	return root
}

// stageDeleteForRecoveryTest 构造可恢复的中断删除状态。
func stageDeleteForRecoveryTest(t *testing.T, root *Root, relativePath string) moveRecoveryRecord {
	t.Helper()
	entry, err := root.LstatEntry(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	parent, name, err := openRenameParent(root, relativePath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	quarantinePath, quarantine, err := createFileMoveQuarantine(parent, relativePath)
	if err != nil {
		t.Fatal(err)
	}
	defer quarantine.Close()
	quarantineStat, err := statxFD(int(quarantine.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	record := moveRecoveryRecord{
		Version: 1, ID: moveRecoveryID(quarantinePath), Kind: moveRecoveryDelete, Phase: moveDeletePrepared,
		SourcePath: relativePath, QuarantinePath: quarantinePath,
		SourceIdentity:     moveObjectIdentity{Device: entry.Device, Inode: entry.Inode},
		QuarantineIdentity: moveObjectIdentity{Device: statxDevice(quarantineStat), Inode: quarantineStat.Ino},
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}
	if err := unix.Renameat2(int(parent.Fd()), name, int(quarantine.Fd()), "source", unix.RENAME_NOREPLACE); err != nil {
		t.Fatal(err)
	}
	if err := quarantine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Sync(); err != nil {
		t.Fatal(err)
	}
	return record
}

// TestDeleteQuarantineHasNoUnexpectedFiles 验证成功删除后不遗留隔离目录。
func TestDeleteQuarantineHasNoUnexpectedFiles(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRemovalTestRoot(t, rootPath, filepath.Join(t.TempDir(), "move-recovery"))
	entry, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveEntryIfUnchanged(entry); err != nil {
		t.Fatal(err)
	}
	items, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), moveRecoveryDirPrefix) {
			t.Fatalf("delete left quarantine directory %q", item.Name())
		}
	}
}
