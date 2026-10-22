// 验证浏览器上传经服务端发布，并可下载校验。
import { createHash, randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { expect, test } from '@playwright/test';

// 从隔离测试服务记录中读取管理根目录。
async function readManagedRoot(page) {
  const port = new URL(page.url()).port;
  if (!port) throw new Error('浏览器测试未连接到隔离服务');
  const statePath = path.join(os.tmpdir(), `filesage-agent-e2e-${port}.json`);
  const { root } = JSON.parse(await readFile(statePath, 'utf8'));
  return root;
}

// 用 SHA-256 校验服务端文件和浏览器下载。
async function sha256File(filePath) {
  const bytes = await readFile(filePath);
  return createHash('sha256').update(bytes).digest('hex');
}

test('上传经服务端确认发布并可下载原件', async ({ page }) => {
  test.skip(process.platform !== 'linux', '安全文件发布依赖 Linux 原子文件系统操作');
  const fileName = `upload-e2e-${randomUUID()}.txt`;
  const content = Buffer.alloc(6 * 1024 * 1024, 0x46);
  const expectedHash = createHash('sha256').update(content).digest('hex');
  const browserErrors = [];
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text());
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));

  const rootListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/files');
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/#files');
  await rootListResponse;
  const root = await readManagedRoot(page);
  const targetPath = path.join(root, fileName);

  await page.getByRole('button', { name: '列表视图' }).click();
  await page.getByRole('button', { name: '上传', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '上传文件' });
  await dialog.getByLabel('选择要上传的文件', { exact: true }).setInputFiles({
    name: fileName,
    mimeType: 'text/plain',
    buffer: content,
  });

  const task = dialog.getByRole('list', { name: '上传队列' })
    .getByRole('listitem')
    .filter({ hasText: fileName });
  await expect(task.getByRole('status')).toContainText('服务端已确认发布', { timeout: 30_000 });
  await expect(task).toContainText(`实际路径：/${fileName}`);
  await expect.poll(() => sha256File(targetPath), { timeout: 5_000 }).toBe(expectedHash);

  await dialog.getByRole('button', { name: '完成', exact: true }).click();
  const fileList = page.getByRole('list', { name: '全部文件中的文件' });
  await expect(fileList.getByRole('button', { name: `查看 ${fileName} 详情` })).toBeVisible();
  await fileList.getByRole('button', { name: `查看 ${fileName} 详情` }).click();
  const downloadEvent = page.waitForEvent('download');
  await page.getByRole('link', { name: '下载原件' }).click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe(fileName);
  const downloadPath = await download.path();
  if (!downloadPath) throw new Error('浏览器未保存下载文件');
  await expect.poll(() => sha256File(downloadPath), { timeout: 5_000 }).toBe(expectedHash);
  expect(browserErrors).toEqual([]);
});
