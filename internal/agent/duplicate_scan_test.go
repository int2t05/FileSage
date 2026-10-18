// 本文件验证重复扫描在排队取消时正确结束。
package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestQueuedDuplicateScanCancellationIsNotReportedAsFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	conversationID, scanID := "conversation-queued-cancel", "scan-queued-cancel"
	if err := metadata.CreateConversation(ctx, conversationID, "查找重复文件", now); err != nil {
		t.Fatal(err)
	}
	if err := metadata.FinishConversationTurn(ctx, conversationID, store.ConversationAwaitingConfirmation, "确认后扫描。", store.AnalysisCard{
		Action: "find_duplicates", Goal: "查找重复文件", Provider: "local", Model: "test", Destination: "local",
	}, "", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := metadata.BeginConversationAnalysis(ctx, conversationID, now); err != nil || !claimed {
		t.Fatalf("claim conversation = (%t, %v)", claimed, err)
	}
	scan := store.DuplicateScan{
		ID: scanID, TaskID: scanID, ConversationID: conversationID,
		Status: store.DuplicateScanQueued, Phase: "enumerating", CreatedAt: now, UpdatedAt: now,
	}
	task := store.TaskRecord{
		ID: scanID, Kind: "duplicate_scan", Status: store.TaskQueued,
		RequestText: "查找重复文件", CreatedAt: now, UpdatedAt: now,
	}
	if err := metadata.CreateDuplicateScan(ctx, scan, task); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.RequestDuplicateScanCancel(ctx, scanID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	jobCtx, cancel := context.WithCancel(ctx)
	cancel()
	manager := &duplicateScanManager{store: metadata, root: root}
	manager.wg.Add(1)
	manager.run(jobCtx, scanID, "", &duplicateScanJob{cancel: func() {}})

	status, err := metadata.DuplicateScan(ctx, scanID)
	if err != nil || status.Status != store.DuplicateScanCancelled || status.ErrorCode != "cancelled" {
		t.Fatalf("queued scan = (%#v, %v), want cancelled", status, err)
	}
	task, err = metadata.Task(ctx, scanID)
	if err != nil || task.Status != store.TaskCancelled {
		t.Fatalf("queued scan task = (%#v, %v), want cancelled", task, err)
	}
	conversation, err := metadata.Conversation(ctx, conversationID)
	if err != nil || conversation.State != store.ConversationFailed {
		t.Fatalf("queued scan conversation = (%#v, %v), want failed terminal state", conversation, err)
	}
	if _, err := metadata.ListDuplicateScanGroups(ctx, scanID, 20, 0); !errors.Is(err, store.ErrDuplicateScanUnavailable) {
		t.Fatalf("queued scan groups error = %v, want unavailable", err)
	}
}
