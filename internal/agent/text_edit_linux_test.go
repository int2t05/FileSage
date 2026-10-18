//go:build linux

// 本文件验证文本文件候选的暂存、审批、执行与恢复边界。
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

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func (m *scriptedConversationModel) completeTextEdit(ctx context.Context, messages []ChatMessage) (string, error) {
	return m.complete(ctx, messages)
}

type textEditFixture struct {
	metadata     *store.Store
	root         *fsroot.Root
	plans        *Service
	conversation *ConversationService
	model        *scriptedConversationModel
	stageDir     string
	databasePath string
	sourcePath   string
}

func newTextEditFixture(t *testing.T, source, candidate string) (*textEditFixture, store.PlanRecord) {
	t.Helper()
	ctx := context.Background()
	rootPath := t.TempDir()
	sourcePath := filepath.Join(rootPath, "notes.md")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	metadata, root, _ := openAgentTestServices(t, databasePath, rootPath)
	stageDir := filepath.Join(t.TempDir(), "agent-text-staging")
	plans, err := NewWithTextStaging(metadata, root, coordination.New(), stageDir)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"请确认内容编辑分析。","goal":"按要求整理正文","action":"edit_text","filePath":"notes.md","targetPath":""}`,
			`{"supported":true,"proposedContent":` + mustJSONString(t, candidate) + `}`,
		},
	}
	conversation := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	created, err := conversation.Create(ctx, "请按我的要求修改 notes.md")
	if err != nil || created.State != store.ConversationAwaitingConfirmation || created.Analysis == nil || created.Analysis.Action != "edit_text" {
		t.Fatalf("Create() = (%#v, %v)", created, err)
	}
	initialModelText := marshalMessages(t, model.requests[0])
	if strings.Contains(initialModelText, source) {
		t.Fatal("source text reached the model before confirmation")
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != source {
		t.Fatalf("source changed before confirmation: %q (%v)", got, err)
	}
	confirmed, err := conversation.ConfirmAnalysis(ctx, created.ID)
	if err != nil || confirmed.State != store.ConversationAwaitingApproval || confirmed.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", confirmed, err)
	}
	model.mu.Lock()
	confirmedRequest := append([]ChatMessage(nil), model.requests[1]...)
	model.mu.Unlock()
	var submitted struct {
		Content string `json:"content"`
	}
	if len(confirmedRequest) != 2 || json.Unmarshal([]byte(confirmedRequest[1].Content), &submitted) != nil || submitted.Content != source {
		t.Fatal("confirmed source text did not reach the model")
	}
	plan, err := plans.Plan(ctx, confirmed.PlanID)
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Action != "edit_text" {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	return &textEditFixture{metadata: metadata, root: root, plans: plans, conversation: conversation, model: model,
		stageDir: stageDir, databasePath: databasePath, sourcePath: sourcePath}, plan
}

func TestTextEditLifecycleStagesPreviewsRevisesAndExecutesOnlyAfterApproval(t *testing.T) {
	ctx := context.Background()
	const source = "original-private-sentinel\n"
	const generated = "model-candidate-private-sentinel\n"
	const revised = "human-reviewed-candidate\n"
	fixture, first := newTextEditFixture(t, source, generated)
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	operation := first.Operations[0]
	if operation.ProposedContentSize != int64(len(generated)) || operation.ProposedContentSHA256 == "" {
		t.Fatalf("staged metadata = (%d, %q)", operation.ProposedContentSize, operation.ProposedContentSHA256)
	}
	stageInfo, err := os.Stat(fixture.stageDir)
	if err != nil || stageInfo.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory mode = (%v, %v)", stageInfo, err)
	}
	stagePath := filepath.Join(fixture.stageDir, operation.ID)
	stageInfo, err = os.Stat(stagePath)
	if err != nil || stageInfo.Mode().Perm() != 0o600 {
		t.Fatalf("candidate mode = (%v, %v)", stageInfo, err)
	}
	preview, err := fixture.plans.PreviewTextEdit(ctx, first.ID, operation.ID, first.Version)
	if err != nil || preview.PlanID != first.ID || preview.OperationID != operation.ID || preview.Path != "notes.md" || preview.SourceContent != source || preview.ProposedContent != generated {
		t.Fatalf("PreviewTextEdit() = (%#v, %v)", preview, err)
	}
	if _, err := fixture.plans.PreviewTextEdit(ctx, first.ID, operation.ID, first.Version+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale preview error = %v", err)
	}
	encodedPlan, err := json.Marshal(first)
	if err != nil || strings.Contains(string(encodedPlan), generated) || strings.Contains(string(encodedPlan), source) {
		t.Fatalf("plan exposed content: %s (%v)", encodedPlan, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, source, generated); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.plans.Execute(ctx, first.ID, first.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved execution error = %v", err)
	}
	updated, err := fixture.plans.ReviseTextCandidate(ctx, first.ID, operation.ID, first.Version, []byte(revised))
	if err != nil || updated.Version != first.Version+1 || updated.Status != store.PlanAwaitingApproval || len(updated.Operations) != 1 {
		t.Fatalf("ReviseTextCandidate() = (%#v, %v)", updated, err)
	}
	newOperation := updated.Operations[0]
	if newOperation.ID == operation.ID || newOperation.Approval != store.ApprovalPending || newOperation.Status != store.OperationPending {
		t.Fatalf("revised operation did not reset approval: %#v", newOperation)
	}
	if _, err := os.Stat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded candidate remains: %v", err)
	}
	preview, err = fixture.plans.PreviewTextEdit(ctx, updated.ID, newOperation.ID, updated.Version)
	if err != nil || preview.SourceContent != source || preview.ProposedContent != revised {
		t.Fatalf("revised preview = (%#v, %v)", preview, err)
	}
	if _, err := fixture.plans.Decide(ctx, updated.ID, newOperation.ID, "approve", updated.Version); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(fixture.sourcePath); err != nil || string(got) != source {
		t.Fatalf("approval wrote source: %q (%v)", got, err)
	}
	completed, err := fixture.plans.Execute(ctx, updated.ID, updated.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if got, err := os.ReadFile(fixture.sourcePath); err != nil || string(got) != revised {
		t.Fatalf("executed content = %q (%v)", got, err)
	}
	if entries, err := os.ReadDir(fixture.stageDir); err != nil || len(entries) != 0 {
		t.Fatalf("terminal candidate files = (%v, %v)", entries, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, source, generated, revised); err != nil {
		t.Fatal(err)
	}
}

func TestTextEditRejectsHardLinksAndInvalidatesChangedSource(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "notes.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(rootPath, "notes.txt"), filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	metadata, root, _ := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test", Model: "model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"确认编辑","goal":"编辑","action":"edit_text","filePath":"notes.txt","targetPath":""}`},
	}
	conversation := &ConversationService{store: metadata, root: root, plans: New(metadata, root, coordination.New()), model: model}
	result, err := conversation.Create(ctx, "编辑 notes.txt")
	if err != nil || result.State != store.ConversationUnsupported || model.callCount() != 1 {
		t.Fatalf("hard-link conversation = (%#v, calls=%d, %v)", result, model.callCount(), err)
	}

	fixture, plan := newTextEditFixture(t, "original\n", "candidate\n")
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	operation := plan.Operations[0]
	if err := os.WriteFile(fixture.sourcePath, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := fixture.plans.Decide(ctx, plan.ID, operation.ID, "approve", plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
		t.Fatalf("stale source approval = (%#v, %v)", updated, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stageDir, operation.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalidated candidate remains: %v", err)
	}
}

func TestTextEditRecoveryNeverReplaysAndUsesPartialWhenUncertain(t *testing.T) {
	for _, mode := range []string{"unchanged", "published", "missing-stage"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			fixture, plan := newTextEditFixture(t, "before recovery\n", "after recovery\n")
			defer fixture.root.Close()
			defer fixture.metadata.Close()
			operation := plan.Operations[0]
			if _, err := fixture.plans.Decide(ctx, plan.ID, operation.ID, "approve", plan.Version); err != nil {
				t.Fatal(err)
			}
			if err := fixture.metadata.ClaimAgentPlanExecution(ctx, plan.ID, plan.Version, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := fixture.metadata.StartAgentOperation(ctx, plan.ID, operation.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "published":
				var expected fsroot.Fingerprint
				if err := json.Unmarshal([]byte(operation.SourceStateJSON), &expected); err != nil {
					t.Fatal(err)
				}
				if err := fixture.root.WriteAtomicIfUnchanged(operation.SourcePath, expected, []byte("after recovery\n"), expected.Mode.Perm()); err != nil {
					t.Fatal(err)
				}
			case "missing-stage":
				if err := os.Remove(filepath.Join(fixture.stageDir, operation.ID)); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.plans.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := fixture.plans.Plan(ctx, plan.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unchanged":
				got, readErr := os.ReadFile(fixture.sourcePath)
				if readErr != nil || string(got) != "before recovery\n" || recovered.Status != store.PlanApproved || recovered.Version != plan.Version+1 || recovered.Operations[0].Status != store.OperationPending {
					t.Fatalf("recovery replayed or misclassified operation: content=%q plan=%#v err=%v", got, recovered, readErr)
				}
				if _, err := os.Stat(filepath.Join(fixture.stageDir, operation.ID)); err != nil {
					t.Fatalf("retry candidate was not preserved: %v", err)
				}
			case "published":
				got, readErr := os.ReadFile(fixture.sourcePath)
				if readErr != nil || string(got) != "after recovery\n" || recovered.Status != store.PlanCompleted || recovered.Operations[0].Status != store.OperationSucceeded {
					t.Fatalf("published recovery = content %q plan %#v err %v", got, recovered, readErr)
				}
				if _, err := os.Stat(filepath.Join(fixture.stageDir, operation.ID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovered candidate remains: %v", err)
				}
			case "missing-stage":
				if recovered.Status != store.PlanFailed || recovered.Operations[0].Status != store.OperationPartial {
					t.Fatalf("missing-stage recovery = %#v", recovered)
				}
			}
		})
	}
}

type textFileWriteFixture struct {
	metadata     *store.Store
	root         *fsroot.Root
	plans        *Service
	conversation *ConversationService
	model        *scriptedConversationModel
	stageDir     string
	databasePath string
	filePath     string
}

func newTextFileWriteFixture(t *testing.T, action, relativePath, source, candidate string) (*textFileWriteFixture, store.PlanRecord) {
	t.Helper()
	ctx := context.Background()
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if action == "overwrite_file" {
		if err := os.WriteFile(filePath, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	metadata, root, plans := openAgentTestServices(t, databasePath, rootPath)
	stageDir := filepath.Join(t.TempDir(), "agent-text-staging")
	plans, err := NewWithTextStaging(metadata, root, coordination.New(), stageDir)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedConversationModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		replies: []string{
			`{"state":"ready","reply":"请确认文本文件操作。","goal":"按要求生成文本","action":"` + action + `","filePath":"` + relativePath + `","targetPath":""}`,
			`{"supported":true,"proposedContent":` + mustJSONString(t, candidate) + `}`,
		},
	}
	conversation := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	created, err := conversation.Create(ctx, "按要求处理文本文件")
	if err != nil || created.State != store.ConversationAwaitingConfirmation || created.Analysis == nil || created.Analysis.Action != action {
		t.Fatalf("Create() = (%#v, %v)", created, err)
	}
	if source != "" && textMessagesContain(model.requests[0], source) {
		t.Fatal("existing source text reached the model before confirmation")
	}
	confirmed, err := conversation.ConfirmAnalysis(ctx, created.ID)
	if err != nil || confirmed.State != store.ConversationAwaitingApproval || confirmed.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", confirmed, err)
	}
	model.mu.Lock()
	confirmedRequest := append([]ChatMessage(nil), model.requests[1]...)
	model.mu.Unlock()
	if action == "create_file" && source != "" && textMessagesContain(confirmedRequest, source) {
		t.Fatal("new-file generation unexpectedly received existing source text")
	}
	if action == "overwrite_file" && !textMessagesContain(confirmedRequest, source) {
		t.Fatal("overwrite generation did not receive confirmed source text")
	}
	plan, err := plans.Plan(ctx, confirmed.PlanID)
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Action != action {
		t.Fatalf("Plan() = (%#v, %v)", plan, err)
	}
	return &textFileWriteFixture{metadata: metadata, root: root, plans: plans, conversation: conversation, model: model,
		stageDir: stageDir, databasePath: databasePath, filePath: filePath}, plan
}

func textMessagesContain(messages []ChatMessage, content string) bool {
	encoded, err := json.Marshal(content)
	if err != nil {
		return false
	}
	for _, message := range messages {
		if strings.Contains(message.Content, content) || strings.Contains(message.Content, string(encoded)) {
			return true
		}
	}
	return false
}

func TestCreateFilePreviewEditAndExecuteAllowsEmptyText(t *testing.T) {
	ctx := context.Background()
	fixture, plan := newTextFileWriteFixture(t, "create_file", "notes/new.md", "", "")
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	operation := plan.Operations[0]
	if operation.SourcePath != "notes" || operation.TargetPath != "notes/new.md" || operation.ProposedContentSize != 0 || operation.ProposedContentSHA256 == "" {
		t.Fatalf("create-file operation = %#v", operation)
	}
	if _, err := os.Stat(fixture.filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file created before approval: %v", err)
	}
	preview, err := fixture.plans.PreviewTextEdit(ctx, plan.ID, operation.ID, plan.Version)
	if err != nil || preview.Path != "notes/new.md" || preview.SourceContent != "" || preview.ProposedContent != "" {
		t.Fatalf("PreviewTextEdit() = (%#v, %v)", preview, err)
	}
	updated, err := fixture.plans.ReviseTextCandidate(ctx, plan.ID, operation.ID, plan.Version, []byte("approved body\n"))
	if err != nil || updated.Version != plan.Version+1 || updated.Operations[0].Action != "create_file" {
		t.Fatalf("ReviseTextCandidate() = (%#v, %v)", updated, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, "approved body\n"); err != nil {
		t.Fatal(err)
	}
	newOperation := updated.Operations[0]
	preview, err = fixture.plans.PreviewTextEdit(ctx, updated.ID, newOperation.ID, updated.Version)
	if err != nil || preview.Path != "notes/new.md" || preview.ProposedContent != "approved body\n" {
		t.Fatalf("revised preview = (%#v, %v)", preview, err)
	}
	if _, err := fixture.plans.Decide(ctx, updated.ID, newOperation.ID, "approve", updated.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approval wrote file: %v", err)
	}
	completed, err := fixture.plans.Execute(ctx, updated.ID, updated.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if got, err := os.ReadFile(fixture.filePath); err != nil || string(got) != "approved body\n" {
		t.Fatalf("created file content = %q (%v)", got, err)
	}
	if entries, err := os.ReadDir(fixture.stageDir); err != nil || len(entries) != 0 {
		t.Fatalf("created-file candidate remains: (%v, %v)", entries, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, "approved body\n"); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Dir(fixture.filePath)); err != nil || len(entries) != 1 || entries[0].Name() != "new.md" {
		t.Fatalf("temporary write residue = (%v, %v)", entries, err)
	}
}

func TestOverwriteFileUsesSeparateActionAndCASWithoutKeepingOldContent(t *testing.T) {
	ctx := context.Background()
	const original = "private original body 9217\n"
	const candidate = "complete replacement body\n"
	fixture, plan := newTextFileWriteFixture(t, "overwrite_file", "notes.md", original, candidate)
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	operation := plan.Operations[0]
	if operation.SourcePath != "notes.md" || operation.TargetPath != "notes.md" || operation.Action != "overwrite_file" {
		t.Fatalf("overwrite-file operation = %#v", operation)
	}
	preview, err := fixture.plans.PreviewTextEdit(ctx, plan.ID, operation.ID, plan.Version)
	if err != nil || preview.Path != "notes.md" || preview.SourceContent != original || preview.ProposedContent != candidate {
		t.Fatalf("PreviewTextEdit() = (%#v, %v)", preview, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, original, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.plans.Decide(ctx, plan.ID, operation.ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(fixture.filePath); err != nil || string(got) != original {
		t.Fatalf("approval changed source: %q (%v)", got, err)
	}
	completed, err := fixture.plans.Execute(ctx, plan.ID, plan.Version)
	if err != nil || completed.Status != store.PlanCompleted || completed.Operations[0].Status != store.OperationSucceeded {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	if got, err := os.ReadFile(fixture.filePath); err != nil || string(got) != candidate {
		t.Fatalf("overwritten file content = %q (%v)", got, err)
	}
	if entries, err := os.ReadDir(fixture.stageDir); err != nil || len(entries) != 0 {
		t.Fatalf("overwrite candidate remains: (%v, %v)", entries, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, original, candidate); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Dir(fixture.filePath)); err != nil || len(entries) != 1 || entries[0].Name() != "notes.md" {
		t.Fatalf("overwrite left backup or temporary file: (%v, %v)", entries, err)
	}
}

func TestCreateFileCollisionAndOverwriteStaleVersionNeverWrite(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	const existing = "keep existing content\n"
	if err := os.WriteFile(filepath.Join(rootPath, "notes.txt"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	model := &scriptedConversationModel{
		status:  ModelStatus{Available: true, Provider: "test", Model: "model", Destination: "local"},
		replies: []string{`{"state":"ready","reply":"确认新建","goal":"新建文本","action":"create_file","filePath":"notes.txt","targetPath":""}`},
	}
	conversation := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	created, err := conversation.Create(ctx, "新建 notes.txt")
	if err != nil || created.State != store.ConversationClarifying || created.PlanID != "" || model.callCount() != 1 {
		t.Fatalf("occupied create-file request = (%#v, calls=%d, %v)", created, model.callCount(), err)
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "notes.txt")); err != nil || string(got) != existing {
		t.Fatalf("occupied create-file request changed source: %q (%v)", got, err)
	}

	fixture, plan := newTextFileWriteFixture(t, "overwrite_file", "notes.md", "before\n", "candidate\n")
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	op := plan.Operations[0]
	if _, err := fixture.plans.Decide(ctx, plan.ID, op.ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	const external = "external edit after approval\n"
	if err := os.WriteFile(fixture.filePath, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.plans.Execute(ctx, plan.ID, plan.Version)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale overwrite execution error = %v", err)
	}
	if got, err := os.ReadFile(fixture.filePath); err != nil || string(got) != external {
		t.Fatalf("stale overwrite replaced changed source: %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stageDir, op.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicted overwrite candidate remains: %v", err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, "candidate\n", "before\n"); err != nil {
		t.Fatal(err)
	}
}

func TestOverwriteFileRejectRemovesPrivateCandidate(t *testing.T) {
	ctx := context.Background()
	const original = "private-original-771\n"
	const candidate = "private-candidate-882\n"
	fixture, plan := newTextFileWriteFixture(t, "overwrite_file", "notes.md", original, candidate)
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	op := plan.Operations[0]
	stageInfo, err := os.Stat(fixture.stageDir)
	if err != nil || stageInfo.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory mode = (%v, %v)", stageInfo, err)
	}
	stageInfo, err = os.Stat(filepath.Join(fixture.stageDir, op.ID))
	if err != nil || stageInfo.Mode().Perm() != 0o600 {
		t.Fatalf("candidate mode = (%v, %v)", stageInfo, err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, original, candidate); err != nil {
		t.Fatal(err)
	}
	rejected, err := fixture.plans.Decide(ctx, plan.ID, op.ID, "reject", plan.Version)
	if err != nil || rejected.Status != store.PlanRejected {
		t.Fatalf("reject overwrite = (%#v, %v)", rejected, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stageDir, op.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected candidate remains: %v", err)
	}
	if err := assertTextEditContentAbsent(t, fixture.databasePath, original, candidate); err != nil {
		t.Fatal(err)
	}
}

func TestStopBeforeTextOperationStartRemovesCancelledCandidate(t *testing.T) {
	ctx := context.Background()
	fixture, plan := newTextFileWriteFixture(t, "create_file", "notes/new.md", "", "private-cancelled-candidate\n")
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	op := plan.Operations[0]
	if _, err := fixture.plans.Decide(ctx, plan.ID, op.ID, "approve", plan.Version); err != nil {
		t.Fatal(err)
	}
	approved, err := fixture.plans.Plan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.metadata.ClaimAgentPlanExecution(ctx, approved.ID, approved.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	executing, err := fixture.plans.Plan(ctx, approved.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := fixture.plans.StopExecution(ctx, executing.ID, executing.Version)
	if err != nil || stopped.Status != store.PlanCancelled || stopped.Operations[0].Status != store.OperationCancelled {
		t.Fatalf("StopExecution() = (%#v, %v)", stopped, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stageDir, op.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled candidate remains: %v", err)
	}
}

func TestRecoveryKeepsValidPendingCandidateAndInvalidatesMissingOne(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		ctx := context.Background()
		fixture, plan := newTextFileWriteFixture(t, "create_file", "notes/new.md", "", "pending-candidate-390\n")
		defer fixture.root.Close()
		defer fixture.metadata.Close()
		op := plan.Operations[0]
		orphanID := strings.Repeat("a", 32)
		if err := os.WriteFile(filepath.Join(fixture.stageDir, orphanID), []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fixture.plans.Recover(ctx); err != nil {
			t.Fatalf("Recover() = %v", err)
		}
		recovered, err := fixture.plans.Plan(ctx, plan.ID)
		if err != nil || recovered.Status != store.PlanAwaitingApproval {
			t.Fatalf("valid pending plan after recovery = (%#v, %v)", recovered, err)
		}
		preview, err := fixture.plans.PreviewTextEdit(ctx, plan.ID, op.ID, plan.Version)
		if err != nil || preview.ProposedContent != "pending-candidate-390\n" {
			t.Fatalf("pending preview after recovery = (%#v, %v)", preview, err)
		}
		if _, err := os.Stat(filepath.Join(fixture.stageDir, orphanID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unreferenced candidate remains: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		ctx := context.Background()
		fixture, plan := newTextFileWriteFixture(t, "overwrite_file", "notes.md", "source-before-restart\n", "candidate-before-restart\n")
		defer fixture.root.Close()
		defer fixture.metadata.Close()
		op := plan.Operations[0]
		if err := os.Remove(filepath.Join(fixture.stageDir, op.ID)); err != nil {
			t.Fatal(err)
		}
		if err := fixture.plans.Recover(ctx); err != nil {
			t.Fatalf("Recover() = %v", err)
		}
		recovered, err := fixture.plans.Plan(ctx, plan.ID)
		if err != nil || recovered.Status != store.PlanInvalidated || recovered.Operations[0].Status != store.OperationSkipped {
			t.Fatalf("missing-candidate plan after recovery = (%#v, %v)", recovered, err)
		}
		if _, err := fixture.plans.PreviewTextEdit(ctx, plan.ID, op.ID, plan.Version); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalidated candidate preview error = %v", err)
		}
	})
}

func TestTextEditInvalidStageCannotBeApproved(t *testing.T) {
	ctx := context.Background()
	fixture, plan := newTextEditFixture(t, "source\n", "candidate\n")
	defer fixture.root.Close()
	defer fixture.metadata.Close()
	operation := plan.Operations[0]
	if err := os.WriteFile(filepath.Join(fixture.stageDir, operation.ID), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := fixture.plans.Decide(ctx, plan.ID, operation.ID, "approve", plan.Version)
	if !errors.Is(err, ErrConflict) || updated.Status != store.PlanInvalidated {
		t.Fatalf("corrupt-stage approval = (%#v, %v)", updated, err)
	}
	if _, err := fixture.plans.PreviewTextEdit(ctx, plan.ID, operation.ID, plan.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("corrupt-stage preview error = %v", err)
	}
}

func assertTextEditContentAbsent(t *testing.T, databasePath string, forbidden ...string) error {
	t.Helper()
	files := []string{databasePath, databasePath + "-wal", databasePath + "-shm"}
	for _, filePath := range files {
		data, err := os.ReadFile(filePath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, value := range forbidden {
			if value != "" && strings.Contains(string(data), value) {
				return errors.New("text edit content was persisted in SQLite files")
			}
		}
	}
	return nil
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
