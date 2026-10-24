import { defineConfig } from '@playwright/test';

// 使用稳定的本机端口，避免 Playwright 重载配置时端口漂移。
const port = Number(process.env.FILESAGE_E2E_PORT || 4179);
if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('FILESAGE_E2E_PORT 必须是有效端口');
const faultProxyUpstream = process.env.FILESAGE_E2E_PROXY_UPSTREAM;
const externalBaseURL = process.env.FILESAGE_E2E_BASE_URL;
if (faultProxyUpstream && externalBaseURL) throw new Error('故障代理模式不能同时设置 FILESAGE_E2E_BASE_URL');

export default defineConfig({
  testDir: './e2e',
  globalSetup: faultProxyUpstream ? './e2e/uploadFaultProxy.mjs' : externalBaseURL ? undefined : './e2e/server.mjs',
  fullyParallel: false,
  workers: 1,
  reporter: process.env.CI ? 'line' : 'list',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: faultProxyUpstream ? `http://127.0.0.1:${port}` : externalBaseURL || `http://127.0.0.1:${port}`,
    headless: true,
    trace: 'retain-on-failure',
    viewport: { width: 390, height: 844 },
  },
});
