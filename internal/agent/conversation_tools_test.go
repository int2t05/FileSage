// 本文件验证 Agent 索引工具和模型工具循环。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"filesage/internal/store"
)

type conversationToolTestIndex struct {
	status store.FileIndexStatus
	files  []store.FileIndexRecord
	query  store.FileIndexQuery
}

func (i *conversationToolTestIndex) Status(context.Context) (store.FileIndexStatus, error) {
	return i.status, nil
}

func (i *conversationToolTestIndex) QueryFileIndex(_ context.Context, query store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	i.query = query
	if query.Limit < len(i.files) {
		return i.files[:query.Limit], nil
	}
	return i.files, nil
}

type scriptedAgentToolModel struct {
	turns    []agentModelTurn
	requests [][]ChatMessage
}

func (m *scriptedAgentToolModel) completeAgentTurn(_ context.Context, messages []ChatMessage, _ func(string)) (agentModelTurn, error) {
	m.requests = append(m.requests, append([]ChatMessage(nil), messages...))
	turn := m.turns[0]
	m.turns = m.turns[1:]
	return turn, nil
}

func TestSearchFileIndexReturnsBoundedMetadata(t *testing.T) {
	captured := time.Date(2022, 3, 4, 5, 6, 7, 0, time.UTC)
	index := &conversationToolTestIndex{
		status: store.FileIndexStatus{State: store.FileIndexReady, ActiveGeneration: 8, Scanned: 125},
		files:  []store.FileIndexRecord{{RelativePath: "Photos/IMG_1.jpg", ParentPath: "Photos", Name: "IMG_1.jpg", Kind: "file", SizeBytes: 128, MediaType: "image/jpeg", ModifiedAt: captured, CapturedAt: &captured}},
	}
	service := &ConversationService{photoIndex: index}
	result, err := service.searchFileIndex(context.Background(), agentFileSearchArgs{
		Directory: "Photos", Recursive: true, Type: "image", CapturedAfter: "2022-01-01T00:00:00Z", Limit: 10,
	})
	if err != nil || !result.Available || result.Generation != 8 || result.Count != 1 || len(result.Files) != 1 {
		t.Fatalf("searchFileIndex() = (%#v, %v)", result, err)
	}
	if index.query.ParentPath != "Photos" || !index.query.Recursive || index.query.Type != "image" || index.query.Generation != 8 || index.query.CapturedFrom == nil || index.query.Limit != 11 {
		t.Fatalf("index query = %#v", index.query)
	}
	if result.Files[0].Path != "Photos/IMG_1.jpg" || result.Files[0].CapturedAt == nil || result.Files[0].MediaType != "image/jpeg" {
		t.Fatalf("metadata result = %#v", result.Files[0])
	}
	if _, err := service.searchFileIndex(context.Background(), agentFileSearchArgs{Directory: "../private"}); err != ErrInvalid {
		t.Fatalf("unsafe search path error = %v", err)
	}
}

func TestFileIndexContextIsInsertedAsUntrustedPerTurnData(t *testing.T) {
	index := &conversationToolTestIndex{
		status: store.FileIndexStatus{State: store.FileIndexReady, ActiveGeneration: 3, Scanned: 9},
		files:  []store.FileIndexRecord{{RelativePath: "Photos", Name: "Photos", Kind: "directory"}},
	}
	service := &ConversationService{photoIndex: index}
	context := service.fileIndexPromptContext(context.Background())
	messages := conversationPromptMessagesWithRuntime(store.AgentConversation{
		Messages: []store.ConversationMessage{{Role: "user", Content: "整理照片"}},
	}, nil, context)
	if !strings.Contains(context, "不可信数据") || !strings.Contains(context, `"generation":3`) || len(messages) < 3 || messages[1].Role != "user" || !strings.Contains(messages[1].Content, "rootDirectories") {
		t.Fatalf("file index context not inserted: %q; %#v", context, messages)
	}
}

func TestPlanToolExposesOnlyConversationSupportedActions(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(planProposalSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	var action struct {
		Enum []string `json:"enum"`
	}
	var operations struct {
		Items struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	if err := json.Unmarshal(schema.Properties["operations"], &operations); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(operations.Items.Properties["action"], &action); err != nil {
		t.Fatal(err)
	}
	if strings.Join(action.Enum, ",") != strings.Join(conversationPlanActionIDs(), ",") {
		t.Fatalf("plan action schema = %#v", action.Enum)
	}
	for _, internalAction := range []string{"archive_photo", "trash_duplicate"} {
		if strings.Contains(strings.Join(action.Enum, ","), `"`+internalAction+`"`) || strings.Contains(conversationToolCatalogPrompt(), internalAction+"：") {
			t.Fatalf("internal action %q was advertised to the conversation model", internalAction)
		}
	}
}

func TestAgentToolLoopSearchesBeforeProposingPlan(t *testing.T) {
	index := &conversationToolTestIndex{
		status: store.FileIndexStatus{State: store.FileIndexReady, ActiveGeneration: 4, Scanned: 2},
		files:  []store.FileIndexRecord{{RelativePath: "Photos/a.jpg", ParentPath: "Photos", Name: "a.jpg", Kind: "file", SizeBytes: 10}},
	}
	planArguments := `{"state":"ready","reply":"我会按拍摄日期整理照片。","goal":"按拍摄日期归档照片","operations":[{"action":"archive_photos","filePath":"Photos"}]}`
	model := &scriptedAgentToolModel{turns: []agentModelTurn{
		{ToolCalls: []ChatToolCall{{ID: "call_search", Type: "function", Function: ChatFunctionCall{Name: "search_files", Arguments: `{"directory":"Photos","recursive":true,"type":"image"}`}}}},
		{ToolCalls: []ChatToolCall{{ID: "call_plan", Type: "function", Function: ChatFunctionCall{Name: "propose_plan", Arguments: planArguments}}}},
	}}
	service := &ConversationService{photoIndex: index}
	var progress []string
	content, planned, err := service.completeWithAgentTools(context.Background(), model, []ChatMessage{{Role: "user", Content: "按日期归档照片"}}, nil, func(value string) { progress = append(progress, value) })
	if err != nil || !planned || content != planArguments || len(model.requests) != 2 {
		t.Fatalf("tool loop = (%q, %t, %v), requests=%d", content, planned, err, len(model.requests))
	}
	second := model.requests[1]
	if len(second) != 3 || second[1].Role != "assistant" || len(second[1].ToolCalls) != 1 || second[2].Role != "tool" || second[2].ToolCallID != "call_search" || !strings.Contains(second[2].Content, "Photos/a.jpg") {
		t.Fatalf("search result did not return to the model: %#v", second)
	}
	if len(progress) < 2 || !strings.Contains(progress[len(progress)-1], "整理执行计划") {
		t.Fatalf("tool progress = %#v", progress)
	}
	var result conversationTurnResult
	if err := decodeStrictModelJSON(content, &result); err != nil {
		t.Fatalf("proposed plan JSON = %v", err)
	}
	normalizeTurnResult(&result, "按拍摄日期归档")
	if err := validateTurnResult(&result); err != nil {
		t.Fatalf("proposed plan = %v", err)
	}
}
