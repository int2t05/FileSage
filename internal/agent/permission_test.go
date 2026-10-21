// 本文件验证 Agent 计划审核、默认 bypass 和手动逐步执行。
package agent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/logging"
	"filesage/internal/store"
)

func permissionFixture(t *testing.T) (*store.Store, *fsroot.Root, *Service) {
	t.Helper()
	ctx := context.Background()
	metadata, err := store.Open(ctx, filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	root, err := fsroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return metadata, root, New(metadata, root, nil)
}

func TestStartPlanUsesPersistedDefaultBypassAfterReview(t *testing.T) {
	ctx := context.Background()
	_, root, service := permissionFixture(t)
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "创建归档目录", Action: "create_directory", SourcePath: "", TargetPath: "archive",
		Reason: "按请求建立归档目录",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPlan(ctx, plan.ID, plan.Version, "bypass", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-delete plan with irreversible-delete confirmation = %v, want conflict", err)
	}
	pending, err := service.store.Plan(ctx, plan.ID)
	if err != nil || pending.Status != store.PlanAwaitingApproval || pending.ReviewedAt != nil {
		t.Fatalf("mismatched confirmation changed plan: (%#v, %v)", pending, err)
	}
	if _, err := root.LstatEntry("archive"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("mismatched confirmation executed plan: %v", err)
	}
	completed, err := service.StartPlan(ctx, plan.ID, plan.Version, "bypass", false)
	if err != nil || completed.Status != store.PlanCompleted || completed.ExecutionMode != "bypass" || completed.ReviewedAt == nil {
		t.Fatalf("bypass start = (%#v, %v)", completed, err)
	}
	if entry, err := root.LstatEntry("archive"); err != nil || entry.Kind != fsroot.KindDirectory {
		t.Fatalf("reviewed bypass plan did not create its directory: (%#v, %v)", entry, err)
	}
}

func TestAgentToolStepLogsStatusWithoutUserPaths(t *testing.T) {
	ctx := logging.WithRequestID(context.Background(), "request-test-id")
	previousLogger := slog.Default()
	var output bytes.Buffer
	slog.SetDefault(slog.New(logging.NewRequestIDHandler(slog.NewTextHandler(&output, nil))))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	_, _, service := permissionFixture(t)
	const privatePath = "private-folder-token"
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "建立文件夹", Action: "create_directory", TargetPath: privatePath,
		Reason: "按用户请求建立文件夹",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := service.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.Execute(ctx, plan.ID, approved.Version)
	if err != nil || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}

	logs := output.String()
	for _, expected := range []string{"agent tool step started", "agent tool step completed", "request_id=request-test-id", "plan_id=", "operation_id=", "action=create_directory", "status=succeeded", "duration_ms="} {
		if !strings.Contains(logs, expected) {
			t.Errorf("tool logs %q do not include %q", logs, expected)
		}
	}
	if strings.Contains(logs, privatePath) {
		t.Fatalf("tool logs expose a managed path: %s", logs)
	}
}

func TestStartPlanRejectsBypassWhenPermissionIsDisabled(t *testing.T) {
	ctx := context.Background()
	metadata, root, service := permissionFixture(t)
	if err := metadata.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	plan, err := service.Propose(ctx, Proposal{
		RequestText: "创建待确认目录", Action: "create_directory", SourcePath: "", TargetPath: "must-stay-pending",
		Reason: "按请求建立目录",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartPlan(ctx, plan.ID, plan.Version, "bypass", false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("bypass with permission disabled = %v, want conflict", err)
	}
	current, err := metadata.Plan(ctx, plan.ID)
	if err != nil || current.Status != store.PlanAwaitingApproval || current.ReviewedAt != nil {
		t.Fatalf("disabled bypass changed plan review state: (%#v, %v)", current, err)
	}
	if _, err := root.LstatEntry("must-stay-pending"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("disabled bypass created its target: %v", err)
	}
}

func TestAgentToolCatalogRequiresExplicitBoundedCapabilities(t *testing.T) {
	tools := AgentToolCatalog()
	if len(tools) < 10 {
		t.Fatalf("tool catalog has %d capabilities", len(tools))
	}
	seen := make(map[string]bool)
	for _, tool := range tools {
		if tool.ID == "" || tool.Description == "" || tool.InputContract == "" || tool.ExecutionPolicy == "" || seen[tool.ID] {
			t.Fatalf("tool capability is incomplete or duplicated: %#v", tool)
		}
		seen[tool.ID] = true
	}
	if !seen["run_shell"] || !seen["rename"] || !seen["trash"] {
		t.Fatalf("catalog is missing bounded file or Shell tools: %#v", seen)
	}
}

func TestPlanOperationLimitsKeepOneGlobalBoundAndToolSpecificCaps(t *testing.T) {
	if got := maxProposalOperations([]Proposal{{Action: "create_directory"}}); got != store.MaxAgentPlanOperations {
		t.Fatalf("general proposal limit = %d, want %d", got, store.MaxAgentPlanOperations)
	}
	if got := maxProposalOperations([]Proposal{{Action: "create_archive"}}); got != 1 {
		t.Fatalf("archive proposal limit = %d, want 1", got)
	}
	if got := maxProposalOperations([]Proposal{{Action: "archive_photo", ConversationID: "conversation"}}); got != store.MaxAgentPlanOperations {
		t.Fatalf("conversation photo archive limit = %d, want %d", got, store.MaxAgentPlanOperations)
	}
	if got := maxStoredPlanOperations([]store.OperationRecord{{Action: "create_directory"}}); got != store.MaxAgentPlanOperations {
		t.Fatalf("stored general plan limit = %d, want %d", got, store.MaxAgentPlanOperations)
	}
}

func TestManualPlanRunsOnlyTheConfirmedNextStep(t *testing.T) {
	ctx := context.Background()
	metadata, root, service := permissionFixture(t)
	if err := metadata.SetAgentBypassPermission(ctx, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	plan, err := service.ProposeMany(ctx, []Proposal{
		{RequestText: "建立两个互不相关目录", Action: "create_directory", SourcePath: "", TargetPath: "first", Reason: "建立第一个目录"},
		{RequestText: "建立两个互不相关目录", Action: "create_directory", SourcePath: "", TargetPath: "second", Reason: "建立第二个目录"},
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := service.StartPlan(ctx, plan.ID, plan.Version, "manual", false)
	if err != nil || approved.Status != store.PlanApproved || approved.ExecutionMode != "manual" || approved.ReviewedAt == nil {
		t.Fatalf("manual plan review = (%#v, %v)", approved, err)
	}
	for _, path := range []string{"first", "second"} {
		if _, err := root.LstatEntry(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("manual plan changed %q before step confirmation: %v", path, err)
		}
	}
	if _, err := service.ExecuteStep(ctx, plan.ID, approved.Operations[1].ID, plan.Version); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("out-of-order step = %v, want conflict", err)
	}
	first, err := service.ExecuteStep(ctx, plan.ID, approved.Operations[0].ID, plan.Version)
	if err != nil || first.Status != store.PlanApproved || first.Operations[0].Status != store.OperationSucceeded || first.Operations[1].Approval != store.ApprovalPending {
		t.Fatalf("first manual step = (%#v, %v)", first, err)
	}
	if _, err := root.LstatEntry("second"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("manual plan ran an unconfirmed later step: %v", err)
	}
	if err := metadata.SetAgentBypassPermission(ctx, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	completed, err := service.ExecuteStep(ctx, plan.ID, approved.Operations[1].ID, plan.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.ExecutionMode != "manual" {
		t.Fatalf("manual step after preference change = (%#v, %v)", completed, err)
	}
	if _, err := root.LstatEntry("second"); err != nil {
		t.Fatalf("confirmed second step did not create its directory: %v", err)
	}
}
