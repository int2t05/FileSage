//go:build linux

// 本文件验证 Shell 整份计划拒绝与暂存清理。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/shellstage"
	"filesage/internal/store"
)

// TestRejectShellPlanRemovesStagedChangesWithoutSandbox 验证整份拒绝不运行 Shell 且清理暂存。
func TestRejectShellPlanRemovesStagedChangesWithoutSandbox(t *testing.T) {
	fixture := newShellRejectFixture(t)
	ctx := fixture.ctx
	service := fixture.service
	metadata := fixture.metadata
	plan := fixture.plan
	changes, err := service.ShellChanges(ctx, plan.ID, store.MaxShellChangePageSize, 0)
	if err != nil || changes.Total != 1 || changes.Changes[0].PayloadID == "" {
		t.Fatalf("ShellChanges() = (%#v, %v)", changes, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.shellDir, changes.Changes[0].PayloadID)); err != nil {
		t.Fatalf("staged payload is unavailable: %v", err)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version); !errors.Is(err, ErrInvalid) {
		t.Fatalf("per-step Shell approval = %v, want invalid", err)
	}

	rejected, err := service.RejectPlan(ctx, plan.ID, plan.Version)
	if err != nil || rejected.Status != store.PlanRejected || rejected.Operations[0].Approval != store.ApprovalRejected || rejected.Operations[0].Status != store.OperationSkipped {
		t.Fatalf("RejectPlan() = (%#v, %v)", rejected, err)
	}
	run, err := service.ShellRun(ctx, plan.ID)
	if err != nil || run.Status != store.ShellRunRejected {
		t.Fatalf("ShellRun() = (%#v, %v), want rejected", run, err)
	}
	changes, err = service.ShellChanges(ctx, plan.ID, store.MaxShellChangePageSize, 0)
	if err != nil || changes.Total != 1 || changes.Changes[0].Approval != store.ApprovalRejected ||
		changes.Changes[0].Status != store.OperationCancelled || changes.Changes[0].ApprovedBy != "" || changes.Changes[0].ApprovedAt != nil {
		t.Fatalf("rejected Shell changes = (%#v, %v)", changes, err)
	}
	if payloads, err := os.ReadDir(fixture.shellDir); err != nil || len(payloads) != 0 {
		t.Fatalf("rejected payload directory = (%v, %v), want empty", payloads, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.rootPath, "staged.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejection published staged file: %v", err)
	}
	task, err := metadata.Task(ctx, plan.TaskID)
	if err != nil || task.Status != store.TaskCancelled {
		t.Fatalf("Task() = (%#v, %v), want cancelled", task, err)
	}
}

// TestRejectedShellCleanupSurvivesCancelledRequest 验证事务提交后断连仍清理暂存并返回状态。
func TestRejectedShellCleanupSurvivesCancelledRequest(t *testing.T) {
	fixture := newShellRejectFixture(t)
	if err := fixture.metadata.RejectShellPlan(fixture.ctx, fixture.plan.ID, fixture.plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(fixture.ctx)
	cancel()

	plan, err := fixture.service.finishRejectedShellPlan(cancelled, fixture.plan.ID)
	if err != nil || plan.Status != store.PlanRejected {
		t.Fatalf("finishRejectedShellPlan() = (%#v, %v), want rejected without error", plan, err)
	}
	if payloads, err := os.ReadDir(fixture.shellDir); err != nil || len(payloads) != 0 {
		t.Fatalf("rejected payload directory = (%v, %v), want empty", payloads, err)
	}
}

type shellRejectFixture struct {
	ctx      context.Context
	metadata *store.Store
	service  *Service
	rootPath string
	shellDir string
	plan     store.PlanRecord
}

// newShellRejectFixture 构造带一个暂存文件的待审 Shell 计划。
func newShellRejectFixture(t *testing.T) shellRejectFixture {
	t.Helper()
	ctx := context.Background()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	rootPath := t.TempDir()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := root.ConfigureMoveRecoveryDirectory(filepath.Join(t.TempDir(), "move-recovery")); err != nil {
		t.Fatal(err)
	}
	shellDir := filepath.Join(t.TempDir(), "shell")
	service, err := NewWithStaging(metadata, root, coordination.New(), filepath.Join(t.TempDir(), "text"), shellDir)
	if err != nil {
		t.Fatal(err)
	}

	const conversationID = "0123456789abcdef0123456789abcdef"
	const request = "修改说明并新建验收文件"
	const command = "printf staged > staged.txt"
	now := time.Now().UTC()
	if err := metadata.CreateConversation(ctx, conversationID, request, now); err != nil {
		t.Fatal(err)
	}
	card := store.AnalysisCard{
		Goal: "修改说明并新建验收文件", Provider: "test-provider", Model: "test-model", Destination: "local",
		Operations: []store.AnalysisOperation{{Action: "run_shell", ShellCommand: command, WorkingDir: "."}},
	}
	if err := metadata.FinishConversationTurn(ctx, conversationID, store.ConversationAwaitingConfirmation, "请确认隔离命令。", card, "", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := metadata.BeginConversationAnalysis(ctx, conversationID, now, false); err != nil || !claimed {
		t.Fatalf("BeginConversationAnalysis() = (%t, %v)", claimed, err)
	}

	workspace := t.TempDir()
	content := []byte("staged")
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	stageRoot, err := service.shellPayloads.newRunDirectory()
	if err != nil {
		t.Fatal(err)
	}
	proposal := ShellProposal{
		RequestText: request, Command: command, WorkingDirectory: ".", ConversationID: conversationID,
		ModelProvider: "test-provider", ModelName: "test-model",
	}
	plan, err := service.saveShellPlan(ctx, proposal, shellstage.Result{
		ExitCode: 0,
		Changes: []shellstage.Change{{
			Path: "staged.txt", Operation: shellstage.Added,
			After: &shellstage.FileState{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), Mode: 0o600},
		}},
	}, nil, workspace, stageRoot)
	if err != nil || plan.Status != store.PlanAwaitingApproval {
		t.Fatalf("saveShellPlan() = (%#v, %v)", plan, err)
	}
	return shellRejectFixture{ctx: ctx, metadata: metadata, service: service, rootPath: rootPath, shellDir: shellDir, plan: plan}
}
