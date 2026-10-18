import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/reader/imagePreview.ts', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('imagePreview.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const preview = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('服务端图片预览地址', () => {
  it('识别需转换的 HEIF 与相机 RAW 格式且忽略大小写', () => {
    for (const name of ['photo.heic', 'photo.HEIF', 'photo.dng', 'photo.CR2', 'photo.cr3', 'photo.nef', 'photo.arw']) {
      assert.equal(preview.requiresImageConversion(name), true, name);
    }
    for (const name of ['photo.jpg', 'photo.png', 'photo.avif', 'report.pdf']) {
      assert.equal(preview.requiresImageConversion(name), false, name);
    }
  });

  it('把文件版本和缩略图规格绑定到安全编码后的查询参数', () => {
    const url = new URL(preview.imagePreviewURL('相册/旅行 照片.heic', 'v1.test', 'thumb'), 'http://filesage.local');
    assert.equal(url.pathname, '/api/v1/files/image-preview');
    assert.equal(url.searchParams.get('path'), '相册/旅行 照片.heic');
    assert.equal(url.searchParams.get('version'), 'v1.test');
    assert.equal(url.searchParams.get('size'), 'thumb');
  });
});
