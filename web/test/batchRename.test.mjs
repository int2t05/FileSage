import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { afterEach, describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const { ApiError, batchRenameEntries } = filesApi;

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const items = [
  { path: '待整理/a.txt', version: 'a-v1', name: '合同-a.txt' },
  { path: '待整理/b.txt', version: 'b-v1', name: '合同-b.txt' },
];

describe('批量重命名 API', () => {
  it('发送每项的路径、版本和目标名称，并接受完整结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, {
        status: 'completed',
        items: [
          { path: '待整理/a.txt', targetPath: '待整理/合同-a.txt', name: '合同-a.txt', status: 'renamed' },
          { path: '待整理/b.txt', targetPath: '待整理/合同-b.txt', name: '合同-b.txt', status: 'renamed' },
        ],
      });
    };

    assert.deepEqual(await batchRenameEntries(items), {
      status: 'completed',
      items: [
        { path: '待整理/a.txt', targetPath: '待整理/合同-a.txt', name: '合同-a.txt', status: 'renamed' },
        { path: '待整理/b.txt', targetPath: '待整理/合同-b.txt', name: '合同-b.txt', status: 'renamed' },
      ],
    });
    assert.equal(request.url, '/api/v1/files/batch-rename');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), { items });
  });

  it('保留服务端部分结果并展示未开始项目', async () => {
    const requestItems = [...items, { path: '待整理/c.txt', version: 'c-v1', name: '合同-c.txt' }];
    globalThis.fetch = async () => jsonResponse(207, {
      status: 'partial',
      items: [
        { path: requestItems[0].path, targetPath: '待整理/合同-a.txt', name: '合同-a.txt', status: 'renamed' },
        { path: requestItems[1].path, targetPath: '待整理/合同-b.txt', name: '合同-b.txt', status: 'failed', message: '目标已变化。' },
        { path: requestItems[2].path, targetPath: '待整理/合同-c.txt', name: '合同-c.txt', status: 'not_started', message: '前项失败后未执行。' },
      ],
    });

    const result = await batchRenameEntries(requestItems);
    assert.equal(result.status, 'partial');
    assert.deepEqual(result.items.map(({ status }) => status), ['renamed', 'failed', 'not_started']);
    assert.equal(result.items[1].message, '目标已变化。');
  });

  it('提交前拒绝空选、单项、重复路径和超过 256 项', async () => {
    let fetchCount = 0;
    globalThis.fetch = async () => { fetchCount += 1; return jsonResponse(500, {}); };
    await assert.rejects(batchRenameEntries([]), (error) => error instanceof ApiError);
    await assert.rejects(batchRenameEntries(items.slice(0, 1)), (error) => error instanceof ApiError);
    await assert.rejects(batchRenameEntries([items[0], items[0]]), (error) => error instanceof ApiError);
    await assert.rejects(batchRenameEntries(Array.from({ length: 257 }, (_, index) => ({ ...items[0], path: `x/${index}` }))), (error) => error instanceof ApiError);
    assert.equal(fetchCount, 0);
  });

  it('拒绝状态码、状态和逐项结果不一致的响应', async () => {
    globalThis.fetch = async () => jsonResponse(200, { status: 'partial', items: [] });
    await assert.rejects(batchRenameEntries(items), (error) => error instanceof ApiError && error.status === 200);

    globalThis.fetch = async () => jsonResponse(207, {
      status: 'partial',
      items: items.map((item) => ({ path: item.path, targetPath: item.path, name: item.name, status: 'renamed' })),
    });
    await assert.rejects(batchRenameEntries(items), (error) => error instanceof ApiError && error.status === 207);

    globalThis.fetch = async () => jsonResponse(200, {
      status: 'completed',
      items: [
        { path: items[0].path, targetPath: items[0].path, name: items[0].name, status: 'renamed' },
        { path: 'unexpected.txt', targetPath: 'unexpected.txt', name: 'unexpected.txt', status: 'renamed' },
      ],
    });
    await assert.rejects(batchRenameEntries(items), (error) => error instanceof ApiError && error.status === 200);
  });
});
