//go:build linux

package fsroot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateMetadataUsesCurrentIdentityAndRejectsUnsafeEntries(t *testing.T) {
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "note.txt")
	if err := os.WriteFile(filePath, []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("note.txt", filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	entry, err := root.LstatEntry("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.IdentityAvailable || !entry.OwnershipAvailable {
		t.Fatalf("entry identity or ownership unavailable: %+v", entry)
	}
	mode := os.FileMode(0o600)
	updated, err := root.UpdateMetadata("note.txt", entry.Fingerprint(), MetadataUpdate{Mode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Mode.Perm() != mode || updated.OwnerUID != entry.OwnerUID || updated.GroupGID != entry.GroupGID || !updated.OwnershipAvailable {
		t.Fatalf("updated metadata = %+v", updated)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("file mode = %v; want %v", info.Mode().Perm(), mode)
	}
	if _, err := root.UpdateMetadata("note.txt", entry.Fingerprint(), MetadataUpdate{Mode: &mode}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale metadata update error = %v, want ErrConflict", err)
	}

	directory, err := root.LstatEntry("folder")
	if err != nil {
		t.Fatal(err)
	}
	directoryMode := os.FileMode(0o700)
	if updated, err := root.UpdateMetadata("folder", directory.Fingerprint(), MetadataUpdate{Mode: &directoryMode}); err != nil || updated.Mode.Perm() != directoryMode {
		t.Fatalf("directory metadata = %+v, error = %v", updated, err)
	}

	link, err := root.LstatEntry("alias.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.UpdateMetadata("alias.txt", link.Fingerprint(), MetadataUpdate{Mode: &mode}); !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("symlink metadata update error = %v, want ErrSymlinkPath", err)
	}
	if _, err := root.UpdateMetadata("note.txt", entry.Fingerprint(), MetadataUpdate{}); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("empty metadata update error = %v, want invalid argument", err)
	}
}
