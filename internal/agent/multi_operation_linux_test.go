//go:build linux

// 本文件验证多项方案审批、执行停止和重启恢复边界。
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/store"
)

func TestStopBeforeOperationStartsPreventsFilesystemChange(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	plan := proposeDirectories(t, ctx, service, "parent", "one", "two")
	approveAllOperations(t, ctx, service, plan)
	first := plan.Operations[0]
	blockedKeys := coordination.PathKeys(first.SourcePath, first.TargetPath)
	releaseBlocker, err := service.locks.Lock(ctx, blockedKeys...)
	if err != nil {
		t.Fatal(err)
	}
	type executeResult struct {
		plan store.PlanRecord
		err  error
	}
	done := make(chan executeResult, 1)
	go func() {
		updated, executeErr := service.Execute(ctx, plan.ID, plan.Version)
		done <- executeResult{plan: updated, err: executeErr}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		current, readErr := service.Plan(ctx, plan.ID)
		if readErr != nil {
			releaseBlocker()
			t.Fatal(readErr)
		}
		if current.Status == store.PlanExecuting {
			break
		}
		if time.Now().After(deadline) {
			releaseBlocker()
			t.Fatal("execution did not claim the plan")
		}
		time.Sleep(time.Millisecond)
	}
	stopped, err := service.StopExecution(ctx, plan.ID, plan.Version)
	if err != nil || stopped.Status != store.PlanCancelled || stopped.Operations[0].Status != store.OperationCancelled {
		releaseBlocker()
		t.Fatalf("stop before operation start = (%#v, %v)", stopped, err)
	}
	releaseBlocker()
	result := <-done
	if result.err != nil || result.plan.Status != store.PlanCancelled {
		t.Fatalf("execute after stop = (%#v, %v)", result.plan, result.err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := os.Stat(filepath.Join(rootPath, "parent", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stopped operation created %q: %v", name, err)
		}
	}
}

func TestMultiOperationApprovalsAndExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectories(t, ctx, service, "parent", "one", "two")
	if plan.Status != store.PlanAwaitingApproval || len(plan.Operations) != 2 || plan.Operations[0].Ordinal != 0 || plan.Operations[1].Ordinal != 1 {
		t.Fatalf("initial plan = %#v", plan)
	}
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale approval error = %v, want conflict", err)
	}
	first, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil || first.Status != store.PlanAwaitingApproval || first.Operations[0].Approval != store.ApprovalApproved || first.Operations[1].Approval != store.ApprovalPending {
		t.Fatalf("first approval = (%#v, %v)", first, err)
	}
	approved, err := service.Decide(ctx, plan.ID, plan.Operations[1].ID, "approve", plan.Version)
	if err != nil || approved.Status != store.PlanApproved {
		t.Fatalf("second approval = (%#v, %v)", approved, err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := os.Stat(filepath.Join(rootPath, "parent", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("approval created %q before execution: %v", name, err)
		}
	}
	if _, err := service.Execute(ctx, plan.ID, plan.Version+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale execution error = %v, want conflict", err)
	}
	completed, err := service.Execute(ctx, plan.ID, plan.Version)
	if err != nil || completed.Status != store.PlanCompleted || len(completed.Operations) != 2 {
		t.Fatalf("execution = (%#v, %v)", completed, err)
	}
	for _, operation := range completed.Operations {
		if operation.Status != store.OperationSucceeded {
			t.Fatalf("operation %q status = %q", operation.ID, operation.Status)
		}
		if info, err := os.Stat(filepath.Join(rootPath, filepath.FromSlash(operation.TargetPath))); err != nil || !info.IsDir() {
			t.Fatalf("target %q = (%v, %v)", operation.TargetPath, info, err)
		}
	}
}

func TestMultiOperationPlanSupportsMoreThanLegacyLimit(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	names := make([]string, 21)
	for index := range names {
		names[index] = fmt.Sprintf("category-%02d", index)
	}
	plan := proposeDirectories(t, ctx, service, "parent", names...)
	if len(plan.Operations) != len(names) || plan.Status != store.PlanAwaitingApproval {
		t.Fatalf("plan with %d independent steps = (%d, %s)", len(names), len(plan.Operations), plan.Status)
	}
	approveAllOperations(t, ctx, service, plan)
	completed, err := service.Execute(ctx, plan.ID, plan.Version)
	if err != nil || completed.Status != store.PlanCompleted || len(completed.Operations) != len(names) {
		t.Fatalf("execute %d-step plan = (%d operations, %s, %v)", len(names), len(completed.Operations), completed.Status, err)
	}
}

func TestMultiOperationPlanExecutesMixedSupportedActions(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "source.md"), []byte("keep this content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secure.md"), []byte("permissions only"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	mode := uint32(0o600)
	plan, err := service.ProposeMany(ctx, []Proposal{
		{RequestText: "建立目录、移动文件并修改权限", Action: "create_directory", SourcePath: "parent", TargetPath: "parent/new", Reason: "按用户确认新建目录"},
		{RequestText: "建立目录、移动文件并修改权限", Action: "move_file", SourcePath: "parent/source.md", TargetPath: "parent/moved.md", Reason: "按用户确认移动文件"},
		{RequestText: "建立目录、移动文件并修改权限", Action: "set_permissions", SourcePath: "parent/secure.md", TargetPath: "parent/secure.md", Mode: &mode, Reason: "按用户确认设置权限"},
	})
	if err != nil || len(plan.Operations) != 3 {
		t.Fatalf("ProposeMany() = (%#v, %v)", plan, err)
	}
	approveAllOperations(t, ctx, service, plan)
	completed, err := service.Execute(ctx, plan.ID, plan.Version)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "new")); err != nil {
		t.Fatalf("directory operation failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "source.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("move left the source in place: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(parent, "moved.md"))
	if err != nil || string(content) != "keep this content" {
		t.Fatalf("moved file content = %q (%v)", content, err)
	}
	info, err := os.Stat(filepath.Join(parent, "secure.md"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata operation mode = (%v, %v)", info, err)
	}
}

func TestMultiOperationRejectionTerminatesWholePlan(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectories(t, ctx, service, "parent", "one", "two")
	if _, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	rejected, err := service.Decide(ctx, plan.ID, plan.Operations[1].ID, "reject", plan.Version)
	if err != nil || rejected.Status != store.PlanRejected || rejected.Operations[1].Approval != store.ApprovalRejected || rejected.Operations[0].Status != store.OperationCancelled {
		t.Fatalf("rejected plan = (%#v, %v)", rejected, err)
	}
	if _, err := service.Execute(ctx, plan.ID, plan.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejected plan execution error = %v, want conflict", err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := os.Stat(filepath.Join(rootPath, "parent", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejection created %q: %v", name, err)
		}
	}
}

func TestMultiOperationExecutionStopsAtConflict(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectories(t, ctx, service, "parent", "one", "two", "three")
	approveAllOperations(t, ctx, service, plan)
	if err := os.Mkdir(filepath.Join(rootPath, "parent", "two"), 0o700); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Execute(ctx, plan.ID, plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
		t.Fatalf("execution with changed second target = (%#v, %v)", updated, err)
	}
	if updated.Operations[0].Status != store.OperationSucceeded || updated.Operations[1].Status != store.OperationSkipped || updated.Operations[2].Status != store.OperationCancelled {
		t.Fatalf("fail-stop operation states = %#v", updated.Operations)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "one")); err != nil {
		t.Fatalf("first operation did not remain successful: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "three")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later operation ran after the conflict: %v", err)
	}
}

func TestMultiOperationRecoveryRequiresConfirmationAndDoesNotReplaySuccess(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectories(t, ctx, service, "parent", "one", "two")
	approveAllOperations(t, ctx, service, plan)
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first := plan.Operations[0]
	if err := metadata.StartAgentOperation(ctx, plan.ID, first.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.createDirectory(first); err != nil {
		t.Fatal(err)
	}
	if err := metadata.FinishAgentOperation(ctx, plan.ID, first.ID, store.OperationSucceeded, "目录已创建", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Plan(ctx, plan.ID)
	if err != nil || recovered.Status != store.PlanApproved || recovered.Version != plan.Version+1 || recovered.Operations[0].Status != store.OperationSucceeded || recovered.Operations[1].Status != store.OperationPending {
		t.Fatalf("recovered plan = (%#v, %v)", recovered, err)
	}
	if _, err := service.Execute(ctx, plan.ID, plan.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("pre-restart plan version remained executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "two")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery automatically ran the remaining operation: %v", err)
	}
	completed, err := service.Execute(ctx, plan.ID, recovered.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded || completed.Operations[1].Status != store.OperationSucceeded {
		t.Fatalf("confirmed recovery execution = (%#v, %v)", completed, err)
	}
}

func TestMultiOperationRetryRecoveryBumpsVersion(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	plan := proposeDirectories(t, ctx, service, "parent", "one", "two")
	approveAllOperations(t, ctx, service, plan)
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Plan(ctx, plan.ID)
	if err != nil || recovered.Status != store.PlanApproved || recovered.Version != plan.Version+1 || recovered.Operations[0].Status != store.OperationPending {
		t.Fatalf("retry recovery = (%#v, %v)", recovered, err)
	}
	if _, err := service.Execute(ctx, plan.ID, plan.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("pre-restart plan version remained executable after retry recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "parent", "one")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale version executed after recovery: %v", err)
	}
	completed, err := service.Execute(ctx, plan.ID, recovered.Version)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("new plan version did not continue: (%#v, %v)", completed, err)
	}
}

func TestFailedAndPartialFinishAreAtomicAcrossRestart(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation store.OperationStatus
		task      store.TaskStatus
	}{
		{name: "failed", operation: store.OperationFailed, task: store.TaskFailed},
		{name: "partial", operation: store.OperationPartial, task: store.TaskPartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			rootPath := t.TempDir()
			if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
			metadata, root, service := openAgentTestServices(t, databasePath, rootPath)
			defer func() {
				if root != nil {
					_ = root.Close()
				}
				if metadata != nil {
					_ = metadata.Close()
				}
			}()

			plan := proposeDirectories(t, ctx, service, "parent", "one", "two")
			approveAllOperations(t, ctx, service, plan)
			if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := metadata.FinishAgentOperation(ctx, plan.ID, plan.Operations[0].ID, test.operation, "首项执行未完成", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			finished, err := service.Plan(ctx, plan.ID)
			if err != nil || finished.Status != store.PlanFailed || finished.Operations[0].Status != test.operation || finished.Operations[1].Status != store.OperationCancelled {
				t.Fatalf("atomic %s finish = (%#v, %v)", test.name, finished, err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			root = nil
			if err := metadata.Close(); err != nil {
				t.Fatal(err)
			}
			metadata = nil
			metadata, root, service = openAgentTestServices(t, databasePath, rootPath)
			if err := service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := service.Plan(ctx, plan.ID)
			if err != nil || recovered.Status != store.PlanFailed || recovered.Operations[0].Status != test.operation || recovered.Operations[1].Status != store.OperationCancelled {
				t.Fatalf("%s finish after restart = (%#v, %v)", test.name, recovered, err)
			}
			tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Kind: "agent", Limit: 10})
			if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != test.task {
				t.Fatalf("%s task summary after restart = (%#v, %v)", test.name, tasks.Tasks, err)
			}
			running, err := metadata.RunningAgentPlans(ctx)
			if err != nil || len(running) != 0 {
				t.Fatalf("failed plan became runnable after restart: (%#v, %v)", running, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, "parent", "two")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restart executed cancelled operation: %v", err)
			}
		})
	}
}

func TestMultiOperationPreflightRejectsDependencyWithoutCreatingPlan(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, root, service := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	_, err := service.ProposeMany(ctx, []Proposal{
		{RequestText: "建立目录", Action: "create_directory", SourcePath: "parent", TargetPath: "parent/a", Reason: "第一项"},
		{RequestText: "建立目录", Action: "create_directory", SourcePath: "parent/a", TargetPath: "parent/a/child", Reason: "依赖第一项"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("dependent operations error = %v, want conflict", err)
	}
	page, err := metadata.ListTasks(ctx, store.TaskListQuery{Kind: "agent", Limit: 20})
	if err != nil || len(page.Tasks) != 0 {
		t.Fatalf("partial task persisted: tasks=%#v err=%v", page.Tasks, err)
	}
}

func proposeDirectories(t *testing.T, ctx context.Context, service *Service, parent string, names ...string) store.PlanRecord {
	t.Helper()
	proposals := make([]Proposal, 0, len(names))
	for _, name := range names {
		proposals = append(proposals, Proposal{
			RequestText: "按类别建立目录", Action: "create_directory", SourcePath: parent,
			TargetPath: filepath.ToSlash(filepath.Join(parent, name)), Reason: "按用户确认的新建目录",
		})
	}
	plan, err := service.ProposeMany(ctx, proposals)
	if err != nil {
		t.Fatalf("ProposeMany() error = %v", err)
	}
	return plan
}

func approveAllOperations(t *testing.T, ctx context.Context, service *Service, plan store.PlanRecord) {
	t.Helper()
	for _, operation := range plan.Operations {
		if _, err := service.Decide(ctx, plan.ID, operation.ID, "approve", plan.Version); err != nil {
			t.Fatalf("approve operation %q: %v", operation.ID, err)
		}
	}
}
