import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';

const source = await readFile(new URL('../src/features/agent/ShellChangeReview.tsx', import.meta.url), 'utf8');
const review = await readFile(new URL('../src/features/agent/AgentPlanReview.tsx', import.meta.url), 'utf8');
const styles = await readFile(new URL('../src/features/agent/AgentPage.css', import.meta.url), 'utf8');

describe('Agent Shell 审核界面', () => {
  it('只在用户展开时读取差异，并将服务端文本按纯文本显示', () => {
    assert.match(source, /if \(!previewChangeId\) return undefined/);
    assert.match(source, /getAgentShellChangePreview\(planId, previewChangeId, selectedChange\.path/);
    assert.match(source, /<pre>\{preview\.textDiff \|\| '没有文本差异。'\}<\/pre>/);
    assert.doesNotMatch(source, /dangerouslySetInnerHTML/);
    assert.match(source, /aria-expanded=\{previewOpen\}/);
  });

  it('仅显示新增、修改或删除的变化摘要，二进制项不提供文本预览', () => {
    assert.match(source, /change\.binary \? ' · 二进制文件' : ''/);
    assert.match(source, /!change\.binary && \(/);
    assert.match(source, /批准此项/);
    assert.match(source, /change\.approval === 'pending'/);
    assert.doesNotMatch(source, /拒绝此项/);
  });

  it('保留整份方案拒绝和最后执行确认，并适配窄屏状态摘要', () => {
    assert.match(review, /拒绝整份方案/);
    assert.match(review, /已批准全部文件变化，确认后运行 Shell 命令/);
    assert.match(styles, /@media \(max-width: 700px\)[\s\S]*?\.agent-shell-change-states\s*\{\s*grid-template-columns: minmax\(0, 1fr\);/);
    assert.match(styles, /\.agent-shell-command\s*\{[\s\S]*?overflow-wrap: anywhere/);
  });
});
