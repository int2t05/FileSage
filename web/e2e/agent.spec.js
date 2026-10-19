// 验证手机视口下 Agent 的确认、计划审阅和文件系统写入主链路。
import { expect, test } from '@playwright/test';
import { access, readFile, stat } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

const targetName = 'agent-e2e-reviewed';

// 读取测试服务记录的临时根目录，直接核验真实文件写入。
async function readManagedRoot(page) {
  const port = new URL(page.url()).port;
  if (!port) throw new Error('浏览器测试未连接到隔离服务');
  const statePath = path.join(os.tmpdir(), `filesage-agent-e2e-${port}.json`);
  const { root } = JSON.parse(await readFile(statePath, 'utf8'));
  return root;
}

test('Agent 在窄屏下先审阅计划，再按 bypass 执行并创建真实目录', async ({ page }) => {
  const browserErrors = [];
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text());
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));

  await page.goto('/#agent');
  const root = await readManagedRoot(page);
  const targetPath = path.join(root, targetName);
  await expect(access(targetPath)).rejects.toThrow();
  await expect(page.getByRole('heading', { name: '整理助手' })).toBeVisible();
  const composer = page.getByRole('textbox', { name: '整理需求消息' });
  await composer.fill(`请在根目录新建一个名为 ${targetName} 的文件夹。`);
  await page.getByRole('button', { name: '发送', exact: true }).click();

  const confirmation = page.getByRole('region', { name: '确认操作' });
  await expect(confirmation).toContainText(targetName);
  await expect(page.getByRole('button', { name: '确认并生成方案' })).toBeVisible();
  await expect(access(targetPath)).rejects.toThrow();
  await page.getByRole('button', { name: '确认并生成方案' }).click();

  await expect(page.getByRole('heading', { name: '执行计划' })).toBeVisible();
  const steps = page.getByRole('list', { name: '执行步骤' });
  await expect(steps).toContainText(targetName);
  await expect(page.getByRole('button', { name: '审阅完成，开始执行' })).toBeEnabled();
  await expect(access(targetPath)).rejects.toThrow();
  const viewport = await page.evaluate(() => ({
    width: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(viewport.scrollWidth).toBeLessThanOrEqual(viewport.width);

  await page.getByRole('button', { name: '审阅完成，开始执行' }).click();
  await expect(steps.getByRole('listitem').getByText('已完成', { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect.poll(async () => (await stat(targetPath).catch(() => undefined))?.isDirectory() ?? false, { timeout: 5_000 }).toBe(true);
  expect(browserErrors).toEqual([]);
});
