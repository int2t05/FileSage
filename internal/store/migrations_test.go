// 本文件验证索引迁移后的拍摄日期筛选语义。
package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
