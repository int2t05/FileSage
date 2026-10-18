// 验证文件页能清楚说明软链接状态。
import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/files/FilesPage.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('FilesPage.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const names = ['linkStatusLabel', 'linkStatusDescription'];
const functions = names.map((name) => {
  const declaration = sourceFile.statements.find((statement) => ts.isFunctionDeclaration(statement)
    && statement.name?.text === name);
  assert.ok(declaration, `应提供 ${name} 软链接状态文案`);
  return declaration.getText(sourceFile);
});
const { outputText } = ts.transpileModule(`${functions.join('\n')}\nexport { ${names.join(', ')} };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { linkStatusLabel, linkStatusDescription } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('文件页软链接状态', () => {
  it('在列表中区分根内、越界、循环、失效和不可用链接', () => {
    assert.equal(linkStatusLabel('internal'), '根目录内');
    assert.equal(linkStatusLabel('outside_root'), '目标越界');
    assert.equal(linkStatusLabel('cycle'), '链接循环');
    assert.equal(linkStatusLabel('dangling'), '目标不存在');
    assert.equal(linkStatusLabel('unavailable'), '状态不可用');
  });

  it('对不可访问或未知状态明确说明不会跟随', () => {
    assert.match(linkStatusDescription('outside_root'), /管理根目录之外，已禁止跟随/);
    assert.match(linkStatusDescription('cycle'), /形成循环，已禁止跟随/);
    assert.match(linkStatusDescription('dangling'), /目标不存在，无法访问/);
    assert.match(linkStatusDescription(undefined), /状态未知，已禁止跟随/);
    assert.match(source, /linkStatusLabel\(entry\.linkStatus\)/);
    assert.match(source, /linkStatusDescription\(activeEntry\.linkStatus\)/);
  });
});
