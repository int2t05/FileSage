import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const agentPage = await readFile(new URL('../src/features/agent/AgentPage.tsx', import.meta.url), 'utf8');
const agentStyles = await readFile(new URL('../src/features/agent/AgentPage.css', import.meta.url), 'utf8');
const settingsPage = await readFile(new URL('../src/features/settings/AgentSettingsPage.tsx', import.meta.url), 'utf8');
const agentPageFile = ts.createSourceFile('AgentPage.tsx', agentPage, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const messageHelpers = agentPageFile.statements.filter((statement) => ts.isFunctionDeclaration(statement)
  && ['conversationMessagesForDisplay', 'assistantMessagePreview'].includes(statement.name?.text ?? ''));
const { outputText: messageHelperOutput } = ts.transpileModule(`${messageHelpers.map((statement) => statement.getText(agentPageFile)).join('\n')}\nexport { conversationMessagesForDisplay, assistantMessagePreview };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { conversationMessagesForDisplay, assistantMessagePreview } = await import(`data:text/javascript;base64,${Buffer.from(messageHelperOutput).toString('base64')}`);

describe('Agent 会话与设置界面', () => {
  it('把保存的对话放在常驻侧栏，新草稿切换时不会清除历史', () => {
    assert.match(agentPage, /<aside className="agent-session-sidebar"/);
    assert.match(agentPage, /aria-label="已保存对话"/);
    assert.match(agentPage, /onClick=\{\(\) => selectConversation\(item\.id\)\}/);
    assert.match(agentPage, /value === 'draft'\) return \{ kind: 'draft' \}/);
    assert.match(agentPage, /window\.localStorage\.setItem\(SELECTED_ITEM_KEY, selection\)/);
    assert.doesNotMatch(agentPage, /<details className="agent-history-section"/);
  });

  it('刷新后定位已保存的旧会话，并在窄屏提供横向切换', () => {
    assert.match(agentPage, /autoExpandHistoryOffset/);
    assert.match(agentPage, /setConversationHistoryOffset\(\(offset\) => offset \+ MAX_CONVERSATIONS\)/);
    assert.match(agentStyles, /@media \(max-width: 960px\)[\s\S]*?\.agent-session-list\s*\{[\s\S]*?overflow-x: auto/);
    assert.match(agentStyles, /\.agent-session-new\.is-active/);
  });

  it('新对话居中显示常见整理示例，点击只预填消息', () => {
    assert.match(agentPage, /AGENT_STARTER_PROMPTS = \['按拍摄日期归档照片', '按内容整理 PDF', '查找重复文件'\]/);
    assert.match(agentPage, /type="button" key=\{prompt\} onClick=\{\(\) => selectStarterPrompt\(prompt\)\}/);
    assert.match(agentPage, /function selectStarterPrompt\(prompt: string\)[\s\S]*?setDraft\(prompt\)[\s\S]*?composerRef\.current\?\.focus\(\)/);
    assert.match(agentStyles, /\.agent-conversation-flow\.is-empty\s*\{[^}]*align-content: center/);
    assert.match(agentStyles, /@media \(max-width: 440px\)[\s\S]*?\.agent-prompt-suggestions\s*\{[^}]*display: none/);
    assert.match(agentStyles, /@media \(max-width: 440px\) and \(max-height: 760px\)[\s\S]*?\.agent-chat-composer textarea\s*\{[^}]*height: 56px/);
    assert.match(agentStyles, /@media \(max-width: 440px\) and \(max-height: 760px\)[\s\S]*?\.agent-composer-keyboard-hint\s*\{[^}]*display: none/);
    assert.match(agentPage, /agent-composer-hint agent-composer-keyboard-hint/);
  });

  it('把聊天放在主视区并收起次要任务历史', () => {
    assert.match(agentPage, /<header className="agent-chat-header">/);
    assert.match(agentPage, /<h2>\{selectedTaskId \? '任务记录' : selectedConversationId \? '对话' : '新对话'\}<\/h2>/);
    assert.match(agentPage, /currentConversation && !currentConversation\.planId && <span className="agent-conversation-status">/);
    assert.doesNotMatch(agentPage, /activeConversationTitle/);
    assert.match(agentPage, /<details className="agent-task-history" open=\{Boolean\(selectedTaskId\)\}>/);
    assert.match(agentStyles, /\.agent-chat-thread,[\s\S]*?\.agent-chat-composer\s*\{[\s\S]*?width: min\(100%, 840px\)/);
    assert.match(agentStyles, /\.agent-chat-header h2\s*\{[^}]*flex: 1/);
    assert.match(agentStyles, /\.agent-chat-header \.agent-conversation-status\s*\{[^}]*white-space: nowrap/);
    assert.match(agentStyles, /\.agent-chat-composer-footer > div\s*\{[^}]*margin-left: auto/);
    assert.match(agentStyles, /\.agent-workspace\s*\{[^}]*border: 1px solid var\(--color-line\)[^}]*background: var\(--color-surface\)/);
    assert.match(agentStyles, /\.agent-session-sidebar\s*\{[^}]*border-right: 1px solid var\(--color-line\)/);
    assert.match(agentStyles, /\.agent-chat-panel\s*\{[^}]*border: 0[^}]*background: #fbfcfa/);
    assert.match(agentStyles, /\.agent-session-item\s*\{[^}]*display: flex[^}]*align-items: center/);
    assert.match(agentStyles, /\.agent-session-item\.is-active \.agent-session-state\s*\{[^}]*display: none/);
  });

  it('首条消息创建会话后，在历史列表同步前保留当前会话', () => {
    assert.match(agentPage, /currentConversation\?\.id === selectedConversationId/);
    assert.match(agentPage, /conversationHistoryOffset, selectedConversationId, currentConversation\]/);
    assert.match(agentPage, /setConversation\(updated\);[\s\S]*?setSelectedConversationId\(updated\.id\)/);
  });

  it('计划结果同步刷新会话摘要与任务历史', () => {
    assert.match(agentPage, /function refreshAgentHistory\(\)[\s\S]*?setHistoryRefreshKey[\s\S]*?setConversationHistoryRefreshKey/);
    const planRequests = agentPage.slice(agentPage.indexOf('async function runPlanRequest'), agentPage.indexOf('async function runPlan()'));
    assert.match(planRequests, /refreshAgentHistory\(\)/);
  });

  it('设置页简洁说明模式范围与逐步确认行为', () => {
    assert.match(settingsPage, /getAgentSettings/);
    assert.match(settingsPage, /updateAgentSettings/);
    assert.match(settingsPage, /role="switch"/);
    assert.match(settingsPage, /只影响之后启动的新计划/);
    assert.match(settingsPage, /关闭后每一步都需确认/);
  });

  it('Shell 工具结果只在步骤卡展示，不重复追加到对话', () => {
    const conversation = {
      planId: 'shell-plan',
      messages: [
        { role: 'user', content: '创建文件' },
        { role: 'assistant', content: '将在隔离副本中运行命令。' },
        { role: 'assistant', content: '隔离执行发现 1 项文件变化。' },
      ],
    };
    const shellPlan = { planId: 'shell-plan', operations: [{ action: 'run_shell' }] };
    assert.deepEqual(conversationMessagesForDisplay(conversation, shellPlan), conversation.messages.slice(0, 2));
    assert.deepEqual(conversationMessagesForDisplay(conversation, { ...shellPlan, planId: 'other' }), conversation.messages);
    assert.match(agentPage, /conversationMessagesForDisplay\(currentConversation, displayedPlan\)/);
    assert.match(agentPage, /currentConversation\?\.planId \? \([\s\S]*?agent-chat-composer-state/);
    assert.match(agentPage, /本轮已进入执行计划；新任务请新建对话/);
  });

  it('待确认卡片作为操作回执时隐藏重复的助手复述', () => {
    const conversation = {
      state: 'awaiting_confirmation',
      analysisCard: { action: 'move_file' },
      messages: [
        { role: 'user', content: '把文件移到归档目录' },
        { role: 'assistant', content: '将把文件移到归档目录，请确认。' },
      ],
    };
    assert.deepEqual(conversationMessagesForDisplay(conversation, null), conversation.messages.slice(0, 1));
    assert.match(agentPage, /确认卡和 Shell 步骤已包含结构化回执/);
  });

  it('长助手回复默认折叠，短回复和用户消息保持完整', () => {
    const longReply = '请先核对文件范围和操作，再确认执行。'.repeat(10);
    const preview = assistantMessagePreview('assistant', longReply);
    assert.ok(preview.length <= 113);
    assert.ok(preview.endsWith('…'));
    assert.equal(assistantMessagePreview('assistant', '方案已准备，请核对路径。'), '');
    assert.equal(assistantMessagePreview('user', longReply), '');
    assert.match(agentPage, /<details className="agent-chat-message-details">[\s\S]*?<p>\{message\.content\}<\/p>/);
    assert.match(agentStyles, /\.agent-chat-message-details/);
  });
});
