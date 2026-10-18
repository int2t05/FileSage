// 验证操作记录接口解析、游标分页与展示映射。
import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/audit.ts', import.meta.url), 'utf8');
const filesModule = `data:text/javascript;base64,${Buffer.from(`export class ApiError extends Error { constructor(message, status) { super(message); this.name = 'ApiError'; this.status = status; } }`).toString('base64')}`;
const compiled = ts.transpileModule(source.replace("from './files'", `from '${filesModule}'`), {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const audit = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`);

const sampleEvent = {
  id: 17,
  taskId: 'task-1',
  eventType: 'operation_finished',
  actor: 'agent',
  outcome: 'succeeded',
  sourcePath: '文档/待整理/合同.pdf',
  targetPath: '文档/合同.pdf',
  createdAt: '2026-10-16T08:00:00Z',
};

test('解析全局操作记录并忽略正文等非展示字段', () => {
  const page = audit.parseAuditPage({
    events: [{ ...sampleEvent, message: '不应进入前端', requestText: 'PRIVATE' }],
    limit: 50,
    nextCursor: 17,
    hasMore: true,
  });

  assert.deepEqual(page, {
    events: [sampleEvent],
    limit: 50,
    nextCursor: 17,
    hasMore: true,
  });
});

test('拒绝结构不完整的操作记录响应', () => {
  assert.throws(() => audit.parseAuditPage({ events: [], limit: 50, nextCursor: '17', hasMore: true }));
  assert.throws(() => audit.parseAuditPage({ events: [{ ...sampleEvent, id: 1.5 }], limit: 50, nextCursor: 17, hasMore: true }));
});

test('首屏和后续请求使用 before 游标分页', async () => {
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, init) => {
    requests.push({ url, init });
    return new Response(JSON.stringify({ events: [sampleEvent], limit: 50, nextCursor: 17, hasMore: true }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };

  try {
    await audit.getAuditHistory(50);
    await audit.getAuditHistory(50, 17);
  } finally {
    globalThis.fetch = originalFetch;
  }

  assert.deepEqual(requests.map(({ url }) => url), [
    '/api/v1/audit?limit=50',
    '/api/v1/audit?limit=50&before=17',
  ]);
  assert.equal(requests[0].init.headers.Accept, 'application/json');
});

test('操作名称使用白名单映射，未知事件统一显示文件操作', () => {
  assert.equal(audit.auditEventLabel('upload.publication'), '上传文件');
  assert.equal(audit.auditEventLabel('manual_create_directory'), '新建文件夹');
  assert.equal(audit.auditEventLabel('manual_batch_rename'), '批量重命名文件');
  assert.equal(audit.auditEventLabel('manual_archive_create'), '创建压缩包');
  assert.equal(audit.auditEventLabel('operation_recovered'), '恢复整理操作状态');
  assert.equal(audit.auditEventLabel('operation_finished'), '整理操作执行完成');
  assert.equal(audit.auditEventLabel('plan_stop_requested'), '请求停止后续操作');
  assert.equal(audit.auditEventLabel('operation_cancelled'), '取消未执行操作');
  assert.equal(audit.auditEventLabel('plan_execution_stopped'), '停止后续操作');
  assert.equal(audit.auditEventLabel('duplicate_scan_queued'), '重复扫描已排队');
  assert.equal(audit.auditEventLabel('duplicate_scan_started'), '开始重复文件扫描');
  assert.equal(audit.auditEventLabel('duplicate_scan_finished'), '重复文件扫描结束');
  assert.equal(audit.auditEventLabel('future_unknown_event'), '文件操作');
  assert.equal(audit.auditEventLabel('constructor'), '文件操作');
  assert.equal(audit.auditOutcomeLabel('succeeded'), '已完成');
  assert.equal(audit.auditOutcomeLabel('queued'), '排队中');
  assert.equal(audit.auditOutcomeLabel('manual_review'), '需人工复核');
  assert.equal(audit.auditOutcomeLabel('invalidated'), '需重新审批');
  assert.equal(audit.auditOutcomeLabel('cancelled'), '已取消');
  assert.equal(audit.auditOutcomeLabel('requested'), '已请求停止');
  assert.equal(audit.auditOutcomeLabel('stopped'), '已停止');
  assert.equal(audit.auditActorLabel('user'), '用户');
  assert.equal(audit.auditActorLabel('system'), '系统');
});
