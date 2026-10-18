import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import {
  cancelDuplicateScan,
  createDuplicateCleanupPlan,
  DuplicateScanApiError,
  getDuplicateScan,
  getDuplicateScanFiles,
  getDuplicateScanGroups,
  parseDuplicateScan,
  parseDuplicateScanFilePage,
  parseDuplicateScanGroupPage,
} from '../src/api/duplicateScans.ts';
import {
  canSendAgentMessage,
  parseAgentConversation,
} from '../src/api/agentConversation.ts';

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function scan(overrides = {}) {
  return {
    id: 'scan-1',
    status: 'running',
    phase: 'hashing',
    scopePath: '资料',
    entriesVisited: 12,
    candidateFiles: 6,
    filesHashed: 3,
    bytesHashed: 1536,
    totalBytes: 4096,
    groupCount: 0,
    duplicateFileCount: 0,
    reclaimableBytes: 0,
    errorCode: '',
    updatedAt: '2026-10-16T00:00:00Z',
    ...overrides,
  };
}

describe('重复文件扫描 API 解析', () => {
  it('接受有效状态并只返回界面需要的非摘要字段', () => {
    const parsed = parseDuplicateScan({ ...scan(), sha256: 'must-not-be-returned' });
    assert.deepEqual(parsed, scan());
    assert.equal(Object.hasOwn(parsed, 'sha256'), false);
  });

  it('拒绝未知状态、负计数和超出安全整数的计数', () => {
    assert.throws(() => parseDuplicateScan(scan({ status: 'partial' })), DuplicateScanApiError);
    assert.throws(() => parseDuplicateScan(scan({ filesHashed: -1 })), DuplicateScanApiError);
    assert.throws(() => parseDuplicateScan(scan({ bytesHashed: Number.MAX_SAFE_INTEGER + 1 })), DuplicateScanApiError);
  });

  it('解析组与文件分页，不把额外摘要字段返回给界面', () => {
    assert.deepEqual(parseDuplicateScanGroupPage({
      groups: [{ id: 'g-1', size: 2048, duplicateCount: 3, sha256: 'hidden' }],
      limit: 20,
      offset: 0,
      hasMore: true,
    }), {
      groups: [{ id: 'g-1', size: 2048, duplicateCount: 3 }],
      limit: 20,
      offset: 0,
      hasMore: true,
    });
    assert.deepEqual(parseDuplicateScanFilePage({
      files: [{ path: '资料/a.pdf', size: 2048, modifiedAt: '2026-10-15T12:00:00Z', fingerprint: 'hidden' }],
      limit: 100,
      offset: 0,
      hasMore: false,
    }), {
      files: [{ path: '资料/a.pdf', size: 2048, modifiedAt: '2026-10-15T12:00:00Z' }],
      limit: 100,
      offset: 0,
      hasMore: false,
    });
    assert.throws(() => parseDuplicateScanGroupPage({ groups: [], limit: 0, offset: 0, hasMore: false }), DuplicateScanApiError);
  });

  it('读取状态、分页组、分页路径并发送取消请求', async () => {
    const requests = [];
    globalThis.fetch = async (url, init) => {
      requests.push({ url: String(url), init });
      if (String(url).endsWith('/cancel')) return new Response(null, { status: 204 });
      if (String(url).includes('/groups/g%2F1/files?')) {
        return new Response(JSON.stringify({ files: [], limit: 100, offset: 0, hasMore: false }), { status: 200 });
      }
      if (String(url).includes('/groups?')) {
        return new Response(JSON.stringify({ groups: [], limit: 20, offset: 0, hasMore: false }), { status: 200 });
      }
      return new Response(JSON.stringify(scan()), { status: 200 });
    };

    assert.equal((await getDuplicateScan('scan/1')).id, 'scan-1');
    assert.equal((await getDuplicateScanGroups('scan/1')).hasMore, false);
    assert.equal((await getDuplicateScanFiles('scan/1', 'g/1')).files.length, 0);
    await cancelDuplicateScan('scan/1');
    assert.deepEqual(requests.map(({ url }) => url), [
      '/api/v1/agent/duplicate-scans/scan%2F1',
      '/api/v1/agent/duplicate-scans/scan%2F1/groups?limit=20&offset=0',
      '/api/v1/agent/duplicate-scans/scan%2F1/groups/g%2F1/files?limit=100&offset=0',
      '/api/v1/agent/duplicate-scans/scan%2F1/cancel',
    ]);
    assert.equal(requests[3].init.method, 'POST');
  });

  it('仅提交用户明确选择的保留文件与待回收路径', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ taskId: 'task-1' }), { status: 201 });
    };

    assert.deepEqual(await createDuplicateCleanupPlan('scan/1', '2', 'docs/keep.pdf', ['docs/copy.pdf']), { taskId: 'task-1' });
    assert.equal(request.url, '/api/v1/agent/duplicate-scans/scan%2F1/cleanup-plan');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), {
      groupId: '2', keeperPath: 'docs/keep.pdf', trashPaths: ['docs/copy.pdf'],
    });
  });
});

describe('重复扫描对话契约', () => {
  it('恢复确认范围与已完成扫描 ID，并允许继续自然语言对话', () => {
    const conversation = parseAgentConversation({
      id: 'conversation-1',
      state: 'analysis_complete',
      messages: [{ role: 'assistant', content: '已完成扫描。' }],
      analysisCard: {
        action: 'find_duplicates',
        goal: '查找重复文件',
        filePath: '资料',
        targetPath: '',
        dataTypes: ['文件路径、大小与内容摘要（仅服务端）'],
        provider: 'ollama',
        model: 'qwen3:8b',
        destination: 'local',
      },
      taskId: '',
      planId: '',
      duplicateScanId: 'scan-1',
      error: '',
    });
    assert.equal(conversation.analysisCard.action, 'find_duplicates');
    assert.equal(conversation.analysisCard.filePath, '资料');
    assert.equal(conversation.duplicateScanId, 'scan-1');
    assert.equal(canSendAgentMessage(conversation.state), true);
  });
});
