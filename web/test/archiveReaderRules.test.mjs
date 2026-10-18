import assert from 'node:assert/strict';
import test from 'node:test';
import { archivePreviewKind, formatArchiveSize, isSupportedArchiveName, sortArchiveEntries } from '../src/features/reader/archiveReaderRules.ts';

test('仅识别服务端支持浏览的压缩格式', () => {
  for (const name of ['材料.ZIP', '备份.7z', '档案.rar', '源文件.tar', '归档.tar.gz', '归档.tgz']) {
    assert.equal(isSupportedArchiveName(name), true, name);
  }
  for (const name of ['普通数据.gz', '备份.bz2', '备份.xz', '说明.txt']) {
    assert.equal(isSupportedArchiveName(name), false, name);
  }
});

test('压缩包条目按目录优先并按中文名称排序', () => {
  const entries = [
    { name: '10.txt', path: '10.txt', kind: 'file', size: 0, modifiedAt: '' },
    { name: '图片', path: '图片', kind: 'directory', size: 0, modifiedAt: '' },
    { name: '2.txt', path: '2.txt', kind: 'file', size: 0, modifiedAt: '' },
  ];
  assert.deepEqual(sortArchiveEntries(entries).map(({ name }) => name), ['图片', '2.txt', '10.txt']);
});

test('只将现有阅读器支持的普通文件列为可预览', () => {
  const entry = (name, kind = 'file') => ({ name, path: name, kind, size: 1, modifiedAt: '' });
  assert.equal(archivePreviewKind(entry('合同.PDF')), 'pdf');
  assert.equal(archivePreviewKind(entry('表格.xlsx')), 'xlsx');
  assert.equal(archivePreviewKind(entry('合同.docx')), 'office');
  assert.equal(archivePreviewKind(entry('没有支持的.bin')), null);
  assert.equal(archivePreviewKind(entry('合同.pdf', 'unsupported')), null);
});

test('压缩包文件大小使用紧凑单位', () => {
  assert.equal(formatArchiveSize(12), '12 B');
  assert.equal(formatArchiveSize(1536), '1.5 KB');
});
