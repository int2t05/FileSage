// 本文件验证重复扫描结果只在完整成功后发布。
package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDuplicateScanPublishesResultsAndRestoresConversationHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	db, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	createScanningConversation(t, db, "conversation-scan-success", "scan-success", "docs", now)

	if _, err := db.ListDuplicateScanGroups(ctx, "scan-success", 20, 0); !errors.Is(err, ErrDuplicateScanUnavailable) {
		t.Fatalf("groups before completion = %v, want unavailable", err)
	}
	groups := []DuplicateScanResultGroup{
		{SizeBytes: 4, Files: []DuplicateScanResultFile{
			{Path: "docs/a.txt", SizeBytes: 4, ModifiedAt: now},
			{Path: "docs/b.txt", SizeBytes: 4, ModifiedAt: now.Add(time.Second)},
		}},
		{SizeBytes: 8, Files: []DuplicateScanResultFile{
			{Path: "docs/c.txt", SizeBytes: 8, ModifiedAt: now},
			{Path: "docs/d.txt", SizeBytes: 8, ModifiedAt: now},
			{Path: "docs/e.txt", SizeBytes: 8, ModifiedAt: now},
		}},
	}
	progress := DuplicateScanProgress{Phase: "complete", EntriesVisited: 8, CandidateFiles: 5, FilesHashed: 5, BytesHashed: 32, TotalBytes: 32}
	if err := db.FinishDuplicateScan(ctx, "scan-success", string(DuplicateScanSucceeded), "", progress, groups, "扫描完成。", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err := db.DuplicateScan(ctx, "scan-success")
	if err != nil || status.Status != DuplicateScanSucceeded || status.GroupCount != 2 || status.DuplicateFileCount != 5 || status.ReclaimableBytes != 20 {
		t.Fatalf("scan status = (%#v, %v)", status, err)
	}
	groupPage, err := db.ListDuplicateScanGroups(ctx, "scan-success", 1, 0)
	if err != nil || len(groupPage.Groups) != 1 || !groupPage.HasMore || groupPage.Groups[0].SizeBytes != 4 {
		t.Fatalf("group page = (%#v, %v)", groupPage, err)
	}
	filePage, err := db.ListDuplicateScanFiles(ctx, "scan-success", groupPage.Groups[0].ID, 1, 0)
	if err != nil || len(filePage.Files) != 1 || !filePage.HasMore || filePage.Files[0].Path != "docs/a.txt" {
		t.Fatalf("file page = (%#v, %v)", filePage, err)
	}
	task, err := db.Task(ctx, "scan-success")
	if err != nil || task.Kind != "duplicate_scan" || task.Status != TaskSucceeded {
		t.Fatalf("scan task = (%#v, %v)", task, err)
	}
	conversation, err := db.Conversation(ctx, "conversation-scan-success")
	if err != nil || conversation.State != ConversationAnalysisComplete || conversation.DuplicateScanID != "scan-success" {
		t.Fatalf("scan conversation = (%#v, %v)", conversation, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conversation, err = db.Conversation(ctx, "conversation-scan-success")
	if err != nil || conversation.DuplicateScanID != "scan-success" || conversation.State != ConversationAnalysisComplete {
		t.Fatalf("restored scan conversation = (%#v, %v)", conversation, err)
	}
	stored, err := db.ListDuplicateScanFiles(ctx, "scan-success", 2, 10, 0)
	if err != nil || len(stored.Files) != 3 {
		t.Fatalf("restored scan files = (%#v, %v)", stored, err)
	}
}

func TestDuplicateScanFailureAndRestartDiscardGroups(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	createScanningConversation(t, db, "conversation-scan-failure", "scan-failure", "", now)
	if err := db.FinishDuplicateScan(ctx, "scan-failure", string(DuplicateScanFailed), "file_changed", DuplicateScanProgress{Phase: "hashing", FilesHashed: 1}, nil, "文件变化。", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ListDuplicateScanGroups(ctx, "scan-failure", 20, 0); !errors.Is(err, ErrDuplicateScanUnavailable) {
		t.Fatalf("failed scan groups error = %v", err)
	}
	failedConversation, err := db.Conversation(ctx, "conversation-scan-failure")
	if err != nil || failedConversation.State != ConversationFailed || failedConversation.DuplicateScanID != "scan-failure" {
		t.Fatalf("failed scan conversation = (%#v, %v)", failedConversation, err)
	}

	createScanningConversation(t, db, "conversation-scan-restart", "scan-restart", "docs", now)
	if _, err := db.db.ExecContext(ctx, `INSERT INTO duplicate_scan_groups(scan_id, group_id, size_bytes, duplicate_count) VALUES ('scan-restart', 1, 4, 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO duplicate_scan_files(scan_id, group_id, path, size_bytes, modified_at_ns) VALUES ('scan-restart', 1, 'docs/a', 4, 1), ('scan-restart', 1, 'docs/b', 4, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverDuplicateScans(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	restarted, err := db.DuplicateScan(ctx, "scan-restart")
	if err != nil || restarted.Status != DuplicateScanFailed || restarted.ErrorCode != "service_restarted" || restarted.GroupCount != 0 {
		t.Fatalf("recovered scan = (%#v, %v)", restarted, err)
	}
	if _, err := db.ListDuplicateScanGroups(ctx, "scan-restart", 20, 0); !errors.Is(err, ErrDuplicateScanUnavailable) {
		t.Fatalf("restarted scan groups error = %v", err)
	}
	var remaining int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM duplicate_scan_files WHERE scan_id = 'scan-restart'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("restarted files remaining = %d, err=%v", remaining, err)
	}
	restartedConversation, err := db.Conversation(ctx, "conversation-scan-restart")
	if err != nil || restartedConversation.State != ConversationFailed || restartedConversation.Error != "service_restarted" {
		t.Fatalf("recovered conversation = (%#v, %v)", restartedConversation, err)
	}
}

func TestDuplicateScanCancellationWinsBeforeStartAndPublication(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	createQueuedScanningConversation(t, db, "conversation-scan-cancel", "scan-cancel", "", now)
	if _, err := db.RequestDuplicateScanCancel(ctx, "scan-cancel", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.StartDuplicateScan(ctx, "scan-cancel", now.Add(2*time.Second)); !errors.Is(err, ErrDuplicateScanCancelled) {
		t.Fatalf("StartDuplicateScan() error = %v, want cancelled", err)
	}
	groups := []DuplicateScanResultGroup{{SizeBytes: 4, Files: []DuplicateScanResultFile{
		{Path: "a.txt", SizeBytes: 4, ModifiedAt: now},
		{Path: "b.txt", SizeBytes: 4, ModifiedAt: now},
	}}}
	if err := db.FinishDuplicateScan(ctx, "scan-cancel", string(DuplicateScanSucceeded), "", DuplicateScanProgress{Phase: "complete"}, groups, "扫描完成。", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	scan, err := db.DuplicateScan(ctx, "scan-cancel")
	if err != nil || scan.Status != DuplicateScanCancelled || scan.ErrorCode != "cancelled" || scan.GroupCount != 0 {
		t.Fatalf("cancelled scan = (%#v, %v)", scan, err)
	}
	if _, err := db.ListDuplicateScanGroups(ctx, "scan-cancel", 20, 0); !errors.Is(err, ErrDuplicateScanUnavailable) {
		t.Fatalf("cancelled scan groups error = %v", err)
	}
	task, err := db.Task(ctx, "scan-cancel")
	if err != nil || task.Status != TaskCancelled {
		t.Fatalf("cancelled scan task = (%#v, %v)", task, err)
	}
	conversation, err := db.Conversation(ctx, "conversation-scan-cancel")
	if err != nil || conversation.State != ConversationFailed {
		t.Fatalf("cancelled scan conversation = (%#v, %v)", conversation, err)
	}
}

func TestRecoverConversationAfterFollowUpClaimKeepsOldScanHistory(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	conversationID, oldScanID := "conversation-restart-follow-up", "scan-restart-follow-up-old"
	createScanningConversation(t, db, conversationID, oldScanID, "docs", now)
	if err := db.FinishDuplicateScan(ctx, oldScanID, string(DuplicateScanSucceeded), "", DuplicateScanProgress{Phase: "complete"}, nil, "首次扫描完成。", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.StartConversationTurn(ctx, conversationID, "再扫描一次", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	conversation, err := db.Conversation(ctx, conversationID)
	if err != nil || conversation.DuplicateScanID != "" {
		t.Fatalf("follow-up turn retained old scan association: (%#v, %v)", conversation, err)
	}
	if err := db.FinishConversationTurn(ctx, conversationID, ConversationAwaitingConfirmation, "请确认重新扫描。", AnalysisCard{
		Action: "find_duplicates", Goal: "再次查找重复文件", FilePath: "docs",
		Provider: "local", Model: "test", Destination: "local",
	}, "", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.BeginConversationAnalysis(ctx, conversationID, now.Add(4*time.Second)); err != nil || !claimed {
		t.Fatalf("claim follow-up conversation = (%t, %v)", claimed, err)
	}
	if err := db.RecoverConversations(ctx, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverDuplicateScans(ctx, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	conversation, err = db.Conversation(ctx, conversationID)
	if err != nil || conversation.State != ConversationFailed || conversation.Error != "service_restarted" || conversation.DuplicateScanID != "" {
		t.Fatalf("recovered follow-up = (%#v, %v)", conversation, err)
	}
	oldScan, err := db.DuplicateScan(ctx, oldScanID)
	if err != nil || oldScan.Status != DuplicateScanSucceeded {
		t.Fatalf("prior scan after recovery = (%#v, %v)", oldScan, err)
	}
}

func createScanningConversation(t *testing.T, db *Store, conversationID, scanID, scopePath string, at time.Time) {
	t.Helper()
	createQueuedScanningConversation(t, db, conversationID, scanID, scopePath, at)
	if err := db.StartDuplicateScan(context.Background(), scanID, at); err != nil {
		t.Fatal(err)
	}
}

func createQueuedScanningConversation(t *testing.T, db *Store, conversationID, scanID, scopePath string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateConversation(ctx, conversationID, "查找重复文件", at); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishConversationTurn(ctx, conversationID, ConversationAwaitingConfirmation, "确认后扫描。", AnalysisCard{
		Action: "find_duplicates", Goal: "找出重复文件", FilePath: scopePath,
		Provider: "local", Model: "test", Destination: "local",
	}, "", at); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.BeginConversationAnalysis(ctx, conversationID, at); err != nil || !claimed {
		t.Fatalf("claim conversation = (%t, %v)", claimed, err)
	}
	scan := DuplicateScan{ID: scanID, TaskID: scanID, ConversationID: conversationID, ScopePath: scopePath, Status: DuplicateScanQueued, Phase: "enumerating", CreatedAt: at, UpdatedAt: at}
	task := TaskRecord{ID: scanID, Kind: "duplicate_scan", Status: TaskQueued, RequestText: "找出重复文件", ScopePath: scopePath, CreatedAt: at, UpdatedAt: at}
	if err := db.CreateDuplicateScan(ctx, scan, task); err != nil {
		t.Fatal(err)
	}
}
