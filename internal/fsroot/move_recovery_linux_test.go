//go:build linux

package fsroot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverInterruptedFileMoveRestoresSource(t *testing.T) {
	rootPath := t.TempDir()
	journalPath := filepath.Join(t.TempDir(), "move-recovery")
	sourcePath := filepath.Join(rootPath, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("preserve this file"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		t.Fatal(err)
	}
	record := prepareInterruptedFileMove(t, root, rootPath, "source.txt")
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}

	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Restored != 1 || report.Pending != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want one restored source", report, err)
	}
	if content, err := os.ReadFile(sourcePath); err != nil || string(content) != "preserve this file" {
		t.Fatalf("restored source = (%q, %v)", content, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath))); !os.IsNotExist(err) {
		t.Fatalf("quarantine remains after recovery: %v", err)
	}
	if entries, err := os.ReadDir(journalPath); err != nil || len(entries) != 0 {
		t.Fatalf("recovery journal entries = (%v, %v), want empty", entries, err)
	}
}

func TestRecoverInterruptedMoveNeverOverwritesNewSource(t *testing.T) {
	rootPath := t.TempDir()
	journalPath := filepath.Join(t.TempDir(), "move-recovery")
	sourcePath := filepath.Join(rootPath, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("approved source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		t.Fatal(err)
	}
	record := prepareInterruptedFileMove(t, root, rootPath, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("new user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}

	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Pending != 1 || report.Restored != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want one pending conflict", report, err)
	}
	if content, err := os.ReadFile(sourcePath); err != nil || string(content) != "new user file" {
		t.Fatalf("occupied source = (%q, %v), want unchanged", content, err)
	}
	retainedPath := filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath), "source")
	if content, err := os.ReadFile(retainedPath); err != nil || string(content) != "approved source" {
		t.Fatalf("quarantined source = (%q, %v), want retained", content, err)
	}
	if entries, err := os.ReadDir(journalPath); err != nil || len(entries) != 1 {
		t.Fatalf("recovery journal entries = (%v, %v), want pending record", entries, err)
	}
}

func TestRecoverInterruptedFileMoveKeepsJournalWhenDestinationChanged(t *testing.T) {
	rootPath := t.TempDir()
	journalPath := filepath.Join(t.TempDir(), "move-recovery")
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("approved source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		t.Fatal(err)
	}
	record := prepareInterruptedFileMove(t, root, rootPath, "source.txt")
	quarantineSource := filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath), "source")
	if err := os.Remove(quarantineSource); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(record.DestinationPath)), []byte("changed target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}

	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Pending != 1 || report.Cleaned != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want one pending target conflict", report, err)
	}
	if entries, err := os.ReadDir(journalPath); err != nil || len(entries) != 1 {
		t.Fatalf("recovery journal entries = (%v, %v), want pending record", entries, err)
	}
}

func TestRecoverInterruptedTreeMoveKeepsJournalWhenDestinationMissing(t *testing.T) {
	rootPath := t.TempDir()
	journalPath := filepath.Join(t.TempDir(), "move-recovery")
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "note.txt"), []byte("tree contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		t.Fatal(err)
	}
	record := prepareInterruptedTreeMove(t, root, rootPath, "source")
	if err := os.RemoveAll(filepath.Join(rootPath, filepath.FromSlash(record.QuarantinePath))); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(rootPath, filepath.FromSlash(record.DestinationPath))); err != nil {
		t.Fatal(err)
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}

	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Pending != 1 || report.Cleaned != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want one pending destination conflict", report, err)
	}
	if entries, err := os.ReadDir(journalPath); err != nil || len(entries) != 1 {
		t.Fatalf("recovery journal entries = (%v, %v), want pending record", entries, err)
	}
}

func TestRecoverInterruptedTreeMoveRestoresSource(t *testing.T) {
	rootPath := t.TempDir()
	journalPath := filepath.Join(t.TempDir(), "move-recovery")
	sourcePath := filepath.Join(rootPath, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "note.txt"), []byte("tree contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		t.Fatal(err)
	}
	record := prepareInterruptedTreeMove(t, root, rootPath, "source")
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}

	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Restored != 1 || report.Pending != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want one restored directory", report, err)
	}
	if content, err := os.ReadFile(filepath.Join(sourcePath, "note.txt")); err != nil || string(content) != "tree contents" {
		t.Fatalf("restored tree = (%q, %v)", content, err)
	}
}

func TestRecoverInterruptedMoveClearsRecordWhenSourceIsAlreadyRestored(t *testing.T) {
	rootPath := t.TempDir()
	journalPath := filepath.Join(t.TempDir(), "move-recovery")
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	destinationContents := []byte("published copy")
	if err := os.WriteFile(filepath.Join(rootPath, "destination.txt"), destinationContents, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.ConfigureMoveRecoveryDirectory(journalPath); err != nil {
		t.Fatal(err)
	}
	source, err := root.LstatEntry("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	quarantinePath := ".filesage-move-quarantine-000000000000000000000001"
	if err := os.Mkdir(filepath.Join(rootPath, quarantinePath), 0o700); err != nil {
		t.Fatal(err)
	}
	quarantine, err := root.LstatEntry(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := root.LstatEntry("destination.txt")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(destinationContents)
	record := moveRecoveryRecord{
		Version: 1, ID: moveRecoveryID(quarantinePath), Kind: moveRecoveryFile,
		SourcePath: "source.txt", QuarantinePath: quarantinePath, DestinationPath: "destination.txt",
		SourceIdentity:         moveObjectIdentity{Device: source.Device, Inode: source.Inode},
		QuarantineIdentity:     moveObjectIdentity{Device: quarantine.Device, Inode: quarantine.Inode},
		DestinationIdentity:    moveObjectIdentity{Device: destination.Device, Inode: destination.Inode},
		DestinationFingerprint: destination.Fingerprint(), DestinationDigest: hex.EncodeToString(digest[:]),
	}
	if err := root.writeMoveRecoveryRecord(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootPath, quarantinePath)); err != nil {
		t.Fatal(err)
	}

	report, err := root.RecoverInterruptedMoves(context.Background())
	if err != nil || report.Cleaned != 1 || report.Pending != 0 {
		t.Fatalf("RecoverInterruptedMoves() = (%+v, %v), want one stale record cleaned", report, err)
	}
	if entries, err := os.ReadDir(journalPath); err != nil || len(entries) != 0 {
		t.Fatalf("recovery journal entries = (%v, %v), want empty", entries, err)
	}
}

func prepareInterruptedFileMove(t *testing.T, root *Root, rootPath, source string) moveRecoveryRecord {
	t.Helper()
	destinationPath := "destination.txt"
	contents, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(source)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, destinationPath), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := root.LstatEntry(source)
	if err != nil {
		t.Fatal(err)
	}
	quarantinePath, err := createMoveQuarantinePath(source)
	if err != nil {
		t.Fatal(err)
	}
	absoluteQuarantine := filepath.Join(rootPath, filepath.FromSlash(quarantinePath))
	if err := os.Mkdir(absoluteQuarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootPath, filepath.FromSlash(source)), filepath.Join(absoluteQuarantine, "source")); err != nil {
		t.Fatal(err)
	}
	quarantineEntry, err := root.LstatEntry(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := root.LstatEntry(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return moveRecoveryRecord{
		Version: 1, ID: moveRecoveryID(quarantinePath), Kind: moveRecoveryFile,
		SourcePath: source, QuarantinePath: quarantinePath, DestinationPath: destinationPath,
		SourceIdentity:         moveObjectIdentity{Device: entry.Device, Inode: entry.Inode},
		QuarantineIdentity:     moveObjectIdentity{Device: quarantineEntry.Device, Inode: quarantineEntry.Inode},
		DestinationIdentity:    moveObjectIdentity{Device: destination.Device, Inode: destination.Inode},
		DestinationFingerprint: destination.Fingerprint(), DestinationDigest: hex.EncodeToString(digest[:]),
	}
}

func prepareInterruptedTreeMove(t *testing.T, root *Root, rootPath, source string) moveRecoveryRecord {
	t.Helper()
	destinationPath := "destination"
	if err := os.Mkdir(filepath.Join(rootPath, destinationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, destinationPath, "note.txt"), []byte("tree contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := root.LstatEntry(source)
	if err != nil {
		t.Fatal(err)
	}
	quarantinePath, err := createMoveQuarantinePath(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootPath, filepath.FromSlash(source)), filepath.Join(rootPath, filepath.FromSlash(quarantinePath))); err != nil {
		t.Fatal(err)
	}
	destinationSnapshot, err := root.SnapshotTree(context.Background(), destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	destination := destinationSnapshot.Entries[0].Entry
	return moveRecoveryRecord{
		Version: 1, ID: moveRecoveryID(quarantinePath), Kind: moveRecoveryTree,
		SourcePath: source, QuarantinePath: quarantinePath, DestinationPath: destinationPath,
		SourceIdentity:         moveObjectIdentity{Device: entry.Device, Inode: entry.Inode},
		QuarantineIdentity:     moveObjectIdentity{Device: entry.Device, Inode: entry.Inode},
		DestinationIdentity:    moveObjectIdentity{Device: destination.Device, Inode: destination.Inode},
		DestinationFingerprint: destination.Fingerprint(), DestinationDigest: destinationSnapshot.Digest,
	}
}
