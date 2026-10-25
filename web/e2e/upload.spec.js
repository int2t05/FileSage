// 验证浏览器上传经服务端发布，并可下载校验。
import { createHash, randomUUID } from 'node:crypto';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
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
  const externalService = Boolean(process.env.FILESAGE_E2E_BASE_URL);
  test.skip(process.platform !== 'linux' && !externalService, '需要 Linux 服务；可用隔离远程服务运行');
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
  const root = externalService ? undefined : await readManagedRoot(page);

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
  if (root) await expect.poll(() => sha256File(path.join(root, fileName)), { timeout: 5_000 }).toBe(expectedHash);

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

test('清除已完成上传后刷新不再恢复记录', async ({ page }) => {
  const externalService = Boolean(process.env.FILESAGE_E2E_BASE_URL);
  test.skip(process.platform !== 'linux' && !externalService, '需要 Linux 服务；可用隔离远程服务运行');

  const fileName = `upload-clear-${randomUUID()}.txt`;
  const content = Buffer.from('clear completed upload');
  await page.goto('/#files');
  await page.getByRole('button', { name: '列表视图' }).click();
  await page.getByRole('button', { name: '上传', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '上传文件' });
  const queue = dialog.getByRole('list', { name: '上传队列' });
  await dialog.getByLabel('选择要上传的文件', { exact: true }).setInputFiles({
    name: fileName,
    mimeType: 'text/plain',
    buffer: content,
  });

  const task = queue.getByRole('listitem').filter({ hasText: fileName });
  await expect(task.getByRole('status')).toContainText('服务端已确认发布', { timeout: 30_000 });
  await dialog.getByRole('button', { name: '清除已完成' }).click();
  await expect(queue.getByRole('listitem')).toHaveCount(0);

  await page.reload();
  await page.getByRole('button', { name: '列表视图' }).click();
  await page.getByRole('button', { name: '上传', exact: true }).click();
  const restoredDialog = page.getByRole('dialog', { name: '上传文件' });
  const restoredQueue = restoredDialog.getByRole('list', { name: '上传队列' });
  await expect(restoredQueue.getByRole('listitem')).toHaveCount(0);
  await expect(restoredDialog.getByText('选择文件后会在这里显示真实进度与发布结果。')).toBeVisible();

  const taskResponse = await page.request.get('/api/v1/tasks?kind=upload&limit=50&offset=0');
  expect(taskResponse.ok()).toBe(true);
  const storedTasks = (await taskResponse.json()).tasks;
  expect(storedTasks.filter((item) => item.scopePath === fileName)).toHaveLength(1);
  const fileResponse = await page.request.get(`/api/v1/files?path=${encodeURIComponent(fileName)}`);
  expect(fileResponse.ok()).toBe(true);
  expect((await fileResponse.json()).entries[0]?.name).toBe(fileName);
});

test('确认丢失时按服务端发布状态核对结果', async ({ page }) => {
  const externalService = Boolean(process.env.FILESAGE_E2E_BASE_URL);
  test.skip(process.platform !== 'linux' && !externalService, '需要 Linux 服务；可用隔离远程服务运行');

  const fileName = `upload-ack-lost-${randomUUID()}.txt`;
  const content = Buffer.alloc(512 * 1024, 0x46);
  const expectedHash = createHash('sha256').update(content).digest('hex');
  const patchOffsets = [];
  const publicationStatuses = [];
  let uploadId;
  let pendingStatusResponses = 0;
  let publishedBeforeDrop = false;
  let droppedAcknowledgement = false;

  page.on('response', async (response) => {
    const request = response.request();
    const url = new URL(response.url());
    if (droppedAcknowledgement && uploadId && request.method() === 'GET'
      && url.pathname === `/api/v1/uploads/status/${uploadId}`) {
      try {
        publicationStatuses.push((await response.json()).status);
      } catch {
        publicationStatuses.push('invalid');
      }
    }
  });

  // 等待服务端发布完成后丢弃确认，覆盖上传资源已清理的竞态。
  await page.route('**/api/v1/uploads/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (droppedAcknowledgement && request.method() === 'GET'
      && url.pathname === `/api/v1/uploads/status/${uploadId}` && pendingStatusResponses < 5) {
      pendingStatusResponses += 1;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          uploadId,
          status: 'publishing',
          size: content.length,
          offset: content.length,
          targetPath: '.',
          updatedAt: new Date().toISOString(),
        }),
      });
      return;
    }
    if (request.method() === 'PATCH') {
      patchOffsets.push(request.headers()['upload-offset']);
      if (!droppedAcknowledgement) {
        const response = await route.fetch();
        expect(response.status()).toBe(204);
        expect(response.headers()['upload-offset']).toBe(String(content.length));
        uploadId = new URL(request.url()).pathname.split('/').at(-1);
        const statusURL = new URL(`/api/v1/uploads/status/${uploadId}`, request.url()).href;
        await expect.poll(async () => {
          const statusResponse = await fetch(statusURL);
          if (!statusResponse.ok) return '';
          return (await statusResponse.json()).status;
        }, { timeout: 30_000 }).toBe('published');
        publishedBeforeDrop = true;
        droppedAcknowledgement = true;
        await response.dispose();
        await route.abort('connectionreset');
        return;
      }
    }
    await route.continue();
  });

  const rootListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/files');
  await page.goto('/#files');
  await rootListResponse;
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
  await expect(task.getByRole('status')).toContainText('服务端已确认发布', { timeout: 60_000 });
  expect(pendingStatusResponses).toBe(5);
  await expect(task).toContainText(`实际路径：/${fileName}`);
  expect(droppedAcknowledgement).toBe(true);
  expect(publishedBeforeDrop).toBe(true);
  expect(patchOffsets[0]).toBe('0');
  expect(patchOffsets.every((offset) => offset === '0')).toBe(true);
  expect(publicationStatuses).toContain('published');

  if (!externalService) {
    const root = await readManagedRoot(page);
    expect(await sha256File(path.join(root, fileName))).toBe(expectedHash);
  }

  await dialog.getByRole('button', { name: '完成', exact: true }).click();
  const fileList = page.getByRole('list', { name: '全部文件中的文件' });
  await expect(fileList.getByRole('button', { name: `查看 ${fileName} 详情` })).toHaveCount(1);
  await fileList.getByRole('button', { name: `查看 ${fileName} 详情` }).click();
  const downloadEvent = page.waitForEvent('download');
  await page.getByRole('link', { name: '下载原件' }).click();
  const download = await downloadEvent;
  const downloadPath = await download.path();
  if (!downloadPath) throw new Error('浏览器未保存下载文件');
  expect(download.suggestedFilename()).toBe(fileName);
  await expect.poll(() => sha256File(downloadPath), { timeout: 5_000 }).toBe(expectedHash);
});

test('服务端发布状态长时间未终结时停止轮询并保留重试入口', async ({ page }) => {
  const externalService = Boolean(process.env.FILESAGE_E2E_BASE_URL);
  test.skip(process.platform !== 'linux' && !externalService, '需要 Linux 服务；可用隔离远程服务运行');

  const fileName = `upload-status-pending-${randomUUID()}.txt`;
  const content = Buffer.alloc(512 * 1024, 0x46);
  const expectedHash = createHash('sha256').update(content).digest('hex');
  let uploadId;
  let patchAttempts = 0;
  let pendingStatusResponses = 0;
  let manualStatusRequests = 0;
  const manualStatusResults = [];

  page.on('response', async (response) => {
    const request = response.request();
    const url = new URL(response.url());
    if (manualStatusRequests && request.method() === 'GET' && uploadId
      && url.pathname === `/api/v1/uploads/status/${uploadId}`) {
      manualStatusResults.push(await response.json());
    }
  });

  await page.route('**/api/v1/uploads/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (request.method() === 'PATCH') {
      patchAttempts += 1;
      uploadId = url.pathname.split('/').at(-1);
      if (patchAttempts === 1) {
        await route.fulfill({ status: 413, contentType: 'text/plain', body: 'test transfer interruption' });
        return;
      }
    }
    if (request.method() === 'GET' && uploadId
      && url.pathname === `/api/v1/uploads/status/${uploadId}` && pendingStatusResponses < 5) {
      pendingStatusResponses += 1;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          uploadId,
          status: 'publishing',
          size: content.length,
          offset: content.length,
          targetPath: '.',
          updatedAt: new Date().toISOString(),
        }),
      });
      return;
    }
    if (request.method() === 'GET' && uploadId
      && url.pathname === `/api/v1/uploads/status/${uploadId}`) {
      manualStatusRequests += 1;
    }
    await route.continue();
  });

  const rootListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
    && new URL(response.url()).pathname === '/api/v1/files');
  await page.goto('/#files');
  await rootListResponse;
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
  await expect(task.getByRole('status')).toHaveText('无法确认发布状态', { timeout: 45_000 });
  expect(patchAttempts).toBe(1);
  expect(pendingStatusResponses).toBe(5);
  await expect(task).toContainText('服务端仍在处理中');
  await expect(task.getByRole('button', { name: '重新查询' })).toBeVisible();

  await task.getByRole('button', { name: '重新查询' }).click();
  await expect.poll(() => manualStatusResults.length).toBe(1);
  expect(manualStatusResults[0].status).toBe('receiving');
  await expect(task.getByText('上传失败', { exact: true })).toBeVisible();
  await expect(task.getByRole('button', { name: '重试上传' })).toBeVisible();
  await task.getByRole('button', { name: '重试上传' }).click();
  await expect(task.getByRole('status')).toContainText('服务端已确认发布', { timeout: 30_000 });
  expect(patchAttempts).toBeGreaterThan(1);

  await dialog.getByRole('button', { name: '完成', exact: true }).click();
  const fileList = page.getByRole('list', { name: '全部文件中的文件' });
  await expect(fileList.getByRole('button', { name: `查看 ${fileName} 详情` })).toHaveCount(1);
  await fileList.getByRole('button', { name: `查看 ${fileName} 详情` }).click();
  const downloadEvent = page.waitForEvent('download');
  await page.getByRole('link', { name: '下载原件' }).click();
  const download = await downloadEvent;
  const downloadPath = await download.path();
  if (!downloadPath) throw new Error('浏览器未保存下载文件');
  expect(download.suggestedFilename()).toBe(fileName);
  await expect.poll(() => sha256File(downloadPath), { timeout: 5_000 }).toBe(expectedHash);
});

test('分块确认丢失后按服务端偏移继续上传', async ({ page }) => {
  const externalService = Boolean(process.env.FILESAGE_E2E_BASE_URL);
  test.skip(process.platform !== 'linux' && !externalService, '需要 Linux 服务；可用隔离远程服务运行');
  test.setTimeout(180_000);

  const chunkSize = 64 * 1024 * 1024;
  const fileName = `upload-chunk-resume-${randomUUID()}.bin`;
  const content = Buffer.alloc(chunkSize + 1024, 0x46);
  const fixtureDir = await mkdtemp(path.join(os.tmpdir(), 'filesage-upload-e2e-'));
  try {
    const fixturePath = path.join(fixtureDir, fileName);
    await writeFile(fixturePath, content);
    const expectedHash = createHash('sha256').update(content).digest('hex');
    const patchOffsets = [];
    const headOffsets = [];
    let lostChunkAcknowledgement = false;

    page.on('response', (response) => {
      const request = response.request();
      if (request.method() === 'HEAD' && new URL(response.url()).pathname.startsWith('/api/v1/uploads/')) {
        headOffsets.push(response.headers()['upload-offset']);
      }
    });
    await page.route('**/api/v1/uploads/**', async (route) => {
      const request = route.request();
      if (request.method() !== 'PATCH') {
        await route.continue();
        return;
      }

      const offset = request.headers()['upload-offset'];
      const bodySize = request.postDataBuffer()?.length ?? 0;
      patchOffsets.push({ offset, bodySize });
      const response = await route.fetch();
      if (!lostChunkAcknowledgement && offset === '0' && bodySize === chunkSize) {
        expect(response.status()).toBe(204);
        expect(response.headers()['upload-offset']).toBe(String(chunkSize));
        lostChunkAcknowledgement = true;
        await response.dispose();
        await route.abort('connectionreset');
        return;
      }
      await route.fulfill({ response });
    });

    const rootListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
      && new URL(response.url()).pathname === '/api/v1/files');
    await page.goto('/#files');
    await rootListResponse;
    await page.getByRole('button', { name: '列表视图' }).click();
    await page.getByRole('button', { name: '上传', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: '上传文件' });
    await dialog.getByLabel('选择要上传的文件', { exact: true }).setInputFiles(fixturePath);

    const task = dialog.getByRole('list', { name: '上传队列' })
      .getByRole('listitem')
      .filter({ hasText: fileName });
    await expect(task.getByRole('status')).toContainText('服务端已确认发布', { timeout: 150_000 });
    await expect(task).toContainText(`实际路径：/${fileName}`);
    expect(lostChunkAcknowledgement).toBe(true);
    expect(patchOffsets[0]).toEqual({ offset: '0', bodySize: chunkSize });
    expect(patchOffsets.some(({ offset }) => offset === String(chunkSize))).toBe(true);
    expect(headOffsets).toContain(String(chunkSize));

    await dialog.getByRole('button', { name: '完成', exact: true }).click();
    const fileList = page.getByRole('list', { name: '全部文件中的文件' });
    await expect(fileList.getByRole('button', { name: `查看 ${fileName} 详情` })).toHaveCount(1, { timeout: 30_000 });
    await fileList.getByRole('button', { name: `查看 ${fileName} 详情` }).click();
    const downloadEvent = page.waitForEvent('download');
    await page.getByRole('link', { name: '下载原件' }).click();
    const download = await downloadEvent;
    const downloadPath = await download.path();
    if (!downloadPath) throw new Error('浏览器未保存下载文件');
    expect(download.suggestedFilename()).toBe(fileName);
    await expect.poll(() => sha256File(downloadPath), { timeout: 10_000 }).toBe(expectedHash);
  } finally {
    await rm(fixtureDir, { recursive: true, force: true });
  }
});

test('PATCH传输中途断开后按服务端偏移重传当前分块', async ({ page }) => {
  test.skip(!process.env.FILESAGE_E2E_PROXY_UPSTREAM, '需要隔离 Linux 服务和上传故障代理');
  test.setTimeout(180_000);

  const chunkSize = 64 * 1024 * 1024;
  const fileName = `upload-partial-patch-${randomUUID()}.bin`;
  const content = Buffer.alloc(chunkSize + 1024, 0x46);
  const fixtureDir = await mkdtemp(path.join(os.tmpdir(), 'filesage-upload-e2e-'));

  try {
    const fixturePath = path.join(fixtureDir, fileName);
    await writeFile(fixturePath, content);
    const expectedHash = createHash('sha256').update(content).digest('hex');

    const rootListResponse = page.waitForResponse((response) => response.request().method() === 'GET'
      && new URL(response.url()).pathname === '/api/v1/files');
    await page.goto('/#files');
    await rootListResponse;
    await page.getByRole('button', { name: '列表视图' }).click();
    await page.getByRole('button', { name: '上传', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: '上传文件' });
    await dialog.getByLabel('选择要上传的文件', { exact: true }).setInputFiles(fixturePath);

    const task = dialog.getByRole('list', { name: '上传队列' })
      .getByRole('listitem')
      .filter({ hasText: fileName });
    await expect(task.getByRole('status')).toContainText('服务端已确认发布', { timeout: 150_000 });
    await expect(task).toContainText(`实际路径：/${fileName}`);

    const proxyStateResponse = await fetch(new URL('/__filesage_upload_proxy_state', page.url()));
    expect(proxyStateResponse.ok).toBe(true);
    const proxyState = await proxyStateResponse.json();
    expect(proxyState.interrupted).toBe(true);
    expect(proxyState.bytesForwarded).toBe(8 * 1024 * 1024);
    expect(proxyState.patches[0]).toEqual({ offset: '0', bodyLength: chunkSize });
    const resumeOffset = String(8 * 1024 * 1024);
    expect(proxyState.headOffsets).toContain(resumeOffset);
    expect(proxyState.patches.some(({ offset, bodyLength }) => offset === resumeOffset
      && bodyLength === content.length - Number(resumeOffset))).toBe(true);

    await dialog.getByRole('button', { name: '完成', exact: true }).click();
    const fileList = page.getByRole('list', { name: '全部文件中的文件' });
    await expect(fileList.getByRole('button', { name: `查看 ${fileName} 详情` })).toHaveCount(1, { timeout: 30_000 });
    await fileList.getByRole('button', { name: `查看 ${fileName} 详情` }).click();
    const downloadEvent = page.waitForEvent('download');
    await page.getByRole('link', { name: '下载原件' }).click();
    const download = await downloadEvent;
    const downloadPath = await download.path();
    if (!downloadPath) throw new Error('浏览器未保存下载文件');
    expect(download.suggestedFilename()).toBe(fileName);
    await expect.poll(() => sha256File(downloadPath), { timeout: 10_000 }).toBe(expectedHash);
  } finally {
    await rm(fixtureDir, { recursive: true, force: true });
  }
});
