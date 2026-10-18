/** PPTX 放映使用的键盘与触屏导航规则。 */

export type PresentationKeyAction = 'previous' | 'next' | 'first' | 'last' | 'exit';

// 将常见方向键映射到幻灯片操作。
export function presentationKeyAction(key: string): PresentationKeyAction | null {
  if (key === 'ArrowLeft' || key === 'ArrowUp' || key === 'PageUp') return 'previous';
  if (key === 'ArrowRight' || key === 'ArrowDown' || key === 'PageDown' || key === ' ') return 'next';
  if (key === 'Home') return 'first';
  if (key === 'End') return 'last';
  if (key === 'Escape') return 'exit';
  return null;
}

// 忽略轻微或纵向滑动，避免抢占页面滚动手势。
export function slideSwipeDirection(startX: number, startY: number, endX: number, endY: number): 'previous' | 'next' | null {
  const deltaX = endX - startX;
  const deltaY = endY - startY;
  if (Math.abs(deltaX) < 48 || Math.abs(deltaX) < Math.abs(deltaY) * 1.25) return null;
  return deltaX > 0 ? 'previous' : 'next';
}
