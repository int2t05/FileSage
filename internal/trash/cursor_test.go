// 本文件验证回收站游标编码边界。
package trash

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestTrashCursorRoundTripAndRejectsInvalidValues(t *testing.T) {
	cursor := encodeCursor(1234, 56)
	snapshot, beforeID, err := decodeCursor(cursor)
	if err != nil || snapshot != 1234 || beforeID != 56 {
		t.Fatalf("decodeCursor() = (%d, %d, %v)", snapshot, beforeID, err)
	}
	for _, invalid := range []string{"not-a-cursor", "", "AAAA"} {
		if _, _, err := decodeCursor(invalid); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("decodeCursor(%q) error = %v", invalid, err)
		}
	}
}

func TestManagerListContinuesWithSnapshotBoundCursor(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	identity := uint64(1)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		id, err := database.BeginTrash(ctx, store.TrashRecord{
			OriginalPath: name, Kind: "file", SizeBytes: 1,
			Device: &identity, Inode: &identity, LinkCount: &identity, IdentityAvailable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		trashPath := "trash/" + name
		if err := database.SetTrashDestination(ctx, id, trashPath); err != nil {
			t.Fatal(err)
		}
		if err := database.SetTrashStatus(ctx, id, store.TrashMoving, store.TrashTrashed, "", "", "user", "trash_trashed"); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	manager := &Manager{root: root, data: database}

	first, err := manager.List(ctx, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].Name != "c.txt" || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := manager.List(ctx, 1, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Name != "b.txt" || second.Snapshot != first.Snapshot {
		t.Fatalf("second page = %#v", second)
	}
	if _, _, err := decodeCursor(first.NextCursor + "x"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("tampered cursor error = %v", err)
	}
}
