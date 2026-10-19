import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const page = await readFile(new URL('../src/features/tasks/TasksPage.tsx', import.meta.url), 'utf8');
const styles = await readFile(new URL('../src/features/tasks/TasksPage.css', import.meta.url), 'utf8');
const pathSemantics = await readFile(new URL('../src/features/agent/pathSemantics.ts', import.meta.url), 'utf8');
const pageFile = ts.createSourceFile('TasksPage.tsx', page, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const pathFile = ts.createSourceFile('pathSemantics.ts', pathSemantics, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const labelsDeclaration = pageFile.statements.find((statement) => ts.isVariableStatement(statement)
  && statement.declarationList.declarations.some((declaration) => declaration.name.getText(pageFile) === 'agentActionLabels'));
const actionLabelDeclaration = pageFile.statements.find((statement) => ts.isFunctionDeclaration(statement)
  && statement.name?.text === 'agentActionLabel');
const sameDirectoryDeclaration = pathFile.statements.find((statement) => ts.isFunctionDeclaration(statement)
  && statement.name?.text === 'isSameDirectoryPath');
assert.ok(labelsDeclaration && actionLabelDeclaration && sameDirectoryDeclaration, '任务操作名称应提供可测试的路径规则');
const { outputText } = ts.transpileModule([
  sameDirectoryDeclaration.getText(pathFile),
  labelsDeclaration.getText(pageFile),
  actionLabelDeclaration.getText(pageFile),
  'export { agentActionLabel };',
].join('\n'), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const helpers = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('任务历史中的 Agent 操作名称', () => {
  it('根据同目录源／目标路径区分改名和跨目录移动', () => {
    assert.equal(helpers.agentActionLabel({ action: 'move_file', sourcePath: 'notes/old.txt', targetPath: 'notes/new.txt' }), '重命名');
    assert.equal(helpers.agentActionLabel({ action: 'move_file', sourcePath: 'notes/a.txt', targetPath: 'archive/a.txt' }), '移动文件');
    assert.equal(helpers.agentActionLabel({ action: 'create_directory', sourcePath: '', targetPath: 'notes/new' }), '新建文件夹');
  });
});

describe('任务记录中的 Shell 审阅', () => {
  it('已完成任务复用文件变化差异，并保留方案版本与步骤状态', () => {
    assert.match(page, /import \{ ShellChangeReview \} from '\.\.\/agent\/ShellChangeReview'/);
    assert.match(page, /<ShellChangeReview[\s\S]*?planId=\{planId\}[\s\S]*?planVersion=\{planVersion\}[\s\S]*?executionStatus=\{operation\.executionStatus\}/);
  });

  it('任务历史将文件 Diff 与命令结果收在同一条 Shell 调用中', () => {
    assert.match(page, /const shellOnly = plan !== undefined && plan\.operations\.length === 1 && plan\.operations\[0\]\?\.action === 'run_shell'/);
    assert.match(page, /!shellOnly && <h3>操作清单<\/h3>/);
    assert.match(page, /shellOnly \? ' is-shell-only' : ''/);
    assert.doesNotMatch(page, /工具调用' : '操作清单/);
    assert.match(page, /!isShell && <div className="tasks-agent-heading"/);
    assert.match(page, /<ShellToolCall[\s\S]*?command=\{operation\.command \?\? ''\}[\s\S]*?>\s*<ShellChangeReview/);
    assert.match(page, /showShellStatus=\{plan\.operations\.length > 1\}/);
    assert.match(page, /showStatus=\{showShellStatus\}/);
    assert.doesNotMatch(page, /purpose=\{operation\.reason\}/);
    assert.match(page, /\.join\('\/'\) \|\| '管理根目录'/);
    assert.match(page, /resultMessage=\{operation\.resultMessage \?\? ''\}/);
    assert.match(page, /executionStatus=\{operation\.executionStatus\}/);
    assert.doesNotMatch(page, /shellApprovalLabel/);
    assert.doesNotMatch(page, /tasks-agent-tool-meta/);
    assert.doesNotMatch(styles, /\.tasks-agent-tool-call/);
    assert.match(styles, /\.tasks-agent-panel\.is-shell-only \.tasks-agent-item\s*\{\s*padding: 0;\s*border: 0;/);
    assert.match(styles, /\.tasks-agent-panel\.is-shell-only \.agent-shell-tool-call\s*\{\s*margin: 0;/);
  });

  it('历史审计默认折叠，并显示记录数量', () => {
    assert.match(page, /<details className="tasks-audit" aria-label="历史审计">/);
    assert.match(page, /className="tasks-audit-count"/);
    assert.match(styles, /\.tasks-audit > summary\s*\{/);
  });
});
