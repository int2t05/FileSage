//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"filesage/internal/documenttext"
	"filesage/internal/fsroot"
	"filesage/internal/office"
	"filesage/internal/store"
	"filesage/internal/testutil/imagepdf"
)

type classificationScriptModel struct {
	mu                   sync.Mutex
	status               ModelStatus
	intent               string
	classification       string
	requests             [][]ChatMessage
	classificationCalls  int
	duringClassification func()
}

func (m *classificationScriptModel) Status() ModelStatus { return m.status }

func (m *classificationScriptModel) complete(_ context.Context, messages []ChatMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, append([]ChatMessage(nil), messages...))
	return m.intent, nil
}

func (m *classificationScriptModel) completeClassification(_ context.Context, messages []ChatMessage) (string, error) {
	m.mu.Lock()
	m.requests = append(m.requests, append([]ChatMessage(nil), messages...))
	m.classificationCalls++
	callback := m.duringClassification
	m.mu.Unlock()
	if callback != nil {
		callback()
	}
	return m.classification, nil
}

func (m *classificationScriptModel) requestsSnapshot() ([][]ChatMessage, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := make([][]ChatMessage, len(m.requests))
	for index := range m.requests {
		requests[index] = append([]ChatMessage(nil), m.requests[index]...)
	}
	return requests, m.classificationCalls
}

func classifyIntent() string {
	return `{"state":"ready","reply":"我会检查来源目录中的文本和类别目录，请确认后分析正文。","goal":"按正文主题归类文档","operations":[{"action":"classify_documents","filePath":"","targetPath":"categories"}]}`
}

func newClassificationTestService(t *testing.T, classification string) (*ConversationService, *Service, *store.Store, *fsroot.Root, string, string, *classificationScriptModel) {
	t.Helper()
	rootPath := t.TempDir()
	for _, directory := range []string{"source/a", "source/b", "source/c", "categories/Personal", "categories/Work"} {
		if err := os.MkdirAll(filepath.Join(rootPath, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"source/a/note.md":            "CLASSIFY_SENTINEL_A confidential project alpha",
		"source/b/note.md":            "CLASSIFY_SENTINEL_B confidential project beta",
		"source/c/skip.txt":           "CLASSIFY_SENTINEL_SKIP uncertain draft",
		"categories/category-root.md": "CATEGORY_ROOT_SENTINEL must be excluded",
		"categories/Work/existing.md": "already classified",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(t.TempDir(), "filesage.sqlite")
	metadata, root, plans := openAgentTestServices(t, databasePath, rootPath)
	t.Cleanup(func() {
		_ = root.Close()
		_ = metadata.Close()
	})
	model := &classificationScriptModel{
		status: ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
		intent: classifyIntent(), classification: classification,
	}
	conversation := &ConversationService{store: metadata, root: root, plans: plans, model: model}
	return conversation, plans, metadata, root, rootPath, databasePath, model
}

func TestClassifyDocumentsConfirmationApprovalAndExecution(t *testing.T) {
	ctx := context.Background()
	classification := `{"items":[{"fileId":2,"categoryId":-1,"reasonCode":"other"},{"fileId":1,"categoryId":1,"reasonCode":"topic"},{"fileId":0,"categoryId":1,"reasonCode":"topic"}]}`
	conversationService, plans, metadata, root, rootPath, databasePath, model := newClassificationTestService(t, classification)
	created, err := conversationService.Create(ctx, "把来源里的文档按内容放进类别目录")
	if err != nil || created.State != store.ConversationAwaitingConfirmation || created.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", created, err)
	}
	card := created.Analysis
	if card.Action != "classify_documents" || len(card.Operations) != 1 || len(card.Operations[0].Files) != 3 || len(card.Operations[0].Categories) != 2 {
		t.Fatalf("classification card = %#v", card)
	}
	files := card.Operations[0].Files
	if files[0].Path != "source/a/note.md" || files[1].Path != "source/b/note.md" || files[2].Path != "source/c/skip.txt" || files[0].Version == "" {
		t.Fatalf("candidate paths are not stable or include category subtree: %#v", files)
	}
	if card.Operations[0].Categories[0] != (store.AnalysisCategory{ID: 0, Path: "categories/Personal"}) ||
		card.Operations[0].Categories[1] != (store.AnalysisCategory{ID: 1, Path: "categories/Work"}) {
		t.Fatalf("categories are not sorted and numbered: %#v", card.Operations[0].Categories)
	}
	requests, calls := model.requestsSnapshot()
	if calls != 0 || strings.Contains(marshalMessages(t, requests[0]), "CLASSIFY_SENTINEL") {
		t.Fatal("document body reached the model before confirmation")
	}

	confirmed, err := conversationService.ConfirmAnalysis(ctx, created.ID)
	if err != nil || confirmed.State != store.ConversationAwaitingApproval || confirmed.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", confirmed, err)
	}
	requests, calls = model.requestsSnapshot()
	if calls != 1 {
		t.Fatalf("classification model calls = %d, want 1", calls)
	}
	analysisRequest := marshalMessages(t, requests[1])
	for _, marker := range []string{"CLASSIFY_SENTINEL_A", "CLASSIFY_SENTINEL_B", "CLASSIFY_SENTINEL_SKIP"} {
		if !strings.Contains(analysisRequest, marker) {
			t.Errorf("confirmed request omitted %q", marker)
		}
	}
	if strings.Contains(analysisRequest, "CATEGORY_ROOT_SENTINEL") {
		t.Fatal("category-root subtree was included in the source scan")
	}
	plan, err := plans.Plan(ctx, confirmed.PlanID)
	if err != nil || len(plan.Operations) != 2 || plan.DataScopeJSON != `{"proposalSource":"model-analysis"}` {
		t.Fatalf("classification plan = (%#v, %v)", plan, err)
	}
	if plan.Operations[0].SourcePath != "source/a/note.md" || plan.Operations[0].TargetPath != "categories/Work/note.md" ||
		plan.Operations[1].SourcePath != "source/b/note.md" || plan.Operations[1].TargetPath != "categories/Work/note (1).md" {
		t.Fatalf("classification targets are not stable and non-overwriting: %#v", plan.Operations)
	}
	for _, operation := range plan.Operations {
		if operation.Action != "move_file" || operation.Evidence != "" || operation.Reason != "根据正文主题分类" {
			t.Fatalf("classification operation contains unsupported evidence: %#v", operation)
		}
		var targetState fsroot.Fingerprint
		if err := json.Unmarshal([]byte(operation.TargetStateJSON), &targetState); err != nil || targetState.Kind != fsroot.KindDirectory {
			t.Fatalf("target parent state = (%#v, %v)", targetState, err)
		}
	}
	stored, err := json.Marshal(struct {
		Conversation store.AgentConversation
		Plan         store.PlanRecord
	}{confirmed, plan})
	if err != nil || strings.Contains(string(stored), "CLASSIFY_SENTINEL") {
		t.Fatalf("body appeared in persisted conversation or plan: err=%v", err)
	}
	audit, err := metadata.AuditForTaskPage(ctx, plan.TaskID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	auditBytes, _ := json.Marshal(audit)
	if strings.Contains(string(auditBytes), "CLASSIFY_SENTINEL") {
		t.Fatal("body appeared in audit evidence")
	}
	if repeated, err := conversationService.ConfirmAnalysis(ctx, created.ID); err != nil || repeated.PlanID != plan.ID {
		t.Fatalf("repeated confirmation = (%#v, %v)", repeated, err)
	}
	if _, calls = model.requestsSnapshot(); calls != 1 {
		t.Fatalf("repeated confirmation called the model again: %d", calls)
	}
	first, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := plans.Decide(ctx, plan.ID, plan.Operations[1].ID, "approve", first.Version)
	if err != nil || approved.Status != store.PlanApproved {
		t.Fatalf("per-item approval = (%#v, %v)", approved, err)
	}
	if _, err := plans.Execute(ctx, plan.ID, approved.Version); err != nil {
		t.Fatalf("second execution confirmation failed: %v", err)
	}
	for _, target := range []string{"categories/Work/note.md", "categories/Work/note (1).md"} {
		reader, _, err := root.OpenReadNoFollow(target)
		if err != nil {
			t.Errorf("approved target %q cannot be opened: %v", target, err)
		} else {
			_ = reader.Close()
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, "categories", "Work", "note.md")); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	databaseBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(databaseBytes), "CLASSIFY_SENTINEL") {
		t.Fatal("document body appeared in SQLite")
	}
}

func TestClassifyDocumentsConfirmationCreatesPlan(t *testing.T) {
	ctx := context.Background()
	classification := `{"items":[{"fileId":0,"categoryId":1,"reasonCode":"topic"},{"fileId":1,"categoryId":-1,"reasonCode":"other"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`
	conversationService, plans, _, _, _, _, model := newClassificationTestService(t, classification)
	created, err := conversationService.Create(ctx, "按正文主题整理文件")
	if err != nil || created.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", created, err)
	}
	requests, calls := model.requestsSnapshot()
	if calls != 0 || strings.Contains(marshalMessages(t, requests[0]), "CLASSIFY_SENTINEL") {
		t.Fatal("document body reached the model before confirmation")
	}
	confirmed, err := conversationService.ConfirmAnalysis(ctx, created.ID, true)
	if err != nil || confirmed.State != store.ConversationAwaitingApproval || confirmed.Analysis != nil || confirmed.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", confirmed, err)
	}
	plan, err := plans.Plan(ctx, confirmed.PlanID)
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Action != "move_file" {
		t.Fatalf("classification plan = (%#v, %v)", plan, err)
	}
	requests, calls = model.requestsSnapshot()
	if calls != 1 || !strings.Contains(marshalMessages(t, requests[1]), "CLASSIFY_SENTINEL_A") {
		t.Fatalf("confirmed classification request = %d calls, body missing", calls)
	}
}

// TestClassifyDocumentsLimitsOCRAndConcurrentAnalysis 验证批量和并发上限在读取前生效。
func TestClassifyDocumentsLimitsOCRAndConcurrentAnalysis(t *testing.T) {
	ctx := context.Background()
	classification := `{"items":[{"fileId":0,"categoryId":0,"reasonCode":"topic"},{"fileId":1,"categoryId":0,"reasonCode":"topic"},{"fileId":2,"categoryId":0,"reasonCode":"topic"},{"fileId":3,"categoryId":0,"reasonCode":"topic"},{"fileId":4,"categoryId":0,"reasonCode":"topic"},{"fileId":5,"categoryId":0,"reasonCode":"topic"}]}`
	service, _, _, _, rootPath, _, model := newClassificationTestService(t, classification)
	for index := 0; index < 3; index++ {
		name := filepath.Join(rootPath, "source", "scan-"+strconv.Itoa(index)+".pdf")
		if err := os.WriteFile(name, []byte("%PDF-1.4"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	created, err := service.Create(ctx, "按正文整理文件")
	if err != nil {
		t.Fatal(err)
	}
	limited, err := service.ConfirmAnalysis(ctx, created.ID, true)
	if !errors.Is(err, ErrUnsupported) || limited.State != store.ConversationFailed {
		t.Fatalf("OCR batch limit = (%#v, %v), want a failed bounded analysis", limited, err)
	}
	if _, calls := model.requestsSnapshot(); calls != 0 {
		t.Fatal("OCR batch limit was checked after sending document content to the model")
	}

	for range cap(classificationAnalysisSlots) {
		classificationAnalysisSlots <- struct{}{}
	}
	defer func() {
		for range cap(classificationAnalysisSlots) {
			<-classificationAnalysisSlots
		}
	}()
	busyService, _, _, _, _, _, busyModel := newClassificationTestService(t, classification)
	busyCreated, err := busyService.Create(ctx, "按正文整理文件")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := busyService.ConfirmAnalysis(ctx, busyCreated.ID)
	if !errors.Is(err, documenttext.ErrBusy) || busy.State != store.ConversationFailed {
		t.Fatalf("concurrent analysis limit = (%#v, %v), want ErrBusy", busy, err)
	}
	if _, calls := busyModel.requestsSnapshot(); calls != 0 {
		t.Fatal("busy analysis read documents or called the classification model")
	}
}

func TestClassifyScannedPDFThroughAgentAfterOCRConfirmation(t *testing.T) {
	if os.Getenv("FILESAGE_DOCUMENTTEXT_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_DOCUMENTTEXT_INTEGRATION=1 to run real OCR classification")
	}
	ctx := context.Background()
	classification := `{"items":[{"fileId":0,"categoryId":-1,"reasonCode":"other"},{"fileId":1,"categoryId":-1,"reasonCode":"other"},{"fileId":2,"categoryId":-1,"reasonCode":"other"},{"fileId":3,"categoryId":0,"reasonCode":"topic"}]}`
	conversationService, plans, _, _, rootPath, databasePath, model := newClassificationTestService(t, classification)
	docx, err := os.ReadFile(filepath.Join("..", "office", "testdata", "preview.docx"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := office.Convert(ctx, docx, ".docx")
	if err != nil {
		t.Fatalf("create PDF fixture: %v", err)
	}
	scannedPDF, err := imagepdf.FromPDF(ctx, pdf)
	if err != nil {
		t.Fatalf("create scanned PDF fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source", "scan.pdf"), scannedPDF, 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := conversationService.Create(ctx, "按正文主题整理扫描件")
	if err != nil || created.State != store.ConversationAwaitingConfirmation {
		t.Fatalf("Create() = (%#v, %v)", created, err)
	}
	requests, calls := model.requestsSnapshot()
	if calls != 0 || strings.Contains(marshalMessages(t, requests[0]), "DOCX preview test fixture") {
		t.Fatal("scanned document content reached the model before confirmation")
	}
	confirmed, err := conversationService.ConfirmAnalysis(ctx, created.ID, true)
	if err != nil || confirmed.State != store.ConversationAwaitingApproval || confirmed.Analysis != nil || confirmed.PlanID == "" {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", confirmed, err)
	}
	requests, calls = model.requestsSnapshot()
	if calls != 1 || !strings.Contains(marshalMessages(t, requests[1]), "DOCX preview test fixture") {
		t.Fatalf("real OCR text did not reach the classification model: calls=%d", calls)
	}
	plan, err := plans.Plan(ctx, confirmed.PlanID)
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].SourcePath != "source/scan.pdf" {
		t.Fatalf("scanned PDF classification plan = (%#v, %v)", plan, err)
	}
	databaseBytes, err := os.ReadFile(databasePath)
	if err != nil || strings.Contains(string(databaseBytes), "DOCX preview test fixture") {
		t.Fatalf("OCR text appeared in SQLite: %v", err)
	}
}

func TestClassifyDocumentsRejectsInvalidBatchAndStaleCandidates(t *testing.T) {
	ctx := context.Background()
	invalidResponses := []string{
		`{"items":[{"fileId":0,"categoryId":0,"reasonCode":"topic"}]}`,
		`{"items":[{"fileId":0,"categoryId":0,"reasonCode":"topic"},{"fileId":0,"categoryId":0,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`,
		`{"items":[{"fileId":0,"categoryId":9,"reasonCode":"topic"},{"fileId":1,"categoryId":0,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`,
		`{"items":[{"fileId":0,"categoryId":0,"reasonCode":"free text"},{"fileId":1,"categoryId":0,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`,
	}
	for index, response := range invalidResponses {
		t.Run("invalid_model_batch_"+string(rune('a'+index)), func(t *testing.T) {
			service, plans, _, _, _, _, _ := newClassificationTestService(t, response)
			created, err := service.Create(ctx, "分类来源文档")
			if err != nil {
				t.Fatal(err)
			}
			failed, err := service.ConfirmAnalysis(ctx, created.ID)
			if !errors.Is(err, ErrModelResponse) || failed.PlanID != "" || failed.State != store.ConversationFailed {
				t.Fatalf("invalid batch result = (%#v, %v)", failed, err)
			}
			tasks, err := plans.store.ListTasks(ctx, store.TaskListQuery{Limit: 10})
			if err != nil || len(tasks.Tasks) != 0 {
				t.Fatalf("invalid batch created a task: (%#v, %v)", tasks, err)
			}
		})
	}
	t.Run("stale_candidate_set", func(t *testing.T) {
		service, _, _, _, rootPath, _, model := newClassificationTestService(t, `{"items":[{"fileId":0,"categoryId":0,"reasonCode":"topic"},{"fileId":1,"categoryId":0,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`)
		created, err := service.Create(ctx, "分类来源文档")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "new.md"), []byte("new candidate"), 0o600); err != nil {
			t.Fatal(err)
		}
		failed, err := service.ConfirmAnalysis(ctx, created.ID)
		_, calls := model.requestsSnapshot()
		if !errors.Is(err, ErrConflict) || failed.PlanID != "" || calls != 0 {
			t.Fatalf("stale candidate handling = (%#v, %v), classifier calls=%d", failed, err, calls)
		}
	})
	t.Run("same_size_replacement", func(t *testing.T) {
		service, _, _, _, rootPath, _, model := newClassificationTestService(t, `{"items":[{"fileId":0,"categoryId":0,"reasonCode":"topic"},{"fileId":1,"categoryId":0,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`)
		created, err := service.Create(ctx, "分类来源文档")
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(rootPath, "source", "a", "note.md")
		original, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		replacement := bytes.Replace(original, []byte("SENTINEL_A"), []byte("SENTINEL_Z"), 1)
		if len(replacement) != len(original) {
			t.Fatal("replacement changed source size")
		}
		if err := os.WriteFile(name, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		failed, err := service.ConfirmAnalysis(ctx, created.ID)
		_, calls := model.requestsSnapshot()
		if !errors.Is(err, ErrConflict) || failed.PlanID != "" || calls != 0 {
			t.Fatalf("same-size replacement handling = (%#v, %v), classifier calls=%d", failed, err, calls)
		}
	})
}

func TestClassifyDocumentsEnforcesBodyAndCandidateLimitsBeforeConfirmation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		before func(string) error
	}{
		{
			name: "candidate_count",
			before: func(rootPath string) error {
				for index := 0; index < maxClassificationFiles-2; index++ {
					name := filepath.Join(rootPath, "source", "extra-"+strconv.Itoa(index)+".md")
					if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name: "single_file_size",
			before: func(rootPath string) error {
				content := bytes.Repeat([]byte("x"), maxClassificationFileBytes+1)
				return os.WriteFile(filepath.Join(rootPath, "source", "a", "note.md"), content, 0o600)
			},
		},
		{
			name: "aggregate_size",
			before: func(rootPath string) error {
				content := bytes.Repeat([]byte("x"), 60<<10)
				for index := 0; index < 5; index++ {
					name := filepath.Join(rootPath, "source", "extra-"+strconv.Itoa(index)+".txt")
					if err := os.WriteFile(name, content, 0o600); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, rootPath, _, model := newClassificationTestService(t, `{"items":[]}`)
			if err := test.before(rootPath); err != nil {
				t.Fatal(err)
			}
			created, err := service.Create(ctx, "分类来源文档")
			_, calls := model.requestsSnapshot()
			if err != nil || created.State != store.ConversationUnsupported || created.Analysis != nil || calls != 0 {
				t.Fatalf("limit handling = (%#v, %v), model classification calls=%d", created, err, calls)
			}
		})
	}
}

func TestClassifyDocumentsRejectsCategoryReplacementDuringModelCall(t *testing.T) {
	ctx := context.Background()
	response := `{"items":[{"fileId":0,"categoryId":1,"reasonCode":"topic"},{"fileId":1,"categoryId":1,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`
	service, plans, _, _, rootPath, _, model := newClassificationTestService(t, response)
	model.duringClassification = func() {
		oldPath := filepath.Join(rootPath, "categories", "Work")
		if err := os.Rename(oldPath, filepath.Join(rootPath, "categories", "Work-old")); err != nil {
			t.Errorf("rename category during model call: %v", err)
			return
		}
		if err := os.Mkdir(oldPath, 0o755); err != nil {
			t.Errorf("replace category during model call: %v", err)
		}
	}
	created, err := service.Create(ctx, "分类来源文档")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.ConfirmAnalysis(ctx, created.ID)
	if !errors.Is(err, ErrConflict) || failed.PlanID != "" {
		t.Fatalf("category replacement result = (%#v, %v)", failed, err)
	}
	tasks, err := plans.store.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("category replacement created a partial task: (%#v, %v)", tasks, err)
	}
}

func TestClassifyDocumentsRejectsSourceReplacementDuringModelCall(t *testing.T) {
	ctx := context.Background()
	response := `{"items":[{"fileId":0,"categoryId":1,"reasonCode":"topic"},{"fileId":1,"categoryId":1,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`
	service, plans, _, _, rootPath, _, model := newClassificationTestService(t, response)
	model.duringClassification = func() {
		source := filepath.Join(rootPath, "source", "a", "note.md")
		if err := os.Rename(source, source+".original"); err != nil {
			t.Errorf("rename source during model call: %v", err)
			return
		}
		if err := os.WriteFile(source, []byte("CLASSIFY_SENTINEL_A confidential project alpha"), 0o600); err != nil {
			t.Errorf("replace source during model call: %v", err)
		}
	}
	created, err := service.Create(ctx, "分类来源文档")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.ConfirmAnalysis(ctx, created.ID)
	if !errors.Is(err, ErrConflict) || failed.PlanID != "" {
		t.Fatalf("source replacement result = (%#v, %v)", failed, err)
	}
	tasks, err := plans.store.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("source replacement created a partial task: (%#v, %v)", tasks, err)
	}
}

func TestClassifyDocumentsExtractsPDFAndOfficeAfterConfirmation(t *testing.T) {
	if os.Getenv("FILESAGE_DOCUMENTTEXT_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_DOCUMENTTEXT_INTEGRATION=1 to run real document extraction")
	}
	ctx := context.Background()
	classification := `{"items":[{"fileId":0,"categoryId":-1,"reasonCode":"other"},{"fileId":1,"categoryId":-1,"reasonCode":"other"},{"fileId":2,"categoryId":-1,"reasonCode":"other"},{"fileId":3,"categoryId":-1,"reasonCode":"other"},{"fileId":4,"categoryId":-1,"reasonCode":"other"},{"fileId":5,"categoryId":-1,"reasonCode":"other"}]}`
	service, _, _, _, rootPath, _, model := newClassificationTestService(t, classification)
	docx, err := os.ReadFile(filepath.Join("..", "office", "testdata", "preview.docx"))
	if err != nil {
		t.Fatal(err)
	}
	pptx, err := os.ReadFile(filepath.Join("..", "office", "testdata", "preview.pptx"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := office.Convert(ctx, docx, ".docx")
	if err != nil {
		t.Fatalf("create PDF fixture: %v", err)
	}
	for name, data := range map[string][]byte{"report.pdf": pdf, "report.docx": docx, "slides.pptx": pptx} {
		if err := os.WriteFile(filepath.Join(rootPath, "source", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	created, err := service.Create(ctx, "按文档正文分类")
	if err != nil || created.State != store.ConversationAwaitingConfirmation || created.Analysis == nil {
		t.Fatalf("Create() = (%#v, %v)", created, err)
	}
	files := created.Analysis.Operations[0].Files
	if len(files) != 6 || files[3].Path != "source/report.docx" || files[4].Path != "source/report.pdf" || files[5].Path != "source/slides.pptx" {
		t.Fatalf("classification candidates = %#v", files)
	}
	requests, calls := model.requestsSnapshot()
	if calls != 0 || strings.Contains(marshalMessages(t, requests[0]), "FileSage preview smoke") {
		t.Fatal("PDF or Office body reached the model before confirmation")
	}
	confirmed, err := service.ConfirmAnalysis(ctx, created.ID)
	if err != nil || confirmed.State != store.ConversationAnalysisComplete {
		t.Fatalf("ConfirmAnalysis() = (%#v, %v)", confirmed, err)
	}
	requests, calls = model.requestsSnapshot()
	if calls != 1 {
		t.Fatalf("classification calls = %d, want 1", calls)
	}
	request := marshalMessages(t, requests[1])
	if strings.Count(request, "FileSage preview smoke") != 3 {
		t.Fatalf("PDF, DOCX, and PPTX text were not all extracted: %s", request)
	}
	stored, err := json.Marshal(confirmed)
	if err != nil || strings.Contains(string(stored), "FileSage preview smoke") {
		t.Fatalf("document body persisted in conversation: err=%v", err)
	}
	for _, name := range []string{"report.pdf", "report.docx", "slides.pptx"} {
		if _, err := os.Stat(filepath.Join(rootPath, "source", name)); err != nil {
			t.Errorf("source document %q changed during classification: %v", name, err)
		}
	}
}

type classificationPlanInterceptor struct {
	conversationWorkflow
	beforePropose func()
}

func (p classificationPlanInterceptor) ProposeMany(ctx context.Context, proposals []Proposal) (store.PlanRecord, error) {
	if p.beforePropose != nil {
		p.beforePropose()
	}
	return p.conversationWorkflow.ProposeMany(ctx, proposals)
}

func TestClassifyDocumentsBindsTargetParentSnapshotAtPlanCreation(t *testing.T) {
	ctx := context.Background()
	response := `{"items":[{"fileId":0,"categoryId":1,"reasonCode":"topic"},{"fileId":1,"categoryId":1,"reasonCode":"topic"},{"fileId":2,"categoryId":-1,"reasonCode":"other"}]}`
	service, plans, _, _, rootPath, _, _ := newClassificationTestService(t, response)
	service.plans = classificationPlanInterceptor{
		conversationWorkflow: plans,
		beforePropose: func() {
			oldPath := filepath.Join(rootPath, "categories", "Work")
			if err := os.Rename(oldPath, filepath.Join(rootPath, "categories", "Work-old")); err != nil {
				t.Errorf("rename category before proposal: %v", err)
				return
			}
			if err := os.Mkdir(oldPath, 0o755); err != nil {
				t.Errorf("replace category before proposal: %v", err)
			}
		},
	}
	created, err := service.Create(ctx, "分类来源文档")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.ConfirmAnalysis(ctx, created.ID)
	if !errors.Is(err, ErrConflict) || failed.PlanID != "" {
		t.Fatalf("target parent replacement result = (%#v, %v)", failed, err)
	}
	tasks, err := plans.store.ListTasks(ctx, store.TaskListQuery{Limit: 10})
	if err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("target parent replacement created a partial task: (%#v, %v)", tasks, err)
	}
}

func TestClassificationReasonCodesAreClosed(t *testing.T) {
	if classificationReason("topic") == "" || classificationReason("free text") != "" {
		t.Fatal("classification reason code accepted an unbounded value")
	}
}

var _ classificationConversationModel = (*classificationScriptModel)(nil)
var _ conversationModel = (*classificationScriptModel)(nil)
var _ conversationWorkflow = (*Service)(nil)
