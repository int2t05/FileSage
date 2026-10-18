import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const { ApiError, createSymlink } = filesApi;
const originalFetch = globalThis.fetch;

afterEach(() => { globalThis.fetch = originalFetch; });

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
}

describe('软链接 API', () => {
  it('提交来源、名称和当前目录，并接受服务端自动编号结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(201, { path: '资料/合同 (1).pdf', name: '合同 (1).pdf' });
    };

    assert.deepEqual(await createSymlink('资料', '合同.pdf', '待整理/合同.pdf'), {
      path: '资料/合同 (1).pdf',
      name: '合同 (1).pdf',
    });
    assert.equal(request.url, '/api/v1/files/symlinks');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), {
      parent: '资料',
      name: '合同.pdf',
      target: '待整理/合同.pdf',
    });
  });

  it('支持根目录并拒绝无效名称或越出管理根目录的路径', async () => {
    let fetchCount = 0;
    globalThis.fetch = async () => {
      fetchCount += 1;
      return jsonResponse(201, { path: '报告.pdf', name: '报告.pdf' });
    };

    assert.deepEqual(await createSymlink('', '报告.pdf', '归档/报告.pdf'), { path: '报告.pdf', name: '报告.pdf' });
    await assert.rejects(createSymlink('', '', '归档/报告.pdf'), ApiError);
    await assert.rejects(createSymlink('', '../报告.pdf', '归档/报告.pdf'), ApiError);
    await assert.rejects(createSymlink('资料', '报告.pdf', '../报告.pdf'), ApiError);
    assert.equal(fetchCount, 1);
  });

  it('保留服务端冲突和服务错误，并拒绝不一致的成功结果', async () => {
    globalThis.fetch = async () => jsonResponse(409, { error: '目录状态已变化。' });
    await assert.rejects(createSymlink('资料', '报告.pdf', '归档/报告.pdf'), (error) =>
      error instanceof ApiError && error.status === 409 && error.message === '目录状态已变化。');

    globalThis.fetch = async () => jsonResponse(503, {});
    await assert.rejects(createSymlink('资料', '报告.pdf', '归档/报告.pdf'), (error) =>
      error instanceof ApiError && error.status === 503 && error.message.includes('暂不可用'));

    globalThis.fetch = async () => jsonResponse(201, { path: '其他/报告.pdf', name: '报告.pdf' });
    await assert.rejects(createSymlink('资料', '报告.pdf', '归档/报告.pdf'), (error) =>
      error instanceof ApiError && error.message.includes('无效的软链接创建结果'));
  });

  it('网络故障时返回可理解的服务连接错误', async () => {
    globalThis.fetch = async () => { throw new TypeError('offline'); };
    await assert.rejects(createSymlink('资料', '报告.pdf', '归档/报告.pdf'), (error) =>
      error instanceof ApiError && error.message.includes('无法连接 FileSage 服务'));
  });
});
