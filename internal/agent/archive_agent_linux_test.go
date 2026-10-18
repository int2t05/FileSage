//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/store"
)

func TestArchiveConversationApprovalAndExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "unpacked"), 0o755); err != nil {
		t.Fatal(err)
	}
	private := "archive body stays local"
	if err := os.WriteFile(filepath.Join(rootPath, "docs", "readme.txt"), []byte(private), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test", Model: "offline", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"将 docs 打包为 docs.zip，请确认。","goal":"将资料打包","operations":[{"action":"create_archive","filePath":"docs","targetPath":"docs.zip"}]}`,
			`{"state":"ready","reply":"将 docs.zip 解压到 unpacked，请确认。","goal":"在本地展开资料","operations":[{"action":"extract_archive","filePath":"docs.zip","targetPath":"unpacked"}]}`,
			`{"state":"ready","reply":"将 docs.zip 解压到管理根目录，请确认。","goal":"在根目录展开资料","operations":[{"action":"extract_archive","filePath":"docs.zip","targetPath":""}]}`,
		},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}

	created, err := conversationService.Create(ctx, "把 docs 打包为 docs.zip")
	if err != nil || created.State != store.ConversationAwaitingConfirmation || created.Analysis == nil {
		t.Fatalf("create archive conversation = (%#v, %v)", created, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "docs.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conversation created archive before confirmation: %v", err)
	}
	created, err = conversationService.ConfirmAnalysis(ctx, created.ID)
	if err != nil || created.State != store.ConversationAwaitingApproval || created.PlanID == "" {
		t.Fatalf("confirm archive conversation = (%#v, %v)", created, err)
	}
	createPlan, err := plans.Plan(ctx, created.PlanID)
	if err != nil || len(createPlan.Operations) != 1 || createPlan.Operations[0].Action != "create_archive" {
		t.Fatalf("create archive plan = (%#v, %v)", createPlan, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "docs.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan creation wrote archive before approval: %v", err)
	}
	createApproved, err := plans.Decide(ctx, createPlan.ID, createPlan.Operations[0].ID, "approve", createPlan.Version)
	if err != nil {
		t.Fatal(err)
	}
	createDone, err := plans.Execute(ctx, createPlan.ID, createApproved.Version)
	if err != nil || createDone.Status != store.PlanCompleted {
		t.Fatalf("create archive execution = (%#v, %v)", createDone, err)
	}

	extracted, err := conversationService.Create(ctx, "把 docs.zip 解压到 unpacked")
	if err != nil || extracted.State != store.ConversationAwaitingConfirmation || extracted.Analysis == nil {
		t.Fatalf("extract archive conversation = (%#v, %v)", extracted, err)
	}
	extracted, err = conversationService.ConfirmAnalysis(ctx, extracted.ID)
	if err != nil || extracted.State != store.ConversationAwaitingApproval || extracted.PlanID == "" {
		t.Fatalf("confirm extract conversation = (%#v, %v)", extracted, err)
	}
	extractPlan, err := plans.Plan(ctx, extracted.PlanID)
	if err != nil || len(extractPlan.Operations) != 1 || extractPlan.Operations[0].Action != "extract_archive" {
		t.Fatalf("extract plan = (%#v, %v)", extractPlan, err)
	}
	var target archiveTargetState
	if err := json.Unmarshal([]byte(extractPlan.Operations[0].TargetStateJSON), &target); err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"unpacked/docs", "unpacked/docs/readme.txt"}
	if !sameAgentPaths(target.PlannedPaths, wantPaths) {
		t.Fatalf("approved output paths = %#v, want %#v", target.PlannedPaths, wantPaths)
	}
	extractApproved, err := plans.Decide(ctx, extractPlan.ID, extractPlan.Operations[0].ID, "approve", extractPlan.Version)
	if err != nil {
		t.Fatal(err)
	}
	extractDone, err := plans.Execute(ctx, extractPlan.ID, extractApproved.Version)
	if err != nil || extractDone.Status != store.PlanCompleted {
		t.Fatalf("extract execution = (%#v, %v)", extractDone, err)
	}
	content, err := os.ReadFile(filepath.Join(rootPath, "unpacked", "docs", "readme.txt"))
	if err != nil || string(content) != private {
		t.Fatalf("extracted content = %q (%v)", content, err)
	}
	if err := os.RemoveAll(filepath.Join(rootPath, "docs")); err != nil {
		t.Fatal(err)
	}
	rootExtracted, err := conversationService.Create(ctx, "把 docs.zip 解压到管理根目录")
	if err != nil || rootExtracted.State != store.ConversationAwaitingConfirmation || rootExtracted.Analysis == nil {
		t.Fatalf("root extraction conversation = (%#v, %v)", rootExtracted, err)
	}
	rootExtracted, err = conversationService.ConfirmAnalysis(ctx, rootExtracted.ID)
	if err != nil || rootExtracted.State != store.ConversationAwaitingApproval || rootExtracted.PlanID == "" {
		t.Fatalf("confirm root extraction = (%#v, %v)", rootExtracted, err)
	}
	rootPlan, err := plans.Plan(ctx, rootExtracted.PlanID)
	if err != nil || len(rootPlan.Operations) != 1 || rootPlan.Operations[0].TargetPath != "" {
		t.Fatalf("root extraction plan = (%#v, %v)", rootPlan, err)
	}
	rootApproved, err := plans.Decide(ctx, rootPlan.ID, rootPlan.Operations[0].ID, "approve", rootPlan.Version)
	if err != nil {
		t.Fatal(err)
	}
	rootDone, err := plans.Execute(ctx, rootPlan.ID, rootApproved.Version)
	if err != nil || rootDone.Status != store.PlanCompleted {
		t.Fatalf("root extraction execution = (%#v, %v)", rootDone, err)
	}
	rootContent, err := os.ReadFile(filepath.Join(rootPath, "docs", "readme.txt"))
	if err != nil || string(rootContent) != private {
		t.Fatalf("root extraction content = %q (%v)", rootContent, err)
	}

	for _, plan := range []store.PlanRecord{createDone, extractDone, rootDone} {
		page, auditErr := metadata.AuditForTaskPage(ctx, plan.TaskID, 50, 0)
		if auditErr != nil {
			t.Fatal(auditErr)
		}
		events := make(map[string]bool)
		for _, record := range page.Records {
			events[record.EventType] = true
		}
		for _, required := range []string{"plan_created", "operation_approved", "plan_execution_confirmed", "operation_started", "operation_finished"} {
			if !events[required] {
				t.Fatalf("archive audit missing %q: %#v", required, page.Records)
			}
		}
	}
	model.mu.Lock()
	modelPayload, _ := json.Marshal(model.requests)
	model.mu.Unlock()
	if strings.Contains(string(modelPayload), private) {
		t.Fatal("archive body was sent to the conversation model")
	}
}

func TestArchiveConflictInvalidatesAndRecoveryDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "a.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeAgentArchive(t, ctx, service, "source", "source.zip")
	approved, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.zip"), []byte("keep existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Execute(ctx, plan.ID, approved.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated || updated.Operations[0].Status != store.OperationSkipped {
		t.Fatalf("target conflict = (%#v, %v), want invalidated plan", updated, err)
	}
	if content, readErr := os.ReadFile(filepath.Join(rootPath, "source.zip")); readErr != nil || string(content) != "keep existing" {
		t.Fatalf("occupied target changed: %q (%v)", content, readErr)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "source (1).zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive conflict was dynamically renamed: %v", err)
	}

	recovery := proposeAgentArchive(t, ctx, service, "source", "recovery.zip")
	recoveryApproved, err := service.Decide(ctx, recovery.ID, recovery.Operations[0].ID, "approve", recovery.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, recovery.ID, recoveryApproved.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, recovery.ID, recovery.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Plan(ctx, recovery.ID)
	if err != nil || recovered.Status != store.PlanApproved || recovered.Operations[0].Status != store.OperationPending {
		t.Fatalf("empty-output recovery = (%#v, %v), want manual retry readiness", recovered, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "recovery.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup recovery replayed archive creation: %v", err)
	}
}

func TestArchiveRecoveryMarksPublishedOutputPartial(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan := proposeAgentArchive(t, ctx, service, "source.txt", "uncertain.zip")
	approved, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, approved.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "uncertain.zip"), []byte("published before crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Plan(ctx, plan.ID)
	if err != nil || recovered.Status != store.PlanFailed || recovered.Operations[0].Status != store.OperationPartial {
		t.Fatalf("partial recovery = (%#v, %v), want partial result", recovered, err)
	}
	content, err := os.ReadFile(filepath.Join(rootPath, "uncertain.zip"))
	if err != nil || string(content) != "published before crash" {
		t.Fatalf("recovery modified published target: %q (%v)", content, err)
	}
}

// TestArchiveSnapshotIgnoresTimeLocationIdentity 验证持久化时间按时刻而非时区指针比较。
func TestArchiveSnapshotIgnoresTimeLocationIdentity(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "a.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	proposal := Proposal{Action: "create_archive", SourcePath: "source", TargetPath: "source.zip"}
	_, source, target, err := service.snapshotArchiveOperation(ctx, proposal, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	source.Fingerprint.ModifiedAt = source.Fingerprint.ModifiedAt.In(time.FixedZone("same-offset", 3600))
	source.Fingerprint.ChangeTime = source.Fingerprint.ChangeTime.In(time.FixedZone("same-offset", 3600))
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	sourceJSONText, targetJSONText := string(sourceJSON), string(targetJSON)
	if _, _, _, err := service.snapshotArchiveOperation(ctx, proposal, &sourceJSONText, &targetJSONText, false); err != nil {
		t.Fatalf("equivalent stored snapshot rejected: %v", err)
	}
}

func proposeAgentArchive(t *testing.T, ctx context.Context, service *Service, sourcePath, targetPath string) store.PlanRecord {
	t.Helper()
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "创建本地压缩包", Action: "create_archive", SourcePath: sourcePath,
		TargetPath: targetPath, Reason: "按用户确认路径创建压缩包",
	})
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Action != "create_archive" {
		t.Fatalf("Propose(create_archive) = (%#v, %v)", plan, err)
	}
	return plan
}
