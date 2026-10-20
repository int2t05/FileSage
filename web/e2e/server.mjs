// 为浏览器验收启动本地 Go 服务和隔离模型，并在结束后清理测试数据。
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.resolve(scriptDir, '..');
const repoDir = path.resolve(webDir, '..');
const port = Number(process.env.FILESAGE_E2E_PORT || 4179);
const stateFile = path.join(os.tmpdir(), `filesage-agent-e2e-${port}.json`);
const targetName = 'agent-e2e-reviewed';
const followupName = 'agent-e2e-followup';
const separateConversationName = 'agent-e2e-separate-session';

// 用参数数组启动命令，避免在 Shell 中拼接测试路径。
function run(command, args, cwd) {
  const result = spawnSync(command, args, { cwd, stdio: 'inherit', windowsHide: true });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} exited with status ${result.status}`);
}

// 读取有限大小的模型请求，防止测试服务无界缓冲。
async function readBody(request) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > 1024 * 1024) throw new Error('model request exceeded test limit');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString('utf8');
}

// 只为约定的目录任务返回固定 JSON，不访问外网模型。
async function answerModel(request, response) {
  if (request.method !== 'POST' || request.url !== '/v1/chat/completions') {
    response.writeHead(404).end();
    return;
  }
  if (request.headers.authorization !== 'Bearer filesage-e2e-only') {
    response.writeHead(401).end();
    return;
  }
  try {
    const body = JSON.parse(await readBody(request));
    const userMessage = body.messages?.findLast((message) => message.role === 'user')?.content;
    const isFollowup = typeof userMessage === 'string' && userMessage.includes(followupName);
    const isSeparateConversation = typeof userMessage === 'string' && userMessage.includes(separateConversationName);
    const target = isFollowup ? followupName : isSeparateConversation ? separateConversationName : targetName;
    const hasMemory = body.messages?.some((message) => message.role === 'user'
      && message.content.includes('用户希望在同一会话持续处理文件。'));
    if (body.model !== 'filesage-e2e' || typeof userMessage !== 'string'
      || !userMessage.includes(target) || isFollowup && !hasMemory) {
      response.writeHead(422, { 'Content-Type': 'application/json' }).end('{"error":"unexpected test prompt"}');
      return;
    }
    const content = JSON.stringify({
      state: 'ready',
      reply: `将在管理根目录新建文件夹 ${target}，请确认。`,
      contextSummary: '用户希望在同一会话持续处理文件。',
      goal: isFollowup ? '继续处理后续文件整理' : isSeparateConversation ? '新会话创建验收文件夹' : '创建浏览器端到端验收文件夹',
      action: 'create_directory',
      filePath: '',
      targetPath: target,
    });
    response.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ choices: [{ message: { content } }] }));
  } catch {
    response.writeHead(400, { 'Content-Type': 'application/json' }).end('{"error":"invalid test request"}');
  }
}

// 停止服务后只清理本次创建的临时根目录、数据库和状态文件。
async function stopApp(app) {
  if (!app || app.exitCode !== null) return;
  const exited = once(app, 'exit');
  app.kill('SIGTERM');
  await Promise.race([exited, new Promise((resolve) => setTimeout(resolve, 5000))]);
  if (app.exitCode === null) {
    const killed = once(app, 'exit');
    app.kill('SIGKILL');
    await Promise.race([killed, new Promise((resolve) => setTimeout(resolve, 5000))]);
  }
}

// 启动测试服务；返回的回调由 Playwright 在用例完成后执行。
export default async function globalSetup() {
  let tempDir = '';
  let app;
  let model;
  let cleaned = false;
  const cleanup = async () => {
    if (cleaned) return;
    cleaned = true;
    await stopApp(app);
    if (model) {
      model.closeAllConnections();
      await new Promise((resolve) => model.close(() => resolve()));
    }
    if (tempDir) await rm(tempDir, { recursive: true, force: true });
    await rm(stateFile, { force: true });
  };

  try {
    if (!Number.isSafeInteger(port) || port < 1 || port > 65535) throw new Error('FILESAGE_E2E_PORT 必须是有效端口');
    tempDir = await mkdtemp(path.join(os.tmpdir(), 'filesage-agent-e2e-'));
    const root = path.join(tempDir, 'root');
    const data = path.join(tempDir, 'data');
    const binary = path.join(tempDir, process.platform === 'win32' ? 'filesage.exe' : 'filesage');
    await Promise.all([mkdir(root), mkdir(data)]);
    await writeFile(stateFile, JSON.stringify({ root }), { mode: 0o600 });

    run('go', ['build', '-trimpath', '-o', binary, './cmd/filesage'], repoDir);
    model = createServer((request, response) => { void answerModel(request, response); });
    await new Promise((resolve, reject) => {
      model.once('error', reject);
      model.listen(0, '127.0.0.1', resolve);
    });
    const modelAddress = model.address();
    if (!modelAddress || typeof modelAddress === 'string') throw new Error('无法启动本地测试模型');

    app = spawn(binary, [], {
      cwd: repoDir,
      windowsHide: true,
      stdio: 'inherit',
      env: {
        ...process.env,
        FILESAGE_ROOT: root,
        FILESAGE_DATA_DIR: data,
        FILESAGE_WEB_DIR: path.join(webDir, 'dist'),
        FILESAGE_LISTEN_ADDR: `127.0.0.1:${port}`,
        FILESAGE_ALLOWED_HOSTS: '',
        FILESAGE_AGENT_BASE_URL: `http://127.0.0.1:${modelAddress.port}/v1`,
        FILESAGE_AGENT_MODEL: 'filesage-e2e',
        FILESAGE_AGENT_PROVIDER: 'local-test',
        FILESAGE_AGENT_API_KEY: 'filesage-e2e-only',
      },
    });
    let spawnError;
    app.once('error', (error) => { spawnError = error; });

    const deadline = Date.now() + 90_000;
    while (Date.now() < deadline) {
      if (spawnError) throw spawnError;
      if (app.exitCode !== null) throw new Error(`FileSage test server exited with status ${app.exitCode}`);
      try {
        const response = await fetch(`http://127.0.0.1:${port}/api/v1/health`);
        if (response.ok) {
          console.log(`FileSage browser test service ready at http://127.0.0.1:${port}`);
          return cleanup;
        }
      } catch {
        // 服务启动期间暂不可达，继续轮询至超时。
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    throw new Error('FileSage browser test service did not become ready');
  } catch (error) {
    await cleanup();
    throw error;
  }
}
