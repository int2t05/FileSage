//go:build linux

// 本文件验证 Agent 软链接从确认到恢复的安全闭环。
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestSymlinkConversationRequiresApprovalAndCreatesAuditedLinks(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "shortcut"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()

	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"请确认创建软链接 shortcut (1)。","goal":"为常用文件建立快捷入口","operations":[{"action":"create_symlink","filePath":"source.txt","targetPath":"shortcut"}]}`},
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := conversationService.Create(ctx, "给文件和目录建立快捷入口")
	if err != nil || conversation.State != store.ConversationAwaitingConfirmation || conversation.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if got := conversation.Analysis.Operations[0].TargetPath; got != "shortcut (1)" {
		t.Fatalf("resolved collision target = %q, want shortcut (1)", got)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "shortcut (1)")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmation card created link early: %v", err)
	}

	conversation, err = conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	if plan.Operations[0].Action != "create_symlink" || plan.Operations[0].SourcePath != "source.txt" || plan.Operations[0].TargetPath != "shortcut (1)" {
		t.Fatalf("plan operation paths do not match confirmed card: %#v", plan.Operations)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "shortcut (1)")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan creation created link before approval: %v", err)
	}
	if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "shortcut (1)")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("individual approval executed link early: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "shortcut (1)")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approval executed link before explicit Execute: %v", err)
	}
	completed, err := plans.Execute(ctx, plan.ID)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	link, err := root.LstatEntry("shortcut (1)")
	if err != nil || !link.Symlink || link.LinkTarget != "source.txt" || link.LinkStatus != "internal" {
		t.Fatalf("link = (%#v, %v), want internal link to source.txt", link, err)
	}
	content, err := os.ReadFile(filepath.Join(rootPath, "shortcut"))
	if err != nil || string(content) != "preserve" {
		t.Fatalf("existing conflict was changed: %q (%v)", content, err)
	}
	audit, err := metadata.AuditForTaskPage(ctx, plan.TaskID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, record := range audit.Records {
		if record.EventType == "operation_finished" && record.Outcome == string(store.OperationSucceeded) {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("successful operation audit count = %d, records=%#v", finished, audit.Records)
	}
}

func TestSymlinkConversationSupportsDirectoryTargets(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "source-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	_, plan := createSymlinkPlanForTest(t, ctx, root, metadata, plans, "source-dir", "directory-shortcut")
	if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := plans.Execute(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	link, err := root.LstatEntry("directory-shortcut")
	if err != nil || !link.Symlink || link.Kind != "directory" || link.LinkTarget != "source-dir" || link.LinkStatus != "internal" {
		t.Fatalf("directory link = (%#v, %v)", link, err)
	}
}

func TestSymlinkRejectsStaleSourceParentAndTarget(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		change func(string) error
		stage  string
	}{
		{name: "source before approval", stage: "approval", change: func(root string) error {
			return os.WriteFile(filepath.Join(root, "source.txt"), []byte("changed"), 0o600)
		}},
		{name: "parent before approval", stage: "approval", change: func(root string) error {
			if err := os.Rename(filepath.Join(root, "parent"), filepath.Join(root, "old-parent")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(root, "parent"), 0o700)
		}},
		{name: "target after approval", stage: "execution", change: func(root string) error {
			return os.WriteFile(filepath.Join(root, "parent", "link"), []byte("preserve"), 0o600)
		}},
		{name: "source after approval", stage: "execution", change: func(root string) error {
			return os.WriteFile(filepath.Join(root, "source.txt"), []byte("changed"), 0o600)
		}},
		{name: "parent after approval", stage: "execution", change: func(root string) error {
			if err := os.Rename(filepath.Join(root, "parent"), filepath.Join(root, "old-parent")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(root, "parent"), 0o700)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.Mkdir(filepath.Join(rootPath, "parent"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			_, plan := createSymlinkPlanForTest(t, ctx, root, metadata, plans, "source.txt", "parent/link")
			if test.stage == "approval" {
				if err := test.change(rootPath); err != nil {
					t.Fatal(err)
				}
				if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); !errors.Is(err, ErrConflict) {
					t.Fatalf("stale Decide() error = %v, want conflict", err)
				}
				invalidated, err := plans.Plan(ctx, plan.ID)
				if err != nil || invalidated.Status != store.PlanInvalidated {
					t.Fatalf("stale approval plan = (%#v, %v)", invalidated, err)
				}
				if _, err := os.Lstat(filepath.Join(rootPath, "parent", "link")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale approval created link: %v", err)
				}
				return
			}
			if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
				t.Fatal(err)
			}
			if err := test.change(rootPath); err != nil {
				t.Fatal(err)
			}
			updated, err := plans.Execute(ctx, plan.ID)
			if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
				t.Fatalf("stale Execute() = (%#v, %v), want invalidated conflict", updated, err)
			}
			if test.name == "target after approval" {
				content, readErr := os.ReadFile(filepath.Join(rootPath, "parent", "link"))
				if readErr != nil || string(content) != "preserve" {
					t.Fatalf("conflicting target changed: %q (%v)", content, readErr)
				}
			} else if _, err := os.Lstat(filepath.Join(rootPath, "parent", "link")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale snapshot created link: %v", err)
			}
		})
	}
}

func TestSymlinkConversationRejectsExistingLinksAndRecursiveTargets(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		sourcePath string
		targetPath string
		prepare    func(string) error
	}{
		{name: "symlink source", sourcePath: "source-link", targetPath: "new-link", prepare: func(root string) error {
			if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source"), 0o600); err != nil {
				return err
			}
			return os.Symlink("source.txt", filepath.Join(root, "source-link"))
		}},
		{name: "symlink parent", sourcePath: "source.txt", targetPath: "linked-parent/new-link", prepare: func(root string) error {
			if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source"), 0o600); err != nil {
				return err
			}
			if err := os.Mkdir(filepath.Join(root, "real-parent"), 0o700); err != nil {
				return err
			}
			return os.Symlink("real-parent", filepath.Join(root, "linked-parent"))
		}},
		{name: "recursive directory link", sourcePath: "folder", targetPath: "folder/nested/link", prepare: func(root string) error {
			return os.MkdirAll(filepath.Join(root, "folder", "nested"), 0o700)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := test.prepare(rootPath); err != nil {
				t.Fatal(err)
			}
			metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			conversation := createSymlinkConversationForTest(t, ctx, root, metadata, plans, test.sourcePath, test.targetPath)
			if conversation.State != store.ConversationUnsupported || conversation.Analysis != nil || conversation.PlanID != "" {
				t.Fatalf("unsafe symlink conversation = %#v", conversation)
			}
		})
	}
}

func TestSymlinkRecoveryVerifiesExistingLinkAndNeverReplaysAutomatically(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		createLink bool
		wrongLink  bool
		wantStatus store.OperationStatus
	}{
		{name: "verified link", createLink: true, wantStatus: store.OperationSucceeded},
		{name: "absent link requires explicit retry", wantStatus: store.OperationPending},
		{name: "wrong link requires manual review", createLink: true, wrongLink: true, wantStatus: store.OperationPartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, "other.txt"), []byte("other"), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			_, plan := createSymlinkPlanForTest(t, ctx, root, metadata, plans, "source.txt", "link")
			if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve"); err != nil {
				t.Fatal(err)
			}
			if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			linkPath := filepath.Join(rootPath, "link")
			if test.createLink {
				linkTarget := "source.txt"
				if test.wrongLink {
					linkTarget = "other.txt"
				}
				if err := os.Symlink(linkTarget, linkPath); err != nil {
					t.Fatal(err)
				}
			}
			if err := plans.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := plans.Plan(ctx, plan.ID)
			if err != nil || recovered.Operations[0].Status != test.wantStatus {
				t.Fatalf("recovered plan = (%#v, %v), want %q", recovered, err, test.wantStatus)
			}
			if test.createLink {
				target, err := os.Readlink(linkPath)
				if err != nil || target != map[bool]string{true: "other.txt", false: "source.txt"}[test.wrongLink] {
					t.Fatalf("recovery changed the existing link: target=%q, err=%v", target, err)
				}
			} else {
				if _, err := os.Lstat(linkPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovery created link automatically: %v", err)
				}
				if _, err := plans.Execute(ctx, plan.ID); err != nil {
					t.Fatalf("explicit retry Execute() = %v", err)
				}
				if target, err := os.Readlink(linkPath); err != nil || target != "source.txt" {
					t.Fatalf("explicit retry did not create approved link: target=%q, err=%v", target, err)
				}
			}
		})
	}
}

func TestSymlinkMalformedLegacyResponseIsRejectedWithoutPanic(t *testing.T) {
	legacy := conversationTurnResult{
		State: "ready", Reply: "请确认", Goal: "建立快捷入口", Action: "create_symlink",
		FilePath: "source.txt", TargetPath: "link",
	}
	if err := validateTurnResult(&legacy); !errors.Is(err, ErrModelResponse) {
		t.Fatalf("validateTurnResult(legacy symlink response) = %v, want invalid model response", err)
	}
	multiple := conversationTurnResult{
		State: "ready", Reply: "请确认", Goal: "建立入口", Operations: []store.AnalysisOperation{
			{Action: "create_symlink", FilePath: "source.txt", TargetPath: "link-one"},
			{Action: "create_symlink", FilePath: "source.txt", TargetPath: "link-two"},
		},
	}
	if err := validateTurnResult(&multiple); !errors.Is(err, ErrModelResponse) {
		t.Fatalf("validateTurnResult(multiple symlinks) = %v, want invalid model response", err)
	}
	service := &ConversationService{}
	card := store.AnalysisCard{Operations: append([]store.AnalysisOperation(nil), multiple.Operations...)}
	state, reply := service.validateOperationsCard(context.Background(), &card, "请确认")
	if state != store.ConversationUnsupported || len(card.Operations) != 2 || card.Operations[0].TargetPath != "link-one" || card.Operations[1].TargetPath != "link-two" {
		t.Fatalf("multi-symlink card was partially accepted: state=%q reply=%q ops=%#v", state, reply, card.Operations)
	}
}

func createSymlinkConversationForTest(t *testing.T, ctx context.Context, root *fsroot.Root, metadata *store.Store, plans *Service, sourcePath, targetPath string) store.AgentConversation {
	t.Helper()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"请确认建立软链接。","goal":"建立快捷入口","operations":[{"action":"create_symlink","filePath":"` + sourcePath + `","targetPath":"` + targetPath + `"}]}`},
	}
	service := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	conversation, err := service.Create(ctx, "建立快捷入口")
	if err != nil {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	return conversation
}

func createSymlinkPlanForTest(t *testing.T, ctx context.Context, root *fsroot.Root, metadata *store.Store, plans *Service, sourcePath, targetPath string) (store.AgentConversation, store.PlanRecord) {
	t.Helper()
	conversation := createSymlinkConversationForTest(t, ctx, root, metadata, plans, sourcePath, targetPath)
	if conversation.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("symlink confirmation = %#v", conversation)
	}
	conversationService := &ConversationService{store: metadata, root: root, plans: plans}
	conversation, err := conversationService.ConfirmAnalysis(ctx, conversation.ID)
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", conversation, err)
	}
	plan, err := plans.Plan(ctx, conversation.PlanID)
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	return conversation, plan
}
