import assert from 'node:assert/strict';
import test from 'node:test';
import { preserveDraftOnFileChange, watchFileVersion } from '../src/features/reader/fileVersionMonitor.ts';

function headResponse(version, status = 200) {
  return new Response(null, {
    status,
    headers: version ? { 'X-FileSage-Version': version } : {},
  });
}

async function waitFor(predicate) {
  const deadline = Date.now() + 500;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error('条件未在时限内满足');
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

test('版本未变化时只发送无正文 HEAD 检查', async () => {
  const requests = [];
  const stop = watchFileVersion({
    path: '文档/a & b.txt',
    initialVersion: 'version-1',
    intervalMs: 8,
    onExpired: () => assert.fail('相同版本不应过期'),
    fetchImpl: async (url, options) => {
      requests.push({ url, options });
      return headResponse('version-1');
    },
  });

  try {
    await waitFor(() => requests.length >= 2);
  } finally {
    stop();
  }
  assert.match(requests[0].url, /\/api\/v1\/files\/content\?/);
  assert.equal(new URLSearchParams(requests[0].url.split('?')[1]).get('path'), '文档/a & b.txt');
  assert.equal(requests[0].options.method, 'HEAD');
  assert.equal(requests[0].options.body, undefined);
  assert.equal(requests[0].options.cache, 'no-store');
});

test('版本变化时只通知一次', async () => {
  let calls = 0;
  let expiredCount = 0;
  const stop = watchFileVersion({
    path: 'notes/today.md',
    initialVersion: 'version-1',
    intervalMs: 8,
    onExpired: () => expiredCount += 1,
    fetchImpl: async () => headResponse(++calls === 1 ? 'version-1' : 'version-2'),
  });

  try {
    await waitFor(() => expiredCount === 1);
    await new Promise((resolve) => setTimeout(resolve, 20));
  } finally {
    stop();
  }
  assert.equal(expiredCount, 1);
});

test('HEAD 返回文件不存在时提示预览过期', async () => {
  let expiredCount = 0;
  const stop = watchFileVersion({
    path: 'removed.txt',
    initialVersion: 'version-1',
    intervalMs: 8,
    onExpired: () => expiredCount += 1,
    fetchImpl: async () => headResponse(null, 404),
  });

  try {
    await waitFor(() => expiredCount === 1);
    await new Promise((resolve) => setTimeout(resolve, 20));
  } finally {
    stop();
  }
  assert.equal(expiredCount, 1);
});

test('路径切换或卸载时取消轮询中的 HEAD 请求', async () => {
  let oldSignal;
  const oldRequests = [];
  const stopOld = watchFileVersion({
    path: 'old.txt',
    initialVersion: null,
    intervalMs: 8,
    onExpired: () => assert.fail('旧路径不应更新状态'),
    fetchImpl: (url, options) => {
      oldRequests.push(url);
      oldSignal = options.signal;
      return new Promise((resolve, reject) => {
        options.signal.addEventListener('abort', () => reject(new DOMException('取消', 'AbortError')), { once: true });
      });
    },
  });
  await waitFor(() => oldSignal !== undefined);
  stopOld();

  const newRequests = [];
  const stopNew = watchFileVersion({
    path: 'new.txt',
    initialVersion: 'new-version',
    intervalMs: 8,
    onExpired: () => assert.fail('新路径版本未变化'),
    fetchImpl: async (url) => {
      newRequests.push(url);
      return headResponse('new-version');
    },
  });
  try {
    await waitFor(() => newRequests.length > 0);
    await new Promise((resolve) => setTimeout(resolve, 20));
  } finally {
    stopNew();
  }
  assert.equal(oldSignal.aborted, true);
  assert.equal(oldRequests.length, 1);
  assert.ok(newRequests.every((url) => new URLSearchParams(url.split('?')[1]).get('path') === 'new.txt'));
});

test('文件变化冲突保留编辑草稿', () => {
  const draft = '尚未保存的草稿：重要内容';
  const conflict = preserveDraftOnFileChange(draft);
  assert.equal(conflict.draft, draft);
  assert.match(conflict.message, /保存已停用/);
  assert.match(conflict.message, /草稿已保留/);
});
