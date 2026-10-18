import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const { ApiError, archiveFileName, createArchive } = filesApi;

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
}

const items = [{ path: '项目/说明.pdf', version: 'file-v3' }];

describe('创建归档 API', () => {
  it('切换格式时替换旧扩展名', () => {
    assert.equal(archiveFileName('资料.zip', '7z'), '资料.7z');
    assert.equal(archiveFileName('资料.tar.gz', 'zip'), '资料.zip');
  });

  it('显式提交 ZIP 格式、选择、目标目录版本和文件名，并返回自动编号后的名称', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(201, { path: '归档/资料 (1).zip', name: '资料 (1).zip' });
    };

    const result = await createArchive(items, '归档', 'directory-v4', '资料.zip', 'zip');

    assert.equal(request.url, '/api/v1/files/archive/create');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), {
      items,
      destination: '归档',
      destinationVersion: 'directory-v4',
      name: '资料.zip',
      format: 'zip',
    });
    assert.deepEqual(result, { path: '归档/资料 (1).zip', name: '资料 (1).zip' });
  });

  it('支持根目录及单项选择，要求服务端使用 201 成功状态', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(201, { path: '说明.zip', name: '说明.zip' });
    };

    assert.deepEqual(await createArchive(items, '', '', '说明.zip'), { path: '说明.zip', name: '说明.zip' });
    assert.deepEqual(JSON.parse(request.init.body), { items, destination: '', destinationVersion: '', name: '说明.zip', format: 'zip' });

    globalThis.fetch = async () => jsonResponse(200, { path: '说明.zip', name: '说明.zip' });
    await assert.rejects(createArchive(items, '', '', '说明.zip'), (error) => error instanceof ApiError && error.status === 200);
  });

  it('按 7z、TAR 与 TAR.GZ 格式提交名称，并校验服务端返回的扩展名', async () => {
    for (const [format, name] of [['7z', '资料.7z'], ['tar', '资料.tar'], ['tar.gz', '资料.tar.gz']]) {
      let request;
      globalThis.fetch = async (url, init) => {
        request = { url, init };
        return jsonResponse(201, { path: name, name });
      };

      assert.deepEqual(await createArchive(items, '', '', name, format), { path: name, name });
      assert.equal(request.url, '/api/v1/files/archive/create');
      assert.equal(JSON.parse(request.init.body).format, format);

      globalThis.fetch = async () => jsonResponse(201, { path: '資料.zip', name: '資料.zip' });
      await assert.rejects(createArchive(items, '', '', name, format), (error) => error instanceof ApiError && error.status === 201);
    }
  });

  it('发送前拒绝空选择、超量或重复项、缺失版本及无效名称和目标版本', async () => {
    let fetchCount = 0;
    globalThis.fetch = async () => { fetchCount += 1; return jsonResponse(500, {}); };

    await assert.rejects(createArchive([], '', '', '资料.zip'), ApiError);
    await assert.rejects(createArchive(Array.from({ length: 257 }, (_, index) => ({ path: `项目/${index}`, version: 'v1' })), '', '', '资料.zip'), ApiError);
    await assert.rejects(createArchive([items[0], items[0]], '', '', '资料.zip'), ApiError);
    await assert.rejects(createArchive([{ path: '项目/说明.pdf', version: '' }], '', '', '资料.zip'), ApiError);
    await assert.rejects(createArchive(items, '归档', '', '资料.zip'), ApiError);
    await assert.rejects(createArchive(items, '', 'root-version', '资料.zip'), ApiError);
    await assert.rejects(createArchive(items, '', '', '资料'), ApiError);
    await assert.rejects(createArchive(items, '', '', '../资料.zip'), ApiError);
    await assert.rejects(createArchive(items, '', '', '资料.zip', 'tar'), ApiError);
    await assert.rejects(createArchive(items, '', '', '资料.tar', 'invalid'), ApiError);
    assert.equal(fetchCount, 0);
  });

  it('保留 409 冲突并拒绝无效的成功响应', async () => {
    globalThis.fetch = async () => jsonResponse(409, { error: '所选文件状态已变化。' });
    await assert.rejects(createArchive(items, '', '', '资料.zip'), (error) => error instanceof ApiError && error.status === 409 && error.message === '所选文件状态已变化。');

    globalThis.fetch = async () => jsonResponse(201, { path: '资料.zip', name: '资料.tar' });
    await assert.rejects(createArchive(items, '', '', '资料.zip'), (error) => error instanceof ApiError && error.status === 201);
  });
});
