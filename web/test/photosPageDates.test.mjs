import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/photos/PhotosPage.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('PhotosPage.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const helpers = sourceFile.statements.filter((statement) => ts.isFunctionDeclaration(statement)
  && ['dateBoundary', 'photoDay', 'formatCaptureTime', 'photoIndexNotice', 'photoEmptyMessage', 'adjacentPhoto', 'stepPhotoZoom'].includes(statement.name?.text ?? ''));
const { outputText } = ts.transpileModule(`${helpers.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { dateBoundary, photoDay, formatCaptureTime, photoIndexNotice, photoEmptyMessage, adjacentPhoto, stepPhotoZoom };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const photos = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('照片页拍摄日', () => {
  it('将日期筛选边界构造成 UTC 日历日并正确处理跨月', () => {
    assert.equal(photos.dateBoundary('2025-02-28'), '2025-02-28T00:00:00.000Z');
    assert.equal(photos.dateBoundary('2025-02-28', true), '2025-03-01T00:00:00.000Z');
    assert.equal(photos.dateBoundary('2024-02-29', true), '2024-03-01T00:00:00.000Z');
    assert.equal(photos.dateBoundary(''), undefined);
  });

  it('在不同浏览器时区仍按服务端拍摄时间的 UTC 日历字段分组和显示时间', () => {
    const previousTimeZone = process.env.TZ;
    const results = [];
    try {
      for (const timeZone of ['Pacific/Kiritimati', 'America/Los_Angeles']) {
        process.env.TZ = timeZone;
        const entry = { capturedAt: '2025-01-01T00:15:00Z' };
        results.push({
          day: photos.photoDay(entry).key,
          label: photos.photoDay(entry).label,
          time: photos.formatCaptureTime(entry.capturedAt),
        });
      }
    } finally {
      if (previousTimeZone === undefined) delete process.env.TZ;
      else process.env.TZ = previousTimeZone;
    }

    assert.deepEqual(results, [
      { day: '2025-01-01', label: '2025年1月1日星期三', time: '00:15' },
      { day: '2025-01-01', label: '2025年1月1日星期三', time: '00:15' },
    ]);
  });

  it('缺少或无法解析拍摄时间时归入未知日期', () => {
    assert.deepEqual(photos.photoDay({}), { key: 'unknown', label: '拍摄日期未知' });
    assert.deepEqual(photos.photoDay({ capturedAt: 'invalid' }), { key: 'unknown', label: '拍摄日期未知' });
    assert.equal(photos.formatCaptureTime(undefined), '日期未知');
  });

  it('空态只说明下一步，照片存在时才展示排序提示', () => {
    assert.equal(photos.photoIndexNotice('ready'), '按拍摄时间排序；无拍摄时间的照片列在最后。');
    assert.equal(photos.photoIndexNotice('building'), '照片索引尚未完成，结果可能不完整。');
    assert.equal(photos.photoEmptyMessage('ready'), '当前筛选范围内没有照片。');
    assert.equal(photos.photoEmptyMessage('building'), '照片索引完成后，支持格式的图片会显示在这里。');
  });

  it('仅在当前已加载照片列表内按顺序切换', () => {
    const entries = [{ path: 'b.jpg' }, { path: 'a.jpg' }, { path: 'c.jpg' }];
    assert.deepEqual(photos.adjacentPhoto(entries, 'a.jpg', -1), entries[0]);
    assert.deepEqual(photos.adjacentPhoto(entries, 'a.jpg', 1), entries[2]);
    assert.equal(photos.adjacentPhoto(entries, 'b.jpg', -1), null);
    assert.equal(photos.adjacentPhoto(entries, 'c.jpg', 1), null);
    assert.equal(photos.adjacentPhoto(entries, 'missing.jpg', 1), null);
  });

  it('照片缩放按 25% 步进并限制在 50% 到 300%', () => {
    assert.equal(photos.stepPhotoZoom(100, 1), 125);
    assert.equal(photos.stepPhotoZoom(100, -1), 75);
    assert.equal(photos.stepPhotoZoom(50, -1), 50);
    assert.equal(photos.stepPhotoZoom(300, 1), 300);
  });
});
