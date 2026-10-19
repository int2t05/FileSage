// Shell 文件变化直接展示为可审阅的文本差异。
import { useEffect, useState } from 'react';
import { ChevronDown, ChevronLeft, ChevronRight, Loader2 } from 'lucide-react';
import {
  AgentApiError,
  getAgentShellChangePreview,
  getAgentShellChanges,
  type AgentShellChange,
  type AgentShellChangePreview,
} from '../../api/agent';
import { isUnifiedTextDiff, parseTextDiff } from './textDiff';
import './ShellChangeReview.css';

const PAGE_SIZE = 50;
const KIND_LABELS: Record<string, string> = {
  added: '新增',
  modified: '修改',
  deleted: '删除',
};
function statusLabel(labels: Record<string, string>, value: string): string {
  return labels[value] ?? '状态未知';
}

function previewMessage(change: AgentShellChange): string {
  if (change.binary) return '二进制文件，不显示文本差异。';
  if (change.before?.isDirectory || change.after?.isDirectory) return '目录变化，不包含正文差异。';
  if (change.status === 'succeeded' && change.kind !== 'added') return '未保留执行前内容，无法显示原始差异。';
  return '';
}

function TextDiff({ value, emptyFile = false }: { value: string; emptyFile?: boolean }) {
  if (!value.trim()) return <p className="agent-shell-diff-empty">没有文本变化。</p>;
  if (!isUnifiedTextDiff(value)) {
    return <p className="agent-shell-diff-message" role="status">{value.trim()}</p>;
  }
  const rows = parseTextDiff(value);
  const contentRows = rows.filter((row) => row.kind === 'added' || row.kind === 'removed' || row.kind === 'context');
  if (contentRows.length === 0) return <p className="agent-shell-diff-empty">{emptyFile ? '空文件，没有文本行差异。' : '没有可显示的文本行差异。'}</p>;
  const added = rows.filter((row) => row.kind === 'added').length;
  const removed = rows.filter((row) => row.kind === 'removed').length;
  return (
    <div className="agent-shell-diff-view">
      <div className="agent-shell-diff-summary" aria-label={`新增 ${added} 行，删除 ${removed} 行`}>
        <span className="is-added">+{added}</span>
        <span className="is-removed">−{removed}</span>
      </div>
      <pre className="agent-shell-diff"><code>{rows.map((row, index) => (
        row.kind === 'header' ? null : row.kind === 'hunk' ? (
          <span className="agent-shell-diff-line is-hunk" key={`${index}:${row.text}`}>
            <span className="agent-shell-diff-full-line">{row.text}</span>
          </span>
        ) : row.kind === 'meta' ? (
          <span className={`agent-shell-diff-line is-${row.kind}`} key={`${index}:${row.text}`}>
            <span className="agent-shell-diff-full-line">{row.text}</span>
          </span>
        ) : (
          <span className={`agent-shell-diff-line is-${row.kind}`} key={`${index}:${row.text}`}>
            <span className="agent-shell-diff-number">{row.oldLine ?? ''}</span>
            <span className="agent-shell-diff-number">{row.newLine ?? ''}</span>
            <span className="agent-shell-diff-marker" aria-hidden="true">{row.kind === 'added' ? '+' : row.kind === 'removed' ? '−' : ' '}</span>
            <span className="agent-shell-diff-text">{row.text.slice(1) || ' '}</span>
          </span>
        )
      ))}</code></pre>
    </div>
  );
}

export function ShellChangeReview({
  planId,
  planVersion,
  executionStatus,
  actionBusy = '',
  conversationBusy = '',
}: {
  planId: string;
  planVersion: number;
  executionStatus: string;
  actionBusy?: string;
  conversationBusy?: string;
}) {
  const [offset, setOffset] = useState(0);
  const [page, setPage] = useState<Awaited<ReturnType<typeof getAgentShellChanges>> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [refreshKey, setRefreshKey] = useState(0);
  const [openChangeId, setOpenChangeId] = useState('');
  const [preview, setPreview] = useState<AgentShellChangePreview | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState('');
  const changes = page?.changes ?? [];

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setPage(null);
    setOpenChangeId('');
    setError('');
    void getAgentShellChanges(planId, offset, PAGE_SIZE, controller.signal)
      .then((nextPage) => {
        setPage(nextPage);
        const onlyChange = nextPage.changes.length === 1 ? nextPage.changes[0] : undefined;
        if (nextPage.total === 1 && onlyChange && !previewMessage(onlyChange)) setOpenChangeId(onlyChange.id);
      })
      .catch((reason: unknown) => {
        if (reason instanceof DOMException && reason.name === 'AbortError') return;
        setError(reason instanceof AgentApiError && reason.status === 409
          ? '方案已变化，请刷新后重试。'
          : reason instanceof Error ? reason.message : '读取文件变化失败。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [planId, planVersion, executionStatus, offset, refreshKey]);

  useEffect(() => {
    if (!openChangeId) return undefined;
    const controller = new AbortController();
    setPreview(null);
    setPreviewError('');
    const selectedChange = changes.find((change) => change.id === openChangeId);
    if (!selectedChange || previewMessage(selectedChange)) {
      setPreviewLoading(false);
      return () => controller.abort();
    }
    setPreviewLoading(true);
    void getAgentShellChangePreview(planId, openChangeId, selectedChange.path, controller.signal)
      .then(setPreview)
      .catch((reason: unknown) => {
        if (reason instanceof DOMException && reason.name === 'AbortError') return;
        setPreviewError(reason instanceof AgentApiError && reason.status === 409
          ? '文件状态已变化，请刷新方案后重新查看。'
          : reason instanceof Error ? reason.message : '读取差异失败。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setPreviewLoading(false);
      });
    return () => controller.abort();
  }, [planId, openChangeId, planVersion, changes]);

  function retryChanges() {
    setOpenChangeId('');
    setPreview(null);
    setPreviewError('');
    setRefreshKey((value) => value + 1);
  }

  if (!loading && !error && page?.total === 0) return null;

  return (
    <section className="agent-shell-change-review" aria-label="Shell 文件差异">
      <div className="agent-shell-change-content">
        {loading && <p className="agent-shell-change-state-message" role="status"><Loader2 className="agent-spinner" size={15} aria-hidden="true" />正在读取差异…</p>}
        {error && (
          <div className="agent-shell-change-error">
            <p className="agent-inline-error" role="alert">{error}</p>
            <button className="agent-secondary-button" type="button" onClick={retryChanges} disabled={loading || Boolean(actionBusy || conversationBusy)}>重试</button>
          </div>
        )}
        {changes.length > 0 && (
          <ol className="agent-shell-change-list">
            {changes.map((change) => {
              const previewOpen = openChangeId === change.id;
              const message = previewMessage(change);
              return (
                <li className="agent-shell-change" key={change.id}>
                  <button className="agent-shell-change-toggle-file" type="button" aria-expanded={previewOpen} onClick={() => {
                    setPreview(null);
                    setPreviewError('');
                    setOpenChangeId((current) => current === change.id ? '' : change.id);
                  }} aria-label={`${statusLabel(KIND_LABELS, change.kind)} ${change.path}，${previewOpen ? message ? '收起详情' : '收起差异' : message ? '查看详情' : '查看差异'}`}>
                    <span className={`agent-shell-change-kind is-${change.kind}`} aria-hidden="true">{change.kind === 'added' ? '+' : change.kind === 'deleted' ? '−' : '~'}</span>
                    <code>{change.path}</code>
                    <span className="agent-shell-change-label">{statusLabel(KIND_LABELS, change.kind)}</span>
                    <ChevronDown className={previewOpen ? 'is-expanded' : ''} size={15} aria-hidden="true" />
                  </button>
                  {previewOpen && (
                    <div className="agent-shell-change-preview" aria-live="polite">
                      {message && <p>{message}</p>}
                      {previewLoading && <p role="status"><Loader2 className="agent-spinner" size={15} aria-hidden="true" />正在读取差异…</p>}
                      {previewError && <p className="agent-inline-error" role="alert">{previewError}</p>}
                      {preview && preview.change.id === change.id && <TextDiff value={preview.textDiff} emptyFile={preview.change.after?.size === 0} />}
                      {preview?.change.id === change.id && preview.change.diffTruncated && isUnifiedTextDiff(preview.textDiff) && <small>差异过长，后续内容已省略。</small>}
                    </div>
                  )}
                </li>
              );
            })}
          </ol>
        )}
        {page && page.total > PAGE_SIZE && (
          <nav className="agent-shell-change-pagination" aria-label="文件变化分页">
            <button className="agent-secondary-button" type="button" onClick={() => {
              setOpenChangeId('');
              setOffset((value) => Math.max(0, value - PAGE_SIZE));
            }} disabled={offset === 0 || loading || Boolean(actionBusy || conversationBusy)}>
              <ChevronLeft size={15} aria-hidden="true" />上一页
            </button>
            <span>{offset + 1}–{Math.min(offset + changes.length, page.total)} / {page.total}</span>
            <button className="agent-secondary-button" type="button" onClick={() => {
              setOpenChangeId('');
              setOffset((value) => value + PAGE_SIZE);
            }} disabled={!page.hasMore || loading || Boolean(actionBusy || conversationBusy)}>
              下一页<ChevronRight size={15} aria-hidden="true" />
            </button>
          </nav>
        )}
      </div>
    </section>
  );
}
