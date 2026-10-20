import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/agent/textDiff.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { isUnifiedTextDiff, parseTextDiff } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('Shell unified diff', () => {
  it('为新增和删除行分别计算新旧行号', () => {
    const rows = parseTextDiff('--- a/note.txt\n+++ b/note.txt\n@@ -1,2 +1,2 @@\n keep\n-old\n+new\n');
    assert.deepEqual(rows.slice(2), [
      { text: '@@ -1,2 +1,2 @@', kind: 'hunk' },
      { text: ' keep', kind: 'context', oldLine: 1, newLine: 1 },
      { text: '-old', kind: 'removed', oldLine: 2 },
      { text: '+new', kind: 'added', newLine: 2 },
    ]);
  });

  it('支持空文件新增差异并保留无换行标记', () => {
    const rows = parseTextDiff('--- a/new.txt\n+++ b/new.txt\n@@ -0,0 +1 @@\n+内容\n\\ No newline at end of file\n');
    assert.equal(rows[2]?.kind, 'hunk');
    assert.deepEqual(rows[3], { text: '+内容', kind: 'added', newLine: 1 });
    assert.equal(rows[4]?.kind, 'meta');
  });

  it('空差异返回空行集合', () => {
    assert.deepEqual(parseTextDiff(''), []);
  });

  it('只将标准 unified diff 当作代码差异', () => {
    assert.equal(isUnifiedTextDiff('--- a/note.txt\n+++ b/note.txt\n@@ -0,0 +1 @@\n+内容\n'), true);
    assert.equal(isUnifiedTextDiff('文件较大，仅显示变化摘要。'), false);
  });

  it('将空文件差异头识别为差异，并接受 CRLF 行尾', () => {
    assert.equal(isUnifiedTextDiff('--- a/empty.txt\n+++ b/empty.txt\n'), true);
    assert.deepEqual(parseTextDiff('--- a/note.txt\r\n+++ b/note.txt\r\n@@ -1 +1 @@\r\n-old\r\n+new\r\n').slice(2), [
      { text: '@@ -1 +1 @@', kind: 'hunk' },
      { text: '-old', kind: 'removed', oldLine: 1 },
      { text: '+new', kind: 'added', newLine: 1 },
    ]);
  });

  it('把截断提示识别为元信息而不是文件正文', () => {
    assert.deepEqual(parseTextDiff('[diff truncated]\n'), [{ text: '[diff truncated]', kind: 'meta' }]);
  });
});
