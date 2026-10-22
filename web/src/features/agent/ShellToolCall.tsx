// 以紧凑摘要展示 Shell 工具调用，并将完整命令与结果折叠。
import { ChevronDown, Terminal } from 'lucide-react';
import type { ReactNode } from 'react';
import { shellCommandSummary, shellResultSummary, shellStatusLabel, shouldShowShellResultMessage, shouldShowShellResultSummary } from './shellToolCallSummary';
import './ShellToolCall.css';

export function ShellToolCall({
  command,
  resultMessage = '',
  stdout = '',
  stderr = '',
  outputDiscarded = false,
  outputTruncated = false,
  exitCode,
  executionStatus,
  scope = '',
  showStatus = true,
  children,
}: {
  command: string;
  resultMessage?: string;
  stdout?: string;
  stderr?: string;
  outputDiscarded?: boolean;
  outputTruncated?: boolean;
  exitCode?: number;
  executionStatus: string;
  scope?: string;
  showStatus?: boolean;
  children?: ReactNode;
}) {
  const commandSummary = shellCommandSummary(command);
  const commandShownInSummary = commandSummary === command.trim();
  const resultSummary = shellResultSummary({
    resultMessage,
    stdout,
    stderr,
    outputDiscarded,
    outputTruncated,
    exitCode,
    executionStatus,
  });
  const showResultSummary = shouldShowShellResultSummary({ executionStatus, resultMessage, stdout, stderr, outputDiscarded, outputTruncated });
  const hasAttention = executionStatus === 'failed' || executionStatus === 'partial';

  return (
    <section className="agent-shell-tool-call" aria-label={`Bash 工具调用，${shellStatusLabel(executionStatus)}`}>
      <div className="agent-shell-tool-call-main">
        <div className={`agent-shell-tool-call-command${showStatus ? ' has-status' : ''}`}>
          <Terminal size={15} aria-hidden="true" />
          <strong>Bash</strong>
          <code title={command}>{commandSummary || '命令未记录'}</code>
          {showStatus && <span className={`agent-shell-tool-call-status is-${executionStatus}`}>{shellStatusLabel(executionStatus)}</span>}
        </div>
        {showResultSummary && <p className={`agent-shell-tool-call-result${hasAttention ? ' is-warning' : ''}`} role={hasAttention ? 'alert' : 'status'} title={resultSummary}><span className="agent-shell-tool-call-result-label">结果</span><span className="agent-shell-tool-call-result-text">{resultSummary}</span></p>}
      </div>
      {children}
      <details className="agent-shell-tool-call-details">
        <summary>
          <span>工具调用</span>
          <ChevronDown className="agent-shell-tool-call-chevron" size={15} aria-hidden="true" />
        </summary>
        <div className="agent-shell-tool-call-body">
          {scope && <p className="agent-shell-tool-call-scope"><span>范围</span><code>{scope}</code></p>}
          {!commandShownInSummary && <section className="agent-shell-tool-call-block" aria-label="Shell 命令">
            <strong>命令</strong>
            <pre><code>{command || '未记录命令。'}</code></pre>
          </section>}
          <section className="agent-shell-tool-call-block" aria-label="Shell 结果">
            <div className="agent-shell-tool-call-result-heading">
              <strong>结果</strong>
              <span className={executionStatus === 'succeeded' ? 'is-success' : hasAttention ? 'is-error' : ''}>
                {shellStatusLabel(executionStatus)}{exitCode !== undefined ? ` · 退出码 ${exitCode}` : ''}
              </span>
            </div>
            {shouldShowShellResultMessage(resultMessage) && <p>{resultMessage}</p>}
            {outputDiscarded && <p>此历史记录没有保存命令输出。</p>}
            {!outputDiscarded && stdout && <div className="agent-shell-tool-call-output"><small>stdout</small><pre>{stdout}</pre></div>}
            {!outputDiscarded && stderr && <div className="agent-shell-tool-call-output"><small>stderr</small><pre className="is-error">{stderr}</pre></div>}
            {outputTruncated && <p>输出已截断；stdout 与 stderr 每路最多保留 16 KiB。</p>}
            {!shouldShowShellResultMessage(resultMessage) && !outputDiscarded && !stdout && !stderr && !outputTruncated && <p>没有命令输出。</p>}
          </section>
        </div>
      </details>
    </section>
  );
}
