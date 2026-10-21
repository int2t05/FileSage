// 本文件验证持续会话的上下文压缩边界。
package agent

import (
	"fmt"
	"strings"
	"testing"

	"filesage/internal/store"
)

// TestConversationPromptKeepsSummaryAndRecentMessages 验证工具能力、记忆与结果回执均进入上下文。
func TestConversationPromptKeepsSummaryAndRecentMessages(t *testing.T) {
	conversation := store.AgentConversation{ContextSummary: "用户希望使用同一会话继续任务。"}
	for index := 0; index < 60; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		conversation.Messages = append(conversation.Messages, store.ConversationMessage{Role: role, Content: fmt.Sprintf("消息-%02d", index)})
	}

	messages := conversationPromptMessages(conversation)
	if len(messages) != maxConversationRecentMessages+2 || messages[1].Role != "user" || !strings.Contains(messages[1].Content, "不是本轮用户请求或授权") || !strings.Contains(messages[1].Content, conversation.ContextSummary) {
		t.Fatalf("prompt did not include bounded summary and history: %#v", messages)
	}
	if !strings.Contains(messages[0].Content, "摘要以普通 user 消息提供") || !strings.Contains(messages[0].Content, "系统插入的上一计划回执由服务端根据工具执行记录生成") || !strings.Contains(messages[0].Content, "失败、部分完成或未知状态不得推断为成功") || !strings.Contains(messages[0].Content, "不得推断其路径、状态或结果") {
		t.Fatalf("system prompt did not establish summary and result receipt boundaries: %q", messages[0].Content)
	}
	if !strings.Contains(messages[0].Content, "run_shell：在隔离副本中运行 Shell") || !strings.Contains(messages[0].Content, "输入：scopePath, workingDir, shellCommand") {
		t.Fatalf("system prompt omitted the registered tool catalog: %q", messages[0].Content)
	}
	if !strings.Contains(messages[0].Content, "输入契约是服务端预检字段，不是对话 JSON 字段名") ||
		!strings.Contains(messages[0].Content, "对话输出字段以本指令中的 JSON 契约为准") {
		t.Fatal("system prompt did not distinguish backend input contracts from conversation JSON")
	}
	for _, analysisAction := range []string{"find_duplicates", "archive_photos", "classify_documents"} {
		if !strings.Contains(messages[0].Content, analysisAction) {
			t.Errorf("system prompt omitted the conversation analysis capability %q", analysisAction)
		}
	}
	if !strings.Contains(messages[0].Content, "候选文件与类别目录由服务端") || !strings.Contains(messages[0].Content, "不发送给模型、不修改文件") {
		t.Fatal("system prompt omitted the analysis data or side-effect boundaries")
	}
	if !strings.Contains(messages[0].Content, "从上述已登记工具和对话分析能力中选择") {
		t.Fatal("system prompt did not make the analysis actions selectable")
	}
	if messages[2].Content != "消息-36" || messages[len(messages)-1].Content != "消息-59" {
		t.Fatalf("recent window = (%q, %q)", messages[2].Content, messages[len(messages)-1].Content)
	}
	if strings.Contains(messages[2].Content, "消息-00") {
		t.Fatal("old transcript was not compacted")
	}
}

func TestConversationTurnResultValidatesSummary(t *testing.T) {
	summary := "用户希望保留目录结构。"
	valid := conversationTurnResult{State: "clarifying", Reply: "请补充范围。", ContextSummary: &summary}
	if err := validateTurnResult(&valid); err != nil {
		t.Fatalf("validateTurnResult(valid summary) = %v", err)
	}
	tooLongSummary := strings.Repeat("a", store.MaxConversationSummaryBytes+1)
	tooLong := conversationTurnResult{State: "clarifying", Reply: "请补充范围。", ContextSummary: &tooLongSummary}
	if err := validateTurnResult(&tooLong); err == nil {
		t.Fatal("oversized context summary was accepted")
	}
	omitted := conversationTurnResult{State: "clarifying", Reply: "请补充范围。"}
	if err := validateTurnResult(&omitted); err != nil {
		t.Fatalf("validateTurnResult(omitted summary) = %v", err)
	}
}

func TestConversationAllowsReplyOnlySummaryFromToolReceipt(t *testing.T) {
	contextSummary := "已按用户确认创建合同文件夹。"
	result := conversationTurnResult{
		State:          "clarifying",
		Reply:          "上一计划已完成，已创建“合同”文件夹。",
		ContextSummary: &contextSummary,
	}
	if err := validateTurnResult(&result); err != nil {
		t.Fatalf("validateTurnResult(reply-only summary) = %v", err)
	}

	conversation := store.AgentConversation{Messages: []store.ConversationMessage{
		{Role: "user", Content: "创建合同文件夹"},
		{Role: "assistant", Content: "上一计划已完成；步骤结果：\n- 新建文件夹：合同（完成）：已创建文件夹"},
		{Role: "user", Content: "只总结上一轮结果，不要继续操作"},
	}}
	messages := conversationPromptMessages(conversation)
	if !strings.Contains(messages[0].Content, "只要求解释或总结既有计划结果") ||
		!strings.Contains(messages[0].Content, "步骤回执提供实际执行状态、结果和失败边界") ||
		!strings.Contains(messages[len(messages)-2].Content, "已创建文件夹") ||
		!strings.Contains(messages[len(messages)-1].Content, "只总结上一轮结果") {
		t.Fatalf("reply-only turn did not receive the execution receipt and guidance: %#v", messages)
	}
}
