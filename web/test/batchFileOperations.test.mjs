import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const { ApiError, batchCopyFilesToDirectory, batchMoveFilesToDirectory } = filesApi;

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
}

const items = [
  { path: '照片/春游.jpg', version: 'photo-v1' },
  { path: '文档/计划.pdf', version: 'doc-v2' },
];

describe('批量复制与移动 API', () => {
  it('向根目录发送复制请求并接受完整结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, {
        status: 'complete',
        items: [
          { sourcePath: items[0].path, targetPath: '春游.jpg', status: 'copied' },
          { sourcePath: items[1].path, targetPath: '计划.pdf', status: 'included', includedIn: '照片' },
        ],
      });
    };

    const result = await batchCopyFilesToDirectory(items, '', '');
    assert.equal(request.url, '/api/v1/files/batch-copy');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), { items, destination: '', destinationVersion: '' });
    assert.deepEqual(result.items.map(({ status }) => status), ['copied', 'included']);
    assert.equal(result.items[1].includedIn, '照片');
  });

  it('向选定目录发送移动请求并保留 207 逐项结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(207, {
        status: 'partial',
        items: [
          { sourcePath: items[0].path, targetPath: '归档/春游.jpg', status: 'moved' },
          { sourcePath: items[1].path, targetPath: '归档/计划.pdf', status: 'failed', message: '目标文件系统不可用。' },
          { sourcePath: '未开始.txt', targetPath: '', status: 'not_started', message: '前一项失败。' },
        ],
      });
    };
    const requestItems = [...items, { path: '未开始.txt', version: 'other-v1' }];

    const result = await batchMoveFilesToDirectory(requestItems, '归档', 'dir-v4');
    assert.equal(request.url, '/api/v1/files/batch-move');
    assert.deepEqual(JSON.parse(request.init.body), { items: requestItems, destination: '归档', destinationVersion: 'dir-v4' });
    assert.equal(result.status, 'partial');
    assert.deepEqual(result.items.map(({ status }) => status), ['moved', 'failed', 'not_started']);
    assert.equal(result.items[1].message, '目标文件系统不可用。');
  });

  it('提交前拒绝无效项目、缺少版本、无效目标版本和超量请求', async () => {
    let fetchCount = 0;
    globalThis.fetch = async () => { fetchCount += 1; return jsonResponse(500, {}); };
    await assert.rejects(batchCopyFilesToDirectory([], '', ''), ApiError);
    await assert.rejects(batchCopyFilesToDirectory(items.slice(0, 1), '', ''), ApiError);
    await assert.rejects(batchCopyFilesToDirectory([items[0], items[0]], '', ''), ApiError);
    await assert.rejects(batchCopyFilesToDirectory([items[0], { path: 'x.txt', version: '' }], '', ''), ApiError);
    await assert.rejects(batchCopyFilesToDirectory(items, '归档', ''), ApiError);
    await assert.rejects(batchCopyFilesToDirectory(Array.from({ length: 257 }, (_, index) => ({ path: `x/${index}`, version: 'v1' })), '', ''), ApiError);
    assert.equal(fetchCount, 0);
  });

  it('保留服务端错误并拒绝不完整或不一致的成功响应', async () => {
    globalThis.fetch = async () => jsonResponse(409, { error: '项目版本已变化。' });
    await assert.rejects(batchCopyFilesToDirectory(items, '', ''), (error) => error instanceof ApiError && error.status === 409 && error.message === '项目版本已变化。');

    globalThis.fetch = async () => jsonResponse(200, {
      status: 'complete',
      items: [{ sourcePath: items[0].path, targetPath: items[0].path, status: 'copied' }],
    });
    await assert.rejects(batchCopyFilesToDirectory(items, '', ''), (error) => error instanceof ApiError && error.status === 200);

    globalThis.fetch = async () => jsonResponse(207, {
      status: 'partial',
      items: items.map((item) => ({ sourcePath: item.path, targetPath: item.path, status: 'copied' })),
    });
    await assert.rejects(batchCopyFilesToDirectory(items, '', ''), (error) => error instanceof ApiError && error.status === 207);
  });
});
