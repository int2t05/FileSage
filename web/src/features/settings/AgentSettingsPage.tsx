// Agent 设置：读取并更新服务端执行权限模式。
import { useEffect, useState } from 'react';
import { AlertTriangle, Loader2, RefreshCw } from 'lucide-react';
import { getAgentSettings, updateAgentSettings, type AgentSettings } from '../../api/agentSettings';
import './AgentSettingsPage.css';

export function AgentSettingsPage() {
  const [settings, setSettings] = useState<AgentSettings | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [refreshKey, setRefreshKey] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError('');
    getAgentSettings(controller.signal)
      .then((value) => {
        if (!controller.signal.aborted) setSettings(value);
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : '无法读取 Agent 设置。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [refreshKey]);

  async function toggleBypassPermission() {
    if (!settings || saving) return;
    setSaving(true);
    setError('');
    try {
      setSettings(await updateAgentSettings({ bypassPermission: !settings.bypassPermission }));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '保存 Agent 设置失败。');
    } finally {
      setSaving(false);
    }
  }

  return (
    <section className="agent-settings-page" aria-labelledby="agent-settings-title">
      <header className="agent-settings-heading">
        <h1 id="agent-settings-title">Agent 设置</h1>
      </header>

      <section className="agent-settings-card" aria-labelledby="agent-bypass-title">
        <div className="agent-settings-copy">
          <h2 id="agent-bypass-title">跳过逐步确认</h2>
          <p>只影响之后启动的新计划。开启后自动执行后续步骤；关闭后每一步都需确认。</p>
        </div>
        {loading ? (
          <div className="agent-settings-state" role="status"><Loader2 size={17} className="agent-settings-spinner" aria-hidden="true" />正在读取设置…</div>
        ) : settings ? (
          <button
            className={`agent-settings-switch${settings.bypassPermission ? ' is-on' : ''}`}
            type="button"
            role="switch"
            aria-checked={settings.bypassPermission}
            aria-labelledby="agent-bypass-title"
            aria-describedby="agent-bypass-state"
            disabled={saving}
            onClick={() => { void toggleBypassPermission(); }}
          >
            <span className="agent-settings-switch-thumb" aria-hidden="true" />
          </button>
        ) : (
          <button className="agent-settings-retry" type="button" onClick={() => setRefreshKey((value) => value + 1)}>
            <RefreshCw size={15} aria-hidden="true" />重试
          </button>
        )}
        {settings && !loading && <span id="agent-bypass-state" className="agent-settings-value">{saving ? '保存中…' : settings.bypassPermission ? '已开启' : '已关闭'}</span>}
        {error && <p className="agent-settings-error" role="alert"><AlertTriangle size={15} aria-hidden="true" />{error}</p>}
      </section>
    </section>
  );
}
