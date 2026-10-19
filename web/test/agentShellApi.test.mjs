import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/agent.ts', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('agent.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const declarations = sourceFile.statements.filter((statement) => {
  if (ts.isFunctionDeclaration(statement)) {
    return [
      'isRecord', 'optionalInteger', 'isSafeRootRelativePath', 'isShellCommand', 'isShellWorkingDirectory', 'parsePlannedPaths', 'parseOperation', 'parsePlan',
      'parseShellChangeState', 'parseShellChange', 'parseAgentShellChangesPage', 'parseAgentShellChangePreview',
      'readResponseError', 'request', 'readPlanResponse', 'getAgentShellChanges', 'getAgentShellChangePreview',
    ].includes(statement.name?.text ?? '');
  }
  return ts.isClassDeclaration(statement) && statement.name?.text === 'AgentApiError';
});
const helperSource = `${declarations.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { AgentApiError, parseAgentShellChangesPage, getAgentShellChanges, getAgentShellChangePreview };`;
const { outputText } = ts.transpileModule(helperSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const api = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

const before = { sha256: 'a'.repeat(64), mode: 0o644, isDirectory: false, size: 4 };
const after = { sha256: 'b'.repeat(64), mode: 0o600, isDirectory: false, size: 5 };
function change(overrides = {}) {
  return {
    id: 'change-1', path: 'docs/readme.md', kind: 'modified', approval: 'pending', status: 'staged',
    binary: false, diffTruncated: false, before, after, ...overrides,
  };
}
function plan(overrides = {}) {
  return {
    taskId: 'task-1', planId: 'plan/1', planVersion: 5, status: 'awaiting_approval',
    stopRequested: false, proposalSource: 'conversation',
    operations: [{
      id: 'shell-op', action: 'run_shell', sourcePath: '', targetPath: '', reason: '整理文件', evidence: '',
      command: 'printf done > result.txt', workingDir: '', approval: 'approved', executionStatus: 'pending',
    }],
    ...overrides,
  };
}

describe('Agent Shell 变更清单 API', () => {
  it('解析分页摘要和 before/after 文件状态', () => {
    assert.deepEqual(api.parseAgentShellChangesPage({
      changes: [change()], offset: 0, limit: 50, total: 1, hasMore: false,
    }), { changes: [change()], offset: 0, limit: 50, total: 1, hasMore: false });
  });

  it('拒绝越界路径、损坏摘要、状态与类型不符及不一致分页', () => {
    for (const invalid of [
      { changes: [change({ path: '../outside' })], offset: 0, limit: 50, total: 1, hasMore: false },
      { changes: [change({ after: { ...after, sha256: 'bad' } })], offset: 0, limit: 50, total: 1, hasMore: false },
      { changes: [change({ kind: 'added' })], offset: 0, limit: 50, total: 1, hasMore: false },
      { changes: [change(), change({ id: 'change-2' })], offset: 0, limit: 50, total: 1, hasMore: false },
      { changes: [change()], offset: 0, limit: 0, total: 1, hasMore: false },
      { changes: [change()], offset: 0, limit: 50, total: 2, hasMore: false },
    ]) assert.throws(() => api.parseAgentShellChangesPage(invalid), api.AgentApiError);
  });

  it('读取变更清单使用分页参数和 no-store', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ changes: [change()], offset: 50, limit: 50, total: 101, hasMore: true }), { status: 200 });
    };
    const result = await api.getAgentShellChanges('plan/1', 50, 50);
    assert.equal(request.url, '/api/v1/agent/plans/plan%2F1/shell-changes?offset=50&limit=50');
    assert.equal(request.init.cache, 'no-store');
    assert.equal(result.hasMore, true);
  });

  it('文本差异只按需读取并校验 ID；二进制预览会被拒绝', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ change: change({ id: 'change/1' }), textDiff: '--- a\n+++ b' }), { status: 200 });
    };
    const result = await api.getAgentShellChangePreview('plan-1', 'change/1', 'docs/readme.md');
    assert.equal(request.url, '/api/v1/agent/plans/plan-1/shell-changes/change%2F1/preview');
    assert.equal(request.init.cache, 'no-store');
    assert.equal(result.textDiff, '--- a\n+++ b');

    globalThis.fetch = async () => new Response(JSON.stringify({ change: change({ binary: true }), textDiff: '' }), { status: 200 });
    await assert.rejects(api.getAgentShellChangePreview('plan-1', 'change-1', 'docs/readme.md'), api.AgentApiError);
  });

  it('前端不提供逐文件审批 API', () => {
    assert.doesNotMatch(source, /setAgentShellChangeApproval/);
  });
});
