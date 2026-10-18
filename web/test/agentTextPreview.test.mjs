import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const apiSource = await readFile(new URL('../src/api/agent.ts', import.meta.url), 'utf8');
const apiFile = ts.createSourceFile('agent.ts', apiSource, ts.ScriptTarget.Latest, true);
const helperNames = new Set([
  'optionalInteger', 'isRecord', 'isSafeRootRelativePath', 'isSupportedTextPath', 'textOperationPath',
  'isShellWorkingDirectory', 'isShellCommand', 'parseOperation', 'parsePlan', 'readResponseError', 'request',
  'readPlanResponse', 'getAgentOperationPreview', 'reviseAgentOperationContent',
]);
const declarations = apiFile.statements.filter((statement) =>
  (ts.isFunctionDeclaration(statement) && helperNames.has(statement.name?.text ?? ''))
  || (ts.isClassDeclaration(statement) && statement.name?.text === 'AgentApiError'));
const compiledApi = ts.transpileModule(`${declarations.map((statement) => statement.getText(apiFile)).join('\n')}\nexport { AgentApiError, getAgentOperationPreview, reviseAgentOperationContent };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const api = await import(`data:text/javascript;base64,${Buffer.from(compiledApi.outputText).toString('base64')}`);
const { AgentApiError, getAgentOperationPreview, reviseAgentOperationContent } = api;

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function planPayload(planVersion = 2, operationId = 'operation-new', taskId = 'task-1', action = 'edit_text') {
  const path = action === 'create_file' ? '文档/新建.md' : '文档/说明.md';
  const sourcePath = action === 'create_file' ? '文档' : path;
  const targetPath = action === 'create_file' ? path : path;
  return {
    taskId,
    planId: 'plan-1',
    planVersion,
    status: 'awaiting_approval',
    stopRequested: false,
    proposalSource: 'conversation',
    operations: [{
      id: operationId,
      action,
      sourcePath,
      targetPath,
      reason: '修正内容',
      evidence: '用户要求',
      approval: 'pending',
      executionStatus: 'pending',
    }],
  };
}

describe('Agent 文本候选预览与保存 API', () => {
  it('仅以 no-store 请求预览，并校验方案、操作和文件路径', async () => {
    let requestedUrl = '';
    let requestedInit;
    globalThis.fetch = async (url, init) => {
      requestedUrl = String(url);
      requestedInit = init;
      return new Response(JSON.stringify({
        planId: 'plan/1',
        operationId: 'operation/1',
        path: '文档/说明.md',
        sourceContent: '<script>alert(1)</script>',
        proposedContent: '修正后的正文',
      }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    };

    const result = await getAgentOperationPreview('plan/1', 'operation/1', 7, 'edit_text', '文档/说明.md');
    assert.match(requestedUrl, /\/api\/v1\/agent\/plans\/plan%2F1\/operations\/operation%2F1\/preview\?expectedPlanVersion=7$/);
    assert.equal(requestedInit.cache, 'no-store');
    assert.equal(result.sourceContent, '<script>alert(1)</script>');
    assert.equal(result.proposedContent, '修正后的正文');

    for (const field of ['planId', 'operationId', 'path']) {
      globalThis.fetch = async () => new Response(JSON.stringify({
        planId: 'plan/1', operationId: 'operation/1', path: '文档/说明.md',
        sourceContent: '原文', proposedContent: '候选', [field]: 'mismatch',
      }), { status: 200 });
      await assert.rejects(
        getAgentOperationPreview('plan/1', 'operation/1', 7, 'edit_text', '文档/说明.md'),
        /无效或不匹配的修改预览/,
      );
    }
  });

  it('将文件冲突作为可识别的 409 返回', async () => {
    globalThis.fetch = async () => new Response(JSON.stringify({ error: 'file changed' }), { status: 409 });
    await assert.rejects(
      getAgentOperationPreview('plan-1', 'operation-1', 1, 'overwrite_file', '文档/说明.md'),
      (error) => error instanceof AgentApiError && error.status === 409,
    );
  });

  it('候选正文只发送到 content PUT，并要求新方案版本与操作 ID', async () => {
    let requestedUrl = '';
    let requestedInit;
    globalThis.fetch = async (url, init) => {
      requestedUrl = String(url);
      requestedInit = init;
      return new Response(JSON.stringify(planPayload()), { status: 200 });
    };

    const updated = await reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'edit_text', '文档/说明.md', '候选正文');
    assert.equal(requestedUrl, '/api/v1/agent/plans/plan-1/operations/operation-old/content');
    assert.equal(requestedInit.method, 'PUT');
    assert.deepEqual(JSON.parse(requestedInit.body), { expectedPlanVersion: 1, proposedContent: '候选正文' });
    assert.equal(updated.planVersion, 2);
    assert.equal(updated.planId, 'plan-1');
    assert.equal(updated.operations[0].id, 'operation-new');

    globalThis.fetch = async () => new Response(JSON.stringify(planPayload(1, 'operation-old')), { status: 200 });
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'edit_text', '文档/说明.md', '候选正文'),
      /未返回更新后的修改方案/,
    );

    globalThis.fetch = async () => new Response(JSON.stringify(planPayload(2, 'operation-new', 'other-task')), { status: 200 });
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'edit_text', '文档/说明.md', '候选正文'),
      /未返回更新后的修改方案/,
    );

    globalThis.fetch = async () => new Response(JSON.stringify({ ...planPayload(), planId: 'plan-other' }), { status: 200 });
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'edit_text', '文档/说明.md', '候选正文'),
      /未返回更新后的修改方案/,
    );
  });

  it('新建预览不返回原文，覆盖预览展示读取到的全文', async () => {
    for (const [action, path, sourceContent] of [
      ['create_file', '文档/新建.md', ''],
      ['overwrite_file', '文档/说明.md', '完整原文'],
    ]) {
      globalThis.fetch = async () => new Response(JSON.stringify({
        planId: 'plan-1', operationId: 'operation-1', path, sourceContent, proposedContent: '候选正文',
      }), { status: 200 });
      const preview = await getAgentOperationPreview('plan-1', 'operation-1', 1, action, path);
      assert.equal(preview.sourceContent, sourceContent);
      assert.equal(preview.proposedContent, '候选正文');
    }

    globalThis.fetch = async () => new Response(JSON.stringify({
      planId: 'plan-1', operationId: 'operation-1', path: '文档/新建.md', sourceContent: '不应读取', proposedContent: '候选',
    }), { status: 200 });
    await assert.rejects(
      getAgentOperationPreview('plan-1', 'operation-1', 1, 'create_file', '文档/新建.md'),
      /无效或不匹配的修改预览/,
    );
  });

  it('保存新建或覆盖候选后要求新版本、新操作及同一动作路径待审', async () => {
    for (const [action, path] of [
      ['create_file', '文档/新建.md'],
      ['overwrite_file', '文档/说明.md'],
    ]) {
      globalThis.fetch = async () => new Response(JSON.stringify(planPayload(2, 'operation-new', 'task-1', action)), { status: 200 });
      const updated = await reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, action, path, '人工修订候选');
      assert.equal(updated.operations[0].action, action);
      assert.equal(updated.operations[0].approval, 'pending');
    }

    globalThis.fetch = async () => new Response(JSON.stringify(planPayload(2, 'operation-new', 'task-1', 'edit_text')), { status: 200 });
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'overwrite_file', '文档/说明.md', '候选正文'),
      /未返回更新后的修改方案/,
    );
  });

  it('保存候选限制 UTF-8 字节数并拒绝路径错配', async () => {
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'create_file', '../越界.md', '候选'),
      /方案版本、文件路径或正文无效/,
    );
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'overwrite_file', '文档/说明.bin', '候选'),
      /方案版本、文件路径或正文无效/,
    );
    await assert.rejects(
      reviseAgentOperationContent('plan-1', 'task-1', 'operation-old', 1, 'create_file', '文档/新建.md', 'a'.repeat(32 * 1024 + 1)),
      /方案版本、文件路径或正文无效/,
    );
  });

  it('预览正文以纯文本呈现且只保存在组件状态中', async () => {
    const source = await readFile(new URL('../src/features/agent/AgentPlanReview.tsx', import.meta.url), 'utf8');
    assert.match(source, /if \(!open\) return undefined/);
    assert.match(source, /<pre>\{operation\.action === 'create_file' \? '[^']*' : preview\.sourceContent\}<\/pre>/);
    assert.match(source, /<textarea[^>]*value=\{draft\}/);
    assert.match(source, /预览只在用户点击后读取；正文仅保存在当前组件内存/);
    assert.doesNotMatch(source, /dangerouslySetInnerHTML|localStorage|console\.log/);
  });
});
