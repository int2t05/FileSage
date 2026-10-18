import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/reader/ReaderPanel.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('ReaderPanel.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const helpers = sourceFile.statements.filter((statement) =>
  (ts.isTypeAliasDeclaration(statement) && statement.name.text === 'MediaPlaybackStatus')
  || (ts.isFunctionDeclaration(statement) && ['formatCaptureDateTime', 'playbackStatusForEvent', 'formatMediaTime', 'mediaPlaybackLabel'].includes(statement.name?.text ?? '')));
const { outputText } = ts.transpileModule(`${helpers.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { formatCaptureDateTime, playbackStatusForEvent, formatMediaTime, mediaPlaybackLabel };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const media = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('图片与媒体阅读反馈', () => {
  it('将服务端索引的拍摄日期作为可读 EXIF 时间显示，缺失时明确标记', () => {
    assert.equal(media.formatCaptureDateTime('2025-01-01T00:15:06Z'), '2025-01-01 00:15:06');
    assert.equal(media.formatCaptureDateTime(undefined), '未记录');
    assert.equal(media.formatCaptureDateTime('invalid'), '未记录');
    assert.match(source, /kind === 'image'.*reader-image-capture/s);
  });

  it('播放、暂停、等待、拖动和结束事件生成明确状态', () => {
    assert.equal(media.playbackStatusForEvent('loadedmetadata', 'loading', true, false), 'ready');
    assert.equal(media.playbackStatusForEvent('play', 'ready', false, false), 'playing');
    assert.equal(media.playbackStatusForEvent('waiting', 'playing', false, false), 'buffering');
    assert.equal(media.playbackStatusForEvent('seeking', 'paused', true, false), 'seeking');
    assert.equal(media.playbackStatusForEvent('seeked', 'seeking', true, false), 'paused');
    assert.equal(media.playbackStatusForEvent('pause', 'playing', true, false), 'paused');
    assert.equal(media.playbackStatusForEvent('pause', 'playing', true, true), 'ended');
    assert.equal(media.mediaPlaybackLabel('buffering'), '缓冲中');
    assert.match(source, /onWaiting=\{handleMediaEvent\}/);
    assert.match(source, /onSeeking=\{handleMediaEvent\}/);
    assert.match(source, /onEnded=\{handleMediaEvent\}/);
  });

  it('以可读的分秒或时分秒显示播放时间，未知时长不显示为零', () => {
    assert.equal(media.formatMediaTime(0), '0:00');
    assert.equal(media.formatMediaTime(68.9), '1:08');
    assert.equal(media.formatMediaTime(3661), '1:01:01');
    assert.equal(media.formatMediaTime(null), '--:--');
    assert.equal(media.formatMediaTime(Number.POSITIVE_INFINITY), '--:--');
  });
});
