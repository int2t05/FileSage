import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const originalFetch = globalThis.fetch;

afterEach(() => { globalThis.fetch = originalFetch; });

function transpile(source) {
  return ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  }).outputText;
}

async function loadSource(relativePath, replacements = []) {
  let source = await readFile(new URL(relativePath, import.meta.url), 'utf8');
  for (const [from, to] of replacements) source = source.replace(from, to);
  const output = transpile(source);
  return import(`data:text/javascript;base64,${Buffer.from(output).toString('base64')}`);
}

const filesUrl = `data:text/javascript;base64,${Buffer.from(transpile(await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8'))).toString('base64')}`;
const tasksUrl = `data:text/javascript;base64,${Buffer.from(transpile((await readFile(new URL('../src/api/tasks.ts', import.meta.url), 'utf8')).replace("'./files'", `'${filesUrl}'`))).toString('base64')}`;
const uploadsApi = await loadSource('../src/api/uploads.ts', [["'./tasks'", `'${tasksUrl}'`]]);
const recovery = await loadSource('../src/features/uploads/uploadRecovery.ts');
const dismissals = await loadSource('../src/features/uploads/uploadDismissals.ts');
const nobleURL = import.meta.resolve('@noble/hashes/sha2.js');
const fingerprint = await loadSource('../src/features/uploads/uploadFingerprint.ts', [["'@noble/hashes/sha2.js'", `'${nobleURL}'`]]);
const uploadPanelSource = await readFile(new URL('../src/features/uploads/UploadPanel.tsx', import.meta.url), 'utf8');

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
}

function uploadTask(id) {
  return {
    id,
    kind: 'upload',
    status: 'running',
    scopePath: '恢复/合同.pdf',
    createdAt: '2026-10-16T00:00:00Z',
    updatedAt: '2026-10-16T00:00:00Z',
  };
}

function publication(uploadId, status, extra = {}) {
  return {
    uploadId,
    status,
    size: 120,
    offset: status === 'receiving' ? 80 : 120,
    targetPath: '恢复/合同.pdf',
    updatedAt: '2026-10-16T00:00:00Z',
    ...extra,
  };
}

function memoryStorage() {
  const values = new Map();
  return {
    getItem(key) { return values.get(key) ?? null; },
    setItem(key, value) { values.set(key, value); },
  };
}

describe('上传刷新恢复', () => {
  it('路由卸载只解除界面订阅，活动 tus 任务与显式取消状态保留', async () => {
    const { createUploadTaskStore, uploadCancellationRequests } = await import('../src/features/uploads/uploadTaskStore.ts');
    const store = createUploadTaskStore();
    const upload = { aborted: false };
    store.set([{ id: 'active-upload', phase: 'transferring', upload, bytesSent: 10 }]);

    let notifications = 0;
    const unsubscribe = store.subscribe(() => { notifications += 1; });
    unsubscribe();
    store.set((current) => current.map((task) => ({ ...task, bytesSent: 80 })));

    const resumedTasks = store.getSnapshot();
    assert.equal(resumedTasks[0].upload, upload);
    assert.equal(resumedTasks[0].bytesSent, 80);
    assert.equal(upload.aborted, false);
    assert.equal(notifications, 0);

    const nextNotifications = [];
    const unsubscribeAgain = store.subscribe(() => nextNotifications.push(store.getSnapshot()));
    assert.equal(store.getSnapshot()[0].phase, 'transferring');
    uploadCancellationRequests.add('active-upload');
    assert.equal(uploadCancellationRequests.has('active-upload'), true);
    store.set((current) => current.map((task) => ({ ...task, phase: 'cancelling' })));
    assert.equal(nextNotifications.length, 1);
    assert.equal(nextNotifications[0][0].phase, 'cancelling');
    unsubscribeAgain();
    uploadCancellationRequests.delete('active-upload');

    const lifecycle = uploadPanelSource.match(/useEffect\(\(\) => \{\s*mountedRef\.current = true;([\s\S]*?)\n  \}, \[\]\);/)?.[1] ?? '';
    assert.match(uploadPanelSource, /useSyncExternalStore\(uploadTaskStore\.subscribe, uploadTaskStore\.getSnapshot/);
    assert.doesNotMatch(lifecycle, /\.abort\(/);
    assert.doesNotMatch(lifecycle, /uploadCancellationRequests\.add/);
    assert.match(uploadPanelSource, /uploadCancellationRequests\.add\(task\.id\)/);
    assert.match(uploadPanelSource, /await upload\?\.abort\(true\)/);
  });

  it('刷新后从任务列表恢复已发布、失败和待重选续传状态', async () => {
    const statuses = new Map([
      ['received', publication('received', 'receiving')],
      ['published', publication('published', 'published', { publishedPath: '归档/合同.pdf' })],
      ['failed', publication('failed', 'failed', { error: '目标文件已变化。' })],
    ]);
    globalThis.fetch = async (url) => {
      if (String(url).startsWith('/api/v1/tasks?')) {
        return jsonResponse(200, { tasks: ['received', 'published', 'failed'].map(uploadTask), limit: 50, offset: 0, hasMore: false });
      }
      const id = String(url).split('/').at(-1);
      return jsonResponse(200, statuses.get(id));
    };

    const records = await uploadsApi.listUploadRecoveries();
    assert.deepEqual(records.map(({ task, publication: state }) => [task.id, recovery.recoveryPhase(state.status)]), [
      ['received', 'resume-needed'],
      ['published', 'published'],
      ['failed', 'failed'],
    ]);
    assert.equal(records[0].publication.offset, 80);
    assert.equal(records[1].publication.publishedPath, '归档/合同.pdf');
    assert.equal(records[2].publication.error, '目标文件已变化。');
  });

  it('清除的已完成上传在刷新恢复时保持隐藏', () => {
    const storage = memoryStorage();
    const publishedID = '0123456789abcdef0123456789abcdef';
    const cancelledID = 'fedcba9876543210fedcba9876543210';
    const records = [publishedID, cancelledID, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'].map((id) => ({ task: uploadTask(id) }));

    assert.equal(dismissals.dismissUploadIds([publishedID, cancelledID], storage), true);
    assert.deepEqual(dismissals.filterDismissedUploadRecoveries(records, storage).map(({ task }) => task.id), [
      'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    ]);
    assert.match(uploadPanelSource, /filterDismissedUploadRecoveries\(records\)/);
    assert.match(uploadPanelSource, /dismissUploadIds\(uploadIDs\)/);
  });

  it('浏览器存储不可用时不清除可由服务端恢复的任务', () => {
    const records = [{ task: uploadTask('0123456789abcdef0123456789abcdef') }];
    assert.equal(dismissals.dismissUploadIds([records[0].task.id], undefined), false);
    assert.deepEqual(dismissals.filterDismissedUploadRecoveries(records, undefined), records);
  });

  it('状态接口暂时不可读时保留任务并区分不可确认与服务端明确不存在', async () => {
    globalThis.fetch = async (url) => {
      if (String(url).startsWith('/api/v1/tasks?')) {
        return jsonResponse(200, { tasks: [uploadTask('offline'), uploadTask('missing')], limit: 50, offset: 0, hasMore: false });
      }
      return String(url).endsWith('/offline')
        ? jsonResponse(503, { error: '服务暂不可用。' })
        : jsonResponse(404, { error: '上传记录不存在。' });
    };

    const records = await uploadsApi.listUploadRecoveries();
    assert.equal(records[0].task.id, 'offline');
    assert.equal(records[0].errorStatus, 503);
    assert.match(records[0].error, /服务暂不可用/);
    assert.equal(records[1].task.id, 'missing');
    assert.equal(records[1].errorStatus, 404);
  });

  it('服务端状态映射保持取消、发布中和未知状态的含义', () => {
    assert.equal(recovery.recoveryPhase('queued'), 'publishing');
    assert.equal(recovery.recoveryPhase('cancelling'), 'cancelling');
    assert.equal(recovery.recoveryPhase('cancelled'), 'cancelled');
    assert.equal(recovery.recoveryPhase('partial'), 'partial');
    assert.equal(recovery.recoveryPhase('new-server-state'), 'unavailable');
  });

  it('同名同长度但内容不同的文件具有不同 tus 指纹', async () => {
    const makeFile = (body) => {
      const blob = new Blob([body], { type: 'application/pdf' });
      return { name: '合同.pdf', size: blob.size, type: blob.type, slice: blob.slice.bind(blob) };
    };
    const first = makeFile('合同 A');
    const second = makeFile('合同 B');
    assert.equal(first.size, second.size);
    const create = (file) => fingerprint.createUploadFingerprint(file, '文档', '合同.pdf', () => {}, () => false);
    assert.notEqual(await create(first), await create(second));
    assert.equal(await create(first), await create(makeFile('合同 A')));
    assert.match(uploadPanelSource, /fingerprint: getFingerprint/);
    assert.match(uploadPanelSource, /await upload\.findPreviousUploads\(\)/);
  });

  it('续传候选仍须来自 tus 指纹索引并匹配大小、元数据与本机上传端点', () => {
    const expected = { filename: '合同.pdf', target: '文档', relativepath: '合同.pdf', filetype: 'application/pdf' };
    const candidate = {
      size: 120,
      creationTime: '2026-10-16T00:00:00Z',
      urlStorageKey: 'key',
      uploadUrl: `${globalThis.location?.origin ?? 'http://localhost'}/api/v1/uploads/0123456789abcdef0123456789abcdef`,
      metadata: { ...expected },
      parallelUploadUrls: null,
    };
    const origin = 'http://localhost';
    assert.equal(recovery.findMatchingPreviousUpload([candidate], 120, expected, origin), candidate);
    assert.equal(recovery.findMatchingPreviousUpload([], 120, expected, origin), undefined);
    assert.equal(recovery.findMatchingPreviousUpload([candidate], 121, expected, origin), undefined);
    assert.equal(recovery.findMatchingPreviousUpload([candidate], 120, { ...expected, target: '其他' }, origin), undefined);
    assert.equal(recovery.findMatchingPreviousUpload([{ ...candidate, uploadUrl: 'https://example.com/api/v1/uploads/0123456789abcdef0123456789abcdef' }], 120, expected, origin), undefined);
  });

  it('发布重试继续使用服务端 POST，取消状态不会折叠为成功', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, publication('retry-id', 'queued'));
    };
    const result = await uploadsApi.retryUploadPublication('retry-id');
    assert.equal(request.url, '/api/v1/uploads/status/retry-id/retry');
    assert.equal(request.init.method, 'POST');
    assert.equal(result.status, 'queued');
    assert.equal(recovery.recoveryPhase('cancelled'), 'cancelled');
    assert.match(uploadPanelSource, /await upload\?\.abort\(true\)/);
  });
});
