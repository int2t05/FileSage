import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/files/FilesPage.tsx', import.meta.url), 'utf8');
const styles = await readFile(new URL('../src/features/files/FilesPage.css', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('FilesPage.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const helper = sourceFile.statements.find((statement) => ts.isFunctionDeclaration(statement)
  && statement.name?.text === 'videoPosterUrl');
assert.ok(helper, 'FilesPage must define the versioned video poster URL helper');
const { outputText } = ts.transpileModule(`${helper.getText(sourceFile)}\nexport { videoPosterUrl };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { videoPosterUrl } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('视频网格海报', () => {
  it('使用编码后的相对路径和文件版本请求海报', () => {
    const url = new URL(videoPosterUrl('视频/报告 & 演示.mp4', 'v1.abc_123'), 'http://filesage.local');
    assert.equal(url.pathname, '/api/v1/files/video-poster');
    assert.equal(url.searchParams.get('path'), '视频/报告 & 演示.mp4');
    assert.equal(url.searchParams.get('version'), 'v1.abc_123');
  });

  it('仅在网格中的视频卡片懒加载，并在海报失败时显示视频图标', () => {
    assert.match(source, /entry\.displayKind === 'video'\s*\?\s*<VideoPosterThumbnail/);
    assert.match(source, /loading="lazy"/);
    assert.match(source, /decoding="async"/);
    assert.match(source, /onError=\{\(\) => setFailed\(true\)\}/);
    assert.match(source, /if \(!version \|\| failed\) return <span className="files-grid-video-fallback"><Icon/);
    assert.match(styles, /\.files-grid-art\.is-video-poster\s*\{[^}]*overflow:\s*hidden/s);
    assert.match(styles, /\.files-grid-video-poster\s*\{[^}]*object-fit:\s*cover/s);
  });
});
