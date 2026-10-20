import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/agent.ts', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('agent.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const declarations = sourceFile.statements.filter((statement) => {
  if (ts.isFunctionDeclaration(statement)) {
    return [
      'isRecord', 'optionalInteger', 'parseOperation', 'parsePlan', 'readResponseError',
      'request', 'readPlanResponse', 'getAgentTask', 'rejectAgentPlan', 'startAgentPlan', 'executeAgentStep', 'executeAgentPlan', 'requestAgentPlanStop', 'reviseAgentPlan',
    ].includes(statement.name?.text ?? '');
  }
  return ts.isClassDeclaration(statement) && statement.name?.text === 'AgentApiError';
});
const helperSource = `${declarations.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { AgentApiError, getAgentTask, rejectAgentPlan, startAgentPlan, executeAgentStep, executeAgentPlan, requestAgentPlanStop, reviseAgentPlan };`;
const { outputText } = ts.transpileModule(helperSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const api = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function plan() {
  return {
    taskId: 'task-1',
    planId: 'plan-1',
    planVersion: 4,
    status: 'awaiting_approval',
    stopRequested: false,
    proposalSource: 'conversation',
    operations: [{
      id: 'operation-1',
      action: 'rename',
      sourcePath: '文档/旧名.txt',
      targetPath: '文档/新名.txt',
      reason: '用户确认的整理目标',
      evidence: '',
      approval: 'pending',
      executionStatus: 'pending',
    }],
  };
}

describe('Agent 方案版本请求', () => {
  it('任务中心按任务 ID 读取最新审批方案', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ ...plan(), taskId: 'task/1' }), { status: 200 });
    };

    const result = await api.getAgentTask('task/1');
    assert.equal(request.url, '/api/v1/agent/tasks/task%2F1');
    assert.equal(request.init.headers.Accept, 'application/json');
    assert.equal(result.taskId, 'task/1');
  });

  it('整份计划拒绝携带 expectedPlanVersion', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify(plan()), { status: 200 });
    };

    const result = await api.rejectAgentPlan('plan/1', 4);
    assert.equal(request.url, '/api/v1/agent/plans/plan%2F1/reject');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { expectedPlanVersion: 4 });
    assert.equal(result.planVersion, 4);
  });

  it('执行确认携带 expectedPlanVersion 并解析执行结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ ...plan(), status: 'completed' }), { status: 200 });
    };

    const result = await api.executeAgentPlan('plan-1', 4);
    assert.equal(request.url, '/api/v1/agent/plans/plan-1/execute');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { expectedPlanVersion: 4 });
    assert.equal(result.status, 'completed');
  });

  it('审核计划后按设置锁定权限模式并开始执行', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({
        ...plan(), status: 'executing', executionMode: 'bypass', reviewedAt: '2026-10-18T01:00:00Z',
      }), { status: 200 });
    };

    const result = await api.startAgentPlan('plan/1', 4, 'bypass', true);
    assert.equal(request.url, '/api/v1/agent/plans/plan%2F1/start');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { expectedPlanVersion: 4, expectedExecutionMode: 'bypass', confirmPermanentDelete: true });
    assert.equal(result.executionMode, 'bypass');
    assert.equal(result.reviewedAt, '2026-10-18T01:00:00Z');
  });

  it('手动模式只提交指定的下一步和当前方案版本', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ ...plan(), status: 'approved', executionMode: 'manual' }), { status: 200 });
    };

    const result = await api.executeAgentStep('plan/1', 'operation/1', 4);
    assert.equal(request.url, '/api/v1/agent/plans/plan%2F1/steps/operation%2F1/execute');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { expectedPlanVersion: 4 });
    assert.equal(result.executionMode, 'manual');
  });

  it('拒绝无效的权限模式或审核时间响应', async () => {
    globalThis.fetch = async () => new Response(JSON.stringify({ ...plan(), executionMode: 'auto' }), { status: 200 });
    await assert.rejects(api.getAgentTask('task-1'), /无效的方案/);
    globalThis.fetch = async () => new Response(JSON.stringify({ ...plan(), reviewedAt: 'yesterday' }), { status: 200 });
    await assert.rejects(api.getAgentTask('task-1'), /无效的方案/);
  });

  it('停止后续操作携带 expectedPlanVersion 并解析服务端状态', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ ...plan(), status: 'executing', stopRequested: true }), { status: 200 });
    };

    const result = await api.requestAgentPlanStop('plan/1', 4);
    assert.equal(request.url, '/api/v1/agent/plans/plan%2F1/stop');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { expectedPlanVersion: 4 });
    assert.equal(result.stopRequested, true);
    assert.equal(result.status, 'executing');
  });

  it('编辑清单使用版本绑定 PUT，并接受服务端返回的新待审核版本', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ ...plan(), planId: 'plan-2', planVersion: 5, proposalSource: 'user-edit' }), { status: 200 });
    };

    const operations = [{
      action: 'move_file', sourcePath: '待整理/a.pdf', targetPath: '归档/a.pdf', reason: '按年份归档', evidence: '',
    }];
    const result = await api.reviseAgentPlan('plan/1', 4, operations);
    assert.equal(request.url, '/api/v1/agent/plans/plan%2F1/revision');
    assert.equal(request.init.method, 'PUT');
    assert.deepEqual(JSON.parse(request.init.body), { expectedPlanVersion: 4, operations });
    assert.equal(result.planId, 'plan-2');
    assert.equal(result.planVersion, 5);
  });

  it('409 原样作为冲突返回，不在 API 层重试写请求', async () => {
    let requests = 0;
    globalThis.fetch = async () => {
      requests += 1;
      return new Response(JSON.stringify({ error: 'plan version changed' }), { status: 409 });
    };

    await assert.rejects(api.rejectAgentPlan('plan-1', 4), (error) => {
      assert.ok(error instanceof api.AgentApiError);
      assert.equal(error.status, 409);
      return true;
    });
    assert.equal(requests, 1);
    await assert.rejects(api.reviseAgentPlan('plan-1', 4, []), (error) => error instanceof api.AgentApiError && error.status === 409);
    assert.equal(requests, 2);
  });
});
