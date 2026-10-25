//go:build linux

// 本文件验证照片归档从索引确认到计划审阅执行的完整流程。
package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filesage/internal/config"
	"filesage/internal/fsroot"
	"filesage/internal/indexer"
	"filesage/internal/store"
)

const privatePhotoMarker = "PRIVATE_PHOTO_BYTES_9041"

func TestPhotoArchiveConversationApprovalAndExecution(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	for _, directory := range []string{"camera/a", "camera/b", "camera/archive"} {
		if err := os.MkdirAll(filepath.Join(rootPath, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	photoA := append(testJPEGWithCaptureDate("2020:01:02 03:04:05"), []byte(privatePhotoMarker+"-a")...)
	photoB := append(testJPEGWithCaptureDate("2020:01:02 03:04:05"), []byte(privatePhotoMarker+"-b")...)
	unknown := []byte("unknown-date-photo")
	previous := testJPEGWithCaptureDate("2019:12:31 23:59:59")
	for name, content := range map[string][]byte{
		"camera/a/photo.jpg": photoA, "camera/b/photo.jpg": photoB,
		"camera/unknown.jpg": unknown, "camera/archive/previous.jpg": previous,
	} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(name)), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
	defer root.Close()
	defer metadata.Close()
	fileIndex, err := indexer.Open(ctx, root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer fileIndex.Close()
	waitPhotoIndexReady(t, fileIndex)
	var modelRequests [][]ChatMessage
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Messages   []ChatMessage        `json:"messages"`
			Stream     bool                 `json:"stream"`
			ToolChoice string               `json:"tool_choice"`
			Tools      []chatToolDefinition `json:"tools"`
		}
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" || json.NewDecoder(request.Body).Decode(&body) != nil || !body.Stream || body.ToolChoice != "auto" || len(body.Tools) != 3 {
			http.Error(w, "invalid model request", http.StatusBadRequest)
			return
		}
		modelRequests = append(modelRequests, body.Messages)
		if len(modelRequests) == 1 {
			writeAgentToolCall(w, "call-search", "search_files", `{"directory":"","recursive":true,"type":"image"}`)
			return
		}
		if len(modelRequests) == 2 {
			writeAgentToolCall(w, "call-plan", "propose_plan", `{"state":"ready","reply":"我会按拍摄日期归档照片。","contextSummary":"照片归档到默认目录。","goal":"按拍摄日期归档照片","operations":[{"action":"archive_photos","filePath":""}]}`)
			return
		}
		http.Error(w, "unexpected model turn", http.StatusBadRequest)
	}))
	defer model.Close()
	conversationService := &ConversationService{
		store: metadata, root: root, plans: plans,
		model:      newChatClient(config.AgentModelConfig{BaseURL: model.URL + "/v1", Model: "test-model", Provider: "test-provider"}),
		photoIndex: fileIndex,
	}
	conversation, err := conversationService.Create(ctx, "按拍摄日期归档照片")
	if err != nil || conversation.State != store.ConversationAwaitingApproval || conversation.Analysis != nil || conversation.PlanID == "" {
		t.Fatalf("Create() = (%#v, %v)", conversation, err)
	}
	if len(modelRequests) != 2 || strings.Contains(marshalMessages(t, modelRequests[0]), privatePhotoMarker) {
		t.Fatal("photo bytes reached the clarification model")
	}
	searchResultSeen := false
	for _, message := range modelRequests[1] {
		if message.Role == "tool" && message.ToolCallID == "call-search" && strings.Contains(message.Content, "camera/a/photo.jpg") {
			searchResultSeen = true
		}
	}
	if !searchResultSeen {
		t.Fatalf("real index search result did not return to the plan call: %#v", modelRequests[1])
	}
	confirmed := conversation
	plan, err := plans.Plan(ctx, confirmed.PlanID)
	if err != nil || len(plan.Operations) != 2 {
		t.Fatalf("photo plan = (%#v, %v), want two dated photos", plan, err)
	}
	if !strings.Contains(confirmed.Messages[len(confirmed.Messages)-1].Content, "2 个照片归档步骤") || !strings.Contains(confirmed.Messages[len(confirmed.Messages)-1].Content, "1 张缺少拍摄日期并跳过") {
		t.Fatalf("archive summary does not show candidate and skip counts: %#v", confirmed.Messages)
	}
	if plan.Operations[0].SourcePath != "camera/a/photo.jpg" || plan.Operations[0].TargetPath != "按拍摄日期归档/2020/01/photo.jpg" ||
		plan.Operations[1].SourcePath != "camera/b/photo.jpg" || plan.Operations[1].TargetPath != "按拍摄日期归档/2020/01/photo (1).jpg" {
		t.Fatalf("archive targets are not deterministic: %#v", plan.Operations)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "按拍摄日期归档", "2020")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("analysis created archive directories before approval: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "camera", "a", "photo.jpg")); err != nil {
		t.Fatalf("analysis moved a source before approval: %v", err)
	}
	firstApproval, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plans.Decide(ctx, plan.ID, plan.Operations[0].ID, "approve", firstApproval.Version); !errors.Is(err, ErrConflict) && !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate approval error = %v, want conflict", err)
	}
	if _, err := plans.Execute(ctx, plan.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("partially approved Execute() error = %v, want conflict", err)
	}
	approved, err := plans.Decide(ctx, plan.ID, plan.Operations[1].ID, "approve", plan.Version)
	if err != nil || approved.Status != store.PlanApproved {
		t.Fatalf("final approval = (%#v, %v)", approved, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "camera", "a", "photo.jpg")); err != nil {
		t.Fatalf("approval moved a source before explicit execution: %v", err)
	}
	if err := metadata.ClaimAgentPlanExecution(ctx, plan.ID, approved.Version, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := plans.executeOperation(ctx, plan.ID, plan.ScopePath, plan.Operations[0]); err != nil {
		t.Fatalf("first photo operation = %v", err)
	}
	if err := metadata.StartAgentOperation(ctx, plan.ID, plan.Operations[1].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := plans.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := plans.Plan(ctx, plan.ID)
	if err != nil || recovered.Status != store.PlanApproved || recovered.Operations[0].Status != store.OperationSucceeded || recovered.Operations[1].Status != store.OperationPending {
		t.Fatalf("archive recovery = (%#v, %v), want first item complete and second manually retryable", recovered, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "camera", "archive", "2020", "01")); err != nil {
		t.Fatalf("recovery did not preserve directories created by the completed photo operation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "camera", "b", "photo.jpg")); err != nil {
		t.Fatalf("recovery moved the interrupted source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "camera", "archive", "2020", "01", "photo (1).jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery automatically replayed the interrupted move: %v", err)
	}
	completed, err := plans.Execute(ctx, plan.ID, recovered.Version)
	if err != nil || completed.Status != store.PlanCompleted {
		t.Fatalf("Execute() = (%#v, %v)", completed, err)
	}
	for target, expected := range map[string][]byte{
		"camera/archive/2020/01/photo.jpg": photoA, "camera/archive/2020/01/photo (1).jpg": photoB,
	} {
		actual, readErr := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(target)))
		if readErr != nil || string(actual) != string(expected) {
			t.Fatalf("archived photo %q mismatch: %v", target, readErr)
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, "camera", "unknown.jpg")); err != nil {
		t.Fatalf("unknown-date photo was moved: %v", err)
	}
	page, err := metadata.AuditForTaskPage(ctx, plan.TaskID, 100, 0)
	if err != nil || len(page.Records) < 8 {
		t.Fatalf("archive audit = (%#v, %v)", page, err)
	}
	for _, operation := range completed.Operations {
		if operation.Status != store.OperationSucceeded {
			t.Fatalf("archive operation did not succeed: %#v", operation)
		}
	}
	stored, err := json.Marshal(struct {
		Conversation store.AgentConversation
		Plan         store.PlanRecord
		Audit        store.TaskAuditPage
	}{confirmed, completed, page})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), privatePhotoMarker) {
		t.Fatal("photo bytes appeared in conversation, plan, or audit records")
	}
}

func writeAgentToolCall(w http.ResponseWriter, id, name, arguments string) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}},
	}}}})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
}

func TestPhotoArchiveRejectsWholeBatchAboveLimits(t *testing.T) {
	for _, count := range []int{257, 10001} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			ctx := context.Background()
			rootPath := t.TempDir()
			if err := os.Mkdir(filepath.Join(rootPath, "archive"), 0o755); err != nil {
				t.Fatal(err)
			}
			metadata, root, plans := openAgentTestServices(t, filepath.Join(t.TempDir(), "filesage.sqlite"), rootPath)
			defer root.Close()
			defer metadata.Close()
			captured := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
			records := make([]store.FileIndexRecord, count)
			for index := range records {
				records[index] = store.FileIndexRecord{RelativePath: "camera/" + strconv.Itoa(index) + ".jpg", Name: strconv.Itoa(index) + ".jpg", Kind: "file", CapturedAt: &captured}
			}
			model := &scriptedConversationModel{
				status:  ModelStatus{Available: true, Provider: "test-provider", Model: "test-model", Destination: "local"},
				replies: []string{`{"state":"ready","reply":"请确认。","goal":"归档照片","action":"archive_photos","filePath":"","targetPath":"archive"}`},
			}
			conversationService := &ConversationService{store: metadata, root: root, plans: plans, model: model, photoIndex: staticPhotoIndex{records: records}}
			failed, err := conversationService.Create(ctx, "归档所有照片")
			if !errors.Is(err, ErrUnsupported) || failed.State != store.ConversationFailed {
				t.Fatalf("Create() = (%#v, %v), want whole-batch rejection", failed, err)
			}
			tasks, err := metadata.ListTasks(ctx, store.TaskListQuery{Limit: 10})
			if err != nil || len(tasks.Tasks) != 0 {
				t.Fatalf("oversized batch created a partial task: %#v (%v)", tasks, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, "archive", "2020")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("oversized batch wrote to disk: %v", err)
			}
		})
	}
}

func TestPhotoArchiveAllowsSiblingTargetAndRejectsOverlappingAncestor(t *testing.T) {
	rootPath := t.TempDir()
	for _, directory := range []string{"camera/nested", "camera/archive", "photos", "camera-copy"} {
		if err := os.MkdirAll(filepath.Join(rootPath, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	service := &ConversationService{root: root}
	for _, test := range []struct {
		scope, target string
		state         store.ConversationState
	}{
		{scope: "", target: "photos", state: store.ConversationAwaitingConfirmation},
		{scope: "camera", target: "photos", state: store.ConversationAwaitingConfirmation},
		{scope: "camera", target: "camera/archive", state: store.ConversationAwaitingConfirmation},
		{scope: "camera", target: "camera-copy", state: store.ConversationAwaitingConfirmation},
		{scope: "camera", target: "camera", state: store.ConversationClarifying},
		{scope: "camera/nested", target: "camera", state: store.ConversationClarifying},
	} {
		state, _ := service.validatePhotoArchiveCard(test.scope, test.target, "已确认照片归档范围。")
		if state != test.state {
			t.Errorf("source %q and target %q yielded %q, want %q", test.scope, test.target, state, test.state)
		}
	}
}

type staticPhotoIndex struct {
	records []store.FileIndexRecord
}

func (i staticPhotoIndex) Status(context.Context) (store.FileIndexStatus, error) {
	return store.FileIndexStatus{ActiveGeneration: 1, State: store.FileIndexReady}, nil
}

func (i staticPhotoIndex) QueryFileIndex(_ context.Context, query store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	if query.Offset >= len(i.records) {
		return nil, nil
	}
	end := query.Offset + query.Limit
	if end > len(i.records) {
		end = len(i.records)
	}
	return i.records[query.Offset:end], nil
}

func waitPhotoIndexReady(t *testing.T, fileIndex *indexer.Service) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := fileIndex.Status(context.Background())
		if err == nil && status.State == store.FileIndexReady && status.ActiveGeneration > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := fileIndex.Status(context.Background())
	t.Fatalf("photo index did not become ready: %#v (%v)", status, err)
}

func testJPEGWithCaptureDate(value string) []byte {
	tiff := make([]byte, 64)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x8769)
	binary.LittleEndian.PutUint16(tiff[12:14], 4)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint32(tiff[18:22], 26)
	binary.LittleEndian.PutUint16(tiff[26:28], 1)
	binary.LittleEndian.PutUint16(tiff[28:30], 0x9003)
	binary.LittleEndian.PutUint16(tiff[30:32], 2)
	binary.LittleEndian.PutUint32(tiff[32:36], 20)
	binary.LittleEndian.PutUint32(tiff[36:40], 44)
	copy(tiff[44:64], value+"\x00")
	payload := append([]byte("Exif\x00\x00"), tiff...)
	length := uint16(len(payload) + 2)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, byte(length >> 8), byte(length)}
	jpeg = append(jpeg, payload...)
	return append(jpeg, 0xff, 0xda)
}
