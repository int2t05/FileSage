import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';

const source = await readFile(new URL('../src/features/agent/ShellChangeReview.tsx', import.meta.url), 'utf8');
const review = await readFile(new URL('../src/features/agent/AgentPlanReview.tsx', import.meta.url), 'utf8');
const styles = await readFile(new URL('../src/features/agent/ShellChangeReview.css', import.meta.url), 'utf8');
const commandStyles = await readFile(new URL('../src/features/agent/AgentPage.css', import.meta.url), 'utf8');
const toolCall = await readFile(new URL('../src/features/agent/ShellToolCall.tsx', import.meta.url), 'utf8');
const toolCallStyles = await readFile(new URL('../src/features/agent/ShellToolCall.css', import.meta.url), 'utf8');

describe('Agent Shell 审核界面', () => {
  it('直接读取文件变化，并将服务端统一差异按纯文本显示', () => {
    assert.match(source, /getAgentShellChangePreview\(planId, openChangeId, selectedChange\.path/);
    assert.match(source, /executionStatus: string/);
    assert.match(source, /planVersion, executionStatus, offset/);
    assert.match(source, /TextDiff value=\{preview\.textDiff\}/);
    assert.match(source, /isUnifiedTextDiff\(value\)/);
    assert.match(source, /agent-shell-diff-marker/);
    assert.match(source, /parseTextDiff\(value\)/);
    assert.match(source, /row\.kind === 'header' \? null : row\.kind === 'hunk'/);
    assert.match(source, /className="agent-shell-diff-line is-hunk"/);
    assert.match(source, /emptyFile \? '空文件，没有文本行差异。'/);
    assert.match(source, /contentRows\.length === 0/);
    assert.match(source, /nextPage\.total === 1 && onlyChange/);
    assert.match(source, /if \(!loading && !error && page\?\.total === 0\) return null/);
    assert.match(source, /agent-shell-diff-number/);
    assert.doesNotMatch(source, /dangerouslySetInnerHTML/);
    assert.match(source, /aria-label="Shell 文件差异"/);
    assert.doesNotMatch(source, /<strong>文件变化<\/strong>|agent-shell-change-action/);
    assert.match(source, /onClick=\{retryChanges\}/);
    assert.doesNotMatch(source, /agent-shell-change-toggle"/);
    assert.match(source, /新增 \$\{added\} 行，删除 \$\{removed\} 行/);
  });

  it('文件行直接打开差异；执行后不伪造已丢弃的旧正文', () => {
    assert.match(source, /agent-shell-change-toggle-file/);
    assert.match(source, /未保留执行前内容，无法显示原始差异/);
    assert.match(source, /change\.binary/);
    assert.match(source, /agent-shell-change-kind is-\$\{change\.kind\}/);
    assert.match(source, /previewOpen \? message \? '收起详情' : '收起差异' : message \? '查看详情' : '查看差异'/);
    assert.doesNotMatch(source, /批准此项|onApprove|change\.approval/);
  });

  it('文件变化直接显示差异；命令和结果使用统一的紧凑工具调用卡片', () => {
    assert.match(review, /拒绝整份方案/);
    assert.match(review, /审阅完成，开始执行/);
    assert.match(review, /onClick=\{onStartPlan\}/);
    assert.match(review, /<ShellToolCall[\s\S]*?command=\{operation\.command \?\? ''\}[\s\S]*?>\s*<ShellChangeReview/);
    assert.match(review, /executionStatus=\{operation\.executionStatus\}/);
    assert.match(toolCall, /Bash 工具调用/);
    assert.match(toolCall, /<summary>[\s\S]*?工具调用/);
    assert.match(toolCall, /<strong>命令<\/strong>/);
    assert.match(toolCall, /<strong>结果<\/strong>/);
    assert.match(toolCall, /outputDiscarded/);
    assert.doesNotMatch(toolCall, /operation\.stdout|operation\.stderr|标准输出|错误输出/);
    assert.match(toolCall, /exitCode/);
    assert.match(toolCall, /shouldShowShellResultSummary\(\{ executionStatus, resultMessage, outputDiscarded \}\)/);
    assert.match(review, /plan\.operations\.length === 0 && \(/);
    assert.match(toolCall, /agent-shell-tool-call-result-label">结果/);
    assert.match(toolCallStyles, /agent-shell-tool-call-result/);
    assert.match(toolCall, /\{children\}/);
    assert.doesNotMatch(toolCall, /<details[^>]*open=/);
    assert.match(styles, /@media \(max-width: 520px\)[\s\S]*?\.agent-shell-change-toggle-file/);
    assert.match(toolCallStyles, /@media \(max-width: 520px\)/);
    assert.match(commandStyles, /\.agent-analysis-card[\s\S]*?\.agent-shell-command/);
  });
});
