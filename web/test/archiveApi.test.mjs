import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import { archiveContentURL, archiveEntriesURL, ArchiveApiError, extractArchive, listArchiveEntries } from '../src/api/archive.ts';

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

describe('压缩包 API', () => {
  it('为目录和条目内容编码路径、版本与内部目录', () => {
    assert.equal(
      archiveEntriesURL('资料/报告.zip', 'v 1', '合同/扫描件'),
      '/api/v1/files/archive/entries?path=%E8%B5%84%E6%96%99%2F%E6%8A%A5%E5%91%8A.zip&version=v+1&directory=%E5%90%88%E5%90%8C%2F%E6%89%AB%E6%8F%8F%E4%BB%B6',
    );
    assert.equal(
      archiveContentURL('资料/报告.zip', 'v1', '合同/扫描件.pdf'),
      '/api/v1/files/archive/content?path=%E8%B5%84%E6%96%99%2F%E6%8A%A5%E5%91%8A.zip&version=v1&entry=%E5%90%88%E5%90%8C%2F%E6%89%AB%E6%8F%8F%E4%BB%B6.pdf',
    );
  });

  it('读取并校验当前压缩包版本的目录', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, {
        archivePath: '报告.zip',
        version: 'v1',
        directory: '',
        entries: [{ name: '合同', path: '合同', kind: 'directory', size: 0, modifiedAt: '' }],
      });
    };

    const listing = await listArchiveEntries('报告.zip', 'v1');
    assert.equal(listing.entries[0].name, '合同');
    assert.equal(request.url, archiveEntriesURL('报告.zip', 'v1'));
    assert.equal(request.init.cache, 'no-store');

    globalThis.fetch = async () => jsonResponse(200, {
      archivePath: '报告.zip', version: 'stale', directory: '', entries: [],
    });
    await assert.rejects(listArchiveEntries('报告.zip', 'v1'), ArchiveApiError);
  });

  it('提交选中条目和目标目录，识别完整结果', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url, init };
      return jsonResponse(200, { status: 'complete', paths: ['目标/合同.pdf'], errors: [] });
    };

    assert.deepEqual(await extractArchive('报告.zip', 'v1', '目标', ['合同.pdf']), {
      status: 'complete', paths: ['目标/合同.pdf'], errors: [],
    });
    assert.equal(request.url, '/api/v1/files/archive/extract');
    assert.deepEqual(JSON.parse(request.init.body), {
      path: '报告.zip', version: 'v1', destination: '目标', entries: ['合同.pdf'],
    });
  });

  it('保留部分解压状态和逐项错误，不把它当成完整成功', async () => {
    globalThis.fetch = async () => jsonResponse(207, {
      status: 'partial', paths: ['目标/可读.txt'], errors: [{ entry: '损坏.txt', message: 'CRC 校验失败' }],
    });
    const result = await extractArchive('报告.zip', 'v1', '目标', []);
    assert.equal(result.status, 'partial');
    assert.deepEqual(result.errors, [{ entry: '损坏.txt', message: 'CRC 校验失败' }]);

    globalThis.fetch = async () => jsonResponse(500, { error: '无法解密压缩包。' });
    await assert.rejects(extractArchive('报告.zip', 'v1', '', []), (error) => error instanceof ArchiveApiError && error.message === '无法解密压缩包。');
  });
});
