// 验证窄屏下的会话恢复、会话切换和文件写入审批主链路。
import { expect, test } from '@playwright/test';
import { access, readFile, stat } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

// 读取测试服务记录的临时根目录，直接核验真实文件写入。
async function readManagedRoot(page) {
  const port = new URL(page.url()).port;
  if (!port) throw new Error('浏览器测试未连接到隔离服务');
  const statePath = path.join(os.tmpdir(), `filesage-agent-e2e-${port}.json`);
  const { root } = JSON.parse(await readFile(statePath, 'utf8'));
  return root;
}

test('Agent 窄屏下恢复并切换会话，审阅计划后执行真实目录创建', async ({ page }, testInfo) => {
  const suffix = `-${testInfo.repeatEachIndex}`;
  const targetName = `agent-e2e-reviewed${suffix}`;
  const followupName = `agent-e2e-followup${suffix}`;
  const separateConversationName = `agent-e2e-separate-session${suffix}`;
  const browserErrors = [];
  let followupConversationID = '';
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text());
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));
  page.on('request', (request) => {
    const requestURL = new URL(request.url());
    if (request.method() === 'POST' && requestURL.pathname.endsWith('/messages')) {
      followupConversationID = decodeURIComponent(requestURL.pathname.split('/').at(-2));
    }
  });

  const conversationListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await page.goto('/#agent');
  await conversationListResponse;
  const savedConversations = page.getByRole('group', { name: '已保存对话' }).getByRole('button');
  const initialConversationCount = await savedConversations.count();
  const root = await readManagedRoot(page);
  const targetPath = path.join(root, targetName);
  await expect(access(targetPath)).rejects.toThrow();
  await expect(page.getByRole('heading', { name: '整理助手' })).toBeVisible();
  const composer = page.getByRole('textbox', { name: '整理需求消息' });
  const createdConversationResponse = page.waitForResponse((response) => response.request().method() === 'POST'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await composer.fill(`请在根目录新建一个名为 ${targetName} 的文件夹。`);
  await page.getByRole('button', { name: '发送', exact: true }).click();
  const conversationID = (await (await createdConversationResponse).json()).id;

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

  await page.reload();
  await expect(page.getByRole('log', { name: 'Agent 对话' })).toContainText(targetName);
  await expect(page.getByRole('heading', { name: '创建浏览器端到端验收文件夹' })).toBeVisible();
  await expect(steps).toContainText('已完成');
  await expect(composer).toBeEnabled();

  await page.getByRole('button', { name: '新对话', exact: true }).click();
  const separatePath = path.join(root, separateConversationName);
  const separateConversationResponse = page.waitForResponse((response) => response.request().method() === 'POST'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await composer.fill(`请新建一个名为 ${separateConversationName} 的文件夹。`);
  await page.getByRole('button', { name: '发送', exact: true }).click();
  const separateConversationID = (await (await separateConversationResponse).json()).id;
  expect(separateConversationID).not.toBe(conversationID);
  await expect(page.getByRole('region', { name: '确认操作' })).toContainText(separateConversationName);
  await page.getByRole('button', { name: '确认并生成方案' }).click();
  await expect(steps).toContainText(separateConversationName);
  await page.getByRole('button', { name: '审阅完成，开始执行' }).click();
  await expect(steps.getByRole('listitem').getByText('已完成', { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect.poll(async () => (await stat(separatePath).catch(() => undefined))?.isDirectory() ?? false, { timeout: 5_000 }).toBe(true);

  await expect(savedConversations).toHaveCount(initialConversationCount + 2);
  await savedConversations.nth(1).click();
  await expect(page.getByRole('log', { name: 'Agent 对话' })).toContainText(targetName);

  const followupPath = path.join(root, followupName);
  await expect(composer).toBeEnabled();
  await composer.fill(`继续在根目录新建一个名为 ${followupName} 的文件夹。`);
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await expect(page.getByRole('region', { name: '确认操作' })).toContainText(followupName);
  await expect(page.getByRole('log', { name: 'Agent 对话' })).toContainText('目录已创建');
  expect(followupConversationID).toBe(conversationID);
  await page.getByRole('button', { name: '确认并生成方案' }).click();
  await expect(steps).toContainText(followupName);
  await page.getByRole('button', { name: '审阅完成，开始执行' }).click();
  await expect(steps.getByRole('listitem').getByText('已完成', { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect.poll(async () => (await stat(followupPath).catch(() => undefined))?.isDirectory() ?? false, { timeout: 5_000 }).toBe(true);
  expect(browserErrors).toEqual([]);
});
