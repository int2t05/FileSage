// 只在当前浏览器会话保存章节、滚动位置和字号。
export interface EpubPosition {
  index: number;
  scrollTop: number;
  fontSize: number;
}

const FONT_MIN = 15;
const FONT_MAX = 27;

/** 校验并夹紧会话恢复数据，避免旧格式或手工修改造成越界。 */
export function parseEpubPosition(value: string | null, chapterCount: number): EpubPosition | null {
  if (!value || chapterCount < 1) return null;
  try {
    const parsed = JSON.parse(value) as Partial<EpubPosition>;
    if (
      !Number.isInteger(parsed.index) || (parsed.index ?? -1) < 0
      || !Number.isFinite(parsed.scrollTop) || (parsed.scrollTop ?? -1) < 0
      || !Number.isFinite(parsed.fontSize)
    ) return null;
    return {
      index: Math.min(parsed.index!, chapterCount - 1),
      scrollTop: Math.min(parsed.scrollTop!, 10_000_000),
      fontSize: Math.min(FONT_MAX, Math.max(FONT_MIN, parsed.fontSize!)),
    };
  } catch {
    return null;
  }
}

/** 翻章时限制在有效范围内，目录切换仍由受控选择框完成。 */
export function adjacentEpubChapter(index: number, direction: -1 | 1, chapterCount: number): number {
  if (chapterCount < 1) return 0;
  return Math.min(chapterCount - 1, Math.max(0, index + direction));
}
