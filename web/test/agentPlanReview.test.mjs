import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/agent/AgentPlanReview.tsx', import.meta.url), 'utf8');
const agentPageSource = await readFile(new URL('../src/features/agent/AgentPage.tsx', import.meta.url), 'utf8');
const shellReviewSource = await readFile(new URL('../src/features/agent/ShellChangeReview.tsx', import.meta.url), 'utf8');
const shellToolCallSource = await readFile(new URL('../src/features/agent/ShellToolCall.tsx', import.meta.url), 'utf8');
const pathSemanticsSource = await readFile(new URL('../src/features/agent/pathSemantics.ts', import.meta.url), 'utf8');
const pathSemanticsFile = ts.createSourceFile('pathSemantics.ts', pathSemanticsSource, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const sameDirectoryDeclaration = pathSemanticsFile.statements.find((statement) => ts.isFunctionDeclaration(statement) && statement.name?.text === 'isSameDirectoryPath');
assert.ok(sameDirectoryDeclaration, '路径语义应提供同目录判断');
const sourceFile = ts.createSourceFile('AgentPlanReview.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const declarations = sourceFile.statements.filter((statement) => {
  if (ts.isVariableStatement(statement)) {
    return statement.declarationList.declarations.some((declaration) => ['EDIT_ACTION_LABELS', 'EXECUTION_LABELS', 'STEP_STATE_LABELS'].includes(declaration.name.getText(sourceFile)));
  }
  return ts.isFunctionDeclaration(statement)
    && ['statusLabel', 'canExecute', 'canResumeBypassPlan', 'canStartPlanReview', 'executionProgress', 'canStopExecution', 'executionLabel', 'stepPresentationState', 'effectiveExecutionMode', 'shouldShowSafetyNotice', 'requiresTextCandidatePreview', 'canRejectPlan', 'operationPresentation', 'operationPresentationForOperation', 'canEditPlan', 'createPlanEditDrafts', 'cancelPlanEdit', 'shellScopeSummary'].includes(statement.name?.text ?? '');
});
const helperSource = `${sameDirectoryDeclaration.getText(pathSemanticsFile)}\n${declarations.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { isSameDirectoryPath, canExecute, canResumeBypassPlan, canStartPlanReview, executionProgress, canStopExecution, executionLabel, stepPresentationState, effectiveExecutionMode, shouldShowSafetyNotice, requiresTextCandidatePreview, canRejectPlan, operationPresentation, operationPresentationForOperation, canEditPlan, createPlanEditDrafts, cancelPlanEdit, shellScopeSummary };`;
const { outputText } = ts.transpileModule(helperSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const helpers = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('Agent 计划与步骤展示', () => {
  it('使用计划级开始与手动逐步执行回调，不展示逐项批准入口', () => {
    assert.match(source, /executionMode: ExecutionMode/);
    assert.match(source, /onStartPlan: \(\) => void/);
    assert.match(source, /onResumePlan: \(\) => void/);
    assert.match(source, /onRunStep: \(operationId: string\) => void/);
    assert.match(source, /onClick=\{onStartPlan\}/);
    assert.match(source, /onClick=\{\(\) => onRunStep\(operation\.id\)\}/);
    assert.match(source, /<ol className="agent-step-list" aria-label="执行步骤">/);
    assert.doesNotMatch(source, /onDecision|onShellChangeDecision|批准此项/);
  });

  it('待审核计划由开始审核入口启动，审核后的计划才允许执行步骤', () => {
    const pending = { status: 'awaiting_approval', operations: [{ executionStatus: 'pending' }] };
    const approved = { ...pending, status: 'approved' };
    assert.equal(helpers.canStartPlanReview(pending), true);
    assert.equal(helpers.canStartPlanReview(approved), false);
    assert.equal(helpers.canExecute(pending), false);
    assert.equal(helpers.canExecute(approved), true);
    assert.equal(helpers.canResumeBypassPlan({ ...approved, executionMode: 'bypass' }), true);
    assert.equal(helpers.canResumeBypassPlan({ ...approved, executionMode: 'manual' }), false);
    assert.match(agentPageSource, /!canStartPlanReview\(plan\)/);
    assert.match(agentPageSource, /executeAgentPlan\(plan\.planId, plan\.planVersion\)/);
    assert.match(source, /canStartPlanReview\(plan\)/);
  });

  it('按步骤状态显示正在执行、等待、完成和失败', () => {
    const plan = { status: 'executing', operations: [] };
    const statuses = [
      ['running', 'current'],
      ['pending', 'waiting'],
      ['succeeded', 'completed'],
      ['failed', 'failed'],
      ['partial', 'attention'],
      ['unknown', 'attention'],
    ];
    for (const [executionStatus, expected] of statuses) {
      assert.equal(helpers.stepPresentationState(plan, { executionStatus }, 0, 'bypass'), expected);
    }
    assert.equal(helpers.stepPresentationState({ ...plan, status: 'awaiting_approval' }, { executionStatus: 'pending' }, 0, 'bypass'), 'review');
    assert.equal(helpers.stepPresentationState({ ...plan, status: 'approved', operations: [{ executionStatus: 'pending' }] }, { executionStatus: 'pending' }, 0, 'manual'), 'ready');
  });

  it('已审阅计划固定使用审阅时保存的执行模式', () => {
    assert.equal(helpers.effectiveExecutionMode({ reviewedAt: null, executionMode: 'manual' }, 'bypass'), 'bypass');
    assert.equal(helpers.effectiveExecutionMode({ reviewedAt: '2026-10-18T01:00:00Z', executionMode: 'manual' }, 'bypass'), 'manual');
    assert.equal(helpers.effectiveExecutionMode({ reviewedAt: '2026-10-18T01:00:00Z', executionMode: 'unknown' }, 'bypass'), 'manual');
  });

  it('开始前要求审阅文本候选，并将永久删除确认绑定到计划版本', () => {
    assert.equal(helpers.requiresTextCandidatePreview('create_file'), true);
    assert.equal(helpers.requiresTextCandidatePreview('overwrite_file'), true);
    assert.equal(helpers.requiresTextCandidatePreview('edit_text'), false);
    assert.match(source, /mustReviewTextCandidates/);
    assert.match(source, /previewedTextCandidateKey !== `\$\{plan\.planId\}:\$\{plan\.planVersion\}:\$\{operation\.id\}`/);
    assert.match(source, /permanentDeleteConfirmationKey === currentPlanKey/);
    assert.match(source, /我已确认 \{permanentDeleteCount\} 项永久删除无法恢复/);
    assert.match(source, /确认永久删除并执行计划/);
    assert.match(source, /执行后会完整替换原文件，旧内容不可恢复/);
  });

  it('文件差异直接展示，Shell 命令和结果收在折叠的工具调用内', () => {
    assert.match(source, /<ShellToolCall[\s\S]*?command=\{operation\.command \?\? ''\}/);
    assert.match(source, /shellScopeSummary\(operation\)/);
    assert.match(source, /showStatus=\{false\}/);
    assert.match(source, /operation\.action !== 'run_shell' && <div className="agent-step-heading"/);
    assert.doesNotMatch(source, /operation\.action === 'run_shell' && operation\.(reason|resultMessage)/);
    assert.match(shellToolCallSource, /<section className="agent-shell-tool-call"/);
    assert.match(shellToolCallSource, /<details className="agent-shell-tool-call-details">/);
    assert.match(shellToolCallSource, /工具调用/);
    assert.match(shellToolCallSource, /<strong>命令<\/strong>/);
    assert.match(shellToolCallSource, /<strong>结果<\/strong>/);
    assert.match(source, /<ShellToolCall[\s\S]*?>\s*<ShellChangeReview/);
    assert.doesNotMatch(source, /expandedShellCalls/);
    assert.doesNotMatch(source, /shellStepSummary/);
    assert.match(shellReviewSource, /Shell 文件差异/);
    assert.match(shellReviewSource, /TextDiff value=\{preview\.textDiff/);
    assert.match(shellReviewSource, /row\.kind === 'header' \? null : row\.kind === 'hunk'/);
    assert.match(shellReviewSource, /agent-shell-diff-line/);
    assert.doesNotMatch(shellReviewSource, /agent-shell-change-toggle"/);
    assert.doesNotMatch(shellReviewSource, /批准此项|onApprove/);
  });

  it('执行期间展示进度并提供停止后续步骤', () => {
    const plan = {
      status: 'executing', stopRequested: false,
      operations: [
        { executionStatus: 'succeeded' },
        { executionStatus: 'running' },
        { executionStatus: 'pending' },
      ],
    };
    assert.deepEqual(helpers.executionProgress(plan), {
      completed: 1, total: 3, remaining: 1, failed: 0, runningOrdinal: 2,
    });
    assert.equal(helpers.canStopExecution(plan), true);
    assert.equal(helpers.canStopExecution({ ...plan, stopRequested: true }), false);
    assert.match(source, /role="status" aria-live="polite"/);
    assert.match(source, /停止后续操作/);
  });

  it('计划级执行校验不把工具动作白名单或逐项审批状态带入 UI', () => {
    assert.equal(helpers.canExecute({ status: 'approved', operations: [{ action: 'future_tool', approval: 'pending', executionStatus: 'pending' }] }), true);
    assert.equal(helpers.canExecute({ status: 'awaiting_approval', operations: [{ executionStatus: 'pending' }] }), false);
    assert.equal(helpers.canExecute({ status: 'approved', operations: [{ executionStatus: 'succeeded' }] }), false);
    assert.match(helpers.operationPresentation('future_tool', '', '').title, /^其他工具操作 · future_tool$/);
  });

  it('同目录路径变化显示重命名，跨目录路径变化显示移动', () => {
    assert.equal(helpers.isSameDirectoryPath('folder/source.txt', 'folder/target.txt'), true);
    assert.equal(helpers.isSameDirectoryPath('source.txt', 'target.txt'), true);
    assert.equal(helpers.isSameDirectoryPath('folder/source.txt', 'archive/target.txt'), false);
    assert.equal(helpers.isSameDirectoryPath('source.txt', 'source.txt'), false);
    assert.equal(helpers.isSameDirectoryPath('', 'target.txt'), false);
    const rename = helpers.operationPresentation('move_file', 'qa-note.txt', 'qa-note-renamed.txt');
    assert.equal(rename.title, '重命名');
    assert.equal(rename.sourceLabel, '原路径');
    assert.equal(rename.targetLabel, '新路径');

    const move = helpers.operationPresentation('move_file', '待整理/qa-note.txt', '归档/qa-note.txt');
    assert.equal(move.title, '移动文件');
    assert.equal(move.sourceLabel, '从');
    assert.equal(move.targetLabel, '移动到');
  });

  it('保留路径/依据详情、Shell 隔离范围和整份计划拒绝', () => {
    assert.match(source, /查看操作详情/);
    assert.match(source, /查看依据/);
    assert.match(source, /title: 'Bash'/);
    assert.match(source, /ShellChangeReview/);
    assert.match(source, /拒绝整份方案/);
    assert.equal(helpers.operationPresentationForOperation({
      action: 'run_shell', sourcePath: '', workingDir: '', command: 'printf hello',
    }).title, 'Bash');
  });

  it('根目录默认范围不重复展示', () => {
    assert.equal(helpers.shellScopeSummary({ sourcePath: '', workingDir: '.' }), '');
    assert.equal(helpers.shellScopeSummary({ sourcePath: '文档/合同', workingDir: '.' }), '范围：文档/合同');
    assert.equal(helpers.shellScopeSummary({ sourcePath: '文档', workingDir: '合同' }), '范围：文档 · 工作目录：文档/合同');
  });
});
