// 将统一差异解析为可读的行号与变化类型。
export type TextDiffRowKind = 'header' | 'hunk' | 'added' | 'removed' | 'context' | 'meta';

export interface TextDiffRow {
  text: string;
  kind: TextDiffRowKind;
  oldLine?: number;
  newLine?: number;
}

// 只把包含统一差异头和区块的内容作为代码差异渲染。
export function isUnifiedTextDiff(value: string): boolean {
  const lines = value.split(/\r?\n/);
  const hasHeaders = lines.some((line) => line.startsWith('--- a/'))
    && lines.some((line) => line.startsWith('+++ b/'));
  if (!hasHeaders) return false;
  return lines.some((line) => /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.test(line))
    || lines.every((line) => !line || line.startsWith('--- a/') || line.startsWith('+++ b/')
      || line.startsWith('\\') || line.startsWith('[diff truncated]'));
}

export function parseTextDiff(value: string): TextDiffRow[] {
  const lines = value.split(/\r?\n/);
  if (lines.at(-1) === '') lines.pop();

  let oldLine = 0;
  let newLine = 0;
  return lines.map((text): TextDiffRow => {
    if (text.startsWith('--- ') || text.startsWith('+++ ')) return { text, kind: 'header' };
    const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(text);
    if (hunk) {
      oldLine = Number(hunk[1]);
      newLine = Number(hunk[2]);
      return { text, kind: 'hunk' };
    }
    if (text.startsWith('\\') || text.startsWith('[diff truncated]')) return { text, kind: 'meta' };
    if (text.startsWith('+')) return { text, kind: 'added', newLine: newLine++ };
    if (text.startsWith('-')) return { text, kind: 'removed', oldLine: oldLine++ };
    return { text, kind: 'context', oldLine: oldLine++, newLine: newLine++ };
  });
}
