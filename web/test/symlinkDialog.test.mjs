import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/files/CreateSymlinkDialog.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('CreateSymlinkDialog.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);

function importFunction(name) {
  const declaration = sourceFile.statements.find((statement) => ts.isFunctionDeclaration(statement)
    && statement.name?.text === name);
  assert.ok(declaration, `应提供 ${name} 校验规则`);
  const { outputText } = ts.transpileModule(`${declaration.getText(sourceFile)}\nexport { ${name} };`, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  });
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
}

const [nameRules, pathRules] = await Promise.all([
  importFunction('validateSymlinkName'),
  importFunction('isRootRelativePath'),
]);

describe('软链接对话框规则', () => {
  it('阻止空名称、路径分隔符、控制字符和超长名称', () => {
    assert.match(nameRules.validateSymlinkName(''), /请输入链接名称/);
    assert.match(nameRules.validateSymlinkName('a/b'), /单个文件名/);
    assert.match(nameRules.validateSymlinkName('a\\b'), /单个文件名/);
    assert.match(nameRules.validateSymlinkName('..'), /单个文件名/);
    assert.match(nameRules.validateSymlinkName('bad\u0000name'), /单个文件名/);
    assert.match(nameRules.validateSymlinkName('字'.repeat(86)), /255 字节/);
    assert.equal(nameRules.validateSymlinkName('报告 (1).pdf'), '');
  });

  it('仅接受管理根目录内的相对路径，当前目录允许根目录', () => {
    assert.equal(pathRules.isRootRelativePath('', true), true);
    assert.equal(pathRules.isRootRelativePath('资料/合同.pdf', false), true);
    assert.equal(pathRules.isRootRelativePath('', false), false);
    assert.equal(pathRules.isRootRelativePath('../合同.pdf', false), false);
    assert.equal(pathRules.isRootRelativePath('/etc/passwd', false), false);
    assert.equal(pathRules.isRootRelativePath('资料//合同.pdf', false), false);
  });

  it('显示链接来源和位置，保留冲突重试，并提供键盘和移动端语义', async () => {
    const css = await readFile(new URL('../src/features/files/CreateSymlinkDialog.css', import.meta.url), 'utf8');
    assert.match(source, /role="dialog"/);
    assert.match(source, /aria-modal="true"/);
    assert.match(source, /新链接路径/);
    assert.match(source, /指向/);
    assert.match(source, /不会复制或修改目标/);
    assert.match(source, /onConflictRef\.current\?\.\(\)/);
    assert.match(source, /event.key === 'Escape'/);
    assert.match(source, /event.key !== 'Tab'/);
    assert.match(source, /form="files-symlink-form"/);
    assert.match(css, /@media \(max-width: 520px\)/);
  });
});
