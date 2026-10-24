// 本文件验证索引迁移后的拍摄日期筛选语义。
package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCaptureDateFilterWaitsForNewGenerationAfterV5Migration 验证旧代次完成新扫描前不可按拍摄日期筛选。
func TestCaptureDateFilterWaitsForNewGenerationAfterV5Migration(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY CHECK (version > 0),
			applied_at_ns INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:5] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)",
			migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO file_index_scan_runs (generation, state, scanned_count, started_at_ns, finished_at_ns)
		VALUES (1, 'complete', 1, 1, 2);
		UPDATE file_index_state
		SET active_generation = 1, next_generation = 2, state = 'ready', completed_at_ns = 2
		WHERE singleton = 1;
		INSERT INTO file_index_generation_entries (
			generation, relative_path, parent_path, name, kind, size_bytes,
			modified_at_ns, mode, indexed_at_ns
		) VALUES (1, 'photo.jpg', '', 'photo.jpg', 'file', 3, 1, 384, 2);
	`); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v5 database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if version, err := db.SchemaVersion(ctx); err != nil || version != len(migrations) {
		t.Fatalf("SchemaVersion() = (%d, %v), want (%d, nil)", version, err, len(migrations))
	}

	capturedFrom := time.Unix(0, 0).UTC()
	legacyQuery := FileIndexQuery{
		Generation: 1, Limit: 10, CapturedFrom: &capturedFrom,
	}
	if _, err := db.QueryFileIndex(ctx, legacyQuery); !errors.Is(err, ErrFileIndexCaptureMetadataUnavailable) {
		t.Fatalf("legacy capture-date query error = %v", err)
	}
	legacyQuery.CapturedFrom = nil
	rows, err := db.QueryFileIndex(ctx, legacyQuery)
	if err != nil || len(rows) != 1 || rows[0].RelativePath != "photo.jpg" {
		t.Fatalf("legacy non-capture query = (%#v, %v)", rows, err)
	}

	generation, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capturedAt := time.Date(2024, 8, 9, 10, 11, 12, 0, time.UTC)
	record := FileIndexRecord{
		RelativePath: "photo.jpg", Name: "photo.jpg", Kind: "file", SizeBytes: 3,
		ModifiedAt: time.Unix(1, 0).UTC(), CapturedAt: &capturedAt,
	}
	if err := db.UpsertFileIndexBatch(ctx, generation, []FileIndexRecord{record}); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}
	newQuery := FileIndexQuery{
		Generation: generation, Limit: 10, CapturedFrom: &capturedFrom,
	}
	rows, err = db.QueryFileIndex(ctx, newQuery)
	if err != nil || len(rows) != 1 || rows[0].RelativePath != "photo.jpg" {
		t.Fatalf("new capture-date query = (%#v, %v)", rows, err)
	}
}

func TestV24TextFileActionsPreserveConversationHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v23.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:23] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)", migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO agent_conversations (
			id, state, goal, file_path, provider, model, destination,
			created_at_ns, updated_at_ns, analysis_action, operations_json
		) VALUES (
			'legacy-v23', 'awaiting_confirmation', '重命名笔记', 'notes.md', 'local', 'test', 'local',
			1, 2, 'rename', '[{"action":"rename","filePath":"notes.md"}]'
		);
		INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
		VALUES ('legacy-v23', 1, 'user', '整理笔记名称', 1);
	`); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v23 database: %v", err)
	}
	defer db.Close()
	conversation, err := db.Conversation(ctx, "legacy-v23")
	if err != nil || conversation.Analysis == nil || conversation.Analysis.Action != "rename" || len(conversation.Messages) != 1 {
		t.Fatalf("migrated conversation = (%#v, %v)", conversation, err)
	}
	for _, action := range []string{"create_file", "overwrite_file"} {
		id := "conversation-" + action
		if err := db.CreateConversation(ctx, id, "写入文本", time.Now()); err != nil {
			t.Fatal(err)
		}
		card := AnalysisCard{
			Action: action, Goal: "生成文本", FilePath: "notes.md",
			Provider: "local", Model: "test", Destination: "local",
		}
		if err := db.FinishConversationTurn(ctx, id, ConversationAwaitingConfirmation, "请确认文本文件操作。", card, "", time.Now()); err != nil {
			t.Fatalf("persist %s confirmation: %v", action, err)
		}
		stored, err := db.Conversation(ctx, id)
		if err != nil || stored.Analysis == nil || stored.Analysis.Action != action {
			t.Fatalf("restore %s confirmation = (%#v, %v)", action, stored.Analysis, err)
		}
	}
	rows, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("v24 migration left a foreign-key violation")
	}
}

// TestV8ConversationMigrationPreservesHistory 验证会话表重建后保留历史与外键。
func TestV8ConversationMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-agent.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:8] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)", migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO agent_conversations (
			id, state, goal, file_path, provider, model, destination,
			created_at_ns, updated_at_ns, analysis_action, target_path
		) VALUES ('legacy-conversation', 'awaiting_confirmation', '整理照片', '', 'ollama', 'model', 'local', 1, 2, 'create_directory', '照片/2025');
		INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
		VALUES ('legacy-conversation', 1, 'user', '按年份整理照片', 1),
			('legacy-conversation', 2, 'assistant', '请确认创建照片/2025。', 2);
	`); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v8 database: %v", err)
	}
	defer db.Close()
	conversation, err := db.Conversation(ctx, "legacy-conversation")
	if err != nil || conversation.State != ConversationAwaitingConfirmation || len(conversation.Messages) != 2 || conversation.Analysis == nil {
		t.Fatalf("restored conversation = (%#v, %v)", conversation, err)
	}
	if card := conversation.Analysis; card.Action != "create_directory" || card.TargetPath != "照片/2025" {
		t.Fatalf("restored analysis card = %#v", card)
	}
	if version, err := db.SchemaVersion(ctx); err != nil || version != len(migrations) {
		t.Fatalf("SchemaVersion() = (%d, %v), want (%d, nil)", version, err, len(migrations))
	}
	rows, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("v8 conversation migration left a foreign-key violation")
	}
}

// TestV14TextEditMigrationPreservesConversationAndDuplicateHistory 验证新动作约束迁移不丢失会话和扫描结果。
func TestV14TextEditMigrationPreservesConversationAndDuplicateHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-text-edit.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:14] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)", migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO tasks (id, kind, status, created_at_ns, updated_at_ns)
		VALUES ('scan-task', 'duplicate_scan', 'succeeded', 1, 2);
		INSERT INTO agent_conversations (
			id, state, goal, provider, model, destination, created_at_ns, updated_at_ns,
			analysis_action, operations_json, duplicate_scan_id
		) VALUES (
			'scan-conversation', 'analysis_complete', '查找重复项', 'local', 'test', 'local', 1, 2,
			'find_duplicates', '[{"action":"find_duplicates","filePath":""}]', 'scan-run'
		);
		INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
		VALUES ('scan-conversation', 1, 'user', '查找重复文件', 1);
		INSERT INTO duplicate_scan_runs (
			id, task_id, conversation_id, status, phase, entries_visited, candidate_files,
			files_hashed, group_count, duplicate_file_count, reclaimable_bytes, created_at_ns, updated_at_ns
		) VALUES ('scan-run', 'scan-task', 'scan-conversation', 'succeeded', 'complete', 2, 2, 2, 1, 2, 5, 1, 2);
		INSERT INTO duplicate_scan_groups (scan_id, group_id, size_bytes, duplicate_count)
		VALUES ('scan-run', 1, 5, 2);
		INSERT INTO duplicate_scan_files (scan_id, group_id, path, size_bytes, modified_at_ns)
		VALUES ('scan-run', 1, 'a.txt', 5, 1), ('scan-run', 1, 'b.txt', 5, 1);
	`); err != nil {
		t.Fatalf("seed v14 rows: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v14 database: %v", err)
	}
	defer db.Close()
	if version, err := db.SchemaVersion(ctx); err != nil || version != len(migrations) {
		t.Fatalf("SchemaVersion() = (%d, %v), want (%d, nil)", version, err, len(migrations))
	}
	conversation, err := db.Conversation(ctx, "scan-conversation")
	if err != nil || conversation.State != ConversationAnalysisComplete || conversation.DuplicateScanID != "scan-run" || len(conversation.Messages) != 1 {
		t.Fatalf("migrated conversation = (%#v, %v)", conversation, err)
	}
	var groups, files int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM duplicate_scan_groups WHERE scan_id = 'scan-run'").Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM duplicate_scan_files WHERE scan_id = 'scan-run'").Scan(&files); err != nil {
		t.Fatal(err)
	}
	if groups != 1 || files != 2 {
		t.Fatalf("duplicate scan rows after migration = (%d groups, %d files)", groups, files)
	}
	if err := db.CreateConversation(ctx, "text-edit-conversation", "编辑文件", time.Now()); err != nil {
		t.Fatal(err)
	}
	card := AnalysisCard{
		Action: "edit_text", Goal: "润色正文", FilePath: "notes.md",
		Provider: "local", Model: "test", Destination: "local",
	}
	if err := db.FinishConversationTurn(ctx, "text-edit-conversation", ConversationAwaitingConfirmation, "请确认读取并编辑。", card, "", time.Now()); err != nil {
		t.Fatalf("persist edit_text confirmation: %v", err)
	}
	var action string
	if err := db.db.QueryRowContext(ctx, "SELECT analysis_action FROM agent_conversations WHERE id = 'text-edit-conversation'").Scan(&action); err != nil || action != "edit_text" {
		t.Fatalf("stored analysis_action = (%q, %v), want edit_text", action, err)
	}
	if err := db.CreateConversation(ctx, "trash-conversation", "将文件移入回收站", time.Now()); err != nil {
		t.Fatal(err)
	}
	trashCard := AnalysisCard{
		Action: "trash", Goal: "移入可恢复回收站", FilePath: "notes.md",
		Provider: "local", Model: "test", Destination: "local",
	}
	if err := db.FinishConversationTurn(ctx, "trash-conversation", ConversationAwaitingConfirmation, "请确认移入回收站。", trashCard, "", time.Now()); err != nil {
		t.Fatalf("persist trash confirmation: %v", err)
	}
	if err := db.db.QueryRowContext(ctx, "SELECT analysis_action FROM agent_conversations WHERE id = 'trash-conversation'").Scan(&action); err != nil || action != "trash" {
		t.Fatalf("stored analysis_action = (%q, %v), want trash", action, err)
	}
	rows, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("v14 text-edit migration left a foreign-key violation")
	}
}

func TestV21PermanentDeleteMigrationPreservesConversationHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-permanent-delete.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:20] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)", migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO agent_conversations (
			id, state, goal, file_path, provider, model, destination,
			created_at_ns, updated_at_ns, analysis_action, target_path, operations_json
		) VALUES (
			'legacy-trash', 'awaiting_confirmation', '回收旧文件', 'old.txt', 'local', 'test', 'local',
			1, 2, 'trash', '', '[{"action":"trash","filePath":"old.txt"}]'
		);
		INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
		VALUES ('legacy-trash', 1, 'user', '请移入回收站', 1), ('legacy-trash', 2, 'assistant', '请确认移入回收站', 2);
	`); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v20 database: %v", err)
	}
	defer db.Close()
	conversation, err := db.Conversation(ctx, "legacy-trash")
	if err != nil || conversation.State != ConversationAwaitingConfirmation || len(conversation.Messages) != 2 || conversation.Analysis == nil || conversation.Analysis.Action != "trash" {
		t.Fatalf("preserved conversation = (%#v, %v)", conversation, err)
	}
	if version, err := db.SchemaVersion(ctx); err != nil || version != len(migrations) {
		t.Fatalf("SchemaVersion() = (%d, %v), want (%d, nil)", version, err, len(migrations))
	}
	if err := db.CreateConversation(ctx, "permanent-delete", "永久删除文件", time.Now()); err != nil {
		t.Fatal(err)
	}
	card := AnalysisCard{
		Action: "permanent_delete", Goal: "永久删除旧文件", FilePath: "old.txt",
		Operations: []AnalysisOperation{{Action: "permanent_delete", FilePath: "old.txt"}},
		Provider:   "local", Model: "test", Destination: "local",
	}
	if err := db.FinishConversationTurn(ctx, "permanent-delete", ConversationAwaitingConfirmation, "永久删除后无法恢复，请确认。", card, "", time.Now()); err != nil {
		t.Fatalf("persist permanent-delete confirmation: %v", err)
	}
	conversation, err = db.Conversation(ctx, "permanent-delete")
	if err != nil || conversation.Analysis == nil || conversation.Analysis.Action != "permanent_delete" || len(conversation.Analysis.Operations) != 1 {
		t.Fatalf("permanent-delete confirmation = (%#v, %v)", conversation.Analysis, err)
	}
	rows, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("v21 conversation migration left a foreign-key violation")
	}
}

func TestV22ArchiveActionsMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-archive-actions.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:21] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)", migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO tasks (id, kind, status, created_at_ns, updated_at_ns)
		VALUES ('scan-task', 'duplicate_scan', 'succeeded', 1, 2);
		INSERT INTO agent_conversations (
			id, state, goal, provider, model, destination, created_at_ns, updated_at_ns,
			analysis_action, operations_json, duplicate_scan_id
		) VALUES (
			'scan-conversation', 'analysis_complete', '查找重复项', 'local', 'test', 'local', 1, 2,
			'find_duplicates', '[{"action":"find_duplicates","filePath":""}]', 'scan-run'
		);
		INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
		VALUES ('scan-conversation', 1, 'user', '查找重复文件', 1);
		INSERT INTO duplicate_scan_runs (
			id, task_id, conversation_id, status, phase, entries_visited, candidate_files,
			files_hashed, group_count, duplicate_file_count, reclaimable_bytes, created_at_ns, updated_at_ns
		) VALUES ('scan-run', 'scan-task', 'scan-conversation', 'succeeded', 'complete', 2, 2, 2, 1, 2, 5, 1, 2);
		INSERT INTO duplicate_scan_groups (scan_id, group_id, size_bytes, duplicate_count)
		VALUES ('scan-run', 1, 5, 2);
		INSERT INTO duplicate_scan_files (scan_id, group_id, path, size_bytes, modified_at_ns)
		VALUES ('scan-run', 1, 'a.txt', 5, 1), ('scan-run', 1, 'b.txt', 5, 1);
	`); err != nil {
		t.Fatalf("seed v21 history: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v21 database: %v", err)
	}
	defer db.Close()
	if version, err := db.SchemaVersion(ctx); err != nil || version != len(migrations) {
		t.Fatalf("SchemaVersion() = (%d, %v), want (%d, nil)", version, err, len(migrations))
	}
	conversation, err := db.Conversation(ctx, "scan-conversation")
	if err != nil || conversation.State != ConversationAnalysisComplete || conversation.DuplicateScanID != "scan-run" || len(conversation.Messages) != 1 {
		t.Fatalf("migrated conversation = (%#v, %v)", conversation, err)
	}
	var files int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM duplicate_scan_files WHERE scan_id = 'scan-run'").Scan(&files); err != nil || files != 2 {
		t.Fatalf("duplicate scan history = (%d, %v), want two files", files, err)
	}
	foreignKeys, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer foreignKeys.Close()
	if foreignKeys.Next() {
		t.Fatal("Shell migration left a foreign-key violation")
	}
	if err := foreignKeys.Err(); err != nil {
		t.Fatal(err)
	}

	for _, card := range []AnalysisCard{
		{
			Action: "create_archive", Goal: "创建压缩包", FilePath: "source", TargetPath: "source.zip",
			Operations: []AnalysisOperation{{Action: "create_archive", FilePath: "source", TargetPath: "source.zip"}},
			Provider:   "local", Model: "test", Destination: "local",
		},
		{
			Action: "extract_archive", Goal: "解压到根目录", FilePath: "bundle.zip", TargetPath: "",
			Operations: []AnalysisOperation{{Action: "extract_archive", FilePath: "bundle.zip", TargetPath: ""}},
			Provider:   "local", Model: "test", Destination: "local",
		},
	} {
		id := card.Action
		if err := db.CreateConversation(ctx, id, card.Goal, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishConversationTurn(ctx, id, ConversationAwaitingConfirmation, "请确认归档操作。", card, "", time.Now()); err != nil {
			t.Fatalf("persist %s confirmation: %v", card.Action, err)
		}
		migrated, err := db.Conversation(ctx, id)
		if err != nil || migrated.Analysis == nil || migrated.Analysis.Action != card.Action {
			t.Fatalf("stored %s action = (%#v, %v)", card.Action, migrated.Analysis, err)
		}
	}
	rows, err := db.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("v22 archive action migration left a foreign-key violation")
	}
}

func TestV27ShellMigrationDropsPersistedCommandOutput(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-shell-output.db")
	rawDB, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY CHECK (version > 0), applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:26] {
		if _, err := rawDB.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := rawDB.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)", migration.version, int64(migration.version)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawDB.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		t.Fatalf("enable WAL for v26 database: %v", err)
	}
	stdoutMarker := "filesage-shell-output-migration-sentinel"
	stderrMarker := "filesage-shell-error-migration-sentinel"
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO tasks (id, kind, status, created_at_ns, updated_at_ns)
		VALUES ('shell-task', 'agent', 'succeeded', 1, 2);
		INSERT INTO plans (id, task_id, version, status, data_scope_json, created_at_ns, updated_at_ns)
		VALUES ('shell-plan', 'shell-task', 1, 'completed', '{}', 1, 2);
		INSERT INTO operations (
			id, plan_id, ordinal, action, source_state_json, target_conflict_policy,
			approval_state, execution_status, created_at_ns, updated_at_ns
		) VALUES ('shell-operation', 'shell-plan', 0, 'run_shell', '{}', 'fail', 'pending', 'succeeded', 1, 2);
		INSERT INTO shell_runs (
			id, plan_id, operation_id, command, working_directory, stage_id, status,
			created_at_ns, updated_at_ns, exit_code, stdout, stderr, output_available
		) VALUES (
			'shell-run', 'shell-plan', 'shell-operation', 'cat private.txt', '.', 'stage', 'completed',
			1, 2, 0, CAST(? AS TEXT), ?, 1
		);
	`, "\x00"+stdoutMarker, stderrMarker); err != nil {
		t.Fatalf("seed v26 Shell output: %v", err)
	}

	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("upgrade v26 database: %v", err)
	}
	defer db.Close()
	var outputDiscarded, outputTruncated, exitCode int
	var command, stdout, stderr string
	if err := db.db.QueryRowContext(ctx, `SELECT output_discarded, output_truncated, exit_code, command, stdout, stderr FROM shell_runs WHERE id = 'shell-run'`).
		Scan(&outputDiscarded, &outputTruncated, &exitCode, &command, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if outputDiscarded != 1 || outputTruncated != 0 || exitCode != 0 || command != "cat private.txt" || stdout != "" || stderr != "" {
		t.Fatalf("migrated Shell run = (discarded=%d, truncated=%d, exit=%d, command=%q, stdout=%q, stderr=%q)", outputDiscarded, outputTruncated, exitCode, command, stdout, stderr)
	}
	columns, err := db.db.QueryContext(ctx, "PRAGMA table_info(shell_runs)")
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	var hasStdout, hasStderr, hasOutputAvailable bool
	for columns.Next() {
		var index, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := columns.Scan(&index, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		hasStdout = hasStdout || name == "stdout"
		hasStderr = hasStderr || name == "stderr"
		hasOutputAvailable = hasOutputAvailable || name == "output_available"
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
	if !hasStdout || !hasStderr || hasOutputAvailable {
		t.Fatalf("bounded output schema = (stdout=%t, stderr=%t, legacy output_available=%t)", hasStdout, hasStderr, hasOutputAvailable)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close migrated database: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close legacy database connection: %v", err)
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read migrated SQLite file %s: %v", filepath.Base(path), err)
		}
		if bytes.Contains(data, []byte(stdoutMarker)) || bytes.Contains(data, []byte(stderrMarker)) {
			t.Fatalf("legacy Shell output remains in SQLite file %s", filepath.Base(path))
		}
	}
}

// TestV29FileIndexOrderingIndexes 验证默认排序索引已替换。
func TestV29FileIndexOrderingIndexes(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "index-order.db")
	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Errorf("close database: %v", err)
			}
		}
	})

	generation, err := db.BeginFileIndexScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	record := FileIndexRecord{
		RelativePath: "docs/report.pdf", ParentPath: "docs", Name: "report.pdf",
		Kind: "file", SizeBytes: 7, Mode: 0o644, MediaType: "application/pdf",
		ContentSHA256: strings.Repeat("a", 64), IndexedAt: time.Now().UTC(),
	}
	if err := db.UpsertFileIndexBatch(ctx, generation, []FileIndexRecord{record}); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteFileIndexScan(ctx, generation); err != nil {
		t.Fatal(err)
	}

	// 将新库还原为 v28 的结构，验证后续迁移路径。
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		DROP INDEX file_index_generation_parent_order_name;
		DROP INDEX file_index_generation_order_name;
		CREATE INDEX file_index_generation_parent_name
			ON file_index_generation_entries(generation, parent_path, name);
		ALTER TABLE shell_runs DROP COLUMN output_truncated;
		ALTER TABLE shell_runs DROP COLUMN stderr;
		ALTER TABLE shell_runs DROP COLUMN stdout;
		ALTER TABLE agent_conversations DROP COLUMN tool_context_plan_id;
		DROP INDEX agent_conversations_visible_updated;
		ALTER TABLE agent_conversations DROP COLUMN deleted_at_ns;
		DELETE FROM schema_migrations WHERE version >= 29;
	`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	beforeVersion, err := db.SchemaVersion(ctx)
	if err != nil || beforeVersion != 28 {
		t.Fatalf("SchemaVersion() before v29 upgrade = (%d, %v), want (28, nil)", beforeVersion, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil

	db, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("reopen v28 database: %v", err)
	}
	version, err := db.SchemaVersion(ctx)
	if err != nil || version != len(migrations) {
		t.Fatalf("SchemaVersion() after v28 upgrade = (%d, %v), want (%d, nil)", version, err, len(migrations))
	}
	var preservedPath string
	if err := db.db.QueryRowContext(ctx, `
		SELECT relative_path FROM file_index_generation_entries
		WHERE generation = ? AND relative_path = ?
	`, generation, record.RelativePath).Scan(&preservedPath); err != nil {
		t.Fatalf("read file index after v28 upgrade: %v", err)
	}
	if preservedPath != record.RelativePath {
		t.Fatalf("migrated file index path = %q, want %q", preservedPath, record.RelativePath)
	}

	rows, err := db.db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'file_index_generation_entries'
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexes := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"file_index_generation_parent_order_name",
		"file_index_generation_order_name",
	} {
		if !indexes[name] {
			t.Errorf("index %q was not created", name)
		}
	}
	if indexes["file_index_generation_parent_name"] {
		t.Error("redundant parent/name index remains")
	}

	queries := []struct {
		name  string
		sql   string
		args  []any
		index string
	}{
		{
			name: "directory listing",
			sql: `EXPLAIN QUERY PLAN SELECT relative_path, parent_path, name, kind, size_bytes,
				modified_at_ns, mode, device, inode, hard_link_count, media_type,
				captured_at_ns, content_sha256, indexed_at_ns
				FROM file_index_generation_entries
				WHERE generation = ? AND parent_path = ?
				ORDER BY CASE WHEN kind = 'directory' THEN 0 ELSE 1 END,
					name COLLATE NOCASE ASC, relative_path ASC
				LIMIT ? OFFSET ?`,
			args:  []any{1, "", 100, 0},
			index: "file_index_generation_parent_order_name",
		},
		{
			name: "recursive search",
			sql: `EXPLAIN QUERY PLAN SELECT relative_path, parent_path, name, kind, size_bytes,
				modified_at_ns, mode, device, inode, hard_link_count, media_type,
				captured_at_ns, content_sha256, indexed_at_ns
				FROM file_index_generation_entries
				WHERE generation = ? AND (name LIKE ? ESCAPE '\' COLLATE NOCASE
					OR relative_path LIKE ? ESCAPE '\' COLLATE NOCASE)
				ORDER BY CASE WHEN kind = 'directory' THEN 0 ELSE 1 END,
					name COLLATE NOCASE ASC, relative_path ASC
				LIMIT ? OFFSET ?`,
			args:  []any{1, "%e%", "%e%", 100, 0},
			index: "file_index_generation_order_name",
		},
	}
	for _, query := range queries {
		rows, err := db.db.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			t.Fatalf("explain %s: %v", query.name, err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatalf("read %s plan: %v", query.name, err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("read %s plan: %v", query.name, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close %s plan: %v", query.name, err)
		}
		plan := strings.Join(details, "\n")
		if !strings.Contains(plan, query.index) || strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("%s plan = %q, want %s without a temporary sort", query.name, plan, query.index)
		}
	}
}
