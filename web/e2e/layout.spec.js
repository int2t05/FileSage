// 验证核心页面在桌面与手机视口下共享导航并保持可视宽度。
import { expect, test } from '@playwright/test';

const pages = [
  { hash: '#files', heading: '我的文件' },
  { hash: '#photos', heading: '照片' },
  { hash: '#agent', heading: '整理助手' },
  { hash: '#tasks', heading: '任务' },
  { hash: '#trash', heading: '回收站' },
  { hash: '#settings', heading: 'Agent 设置' },
];

async function expectNoHorizontalOverflow(page) {
  const dimensions = await page.evaluate(() => ({
    viewport: document.documentElement.clientWidth,
    content: document.documentElement.scrollWidth,
  }));
  expect(dimensions.content).toBeLessThanOrEqual(dimensions.viewport);
}

test('核心页面统一导航并适配桌面与手机', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  for (const { hash, heading } of pages) {
    await page.goto(`/${hash}`);
    await expect(page.getByRole('heading', { name: heading, exact: true })).toBeVisible();
    await expect(page.locator('.sidebar')).toBeVisible();
    await expect(page.getByRole('navigation', { name: '手机主导航' })).toBeHidden();
    await expectNoHorizontalOverflow(page);
  }

  await page.setViewportSize({ width: 360, height: 780 });
  for (const { hash, heading } of pages) {
    await page.goto(`/${hash}`);
    await expect(page.getByRole('heading', { name: heading, exact: true })).toBeVisible();
    const mobileNavigation = page.getByRole('navigation', { name: '手机主导航' });
    await expect(mobileNavigation).toBeVisible();
    await expect(mobileNavigation.getByRole('button')).toHaveCount(5);
    await expectNoHorizontalOverflow(page);
  }
});
