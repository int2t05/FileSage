import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/agent/shellToolCallSummary.ts', import.meta.url), 'utf8');
const component = await readFile(new URL('../src/features/agent/ShellToolCall.tsx', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { shellCommandSummary, shellResultSummary, shellStatusLabel, shouldShowShellResultMessage, shouldShowShellResultSummary } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('Shell 工具调用摘要', () => {
  it('把命令和结果收在简短入口，状态由外层步骤展示', () => {
    assert.match(component, /<span>工具调用<\/span>/);
    assert.match(component, /<strong>命令<\/strong>/);
    assert.match(component, /<strong>结果<\/strong>/);
    assert.match(component, /role=\{hasAttention \? 'alert' : 'status'\}/);
    assert.match(component, /showStatus = true/);
    assert.match(component, /\{showStatus && <span className=\{`agent-shell-tool-call-status/);
    assert.match(component, /shouldShowShellResultSummary\(\{ executionStatus, resultMessage, outputDiscarded \}\)/);
    assert.match(component, /shouldShowShellResultMessage\(resultMessage\)/);
    assert.match(component, /没有命令输出/);
    assert.match(component, /!commandShownInSummary && <section className="agent-shell-tool-call-block" aria-label="Shell 命令">/);
    assert.match(component, /shellStatusLabel\(executionStatus\)\}\{exitCode !== undefined/);
    assert.doesNotMatch(component, /purpose/);
  });

  it('保留首行命令并限制摘要长度', () => {
    assert.equal(shellCommandSummary("printf 'ok'\\nprintf 'next'"), "printf 'ok'\\nprintf 'next'");
    assert.equal(shellCommandSummary(`printf '${'命'.repeat(100)}'`).length, 89);
  });

  it('不展示命令正文输出，只显示结果说明、退出码或省略提示', () => {
    const base = { resultMessage: '已完成。', outputDiscarded: false, exitCode: 0, executionStatus: 'succeeded' };
    assert.equal(shellResultSummary(base), '已完成。');
    assert.equal(shellResultSummary({ ...base, resultMessage: '', outputDiscarded: true }), '命令输出已省略 · 退出码 0');
    assert.equal(shellResultSummary({ ...base, resultMessage: '已按审批清单提交全部文件变化。', outputDiscarded: true }), '命令输出已省略 · 退出码 0');
    assert.equal(shellResultSummary({ ...base, resultMessage: '', outputDiscarded: false }), '无输出 · 退出码 0');
    assert.equal(shellResultSummary({ ...base, resultMessage: '', outputDiscarded: false, exitCode: undefined }), '已完成');
    assert.equal(shellResultSummary({ ...base, resultMessage: '', outputDiscarded: false, exitCode: undefined, executionStatus: 'unknown' }), '尚无结果');
  });

  it('成功且无输出时隐藏重复结果句，失败和有输出时保留一行摘要', () => {
    const result = (executionStatus, resultMessage = '', outputDiscarded = false) => shouldShowShellResultSummary({ executionStatus, resultMessage, outputDiscarded });
    assert.equal(result('succeeded', '已按审批清单提交全部文件变化。'), false);
    assert.equal(result('succeeded'), false);
    assert.equal(result('succeeded', '隔离运行完成，没有文件变化。'), true);
    assert.equal(result('succeeded', '', true), true);
    assert.equal(result('failed'), true);
    assert.equal(result('partial'), true);
    assert.equal(shouldShowShellResultMessage('已按审批清单提交全部文件变化。'), false);
    assert.equal(shouldShowShellResultMessage('运行完成，输出 3 个文件。'), true);
  });

  it('部分结果优先显示核查说明，避免把普通输出当成成功结论', () => {
    assert.equal(shellResultSummary({
      resultMessage: '部分变化未能提交，需要核查。',
      outputDiscarded: true, executionStatus: 'partial',
    }), '部分变化未能提交，需要核查。');
  });

  it('将执行状态转换为简短中文', () => {
    assert.equal(shellStatusLabel('succeeded'), '已完成');
    assert.equal(shellStatusLabel('partial'), '需核查');
    assert.equal(shellStatusLabel('cancelled'), '已停止');
    assert.equal(shellResultSummary({ resultMessage: '', outputDiscarded: false, executionStatus: 'failed' }), '命令失败');
    assert.equal(shellStatusLabel('unknown'), '状态未知');
  });
});
