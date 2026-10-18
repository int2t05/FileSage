import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/tasks/TasksPage.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('TasksPage.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const declaration = sourceFile.statements.find((statement) => ts.isFunctionDeclaration(statement)
  && statement.name?.text === 'taskKindLabel');
assert.ok(declaration, 'TasksPage 应提供任务类型名称映射');
const compiled = ts.transpileModule(`${declaration.getText(sourceFile)}\nexport { taskKindLabel };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const labels = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`);

describe('任务类型显示名称', () => {
  it('将重复扫描任务显示为友好名称，并保留未知类型兜底', () => {
    assert.equal(labels.taskKindLabel('duplicate_scan'), '重复文件扫描');
    assert.equal(labels.taskKindLabel('future_task'), 'future_task 任务');
  });

  it('任务详情与全局操作记录都使用中文审计事件映射', () => {
    assert.equal((source.match(/auditEventLabel\(event\.eventType\)/g) ?? []).length, 2);
    assert.match(source, /<p>\{auditActorLabel\(event\.actor\)\} · \{auditOutcomeLabel\(event\.outcome\)\}<\/p>/);
    assert.doesNotMatch(source, /<p>\{event\.actor\} · \{event\.outcome\}<\/p>/);
  });
});
