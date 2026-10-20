import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import {
  AgentSettingsApiError,
  getAgentSettings,
  parseAgentSettings,
  updateAgentSettings,
} from '../src/api/agentSettings.ts';

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

describe('Agent 设置 API', () => {
  it('只接受明确的 bypassPermission 布尔值', () => {
    assert.deepEqual(parseAgentSettings({ bypassPermission: true }), { bypassPermission: true });
    assert.deepEqual(parseAgentSettings({ bypassPermission: false }), { bypassPermission: false });
    assert.throws(() => parseAgentSettings({}), AgentSettingsApiError);
    assert.throws(() => parseAgentSettings({ bypassPermission: 'true' }), AgentSettingsApiError);
  });

  it('读取服务端保存的设置', async () => {
    let requestUrl = '';
    globalThis.fetch = async (url, init) => {
      requestUrl = String(url);
      assert.equal(init?.method, undefined);
      return new Response(JSON.stringify({ bypassPermission: true }), { status: 200 });
    };

    assert.deepEqual(await getAgentSettings(), { bypassPermission: true });
    assert.equal(requestUrl, '/api/v1/settings/agent');
  });

  it('把新模式写回服务端', async () => {
    let requestBody = '';
    globalThis.fetch = async (_url, init) => {
      assert.equal(init?.method, 'PUT');
      requestBody = String(init?.body);
      return new Response(JSON.stringify({ bypassPermission: false }), { status: 200 });
    };

    assert.deepEqual(await updateAgentSettings({ bypassPermission: false }), { bypassPermission: false });
    assert.equal(requestBody, JSON.stringify({ bypassPermission: false }));
  });

  it('明确报告设置接口不可用', async () => {
    globalThis.fetch = async () => new Response('', { status: 404 });
    await assert.rejects(getAgentSettings(), (error) => {
      assert.ok(error instanceof AgentSettingsApiError);
      assert.match(error.message, /尚未提供 Agent 设置/);
      assert.equal(error.status, 404);
      return true;
    });
  });
});
