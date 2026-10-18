import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const { ApiError, updateFileMetadata } = filesApi;
const rulesSource = await readFile(new URL('../src/features/files/fileMetadataRules.ts', import.meta.url), 'utf8');
const { outputText: rulesOutput } = ts.transpileModule(rulesSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const metadataRules = await import(`data:text/javascript;base64,${Buffer.from(rulesOutput).toString('base64')}`);
const dialogSource = await readFile(new URL('../src/features/files/FileMetadataDialog.tsx', import.meta.url), 'utf8');
const originalFetch = globalThis.fetch;

afterEach(() => { globalThis.fetch = originalFetch; });

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
}

const entry = {
  name: '说明.txt', path: '文档/说明.txt', kind: 'file', size: 10,
  modifiedAt: '2026-10-16T00:00:00Z', mode: 0o600,
  ownerUid: 1000, groupGid: 100, ownershipAvailable: true,
  symlink: false, version: 'file-v2',
};

describe('文件元数据 API', () => {
  it('使用 PATCH 按版本修改权限并解析服务端条目', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, { entry: { ...entry, mode: 0o644, version: 'file-v3' } });
    };

    assert.deepEqual(await updateFileMetadata({ path: entry.path, version: entry.version, mode: 0o644 }), { ...entry, mode: 0o644, version: 'file-v3' });
    assert.equal(request.url, '/api/v1/files/metadata');
    assert.equal(request.init.method, 'PATCH');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), { path: entry.path, version: entry.version, mode: 0o644 });
  });

  it('属主 UI 投影后只向严格 API 发送 UID/GID，不发送校验标记', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, { entry: { ...entry, ownerUid: 1001, version: 'file-v3' } });
    };

    const parsed = metadataRules.parseOwnerInputs('1001', '');
    assert.equal(parsed.ok, true);
    const fields = metadataRules.ownerChangeFields(parsed);
    assert.deepEqual(fields, { ownerUid: 1001 });
    assert.equal(Object.hasOwn(fields, 'ok'), false);
    assert.match(dialogSource, /ownerChangeFields\(change\)/);

    await updateFileMetadata({ path: entry.path, version: entry.version, ...fields });
    const body = JSON.parse(request.init.body);
    assert.deepEqual(body, { path: entry.path, version: entry.version, ownerUid: 1001 });
    assert.equal(Object.hasOwn(body, 'ok'), false);
  });

  it('请求序列化只使用元数据白名单字段', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, { entry });
    };

    await updateFileMetadata({ path: entry.path, version: entry.version, ownerUid: 1000, ok: true });
    const body = JSON.parse(request.init.body);
    assert.deepEqual(body, { path: entry.path, version: entry.version, ownerUid: 1000 });
    assert.equal(Object.hasOwn(body, 'ok'), false);
  });

  it('409 冲突后将当前对话框锁为只读，并要求关闭后重新确认', () => {
    assert.match(dialogSource, /if \(stale\) return;/);
    assert.match(dialogSource, /status === 409\) \{\s*setStale\(true\);\s*onConflict\(\);\s*\}/);
    assert.equal((dialogSource.match(/readOnly=\{stale\}/g) ?? []).length, 3);
    assert.match(dialogSource, /disabled=\{submitting \|\| stale\}/);
    assert.match(dialogSource, /文件信息已被其他操作修改。请关闭此窗口，基于最新值重新打开后再修改。/);
    assert.equal((dialogSource.match(/onClick=\{close\} disabled=\{submitting\}/g) ?? []).length, 2);
  });

  it('发送前拒绝无效路径、版本、权限、UID/GID 与混合字段', async () => {
    let calls = 0;
    globalThis.fetch = async () => { calls += 1; return jsonResponse(500, {}); };

    await assert.rejects(updateFileMetadata({ path: '../说明.txt', version: 'v1', mode: 0o644 }), ApiError);
    await assert.rejects(updateFileMetadata({ path: entry.path, version: '', mode: 0o644 }), ApiError);
    await assert.rejects(updateFileMetadata({ path: entry.path, version: 'v1', mode: 0o1000 }), ApiError);
    await assert.rejects(updateFileMetadata({ path: entry.path, version: 'v1', ownerUid: -1 }), ApiError);
    await assert.rejects(updateFileMetadata({ path: entry.path, version: 'v1', ownerUid: Number.MAX_SAFE_INTEGER + 1 }), ApiError);
    await assert.rejects(updateFileMetadata({ path: entry.path, version: 'v1' }), ApiError);
    await assert.rejects(updateFileMetadata({ path: entry.path, version: 'v1', mode: 0o644, ownerUid: 1000 }), ApiError);
    assert.equal(calls, 0);
  });

  it('保留服务端冲突与权限错误，并拒绝非 200 或无效条目', async () => {
    globalThis.fetch = async () => jsonResponse(409, { error: '文件版本已变化。' });
    await assert.rejects(updateFileMetadata({ path: entry.path, version: entry.version, mode: 0o644 }), (error) =>
      error instanceof ApiError && error.status === 409 && error.message === '文件版本已变化。');

    globalThis.fetch = async () => jsonResponse(403, { error: '服务账户无权修改属主。' });
    await assert.rejects(updateFileMetadata({ path: entry.path, version: entry.version, ownerUid: 1001 }), (error) =>
      error instanceof ApiError && error.status === 403 && error.message.includes('无权'));

    globalThis.fetch = async () => jsonResponse(201, { entry });
    await assert.rejects(updateFileMetadata({ path: entry.path, version: entry.version, mode: 0o644 }), (error) => error instanceof ApiError && error.status === 201);

    globalThis.fetch = async () => jsonResponse(200, { entry: { ...entry, symlink: true } });
    await assert.rejects(updateFileMetadata({ path: entry.path, version: entry.version, mode: 0o644 }), /无效的文件信息/);

    globalThis.fetch = async () => jsonResponse(200, { entry: { ...entry, ownershipAvailable: undefined } });
    await assert.rejects(updateFileMetadata({ path: entry.path, version: entry.version, mode: 0o644 }), /无效的文件信息/);
  });

  it('网络不可用时提示检查文件服务', async () => {
    globalThis.fetch = async () => { throw new TypeError('offline'); };
    await assert.rejects(updateFileMetadata({ path: entry.path, version: entry.version, mode: 0o644 }), (error) =>
      error instanceof ApiError && error.message.includes('无法连接 FileSage 服务'));
  });
});
