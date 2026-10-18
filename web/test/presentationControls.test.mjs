import assert from 'node:assert/strict';
import test from 'node:test';
import { presentationKeyAction, slideSwipeDirection } from '../src/features/reader/presentationControls.ts';

test('放映方向键与页首页尾键映射到幻灯片导航', () => {
  assert.equal(presentationKeyAction('ArrowLeft'), 'previous');
  assert.equal(presentationKeyAction('PageUp'), 'previous');
  assert.equal(presentationKeyAction('ArrowRight'), 'next');
  assert.equal(presentationKeyAction(' '), 'next');
  assert.equal(presentationKeyAction('Home'), 'first');
  assert.equal(presentationKeyAction('End'), 'last');
  assert.equal(presentationKeyAction('Escape'), 'exit');
  assert.equal(presentationKeyAction('a'), null);
});

test('横向滑动翻页，纵向滚动和短滑动不触发', () => {
  assert.equal(slideSwipeDirection(140, 100, 220, 112), 'previous');
  assert.equal(slideSwipeDirection(220, 100, 140, 112), 'next');
  assert.equal(slideSwipeDirection(140, 100, 170, 103), null);
  assert.equal(slideSwipeDirection(140, 100, 190, 180), null);
});
