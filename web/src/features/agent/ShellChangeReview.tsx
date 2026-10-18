// Shell 方案只按变更清单逐项批准，正文差异按需读取。
import { useEffect, useState } from 'react';
import { Check, ChevronLeft, ChevronRight, Eye, EyeOff, Loader2, RefreshCw } from 'lucide-react';
import {
  AgentApiError,
  getAgentShellChangePreview,
  getAgentShellChanges,
  type AgentShellChange,
  type AgentShellChangePreview,
} from '../../api/agent';

const PAGE_SIZE = 50;
const APPROVAL_LABELS: Record<string, string> = {
  pending: '待审核',
  approved: '已批准',
  rejected: '已拒绝',
  invalidated: '已过期',
};
const KIND_LABELS: Record<string, string> = {
  added: '新增',
  modified: '修改',
  deleted: '删除',
};
const STATUS_LABELS: Record<string, string> = {
  pending: '待写入',
  staged: '已暂存',
  approved: '已批准',
  succeeded: '已写入',
  failed: '失败',
  invalidated: '已过期',
};

// 未识别的服务端状态保留原值，避免误报成功。
function statusLabel(labels: Record<string, string>, value: string): string {
  return labels[value] ?? `未知状态：${value || '空'}`;
}

// 使用紧凑单位显示文件大小。
function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let size = value;
  let unit = -1;
  do {
    size /= 1024;
    unit += 1;
  } while (size >= 1024 && unit < units.length - 1);
  return `${size.toLocaleString('zh-CN', { maximumFractionDigits: 1 })} ${units[unit]}`;
}

// 汇总单个变更的前后文件类型和权限。
function stateSummary(state: AgentShellChange['before']): string {
  if (!state) return '不存在';
  const kind = state.isDirectory ? '文件夹' : '文件';
  const details = state.isDirectory ? [] : [formatBytes(state.size)];
  details.push(`权限 ${state.mode.toString(8).padStart(4, '0')}`);
  return `${kind} · ${details.join(' · ')}`;
}

// 详细摘要默认收起，保留清单的可读性。
function ChangeState({ label, value }: { label: string; value: AgentShellChange['before'] }) {
  return (
    <details className="agent-shell-change-state">
      <summary><span>{label}</span><strong>{stateSummary(value)}</strong></summary>
      {value && <dl>
        <div><dt>SHA-256</dt><dd>{value.sha256 || '—'}</dd></div>
        {!value.isDirectory && <div><dt>大小</dt><dd>{formatBytes(value.size)}</dd></div>}
        <div><dt>权限</dt><dd>{value.mode.toString(8).padStart(4, '0')}</dd></div>
      </dl>}
    </details>
  );
}

// Shell 文件变更按页读取，预览内容只在展开时请求。
export function ShellChangeReview({
  planId,
  planVersion,
  planStatus,
  actionBusy,
  conversationBusy,
  onApprove,
  onRefreshPlan,
}: {
  planId: string;
  planVersion: number;
  planStatus: string;
  actionBusy: string;
  conversationBusy: string;
  onApprove: (changeId: string) => void;
  onRefreshPlan: () => void;
}) {
  const [offset, setOffset] = useState(0);
  const [page, setPage] = useState<Awaited<ReturnType<typeof getAgentShellChanges>> | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [refreshKey, setRefreshKey] = useState(0);
  const [previewChangeId, setPreviewChangeId] = useState('');
  const [preview, setPreview] = useState<AgentShellChangePreview | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState('');

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setPage(null);
    setError('');
    void getAgentShellChanges(planId, offset, PAGE_SIZE, controller.signal)
      .then((result) => setPage(result))
      .catch((reason: unknown) => {
        if (reason instanceof DOMException && reason.name === 'AbortError') return;
        if (reason instanceof AgentApiError && reason.status === 409) {
          setError('方案版本已变化，请刷新方案并重新核对。');
          onRefreshPlan();
        } else {
          setError(reason instanceof Error ? reason.message : '读取 Shell 变更清单失败。');
        }
      })
      .finally(() => setLoading(false));
    return () => controller.abort();
  }, [planId, planVersion, offset, refreshKey]);

  useEffect(() => {
    if (!previewChangeId) return undefined;
    const controller = new AbortController();
    setPreview(null);
    setPreviewLoading(true);
    setPreviewError('');
    const selectedChange = changes.find((change) => change.id === previewChangeId);
    if (!selectedChange) {
      setPreviewLoading(false);
      return () => controller.abort();
    }
    void getAgentShellChangePreview(planId, previewChangeId, selectedChange.path, controller.signal)
      .then(setPreview)
      .catch((reason: unknown) => {
        if (reason instanceof DOMException && reason.name === 'AbortError') return;
        if (reason instanceof AgentApiError && reason.status === 409) {
          setPreviewError('变更状态已变化，请刷新方案后重新核对。');
          onRefreshPlan();
        } else {
          setPreviewError(reason instanceof Error ? reason.message : '读取文本差异失败。');
        }
      })
      .finally(() => setPreviewLoading(false));
    return () => controller.abort();
  }, [planId, previewChangeId, planVersion]);

  const changes = page?.changes ?? [];
  return (
    <section className="agent-shell-change-review" aria-label="Shell 文件变化">
      <div className="agent-shell-change-heading">
        <strong>文件变化{page ? ` · ${page.total}` : ''}</strong>
        <button className="agent-text-button" type="button" onClick={() => {
          setPreviewChangeId('');
          setPreview(null);
          if (error) onRefreshPlan();
          setRefreshKey((value) => value + 1);
        }} disabled={loading || Boolean(actionBusy || conversationBusy)}>
          <RefreshCw size={14} aria-hidden="true" />刷新
        </button>
      </div>
      {loading && <p className="agent-shell-change-state-message" role="status"><Loader2 className="agent-spinner" size={15} aria-hidden="true" />正在读取变化…</p>}
      {error && <p className="agent-inline-error" role="alert">{error}</p>}
      {!loading && !error && page?.total === 0 && <p className="agent-shell-change-state-message">命令没有产生文件变化。</p>}
      {changes.length > 0 && (
        <ol className="agent-shell-change-list">
          {changes.map((change) => {
            const canApprove = planStatus === 'awaiting_approval' && change.approval === 'pending';
            const previewOpen = previewChangeId === change.id;
            return (
              <li className="agent-shell-change" key={change.id}>
                <div className="agent-shell-change-title">
                  <code>{change.path}</code>
                  <span className="agent-operation-state">{statusLabel(APPROVAL_LABELS, change.approval)}</span>
                </div>
                <p className="agent-shell-change-summary">
                  {statusLabel(KIND_LABELS, change.kind)} · {statusLabel(STATUS_LABELS, change.status)}
                  {change.binary ? ' · 二进制文件' : ''}
                  {change.diffTruncated ? ' · 文本差异已截断' : ''}
                </p>
                <div className="agent-shell-change-states">
                  <ChangeState label="之前" value={change.before} />
                  <ChangeState label="之后" value={change.after} />
                </div>
                <div className="agent-shell-change-actions">
                  {!change.binary && (
                    <button className="agent-text-button" type="button" aria-expanded={previewOpen} onClick={() => {
                      setPreview(null);
                      setPreviewError('');
                      setPreviewChangeId((current) => current === change.id ? '' : change.id);
                    }}>
                      {previewOpen ? <EyeOff size={14} aria-hidden="true" /> : <Eye size={14} aria-hidden="true" />}
                      {previewOpen ? '收起差异' : '查看文本差异'}
                    </button>
                  )}
                  {canApprove && (
                    <button className="agent-primary-button" type="button" onClick={() => onApprove(change.id)} disabled={Boolean(actionBusy || conversationBusy)}>
                      {actionBusy === change.id ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                      批准此项
                    </button>
                  )}
                </div>
                {previewOpen && (
                  <div className="agent-shell-change-preview" aria-live="polite">
                    {previewLoading && <p role="status"><Loader2 className="agent-spinner" size={15} aria-hidden="true" />正在读取差异…</p>}
                    {previewError && <p className="agent-inline-error" role="alert">{previewError}</p>}
                    {preview && preview.change.id === change.id && <pre>{preview.textDiff || '没有文本差异。'}</pre>}
                    {preview?.change.id === change.id && preview.change.diffTruncated && <small>只显示了差异摘要，完整差异超过预览上限。</small>}
                  </div>
                )}
              </li>
            );
          })}
        </ol>
      )}
      {page && page.total > PAGE_SIZE && (
        <nav className="agent-shell-change-pagination" aria-label="Shell 文件变化分页">
          <button className="agent-secondary-button" type="button" onClick={() => {
            setPreviewChangeId('');
            setPreview(null);
            setOffset((value) => Math.max(0, value - PAGE_SIZE));
          }} disabled={offset === 0 || loading || Boolean(actionBusy || conversationBusy)}>
            <ChevronLeft size={15} aria-hidden="true" />上一页
          </button>
          <span>{offset + 1}–{Math.min(offset + changes.length, page.total)} / {page.total}</span>
          <button className="agent-secondary-button" type="button" onClick={() => {
            setPreviewChangeId('');
            setPreview(null);
            setOffset((value) => value + PAGE_SIZE);
          }} disabled={!page.hasMore || loading || Boolean(actionBusy || conversationBusy)}>
            下一页<ChevronRight size={15} aria-hidden="true" />
          </button>
        </nav>
      )}
    </section>
  );
}
