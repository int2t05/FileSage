import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { afterEach, describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const { ApiError, copyDirectoryToDirectory, copyFileToDirectory, getFileIndexStatus, moveDirectoryToDirectory, moveFileToDirectory } = filesApi;

const directoryRulesSource = await readFile(new URL('../src/features/files/directoryOperationRules.ts', import.meta.url), 'utf8');
const { outputText: directoryRulesOutput } = ts.transpileModule(directoryRulesSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const directoryRules = await import(`data:text/javascript;base64,${Buffer.from(directoryRulesOutput).toString('base64')}`);

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

describe('文件索引状态 API', () => {
  it('读取有效的索引代次和扫描状态', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, { activeGeneration: 4, buildingGeneration: 5, state: 'building' });
    };

    assert.deepEqual(await getFileIndexStatus(), {
      activeGeneration: 4,
      buildingGeneration: 5,
      state: 'building',
    });
    assert.equal(request.url, '/api/v1/index/status');
    assert.equal(request.init.method, undefined);
  });

  it('拒绝无效响应并保留服务端错误', async () => {
    globalThis.fetch = async () => jsonResponse(200, { activeGeneration: -1, buildingGeneration: 0, state: 'ready' });
    await assert.rejects(getFileIndexStatus(), (error) => error instanceof ApiError && error.message.includes('无效的索引状态'));

    globalThis.fetch = async () => jsonResponse(503, { error: '索引暂不可用。' });
    await assert.rejects(getFileIndexStatus(), (error) => error instanceof ApiError && error.status === 503 && error.message === '索引暂不可用。');
  });
});

describe('文件复制与移动 API', () => {
  it('复制时发送源与目标版本，并接受 HTTP 201 的分配路径', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(201, { path: '归档/报告 (1).txt', name: '报告 (1).txt' });
    };

    assert.deepEqual(await copyFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'), {
      path: '归档/报告 (1).txt',
      name: '报告 (1).txt',
    });
    assert.equal(request.url, '/api/v1/files/copy');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), {
      path: '报告.txt',
      version: 'file-v1',
      destination: '归档',
      destinationVersion: 'dir-v2',
    });
  });

  it('文件夹复制使用独立接口并接受服务端分配的同名结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(201, { path: '备份/项目 (1)', name: '项目 (1)' });
    };

    assert.deepEqual(await copyDirectoryToDirectory('项目', 'folder-v3', '备份', 'directory-v8'), {
      path: '备份/项目 (1)',
      name: '项目 (1)',
    });
    assert.equal(request.url, '/api/v1/files/copy-directory');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), {
      path: '项目',
      version: 'folder-v3',
      destination: '备份',
      destinationVersion: 'directory-v8',
    });
  });

  it('文件夹复制只接受 HTTP 201 和有效路径结果', async () => {
    globalThis.fetch = async () => jsonResponse(409, { error: '源文件夹已变化。' });
    await assert.rejects(
      copyDirectoryToDirectory('项目', 'folder-v3', '备份', 'directory-v8'),
      (error) => error instanceof ApiError && error.status === 409 && error.message === '源文件夹已变化。',
    );

    globalThis.fetch = async () => jsonResponse(201, { path: '备份/项目' });
    await assert.rejects(
      copyDirectoryToDirectory('项目', 'folder-v3', '备份', 'directory-v8'),
      (error) => error instanceof ApiError && error.status === 201,
    );
  });

  it('文件夹移动使用独立接口并发送源与目标版本', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, { path: '归档/项目 (1)', name: '项目 (1)' });
    };

    assert.deepEqual(await moveDirectoryToDirectory('项目', 'folder-v3', '归档', 'directory-v8'), {
      path: '归档/项目 (1)',
      name: '项目 (1)',
    });
    assert.equal(request.url, '/api/v1/files/move-directory');
    assert.equal(request.init.method, 'POST');
    assert.equal(request.init.headers['Content-Type'], 'application/json');
    assert.deepEqual(JSON.parse(request.init.body), {
      path: '项目',
      version: 'folder-v3',
      destination: '归档',
      destinationVersion: 'directory-v8',
    });
  });

  it('文件夹移动保留 HTTP 207 的部分完成信息', async () => {
    globalThis.fetch = async () => jsonResponse(207, {
      path: '归档/项目 (1)',
      name: '项目 (1)',
      status: 'partial',
      message: '目标已发布，源文件夹仍保留。',
    });

    assert.deepEqual(await moveDirectoryToDirectory('项目', 'folder-v3', '归档', 'directory-v8'), {
      path: '归档/项目 (1)',
      name: '项目 (1)',
      status: 'partial',
      message: '目标已发布，源文件夹仍保留。',
    });
  });

  it('文件夹移动拒绝非约定状态与无效部分结果', async () => {
    globalThis.fetch = async () => jsonResponse(207, { path: '归档/项目', name: '项目' });
    await assert.rejects(
      moveDirectoryToDirectory('项目', 'folder-v3', '归档', 'directory-v8'),
      (error) => error instanceof ApiError && error.status === 207,
    );

    globalThis.fetch = async () => jsonResponse(200, { path: '归档/项目' });
    await assert.rejects(
      moveDirectoryToDirectory('项目', 'folder-v3', '归档', 'directory-v8'),
      (error) => error instanceof ApiError && error.status === 200,
    );
  });

  it('文件夹复制连接失败时显示服务不可达错误', async () => {
    globalThis.fetch = async () => { throw new TypeError('network'); };
    await assert.rejects(
      copyDirectoryToDirectory('项目', 'folder-v3', '备份', 'directory-v8'),
      (error) => error instanceof ApiError && error.message.includes('无法连接 FileSage 服务'),
    );
  });

  it('移动时接受 HTTP 200 成功和 HTTP 207 的部分结果', async () => {
    globalThis.fetch = async () => jsonResponse(200, { path: '归档/报告.txt', name: '报告.txt' });
    assert.deepEqual(await moveFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'), {
      path: '归档/报告.txt',
      name: '报告.txt',
    });

    globalThis.fetch = async () => jsonResponse(207, {
      path: '归档/报告.txt',
      name: '报告.txt',
      status: 'partial',
      message: '目标已发布，源文件仍保留。',
    });
    assert.deepEqual(await moveFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'), {
      path: '归档/报告.txt',
      name: '报告.txt',
      status: 'partial',
      message: '目标已发布，源文件仍保留。',
    });
  });

  it('目标为管理根目录时发送空目标路径和版本令牌', async () => {
    let body;
    globalThis.fetch = async (_url, init) => {
      body = JSON.parse(init.body);
      return jsonResponse(201, { path: '报告 (1).txt', name: '报告 (1).txt' });
    };

    await copyFileToDirectory('报告.txt', 'file-v1', '', '');
    assert.deepEqual(body, {
      path: '报告.txt',
      version: 'file-v1',
      destination: '',
      destinationVersion: '',
    });
  });

  it('拒绝非约定状态码与无效响应结构', async () => {
    globalThis.fetch = async () => jsonResponse(200, { path: '归档/报告.txt', name: '报告.txt' });
    await assert.rejects(
      copyFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'),
      (error) => error instanceof ApiError && error.status === 200,
    );

    globalThis.fetch = async () => jsonResponse(207, { path: '归档/报告.txt', name: '报告.txt' });
    await assert.rejects(
      moveFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'),
      (error) => error instanceof ApiError && error.status === 207,
    );

    globalThis.fetch = async () => jsonResponse(200, {
      path: '归档/报告.txt',
      name: '报告.txt',
      status: 'partial',
      message: '源文件仍保留。',
    });
    await assert.rejects(
      moveFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'),
      (error) => error instanceof ApiError && error.status === 200,
    );

    globalThis.fetch = async () => jsonResponse(201, { path: '归档/报告.txt' });
    await assert.rejects(
      copyFileToDirectory('报告.txt', 'file-v1', '归档', 'dir-v2'),
      (error) => error instanceof ApiError && error.status === 201,
    );
  });
});

describe('文件夹复制与移动目标规则', () => {
  const { isDirectoryTargetAllowed } = directoryRules;

  it('禁止选择源文件夹及其子目录，允许选择兄弟目录', () => {
    assert.equal(isDirectoryTargetAllowed('copy', '项目', '项目'), false);
    assert.equal(isDirectoryTargetAllowed('copy', '项目', '项目/子目录'), false);
    assert.equal(isDirectoryTargetAllowed('move', '项目', '项目/子目录/更深'), false);
    assert.equal(isDirectoryTargetAllowed('copy', '项目', '归档'), true);
    assert.equal(isDirectoryTargetAllowed('move', '项目', '归档'), true);
  });

  it('阻止移动到源文件夹当前父目录，但复制仍可保留原位置', () => {
    assert.equal(isDirectoryTargetAllowed('move', '部门/项目', '部门'), false);
    assert.equal(isDirectoryTargetAllowed('copy', '部门/项目', '部门'), true);
    assert.equal(isDirectoryTargetAllowed('move', '项目', ''), false);
    assert.equal(isDirectoryTargetAllowed('move', 'organize', 'archive'), true);
  });
});
