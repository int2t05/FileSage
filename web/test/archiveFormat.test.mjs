import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/api/files.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { archiveFileName } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('归档名称格式', () => {
  it('按格式补齐扩展名', () => {
    assert.equal(archiveFileName('项目资料', 'zip'), '项目资料.zip');
    assert.equal(archiveFileName('项目资料', 'tar'), '项目资料.tar');
    assert.equal(archiveFileName('项目资料', 'tar.gz'), '项目资料.tar.gz');
  });

  it('切换格式时移除旧归档后缀且不会重复', () => {
    assert.equal(archiveFileName('项目资料.zip', 'tar'), '项目资料.tar');
    assert.equal(archiveFileName('项目资料.tar.gz', 'zip'), '项目资料.zip');
    assert.equal(archiveFileName('项目资料.zip.tar.gz.zip', 'tar.gz'), '项目资料.tar.gz');
  });
});
