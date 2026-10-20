import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const apiSource = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const pageSource = await readFile(new URL('../src/features/files/FilesPage.tsx', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(apiSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const filesApi = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const originalFetch = globalThis.fetch;

afterEach(() => { globalThis.fetch = originalFetch; });

function jsonResponse(payload) {
  return new Response(JSON.stringify(payload), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

const options = { limit: 10, cursor: '', query: '', scope: 'current', sort: 'name-asc' };
const volume = {
  name: 'archive', path: 'archive', kind: 'directory', size: 0, modifiedAt: '', mode: 0,
  symlink: false, mountUnavailable: true, readOnly: true, version: '',
};

describe('文件页离线卷与只读状态', () => {
  it('解析并保留 API 的挂载不可用与只读字段，同时拒绝无效字段类型', async () => {
    globalThis.fetch = async () => jsonResponse({
      path: '', entries: [volume], limit: options.limit, hasMore: false, nextCursor: '', indexState: 'ready',
    });
    const listing = await filesApi.listDirectory('', options);
    assert.equal(listing.entries[0].mountUnavailable, true);
    assert.equal(listing.entries[0].readOnly, true);

    for (const invalidVolume of [
      { ...volume, mountUnavailable: 'true' },
      { ...volume, readOnly: 1 },
    ]) {
      globalThis.fetch = async () => jsonResponse({
        path: '', entries: [invalidVolume],
        limit: options.limit, hasMore: false, nextCursor: '', indexState: 'ready',
      });
      await assert.rejects(filesApi.listDirectory('', options), /无效的目录列表/);
    }
  });

  it('标明不可用卷并阻止进入、选择和文件操作', () => {
    assert.match(pageSource, /if \(entry\.mountUnavailable\) return '磁盘不可用'/);
    assert.match(pageSource, /function activateEntry\(entry: FileEntry\) \{\s*setNotice\(''\);\s*if \(entry\.mountUnavailable\) return;/);
    assert.match(pageSource, /disabled=\{entry\.mountUnavailable\}/);
    assert.match(pageSource, /selectableEntries = visibleEntries\.filter\(\(entry\) => !entry\.mountUnavailable\)/);
    assert.match(pageSource, /visibleEntries\.filter\(\(entry\) => entry\.mountUnavailable\)\.forEach\(\(entry\) => next\.delete\(entry\.id\)\)/);
    assert.match(pageSource, /disabled=\{entry\.mountUnavailable\}[\s\S]*?磁盘不可用/);
    assert.match(pageSource, /selectionHasUnavailableEntries/);
    assert.match(pageSource, /!entry\.mountUnavailable && \(/);
    assert.match(pageSource, /磁盘不可用，无法进入/);
    assert.match(pageSource, /磁盘不可用，无法执行文件操作/);
    assert.match(pageSource, /entry\.readOnly\) return entry\.kind === 'directory' \? '文件夹 · 只读' : '文件 · 只读'/);
    assert.match(pageSource, /disabled=\{!entry\.version \|\| entry\.readOnly\}/);
    assert.match(pageSource, /mode === 'move' && selectedEntries\.some\(\(entry\) => entry\.readOnly\)/);
  });
});
