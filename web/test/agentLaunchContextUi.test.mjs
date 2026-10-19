import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';

const app = await readFile(new URL('../src/App.tsx', import.meta.url), 'utf8');
const filesPage = await readFile(new URL('../src/features/files/FilesPage.tsx', import.meta.url), 'utf8');
const agentPage = await readFile(new URL('../src/features/agent/AgentPage.tsx', import.meta.url), 'utf8');

describe('从文件页打开 Agent 的范围上下文', () => {
  it('优先传递完整且有效的选择，否则传递当前目录', () => {
    assert.match(filesPage, /onOpenAgent: \(context: AgentLaunchContext\) => void/);
    assert.match(filesPage, /selectedEntries\.length > 0 && !selectionHasMissingEntries && !selectionHasUnavailableEntries\s*\? \{ kind: 'selection', paths: selectedEntries\.map\(\(entry\) => entry\.path\) \}\s*:\s*\{ kind: 'directory', path: relativePath \}/);
  });

  it('App 把文件页范围交给 Agent，并在离开 Agent 后清除；直接打开仍可恢复保存的选择', () => {
    assert.match(app, /onOpenAgent=\{\(context\) => \{ setAgentLaunchContext\(context\); navigateTo\('agent'\); \}\}/);
    assert.match(app, /next !== 'agent'\) setAgentLaunchContext\(null\)/);
    assert.match(app, /<AgentPage launchContext=\{agentLaunchContext\}/);
    assert.match(agentPage, /useState\(\(\) => launchContext \? '' : savedSelection\?\.kind === 'conversation'/);
    assert.match(agentPage, /if \(activeLaunchContext && !selectedConversationId && !selectedTaskId\) return;/);
  });

  it('只把范围附加到新会话的首条消息，按扩展后的消息计算大小并在创建成功后消费', () => {
    assert.match(agentPage, /selectedPaths: context\.paths/);
    assert.match(agentPage, /currentDirectory: context\.path/);
    assert.match(agentPage, /空目录路径表示管理根目录/);
    assert.match(agentPage, /new TextEncoder\(\)\.encode\(initialMessage\)\.byteLength > MAX_MESSAGE_BYTES/);
    assert.match(agentPage, /createAgentConversation\(initialMessage\)/);
    assert.match(agentPage, /sendAgentConversationMessage\(selectedConversationId, message\)/);
    assert.match(agentPage, /if \(activeLaunchContext\) \{\s*setActiveLaunchContext\(null\);\s*onLaunchContextConsumed\?\.\(\);\s*\}/);
    assert.match(agentPage, /整理范围：\{activeLaunchContext\.kind === 'directory'/);
  });
});
