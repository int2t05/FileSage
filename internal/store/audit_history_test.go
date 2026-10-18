// 本文件验证人工审计约束、并发完成与游标稳定性。
package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openAuditHistoryTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBeginManualAuditFixesActorAndPendingOutcome(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	createdAt := time.Date(2026, 10, 16, 1, 2, 3, 0, time.UTC)
	id, err := db.BeginManualAudit(context.Background(), AuditRecord{
		EventType: "manual_rename", Actor: "agent", Outcome: "succeeded",
		Message: "rename requested", SourcePath: "draft.txt", TargetPath: "final.txt",
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("AuditHistoryPage() = (%#v, %v)", page, err)
	}
	got := page.Events[0]
	if got.ID != id || got.Actor != "user" || got.Outcome != "pending" || got.EventType != "manual_rename" {
		t.Fatalf("manual audit identity/state = %#v", got)
	}
	if got.TaskID != "" || got.PlanID != "" || got.OperationID != "" {
		t.Fatalf("manual audit has an association: %#v", got)
	}
	if got.Message != "rename requested" || got.SourcePath != "draft.txt" || got.TargetPath != "final.txt" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("manual audit metadata = %#v", got)
	}
}

func TestBeginManualAuditRequiresManualEventAndNoAssociations(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	base := AuditRecord{EventType: "manual_copy"}
	cases := []struct {
		name   string
		mutate func(*AuditRecord)
	}{
		{name: "event type", mutate: func(record *AuditRecord) { record.EventType = "copy" }},
		{name: "task ID", mutate: func(record *AuditRecord) { record.TaskID = "task" }},
		{name: "plan ID", mutate: func(record *AuditRecord) { record.PlanID = "plan" }},
		{name: "operation ID", mutate: func(record *AuditRecord) { record.OperationID = "operation" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record := base
			test.mutate(&record)
			if _, err := db.BeginManualAudit(context.Background(), record); err == nil {
				t.Fatal("BeginManualAudit() accepted an invalid record")
			}
		})
	}
	page, err := db.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("invalid records were persisted: (%#v, %v)", page, err)
	}
}

func TestFinishManualAuditAllowsOnlyOneTerminalUpdate(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	id, err := db.BeginManualAudit(context.Background(), AuditRecord{EventType: "manual_move"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishManualAudit(context.Background(), id, "succeeded", "moved"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishManualAudit(context.Background(), id, "failed", "late update"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second finish error = %v, want ErrConflict", err)
	}
	page, err := db.AuditHistoryPage(context.Background(), 0, 10)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("AuditHistoryPage() = (%#v, %v)", page, err)
	}
	if page.Events[0].Outcome != "succeeded" || page.Events[0].Message != "moved" {
		t.Fatalf("finished event = %#v", page.Events[0])
	}
}

func TestFinishManualAuditAcceptsDefinedTerminalOutcomes(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	for _, outcome := range []string{"succeeded", "failed", "partial", "needs_review"} {
		t.Run(outcome, func(t *testing.T) {
			id, err := db.BeginManualAudit(context.Background(), AuditRecord{EventType: "manual_action"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.FinishManualAudit(context.Background(), id, outcome, "finished"); err != nil {
				t.Fatalf("FinishManualAudit() error = %v", err)
			}
		})
	}
}

func TestConcurrentFinishManualAuditHasSingleWinner(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	id, err := db.BeginManualAudit(context.Background(), AuditRecord{EventType: "manual_delete"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, outcome := range []string{"succeeded", "partial"} {
		wait.Add(1)
		go func(outcome string) {
			defer wait.Done()
			<-start
			errs <- db.FinishManualAudit(context.Background(), id, outcome, outcome)
		}(outcome)
	}
	close(start)
	wait.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent finish error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("finish results: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestFinishManualAuditValidatesOutcomeAndRecordState(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	ctx := context.Background()
	if err := db.PutTask(ctx, TaskRecord{ID: "task", Kind: "organize", Status: TaskQueued}); err != nil {
		t.Fatal(err)
	}
	manualID, err := db.BeginManualAudit(ctx, AuditRecord{EventType: "manual_rename"})
	if err != nil {
		t.Fatal(err)
	}
	otherIDs := make([]int64, 0, 2)
	for _, record := range []AuditRecord{
		{EventType: "rename", Actor: "user", Outcome: "pending"},
		{EventType: "manual_rename", Actor: "system", Outcome: "pending"},
		{EventType: "manual_rename", Actor: "user", Outcome: "pending", TaskID: "task"},
	} {
		id, err := db.AppendAudit(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		otherIDs = append(otherIDs, id)
	}
	for _, outcome := range []string{"", "pending", "unknown"} {
		if err := db.FinishManualAudit(ctx, manualID, outcome, "ignored"); err == nil {
			t.Fatalf("FinishManualAudit() accepted outcome %q", outcome)
		}
	}
	if err := db.FinishManualAudit(ctx, 999999, "succeeded", "missing"); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing record error = %v, want ErrConflict", err)
	}
	for _, id := range otherIDs {
		if err := db.FinishManualAudit(ctx, id, "succeeded", "must remain pending"); !errors.Is(err, ErrConflict) {
			t.Fatalf("ineligible record %d error = %v, want ErrConflict", id, err)
		}
	}
	page, err := db.AuditHistoryPage(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.ID == manualID && event.Outcome != "pending" {
			t.Fatalf("invalid outcome changed manual event: %#v", event)
		}
		for _, id := range otherIDs {
			if event.ID == id && event.Outcome != "pending" {
				t.Fatalf("ineligible event was changed: %#v", event)
			}
		}
	}
}

func TestAuditHistoryPageUsesStableDescendingIDCursor(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	ctx := context.Background()
	if err := db.PutTask(ctx, TaskRecord{ID: "task", Kind: "organize", Status: TaskQueued}); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 10, 16, 2, 3, 4, 0, time.UTC)
	ids := make([]int64, 0, 4)
	for index := 1; index <= 4; index++ {
		record := AuditRecord{
			EventType: "manual_action", Actor: "user", Outcome: "succeeded",
			Message: "result", SourcePath: "source.txt", TargetPath: "target.txt", CreatedAt: createdAt,
		}
		if index == 2 {
			record.TaskID = "task"
			record.EventType = "agent_action"
			record.Actor = "agent"
		}
		id, err := db.AppendAudit(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	first, err := db.AuditHistoryPage(ctx, 0, 2)
	if err != nil || len(first.Events) != 2 || !first.HasMore || first.Limit != 2 {
		t.Fatalf("first page = (%#v, %v)", first, err)
	}
	if first.Events[0].ID != ids[3] || first.Events[1].ID != ids[2] || first.NextCursor != ids[2] {
		t.Fatalf("first page order/cursor = %#v", first)
	}
	// 后续新增记录应留在当前游标之前，不得挤入下一页。
	newID, err := db.AppendAudit(ctx, AuditRecord{EventType: "manual_later", Actor: "user", Outcome: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.AuditHistoryPage(ctx, first.NextCursor, 2)
	if err != nil || len(second.Events) != 2 || second.HasMore || second.NextCursor != 0 {
		t.Fatalf("second page = (%#v, %v)", second, err)
	}
	if second.Events[0].ID != ids[1] || second.Events[1].ID != ids[0] || second.Events[0].ID == newID {
		t.Fatalf("second page order = %#v, new ID=%d", second, newID)
	}
	associated := second.Events[0]
	if associated.TaskID != "task" || associated.EventType != "agent_action" || associated.Actor != "agent" || associated.Outcome != "succeeded" {
		t.Fatalf("associated audit fields were not loaded: %#v", associated)
	}
	if associated.Message != "result" || associated.SourcePath != "source.txt" || associated.TargetPath != "target.txt" || !associated.CreatedAt.Equal(createdAt) {
		t.Fatalf("audit metadata was not loaded: %#v", associated)
	}
}

func TestAuditHistoryPageRejectsInvalidCursorAndLimit(t *testing.T) {
	db := openAuditHistoryTestStore(t)
	for _, query := range []struct {
		beforeID int64
		limit    int
	}{
		{beforeID: -1, limit: 1},
		{beforeID: 0, limit: 0},
		{beforeID: 0, limit: 101},
	} {
		if _, err := db.AuditHistoryPage(context.Background(), query.beforeID, query.limit); err == nil {
			t.Fatalf("AuditHistoryPage(%d, %d) succeeded", query.beforeID, query.limit)
		}
	}
}
