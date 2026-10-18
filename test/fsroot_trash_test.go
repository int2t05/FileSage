//go:build linux

package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"filesage/internal/fsroot"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
)

type replacingTrashRoot struct {
	*fsroot.Root
	rootPath  string
	trashPath string
	swapped   bool
}

func (r *replacingTrashRoot) MountID(relativePath string) (uint64, error) {
	if relativePath == r.trashPath && !r.swapped {
		r.swapped = true
		absolute := filepath.Join(r.rootPath, filepath.FromSlash(r.trashPath))
		held := filepath.Join(r.rootPath, fsroot.TrashDirectoryName, "held")
		if err := os.Rename(absolute, held); err != nil {
			return 0, err
		}
		if err := os.WriteFile(absolute, []byte("replacement"), 0o600); err != nil {
			return 0, err
		}
	}
	return r.Root.MountID(relativePath)
}

func TestTrashAreaAndPurgeDoNotFollowLinks(t *testing.T) {
	rootPath := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "inside.txt"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "folder", "outside-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	r, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	area, mountRoot, mountID, err := r.TrashArea("folder")
	if err != nil {
		t.Fatalf("TrashArea() error = %v", err)
	}
	if area != fsroot.TrashDirectoryName || mountRoot != "" || mountID == 0 {
		t.Fatalf("TrashArea() = (%q, %q, %d)", area, mountRoot, mountID)
	}
	if err := r.MoveToTrash("folder", filepath.ToSlash(filepath.Join(area, "42")), mountID); err != nil {
		t.Fatalf("MoveToTrash() error = %v", err)
	}
	trashedEntry, err := r.LstatTrashEntry(filepath.ToSlash(filepath.Join(area, "42")))
	if err != nil {
		t.Fatalf("LstatTrashEntry() error = %v", err)
	}
	if err := r.RemoveTreeNoFollow(trashedEntry.Path, mountID, trashedEntry.Device, trashedEntry.Inode); err != nil {
		t.Fatalf("RemoveTreeNoFollow() error = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("external symlink target was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "folder")); !os.IsNotExist(err) {
		t.Fatalf("trashed directory remains: %v", err)
	}
}

func TestRemoveTreeNoFollowRejectsReplacedRootIdentity(t *testing.T) {
	tests := []struct {
		name string
		make func(string, string) error
	}{
		{name: "file", make: func(rootPath, source string) error {
			return os.WriteFile(filepath.Join(rootPath, source), []byte("original"), 0o600)
		}},
		{name: "directory", make: func(rootPath, source string) error {
			if err := os.Mkdir(filepath.Join(rootPath, source), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(rootPath, source, "child"), []byte("original"), 0o600)
		}},
		{name: "symlink", make: func(rootPath, source string) error {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(rootPath, source))
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := testCase.make(rootPath, "source"); err != nil {
				t.Skipf("fixture unavailable: %v", err)
			}
			r, err := fsroot.Open(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			area, _, mountID, err := r.TrashArea("source")
			if err != nil {
				t.Fatal(err)
			}
			trashPath := filepath.ToSlash(filepath.Join(area, "42"))
			if err := r.MoveToTrash("source", trashPath, mountID); err != nil {
				t.Fatal(err)
			}
			entry, err := r.LstatTrashEntry(trashPath)
			if err != nil {
				t.Fatal(err)
			}
			device, inode := entry.Device, entry.Inode
			if entry.Symlink {
				device, inode = entry.LinkDevice, entry.LinkInode
			}
			if inode == 0 {
				t.Fatal("trash entry has no identity")
			}

			absoluteTrash := filepath.Join(rootPath, filepath.FromSlash(trashPath))
			held := filepath.Join(rootPath, area, "held")
			if err := os.Rename(absoluteTrash, held); err != nil {
				t.Fatal(err)
			}
			switch testCase.name {
			case "file":
				if err := os.WriteFile(absoluteTrash, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(absoluteTrash, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(absoluteTrash, "replacement"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "replacement-target")
				if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, absoluteTrash); err != nil {
					t.Fatal(err)
				}
			}

			if err := r.RemoveTreeNoFollow(trashPath, mountID, device, inode); !errors.Is(err, fsroot.ErrPathChanged) {
				t.Fatalf("RemoveTreeNoFollow() error = %v, want ErrPathChanged", err)
			}
			if _, err := os.Lstat(absoluteTrash); err != nil {
				t.Fatalf("replacement was removed: %v", err)
			}
			if _, err := os.Lstat(held); err != nil {
				t.Fatalf("original item was removed: %v", err)
			}
		})
	}
}

func TestPurgeMarksReplacedTrashItemNeedsAttention(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseRoot, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer baseRoot.Close()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := &replacingTrashRoot{Root: baseRoot, rootPath: rootPath}
	manager, err := trashsvc.Open(ctx, root, database)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := baseRoot.LstatEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	item, err := manager.MoveToTrash(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	root.trashPath = filepath.ToSlash(filepath.Join(fsroot.TrashDirectoryName, item.ID))
	if err := manager.Purge(ctx, item.ID); err == nil {
		t.Fatal("Purge() accepted a replacement trash item")
	}
	if !root.swapped {
		t.Fatal("purge did not exercise the post-verification replacement window")
	}
	id, err := strconv.ParseInt(item.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	record, err := database.Trash(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != store.TrashNeedsAttention {
		t.Fatalf("trash status = %q, want %q", record.Status, store.TrashNeedsAttention)
	}
	if _, err := os.Stat(filepath.Join(rootPath, fsroot.TrashDirectoryName, item.ID)); err != nil {
		t.Fatalf("replacement was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, fsroot.TrashDirectoryName, "held")); err != nil {
		t.Fatalf("original item was removed: %v", err)
	}
}

func TestVerifyTrashAreaDoesNotRecreateMissingArea(t *testing.T) {
	rootPath := t.TempDir()
	r, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mountID, err := r.MountID("")
	if err != nil {
		t.Fatal(err)
	}
	area := fsroot.TrashDirectoryName
	if err := r.VerifyTrashArea(area, mountID); err == nil {
		t.Fatal("VerifyTrashArea() accepted a missing area")
	}
	if _, err := os.Lstat(filepath.Join(rootPath, area)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VerifyTrashArea() created missing area: %v", err)
	}
}

func TestRestoreFromTrashAcceptsMissingTargetUsingParentMount(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "original.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	area, _, mountID, err := r.TrashArea("original.txt")
	if err != nil {
		t.Fatalf("TrashArea() error = %v", err)
	}
	trashed := filepath.ToSlash(filepath.Join(area, "42"))
	if err := r.MoveToTrash("original.txt", trashed, mountID); err != nil {
		t.Fatalf("MoveToTrash() error = %v", err)
	}
	if err := r.RestoreFromTrash(trashed, "restored.txt", mountID); err != nil {
		t.Fatalf("RestoreFromTrash() with absent target error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rootPath, "restored.txt"))
	if err != nil || string(data) != "content" {
		t.Fatalf("restored file = %q, error = %v", data, err)
	}
}

func TestTrashAreaRejectsUnownedReservedDirectory(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "item.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, fsroot.TrashDirectoryName), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, _, _, err := r.TrashArea("item.txt"); err == nil {
		t.Fatal("TrashArea() adopted an unmarked user directory")
	}
}
