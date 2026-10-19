// Agent 设置由 FileSage 服务端保存。
export interface AgentSettings {
  bypassPermission: boolean;
}

export class AgentSettingsApiError extends Error {
  readonly status: number;

  constructor(message: string, status = 0) {
    super(message);
    this.name = 'AgentSettingsApiError';
    this.status = status;
  }
}

export function parseAgentSettings(value: unknown): AgentSettings {
  if (typeof value !== 'object' || value === null || !('bypassPermission' in value)
    || typeof value.bypassPermission !== 'boolean') {
    throw new AgentSettingsApiError('Agent 设置响应无效。');
  }
  return { bypassPermission: value.bypassPermission };
}

async function requestSettings(init?: RequestInit, signal?: AbortSignal): Promise<AgentSettings> {
  let response: Response;
  try {
    response = await fetch('/api/v1/settings/agent', {
      ...init,
      headers: {
        Accept: 'application/json',
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...init?.headers,
      },
      signal,
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new AgentSettingsApiError('无法连接 FileSage 服务。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status === 404 ? '当前 FileSage 服务尚未提供 Agent 设置。'
        : `读取 Agent 设置失败（HTTP ${response.status}）。`;
    throw new AgentSettingsApiError(message, response.status);
  }
  return parseAgentSettings(payload);
}

export function getAgentSettings(signal?: AbortSignal): Promise<AgentSettings> {
  return requestSettings(undefined, signal);
}

export function updateAgentSettings(settings: AgentSettings): Promise<AgentSettings> {
  return requestSettings({ method: 'PUT', body: JSON.stringify(settings) });
}
