// 将长命令和输出压缩成单行摘要。
const SUMMARY_LIMIT = 88;

function firstLine(value: string): string {
  return value.split(/\r?\n/).find((line) => line.trim())?.trim() ?? '';
}

function fitSummary(value: string): string {
  const characters = Array.from(value);
  return characters.length > SUMMARY_LIMIT
    ? `${characters.slice(0, SUMMARY_LIMIT).join('')}…`
    : value;
}

export function shellCommandSummary(command: string): string {
  return fitSummary(firstLine(command));
}

export function shellResultSummary({
  resultMessage,
  outputDiscarded,
  exitCode,
  executionStatus,
}: {
  resultMessage: string;
  outputDiscarded: boolean;
  exitCode?: number;
  executionStatus: string;
}): string {
  const message = shouldShowShellResultMessage(resultMessage) ? firstLine(resultMessage) : '';
  if (message) return fitSummary(message);
  if (outputDiscarded) return exitCode === undefined ? '命令输出已省略' : `命令输出已省略 · 退出码 ${exitCode}`;
  if (exitCode !== undefined) return exitCode === 0 ? '无输出 · 退出码 0' : `退出码 ${exitCode}`;
  if (executionStatus === 'running') return '正在运行';
  if (executionStatus === 'pending') return '等待执行';
  if (executionStatus === 'skipped') return '未执行';
  if (executionStatus === 'cancelled') return '已停止';
  if (executionStatus === 'partial') return '结果需核查';
  if (executionStatus === 'failed') return '命令失败';
  if (executionStatus === 'succeeded') return '已完成';
  return '尚无结果';
}

// 成功状态已表达的通用提交文案不再重复展示。
export function shouldShowShellResultMessage(resultMessage: string): boolean {
  return Boolean(resultMessage.trim())
    && !/^(已按审批清单提交全部文件变化|已提交此项文件变化)[。.]?$/.test(resultMessage.trim());
}

// 省略重复的成功提示，保留实际输出和具体结果。
export function shouldShowShellResultSummary({
  executionStatus,
  resultMessage,
  outputDiscarded,
}: {
  executionStatus: string;
  resultMessage: string;
  outputDiscarded: boolean;
}): boolean {
  if (outputDiscarded || executionStatus === 'failed' || executionStatus === 'partial') return true;
  if (executionStatus !== 'succeeded') return false;
  return shouldShowShellResultMessage(resultMessage);
}

export function shellStatusLabel(status: string): string {
  if (status === 'succeeded') return '已完成';
  if (status === 'failed') return '失败';
  if (status === 'partial') return '需核查';
  if (status === 'running') return '运行中';
  if (status === 'pending') return '待执行';
  if (status === 'skipped') return '已跳过';
  if (status === 'cancelled') return '已停止';
  return '状态未知';
}
