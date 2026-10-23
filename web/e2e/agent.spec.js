// 验证窄屏下的会话恢复、会话切换和文件写入审批主链路。
import { expect, test } from '@playwright/test';
import { access, mkdir, readFile, stat, writeFile } from 'node:fs/promises';
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

test('Agent 拒绝整份计划后不执行文件操作', async ({ page }, testInfo) => {
  const targetName = `agent-e2e-reviewed-${8 + testInfo.repeatEachIndex}`;
  const browserErrors = [];
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text());
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));

  const conversationListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await page.goto('/#agent');
  await conversationListResponse;
  const root = await readManagedRoot(page);
  const targetPath = path.join(root, targetName);
  await expect(access(targetPath)).rejects.toThrow();

  const composer = page.getByRole('textbox', { name: '整理需求消息' });
  const createdConversationResponse = page.waitForResponse((response) => response.request().method() === 'POST'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await composer.fill(`请在根目录新建一个名为 ${targetName} 的文件夹。`);
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await createdConversationResponse;
  await expect(page.getByRole('region', { name: '确认操作' })).toContainText(targetName);
  await page.getByRole('button', { name: '确认并生成方案' }).click();

  const steps = page.getByRole('list', { name: '执行步骤' });
  await expect(steps).toContainText(targetName);
  await expect(page.getByRole('button', { name: '拒绝整份方案' })).toBeEnabled();
  await expect(access(targetPath)).rejects.toThrow();
  await page.getByRole('button', { name: '拒绝整份方案' }).click();

  await expect(page.getByRole('status').filter({ hasText: '已拒绝整份方案。' })).toBeVisible();
  await expect(access(targetPath)).rejects.toThrow();
  expect(browserErrors).toEqual([]);
});

test('Agent 审阅并执行同目录重命名，保留原文件内容', async ({ page }, testInfo) => {
  test.skip(process.platform !== 'linux', '文件版本校验依赖 Linux 文件身份元数据');
  const suffix = `-${testInfo.repeatEachIndex}`;
  const targetName = `agent-e2e-rename${suffix}`;
  const relativeDirectory = targetName;
  const sourceRelative = `${relativeDirectory}/source.md`;
  const targetRelative = `${relativeDirectory}/renamed.md`;
  const originalContent = '# 项目说明\n\n这是用于端到端验收的合成文件。\n';
  const browserErrors = [];
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text());
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));

  const conversationListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await page.goto('/#agent');
  await conversationListResponse;
  const root = await readManagedRoot(page);
  const directory = path.join(root, relativeDirectory);
  const sourcePath = path.join(root, sourceRelative);
  const targetPath = path.join(root, targetRelative);
  await mkdir(directory, { recursive: true });
  await writeFile(sourcePath, originalContent, { mode: 0o600 });

  const composer = page.getByRole('textbox', { name: '整理需求消息' });
  const createdConversationResponse = page.waitForResponse((response) => response.request().method() === 'POST'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await composer.fill(`请根据内容为 ${sourceRelative} 提出同目录重命名建议，任务编号 ${targetName}。`);
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await createdConversationResponse;

  const confirmation = page.getByRole('region', { name: '确认操作' });
  await expect(confirmation).toContainText(sourceRelative);
  await expect(confirmation).toContainText('UTF-8 文件正文全文');
  await expect(confirmation).toContainText('不会修改文件');
  await expect(page.getByRole('button', { name: '确认并分析' })).toBeEnabled();
  await expect(readFile(sourcePath, 'utf8')).resolves.toBe(originalContent);
  await expect(access(targetPath)).rejects.toThrow();

  await page.getByRole('button', { name: '确认并分析' }).click();
  const steps = page.getByRole('list', { name: '执行步骤' });
  await expect(page.getByRole('heading', { name: '执行计划' })).toBeVisible();
  await expect(steps).toContainText(targetRelative);
  await expect(page.getByRole('button', { name: '审阅完成，开始执行' })).toBeEnabled();
  await expect(readFile(sourcePath, 'utf8')).resolves.toBe(originalContent);
  await expect(access(targetPath)).rejects.toThrow();

  await page.getByRole('button', { name: '审阅完成，开始执行' }).click();
  await expect(steps.getByRole('listitem').getByText('已完成', { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect.poll(async () => readFile(targetPath, 'utf8').catch(() => '') , { timeout: 5_000 }).toBe(originalContent);
  await expect(access(sourcePath)).rejects.toThrow();
  expect(browserErrors).toEqual([]);
});

test('Agent 在 Shell 执行前披露续聊会发送的数据范围', async ({ page }) => {
  const browserErrors = [];
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text());
  });
  page.on('pageerror', (error) => browserErrors.push(error.message));

  const conversationListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await page.goto('/#agent');
  await conversationListResponse;
  const root = await readManagedRoot(page);
  await mkdir(path.join(root, 'docs', 'reports'), { recursive: true });
  const composer = page.getByRole('textbox', { name: '整理需求消息' });
  const createdConversationResponse = page.waitForResponse((response) => response.request().method() === 'POST'
    && new URL(response.url()).pathname === '/api/v1/agent/conversations');
  await composer.fill('请准备一个无文件写入的 Shell 命令，供 agent-e2e-shell-confirmation 浏览器验收。');
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await createdConversationResponse;

  const confirmation = page.getByRole('region', { name: '确认隔离运行' });
  await expect(confirmation).toContainText('确认隔离运行');
  await expect(confirmation).toContainText('docs/reports');
  await expect(confirmation).toContainText("printf 'filesage-e2e-output");
  await expect(confirmation).toContainText('变化路径、类型和状态');
  await expect(confirmation).toContainText('stdout/stderr 各限 16 KiB');
  await expect(confirmation).toContainText('FileSage 不会另行读取文件正文，但命令输出可能包含文件内容或其他敏感信息');
  await expect(page.getByRole('button', { name: '运行并查看变化' })).toBeEnabled();
  expect(browserErrors).toEqual([]);
});
