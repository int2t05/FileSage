// 本文件处理模型工具调用的流式传输边界。
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const maxModelToolCalls = 8
const maxModelToolArgumentsBytes = 128 << 10

type chatToolDefinition struct {
	Type     string                 `json:"type"`
	Function chatFunctionDefinition `json:"function"`
}

type chatFunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type agentModelTurn struct {
	Content   string
	ToolCalls []ChatToolCall
}

type indexedChatToolCall struct {
	index int
	call  ChatToolCall
}

// completeAgentTurn 请求模型自主选择只读查询或生成待审计划。
func (c *chatClient) completeAgentTurn(ctx context.Context, messages []ChatMessage, onContent func(string)) (agentModelTurn, error) {
	if c == nil || !c.status.Available {
		return agentModelTurn{}, ErrModelUnavailable
	}
	if len(messages) == 0 || len(messages) > 64 || !validAgentChatMessages(messages) {
		return agentModelTurn{}, ErrModelResponse
	}
	requestBody, err := json.Marshal(struct {
		Model       string               `json:"model"`
		Messages    []ChatMessage        `json:"messages"`
		Stream      bool                 `json:"stream"`
		Temperature float64              `json:"temperature"`
		Tools       []chatToolDefinition `json:"tools"`
		ToolChoice  string               `json:"tool_choice"`
	}{c.status.Model, messages, true, 0, conversationModelTools(), "auto"})
	if err != nil || len(requestBody) > maxModelRequestBytes {
		return agentModelTurn{}, ErrModelResponse
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return agentModelTurn{}, &modelRequestFailure{class: "request_build"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return agentModelTurn{}, &modelRequestFailure{class: "transport"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return agentModelTurn{}, &modelRequestFailure{class: "http_status", status: response.StatusCode}
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), maxModelResponseBytes+1)
	var content strings.Builder
	calls := make(map[int]*ChatToolCall)
	responseBytes := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		responseBytes += len(line) + 1
		if responseBytes > maxModelResponseBytes {
			return agentModelTurn{}, ErrModelResponse
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return agentModelTurn{}, ErrModelResponse
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if content.Len()+len(delta.Content) > maxModelContentBytes {
			return agentModelTurn{}, ErrModelResponse
		}
		if delta.Content != "" {
			content.WriteString(delta.Content)
			if onContent != nil {
				onContent(delta.Content)
			}
		}
		for _, item := range delta.ToolCalls {
			if item.Index < 0 || item.Index >= maxModelToolCalls {
				return agentModelTurn{}, ErrModelResponse
			}
			call := calls[item.Index]
			if call == nil {
				call = &ChatToolCall{Type: "function"}
				calls[item.Index] = call
			}
			if item.ID != "" {
				call.ID += item.ID
			}
			if item.Type != "" {
				call.Type = item.Type
			}
			if len(call.Function.Name)+len(item.Function.Name) > 128 || len(call.Function.Arguments)+len(item.Function.Arguments) > maxModelToolArgumentsBytes {
				return agentModelTurn{}, ErrModelResponse
			}
			call.Function.Name += item.Function.Name
			call.Function.Arguments += item.Function.Arguments
		}
	}
	if err := scanner.Err(); err != nil {
		return agentModelTurn{}, ErrModelResponse
	}
	turn := agentModelTurn{Content: content.String()}
	if len(calls) == 0 {
		if turn.Content == "" {
			return agentModelTurn{}, ErrModelResponse
		}
		return turn, nil
	}
	if len(calls) > maxModelToolCalls {
		return agentModelTurn{}, ErrModelResponse
	}
	ordered := make([]indexedChatToolCall, 0, len(calls))
	for index, call := range calls {
		if !validChatToolCall(*call) {
			return agentModelTurn{}, ErrModelResponse
		}
		ordered = append(ordered, indexedChatToolCall{index: index, call: *call})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	for _, item := range ordered {
		turn.ToolCalls = append(turn.ToolCalls, item.call)
	}
	return turn, nil
}

func validAgentChatMessages(messages []ChatMessage) bool {
	for _, message := range messages {
		if len(message.Content) > 64<<10 {
			return false
		}
		switch message.Role {
		case "system", "user":
			if message.Content == "" || len(message.ToolCalls) > 0 || message.ToolCallID != "" {
				return false
			}
		case "assistant":
			if message.Content == "" && len(message.ToolCalls) == 0 || message.ToolCallID != "" {
				return false
			}
			for _, call := range message.ToolCalls {
				if !validChatToolCall(call) {
					return false
				}
			}
		case "tool":
			if message.Content == "" || message.Name == "" || message.ToolCallID == "" || len(message.ToolCalls) > 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validChatToolCall(call ChatToolCall) bool {
	return call.Type == "function" && len(call.ID) > 0 && len(call.ID) <= 128 &&
		len(call.Function.Name) > 0 && len(call.Function.Name) <= 128 &&
		len(call.Function.Arguments) > 0 && len(call.Function.Arguments) <= maxModelToolArgumentsBytes
}

func conversationModelTools() []chatToolDefinition {
	return []chatToolDefinition{
		{Type: "function", Function: chatFunctionDefinition{
			Name: "get_file_index_status", Description: "查看文件索引是否可用和最新。只返回目录与文件元数据，不读取正文。",
			Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}},
		{Type: "function", Function: chatFunctionDefinition{
			Name: "search_files", Description: "在管理根目录的文件索引中搜索路径和元数据。需要发现文件时调用；不会读取或发送文件正文。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"directory":{"type":"string","description":"管理根目录内相对目录；空字符串表示根目录"},"recursive":{"type":"boolean"},"keyword":{"type":"string"},"type":{"type":"string","enum":["directory","image","video","audio","document","other"]},"capturedAfter":{"type":"string","description":"仅筛选拍摄时间不早于此 RFC3339 时间"},"capturedBefore":{"type":"string","description":"仅筛选拍摄时间早于此 RFC3339 时间"},"limit":{"type":"integer","minimum":1,"maximum":40}},"additionalProperties":false}`),
		}},
		{Type: "function", Function: chatFunctionDefinition{
			Name: "propose_plan", Description: "提交一个待用户审阅的 FileSage 操作计划。此工具只生成计划，不执行文件变更。所有动作和路径仍由服务端校验。",
			Parameters: planProposalSchema(),
		}},
	}
}

func conversationPlanCapabilities() []AgentToolCapability {
	allowed := map[string]bool{
		"rename": true, "edit_text": true, "create_file": true, "overwrite_file": true,
		"create_directory": true, "move_file": true, "create_symlink": true, "copy_file": true,
		"copy_directory": true, "create_archive": true, "extract_archive": true,
		"set_permissions": true, "set_owner": true, "trash": true, "permanent_delete": true,
		"run_shell": true,
	}
	capabilities := make([]AgentToolCapability, 0, len(allowed))
	for _, tool := range AgentToolCatalog() {
		if allowed[tool.ID] {
			capabilities = append(capabilities, tool)
		}
	}
	return capabilities
}

func conversationPlanActionIDs() []string {
	ids := make([]string, 0, len(conversationPlanCapabilities())+3)
	for _, tool := range conversationPlanCapabilities() {
		ids = append(ids, tool.ID)
	}
	ids = append(ids, "find_duplicates", "archive_photos", "classify_documents")
	sort.Strings(ids)
	return ids
}

func planProposalSchema() json.RawMessage {
	properties := map[string]any{
		"action":            map[string]any{"type": "string", "enum": conversationPlanActionIDs()},
		"filePath":          map[string]any{"type": "string"},
		"targetPath":        map[string]any{"type": "string"},
		"shellCommand":      map[string]any{"type": "string"},
		"workingDir":        map[string]any{"type": "string"},
		"requestedMode":     map[string]any{"type": "integer", "minimum": 0, "maximum": 511},
		"requestedOwnerUid": map[string]any{"type": "integer", "minimum": 0},
		"requestedGroupGid": map[string]any{"type": "integer", "minimum": 0},
	}
	operation := map[string]any{"type": "object", "properties": properties, "required": []string{"action"}, "additionalProperties": false}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"state":          map[string]any{"type": "string", "enum": []string{"ready"}},
			"reply":          map[string]any{"type": "string"},
			"contextSummary": map[string]any{"type": "string"},
			"goal":           map[string]any{"type": "string"},
			"operations":     map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "items": operation},
		},
		"required":             []string{"state", "reply", "goal", "operations"},
		"additionalProperties": false,
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return encoded
}

func toolCallName(call ChatToolCall) string { return call.Function.Name }

func marshalToolResult(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"available":false,"error":"结果暂不可用"}`
	}
	if len(encoded) > 24<<10 {
		return `{"available":false,"error":"索引结果过大，请缩小搜索范围"}`
	}
	return string(encoded)
}

func toolCallMessage(call ChatToolCall) ChatMessage {
	return ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{call}}
}

func toolErrorResult(err error) string {
	if errors.Is(err, ErrInvalid) {
		return marshalToolResult(map[string]any{"available": false, "error": "搜索参数无效，请调整目录或筛选条件。"})
	}
	return marshalToolResult(map[string]any{"available": false, "error": "索引暂不可用，请说明无法自动定位文件。"})
}

func toolResultMessage(call ChatToolCall, result string) ChatMessage {
	return ChatMessage{Role: "tool", Name: toolCallName(call), ToolCallID: call.ID, Content: result}
}

func ensureToolResponse(call ChatToolCall, result string) ChatMessage {
	if result == "" {
		result = marshalToolResult(map[string]any{"available": false, "error": "工具未返回结果。"})
	}
	return toolResultMessage(call, result)
}

func toolCallDebugName(call ChatToolCall) string {
	if call.Function.Name == "search_files" {
		return "正在搜索文件索引"
	}
	return "正在读取文件索引状态"
}

func formatToolCallProgress(call ChatToolCall, count int) string {
	return fmt.Sprintf("%s · 找到 %d 项", toolCallDebugName(call), count)
}
