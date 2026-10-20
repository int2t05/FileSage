package test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/store"
)

func TestStoreMigratesAndPersistsMetadataAcrossReopen(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state", "filesage.db")
	openedAt := time.Date(2026, 10, 15, 12, 30, 0, 0, time.UTC)
	device, inode, links := uint64(8), uint64(44), uint64(1)
	capturedAt := time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)

	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})
	if err := db.Health(ctx); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if version, err := db.SchemaVersion(ctx); err != nil || version != 28 {
		t.Fatalf("SchemaVersion() = (%d, %v), want (28, nil)", version, err)
	}

	if err := db.UpsertFileIndex(ctx, store.FileIndexRecord{
		RelativePath:  "documents/contract.pdf",
		ParentPath:    "documents",
		Name:          "contract.pdf",
		Kind:          "file",
		SizeBytes:     2048,
		ModifiedAt:    openedAt,
		Mode:          0o640,
		Device:        &device,
		Inode:         &inode,
		HardLinkCount: &links,
		MediaType:     "application/pdf",
		CapturedAt:    &capturedAt,
		ContentSHA256: strings.Repeat("a", 64),
		IndexedAt:     openedAt,
	}); err != nil {
		t.Fatalf("UpsertFileIndex() error = %v", err)
	}

	task := store.TaskRecord{
		ID:            "task-1",
		Kind:          "agent_organization",
		Status:        store.TaskAwaitingApproval,
		RequestText:   "按日期归档照片并检查完全重复项",
		ScopePath:     "photos",
		ModelProvider: "local",
		ModelName:     "example-model",
		CreatedAt:     openedAt,
		UpdatedAt:     openedAt,
	}
	if err := db.PutTask(ctx, task); err != nil {
		t.Fatalf("PutTask() error = %v", err)
	}
	plan := store.PlanRecord{
		ID:            "plan-1",
		TaskID:        task.ID,
		Version:       1,
		Status:        store.PlanAwaitingApproval,
		ScopePath:     "photos",
		ModelProvider: "local",
		ModelName:     "example-model",
		DataScopeJSON: `{"paths":["photos"],"metadata":["name","capture_time"]}`,
		CreatedAt:     openedAt,
		UpdatedAt:     openedAt,
		Operations: []store.OperationRecord{
			{
				ID:                   "operation-move",
				Ordinal:              0,
				Action:               "move",
				SourcePath:           "photos/IMG_003.jpg",
				TargetPath:           "photos/2026/08/IMG_003.jpg",
				Reason:               "按 EXIF 拍摄日期整理",
				Evidence:             "拍摄时间为 2026-08-09",
				SourceStateJSON:      `{"size":2048,"inode":44}`,
				TargetConflictPolicy: "rename",
				Approval:             store.ApprovalPending,
				Status:               store.OperationPending,
				CreatedAt:            openedAt,
				UpdatedAt:            openedAt,
			},
			{
				ID:                           "operation-delete",
				Ordinal:                      1,
				Action:                       "permanent_delete",
				SourcePath:                   "backup/copy.pdf",
				Reason:                       "内容校验值与保留文件一致",
				Evidence:                     "SHA-256 内容校验一致",
				SourceStateJSON:              `{"size":2048,"inode":45}`,
				TargetConflictPolicy:         "fail",
				RequiresExplicitConfirmation: true,
				Approval:                     store.ApprovalPending,
				Status:                       store.OperationPending,
				CreatedAt:                    openedAt,
				UpdatedAt:                    openedAt,
			},
		},
	}
	if err := db.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("CreatePlan() error = %v", err)
	}
	if err := db.SetOperationApproval(ctx, "operation-move", store.ApprovalApproved, "local-user", openedAt.Add(time.Minute)); err != nil {
		t.Fatalf("SetOperationApproval() error = %v", err)
	}
	if _, err := db.AppendAudit(ctx, store.AuditRecord{
		TaskID:      task.ID,
		PlanID:      plan.ID,
		OperationID: "operation-move",
		EventType:   "operation_approved",
		Actor:       "local-user",
		Outcome:     "approved",
		Message:     "用户批准了该移动操作",
		SourcePath:  "photos/IMG_003.jpg",
		TargetPath:  "photos/2026/08/IMG_003.jpg",
		CreatedAt:   openedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("AppendAudit() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db = nil

	db, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("reopen Open() error = %v", err)
	}
	if version, err := db.SchemaVersion(ctx); err != nil || version != 28 {
		t.Fatalf("reopened SchemaVersion() = (%d, %v), want (28, nil)", version, err)
	}

	fileRecord, err := db.FileIndex(ctx, "documents/contract.pdf")
	if err != nil {
		t.Fatalf("FileIndex() error = %v", err)
	}
	if fileRecord.SizeBytes != 2048 || fileRecord.Inode == nil || *fileRecord.Inode != inode || fileRecord.ContentSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("FileIndex() returned unexpected metadata: %+v", fileRecord)
	}
	if fileRecord.CapturedAt == nil || !fileRecord.CapturedAt.Equal(capturedAt) {
		t.Fatalf("CapturedAt = %v, want %v", fileRecord.CapturedAt, capturedAt)
	}

	gotTask, err := db.Task(ctx, task.ID)
	if err != nil {
		t.Fatalf("Task() error = %v", err)
	}
	if gotTask.Status != task.Status || gotTask.RequestText != task.RequestText || gotTask.ScopePath != task.ScopePath {
		t.Fatalf("Task() returned unexpected metadata: %+v", gotTask)
	}

	gotPlan, err := db.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(gotPlan.Operations) != 2 {
		t.Fatalf("Plan() returned %d operations, want 2", len(gotPlan.Operations))
	}
	if gotPlan.Operations[0].Approval != store.ApprovalApproved || gotPlan.Operations[0].ApprovedBy != "local-user" {
		t.Fatalf("approved operation metadata was not persisted: %+v", gotPlan.Operations[0])
	}
	if !gotPlan.Operations[1].RequiresExplicitConfirmation || gotPlan.Operations[1].Approval != store.ApprovalPending {
		t.Fatalf("permanent-delete approval state was not persisted: %+v", gotPlan.Operations[1])
	}

	audit, err := db.AuditForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("AuditForTask() error = %v", err)
	}
	if len(audit) != 1 || audit[0].Outcome != "approved" || audit[0].SourcePath != "photos/IMG_003.jpg" {
		t.Fatalf("AuditForTask() returned unexpected metadata: %+v", audit)
	}
}

func TestTrashSnapshotAndAuditStateArePersisted(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	identity := uint64(42)
	newItem := func(original, trash string) int64 {
		t.Helper()
		id, err := db.BeginTrash(ctx, store.TrashRecord{
			OriginalPath: original, TrashPath: trash, Kind: "file", SizeBytes: 7,
			Device: &identity, Inode: &identity, LinkCount: &identity, IdentityAvailable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetTrashDestination(ctx, id, trash); err != nil {
			t.Fatal(err)
		}
		if err := db.SetTrashStatus(ctx, id, store.TrashMoving, store.TrashTrashed, "", "", "user", "trash_trashed"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	firstID := newItem("a.txt", ".filesage-trash/1")
	secondID := newItem("b.txt", ".filesage-trash/2")
	if err := db.SetTrashStatus(ctx, secondID, store.TrashTrashed, store.TrashNeedsAttention, "", "状态不匹配", "user", "trash_needs_attention"); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListTrash(ctx, 1, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Snapshot <= secondID || !page.HasMore || len(page.Items) != 1 || page.Items[0].ID != secondID {
		t.Fatalf("ListTrash() = %#v", page)
	}
	secondPage, err := db.ListTrash(ctx, 1, page.Snapshot, page.Items[len(page.Items)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID != firstID || secondPage.HasMore {
		t.Fatalf("keyset continuation = %#v", secondPage)
	}
	firstStateSeq := secondPage.Items[0].StateSeq
	newItem("c.txt", ".filesage-trash/3")
	if err := db.SetTrashStatus(ctx, firstID, store.TrashTrashed, store.TrashNeedsAttention, "", "后续状态变化", "user", "trash_attention"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTrashStatusAtSeq(ctx, firstID, store.TrashNeedsAttention, firstStateSeq, store.TrashTrashed, "", "过期写入", "user", "trash_stale"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale status update error = %v, want conflict", err)
	}
	batch, err := db.ListTrashForPurge(ctx, page.Snapshot, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].ID != secondID {
		t.Fatalf("ListTrashForPurge(snapshot=%d) = %#v", page.Snapshot, batch)
	}
	if batch[0].Status != store.TrashNeedsAttention || batch[0].Message != "状态不匹配" {
		t.Fatalf("needs-attention state was not retained: %#v", batch[0])
	}
	if _, err := db.Trash(ctx, firstID); err != nil {
		t.Fatal(err)
	}
	futureSnapshot := page.Snapshot + 1000
	if _, err := db.ListTrash(ctx, 10, futureSnapshot, 0); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("future list snapshot error = %v, want conflict", err)
	}
	if _, err := db.ListTrashForPurge(ctx, futureSnapshot, 0, 100); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("future purge snapshot error = %v, want conflict", err)
	}
}

func TestTrashStateSequenceMigrationUsesExistingAuditBoundary(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	identity := uint64(9)
	id, err := db.BeginTrash(ctx, store.TrashRecord{
		OriginalPath: "legacy.txt", Kind: "file", SizeBytes: 3,
		Device: &identity, Inode: &identity, LinkCount: &identity, IdentityAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTrashDestination(ctx, id, ".filesage-trash/legacy"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTrashStatus(ctx, id, store.TrashMoving, store.TrashTrashed, "", "", "user", "trash_trashed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendAudit(ctx, store.AuditRecord{EventType: "unrelated", Actor: "system", Outcome: "recorded"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var migrationBoundary int64
	if err := rawDB.QueryRowContext(ctx, `SELECT MAX(id) FROM audit_events`).Scan(&migrationBoundary); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DROP TABLE agent_settings",
		"DROP TABLE duplicate_scan_files",
		"DROP TABLE duplicate_scan_groups",
		"DROP TABLE duplicate_scan_runs",
		"DROP TABLE agent_conversation_messages",
		"DROP TABLE agent_conversations",
		"DROP TABLE file_index_generation_entries",
		"DROP TABLE file_index_state",
		"DROP TABLE file_index_scan_runs",
		"DROP TABLE shell_changes",
		"DROP TABLE shell_runs",
		"ALTER TABLE plans DROP COLUMN stop_requested",
		"ALTER TABLE plans DROP COLUMN reviewed_at_ns",
		"ALTER TABLE plans DROP COLUMN execution_mode",
		"DELETE FROM schema_migrations WHERE version >= 3",
	} {
		if _, err := rawDB.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `DROP INDEX trash_items_snapshot`); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `ALTER TABLE trash_items DROP COLUMN state_seq`); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `ALTER TABLE operations DROP COLUMN target_state_json`); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"group_gid", "owner_uid", "mode"} {
		if _, err := rawDB.ExecContext(ctx, `ALTER TABLE operations DROP COLUMN `+column); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"proposed_content_sha256", "proposed_content_size"} {
		if _, err := rawDB.ExecContext(ctx, `ALTER TABLE operations DROP COLUMN `+column); err != nil {
			t.Fatal(err)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("reopen legacy database: %v", err)
	}
	defer db.Close()
	legacy, err := db.Trash(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.StateSeq != migrationBoundary {
		t.Fatalf("migrated state sequence = %d, want audit boundary %d", legacy.StateSeq, migrationBoundary)
	}
	newID, err := db.BeginTrash(ctx, store.TrashRecord{
		OriginalPath: "new.txt", Kind: "file", SizeBytes: 1,
		Device: &identity, Inode: &identity, LinkCount: &identity, IdentityAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	newRecord, err := db.Trash(ctx, newID)
	if err != nil {
		t.Fatal(err)
	}
	if newRecord.StateSeq <= legacy.StateSeq {
		t.Fatalf("new state sequence %d did not advance past migration boundary %d", newRecord.StateSeq, legacy.StateSeq)
	}
}

func TestStoreEnforcesForeignAndUniqueConstraintsAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	missingTaskPlan := store.PlanRecord{ID: "orphan", TaskID: "missing", Version: 1, Status: store.PlanDraft}
	if err := db.CreatePlan(ctx, missingTaskPlan); err == nil {
		t.Fatal("CreatePlan() accepted a missing task; foreign key should reject it")
	}

	if err := db.PutTask(ctx, store.TaskRecord{ID: "task-unique", Kind: "agent", Status: store.TaskQueued}); err != nil {
		t.Fatalf("PutTask() error = %v", err)
	}
	duplicateOrdinal := store.PlanRecord{
		ID:      "plan-atomic",
		TaskID:  "task-unique",
		Version: 1,
		Status:  store.PlanDraft,
		Operations: []store.OperationRecord{
			{ID: "first", Ordinal: 0, Action: "move"},
			{ID: "second", Ordinal: 0, Action: "rename"},
		},
	}
	if err := db.CreatePlan(ctx, duplicateOrdinal); err == nil {
		t.Fatal("CreatePlan() accepted duplicate operation ordinal")
	}
	if _, err := db.Plan(ctx, "plan-atomic"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed plan transaction left a plan behind: error = %v", err)
	}

	validPlan := store.PlanRecord{
		ID:      "plan-valid",
		TaskID:  "task-unique",
		Version: 1,
		Status:  store.PlanDraft,
		Operations: []store.OperationRecord{
			{ID: "valid-op", Ordinal: 0, Action: "rename"},
		},
	}
	if err := db.CreatePlan(ctx, validPlan); err != nil {
		t.Fatalf("CreatePlan(valid) error = %v", err)
	}
	duplicateVersion := validPlan
	duplicateVersion.ID = "plan-duplicate-version"
	if err := db.CreatePlan(ctx, duplicateVersion); err == nil {
		t.Fatal("CreatePlan() accepted a duplicate task plan version")
	}
	if err := db.SetOperationApproval(ctx, "missing-operation", store.ApprovalApproved, "local-user", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetOperationApproval(missing) error = %v, want ErrNotFound", err)
	}
}

func TestStoreSchemaContainsWorkflowMetadataOnly(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.PutTask(ctx, store.TaskRecord{ID: "metadata-task", Kind: "agent", Status: store.TaskQueued, RequestText: "整理照片"}); err != nil {
		t.Fatalf("PutTask() error = %v", err)
	}
	if _, err := db.AppendAudit(ctx, store.AuditRecord{
		TaskID:    "metadata-task",
		EventType: "task_created",
		Actor:     "local-user",
		Outcome:   "accepted",
		Message:   "已建立整理任务",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AppendAudit() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Inspect SQLite's actual column names rather than relying only on API types.
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	rows, err := rawDB.QueryContext(ctx, `
		SELECT m.name, p.name
		FROM sqlite_master AS m
		JOIN pragma_table_info(m.name) AS p
		WHERE m.type = 'table' AND m.name IN ('file_index', 'tasks', 'plans', 'operations', 'audit_events')`)
	if err != nil {
		t.Fatalf("inspect schema columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var tableName, columnName string
		if err := rows.Scan(&tableName, &columnName); err != nil {
			t.Fatalf("scan schema column: %v", err)
		}
		columns = append(columns, tableName+"."+columnName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema columns: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("no workflow schema columns were found")
	}
	for _, column := range columns {
		lower := strings.ToLower(column)
		for _, forbidden := range []string{"old_content", "previous_content", "content_diff", "backup", "file_bytes", "prior_body", "diff_blob"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("schema contains forbidden recoverable-content column %q", column)
			}
		}
	}
}
