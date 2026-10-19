// 回收站按服务端快照恢复项目，并明确确认不可逆清理。
import { useEffect, useRef, useState } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  File,
  Folder,
  Loader2,
  Recycle,
  RefreshCw,
  RotateCcw,
  Trash2,
  X,
} from 'lucide-react';
import {
  clearTrashSnapshot,
  listTrash,
  permanentlyDeleteTrashItem,
  restoreTrashItem,
  TrashApiError,
  type TrashItem,
  type TrashListing,
} from '../../api/trash';
import './TrashPage.css';

const PAGE_SIZE = 40;

type Confirmation =
  | { kind: 'permanent'; item: TrashItem }
  | { kind: 'clear'; snapshot: number; count: number };

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let index = -1;
  do {
    value /= 1024;
    index += 1;
  } while (value >= 1024 && index < units.length - 1);
  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '删除时间未知';
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function kindLabel(kind: string): string {
  if (kind === 'directory') return '文件夹';
  if (kind === 'file') return '文件';
  return kind || '项目';
}

function apiMessage(error: unknown, fallback: string): string {
  return error instanceof TrashApiError ? error.message : fallback;
}

export function TrashPage() {
  const [listing, setListing] = useState<TrashListing | null>(null);
  const [items, setItems] = useState<TrashItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState('');
  const [paginationError, setPaginationError] = useState('');
  const [actionError, setActionError] = useState('');
  const [notice, setNotice] = useState('');
  const [refreshKey, setRefreshKey] = useState(0);
  const [busyAction, setBusyAction] = useState<{ id: string; kind: 'restore' | 'permanent' } | null>(null);
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const [confirmError, setConfirmError] = useState('');
  const [confirmSubmitting, setConfirmSubmitting] = useState(false);
  const confirmSubmittingRef = useRef(false);
  const loadingMoreRef = useRef(false);
  const listGenerationRef = useRef(0);
  const paginationControllerRef = useRef<AbortController | null>(null);
  const dialogRef = useRef<HTMLElement>(null);
  const cancelButtonRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const generation = ++listGenerationRef.current;
    const controller = new AbortController();
    loadingMoreRef.current = false;
    setLoadingMore(false);
    setLoading(true);
    setError('');
    listTrash(PAGE_SIZE, undefined, controller.signal)
      .then((result) => {
        if (generation !== listGenerationRef.current) return;
        setItems(result.items);
        setListing(result);
        setPaginationError('');
      })
      .catch((caught: unknown) => {
        if (generation !== listGenerationRef.current || (caught instanceof DOMException && caught.name === 'AbortError')) return;
        setError(apiMessage(caught, '读取回收站时发生错误。'));
      })
      .finally(() => {
        if (generation === listGenerationRef.current && !controller.signal.aborted) setLoading(false);
      });
    return () => {
      controller.abort();
      paginationControllerRef.current?.abort();
      paginationControllerRef.current = null;
      if (generation === listGenerationRef.current) {
        listGenerationRef.current += 1;
        loadingMoreRef.current = false;
      }
    };
  }, [refreshKey]);

  useEffect(() => {
    if (!confirmation) return;
    const previousFocus = document.activeElement;
    cancelButtonRef.current?.focus();

    function handleDialogKeys(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        event.preventDefault();
        closeConfirmation();
        return;
      }
      if (event.key !== 'Tab') return;
      const dialog = dialogRef.current;
      if (!dialog) return;
      const focusable = dialog.querySelectorAll<HTMLElement>('button:not(:disabled)');
      if (focusable.length === 0) {
        event.preventDefault();
        dialog.focus();
        return;
      }
      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      if (event.shiftKey && (document.activeElement === first || !dialog.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !dialog.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    }

    window.addEventListener('keydown', handleDialogKeys);
    return () => {
      window.removeEventListener('keydown', handleDialogKeys);
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) previousFocus.focus();
    };
  }, [confirmation]);

  function refresh() {
    listGenerationRef.current += 1;
    loadingMoreRef.current = false;
    paginationControllerRef.current?.abort();
    paginationControllerRef.current = null;
    setLoading(true);
    setLoadingMore(false);
    setPaginationError('');
    setRefreshKey((current) => current + 1);
  }

  async function loadMore() {
    if (!listing?.hasMore || !listing.nextCursor || loadingMoreRef.current) return;
    loadingMoreRef.current = true;
    const generation = listGenerationRef.current;
    const cursor = listing.nextCursor;
    const snapshot = listing.snapshot;
    const controller = new AbortController();
    paginationControllerRef.current = controller;
    setLoadingMore(true);
    setPaginationError('');
    try {
      const result = await listTrash(PAGE_SIZE, cursor, controller.signal);
      if (generation !== listGenerationRef.current) return;
      const knownIds = new Set(items.map((item) => item.id));
      const repeatsItem = result.items.some((item) => knownIds.has(item.id));
      if (result.snapshot !== snapshot || result.limit !== listing.limit
        || (result.hasMore && result.nextCursor === cursor) || repeatsItem) {
        setNotice('回收站内容已变化，正在重新读取；请核对后再操作。');
        refresh();
        return;
      }
      setItems((current) => [...current, ...result.items]);
      setListing(result);
    } catch (caught) {
      if (generation !== listGenerationRef.current || (caught instanceof DOMException && caught.name === 'AbortError')) return;
      if (caught instanceof TrashApiError && caught.status === 409) {
        setNotice('回收站快照已变化，已请求重新载入；请核对后再操作。');
        refresh();
      } else {
        setPaginationError(apiMessage(caught, '读取更多回收项目时发生错误。'));
      }
    } finally {
      if (generation === listGenerationRef.current) {
        loadingMoreRef.current = false;
        setLoadingMore(false);
        if (paginationControllerRef.current === controller) paginationControllerRef.current = null;
      }
    }
  }

  async function restore(item: TrashItem) {
    if (item.status !== 'trashed' || busyAction) return;
    setBusyAction({ id: item.id, kind: 'restore' });
    setActionError('');
    setNotice('');
    try {
      const result = await restoreTrashItem(item.id);
      const path = result.path ? `/${result.path}` : '/';
      setNotice(result.path === item.originalPath
        ? `已恢复到「${path}」。`
        : `原路径无法直接使用，服务端已恢复到「${path}」。`);
      refresh();
    } catch (caught) {
      setActionError(apiMessage(caught, '恢复文件时发生错误，请重试。'));
    } finally {
      setBusyAction(null);
    }
  }

  function openPermanentConfirmation(item: TrashItem) {
    if (item.status !== 'trashed' || busyAction) return;
    setConfirmError('');
    setConfirmation({ kind: 'permanent', item });
  }

  function openClearConfirmation() {
    if (!listing || listing.hasMore || !items.some((item) => item.status === 'trashed') || busyAction || loading) return;
    setConfirmError('');
    setConfirmation({
      kind: 'clear',
      snapshot: listing.snapshot,
      count: items.filter((item) => item.status === 'trashed').length,
    });
  }

  function closeConfirmation() {
    if (confirmSubmittingRef.current) return;
    setConfirmation(null);
    setConfirmError('');
  }

  async function confirmDestructiveAction() {
    if (!confirmation || confirmSubmittingRef.current) return;
    confirmSubmittingRef.current = true;
    setConfirmSubmitting(true);
    setConfirmError('');
    setActionError('');
    setNotice('');
    try {
      if (confirmation.kind === 'permanent') {
        await permanentlyDeleteTrashItem(confirmation.item.id);
        setNotice(`已永久删除「${confirmation.item.name}」。此操作无法恢复。`);
      } else {
        const result = await clearTrashSnapshot(confirmation.snapshot);
        if (result.failed > 0) {
          const details = result.errors.slice(0, 3).map((item) => `${item.id}：${item.message}`).join('；');
          const remaining = result.failed > result.errors.length ? `；另有 ${result.failed - result.errors.length} 项未返回详情` : '';
          setNotice(`已永久删除 ${result.purged} 项；${result.needsAttention} 项需人工处理并保留；${result.failed} 项清理失败。`);
          setActionError(`清理部分失败${details ? `：${details}` : ''}${remaining}`);
        } else {
          setNotice(`已永久删除 ${result.purged} 项；${result.needsAttention} 项需人工处理并保留。`);
        }
      }
      setConfirmation(null);
      refresh();
    } catch (caught) {
      setConfirmError(caught instanceof TrashApiError && caught.status === 409
        ? '回收站状态已变化，清理未执行。请取消确认、刷新列表并重新核对。'
        : apiMessage(caught, '清理回收站时发生错误，请重试。'));
    } finally {
      confirmSubmittingRef.current = false;
      setConfirmSubmitting(false);
    }
  }

  const attentionCount = items.filter((item) => item.status === 'needs_attention').length;
  const clearableCount = items.filter((item) => item.status === 'trashed').length;

  return (
    <section className="trash-page" aria-labelledby="trash-page-title">
      <header className="trash-page-heading">
        <div>
          <p className="trash-eyebrow"><span />普通删除项目</p>
          <h1 id="trash-page-title">回收站</h1>
          <p>可恢复已移入的项目；永久删除后无法找回。</p>
        </div>
        <button className="trash-refresh-button" type="button" onClick={refresh} disabled={loading || Boolean(busyAction)}>
          <RefreshCw size={16} aria-hidden="true" />刷新
        </button>
      </header>

      <div className="trash-info-note" role="note">
        <Recycle size={17} aria-hidden="true" />
        <span>恢复遇到原路径冲突时，服务端会自动改名并显示实际路径。需人工处理的项目不会被恢复、清空或单项永久删除。</span>
      </div>

      {notice && <div className="trash-notice" role="status"><CheckCircle2 size={16} aria-hidden="true" /><span>{notice}</span></div>}
      {actionError && <div className="trash-error" role="alert"><AlertTriangle size={16} aria-hidden="true" /><span>{actionError}</span><button type="button" aria-label="关闭错误提示" onClick={() => setActionError('')}><X size={15} /></button></div>}

      <div className="trash-toolbar">
        <div>
          <strong>{loading ? '正在读取回收站…' : `${items.length}${listing?.hasMore ? '+' : ''} 项`}</strong>
          <small>{listing ? `快照 ${listing.snapshot} · ${listing.hasMore ? '请加载完全部项目后清空' : `已载入完整列表（每页 ${listing.limit} 项）`}` : '按服务端快照读取'}</small>
        </div>
        <button className="trash-clear-button" type="button" onClick={openClearConfirmation} disabled={loading || Boolean(error) || Boolean(busyAction) || clearableCount === 0 || !listing || listing.hasMore}>
          <Trash2 size={15} aria-hidden="true" />{listing?.hasMore ? '先加载全部项目' : '清空可删除项目'}
        </button>
      </div>

      {loading ? (
        <div className="trash-state" role="status"><Loader2 className="trash-spinner" size={20} aria-hidden="true" /><strong>正在读取回收站</strong><p>正在获取服务器回收快照。</p></div>
      ) : error ? (
        <div className="trash-state is-error" role="alert"><AlertTriangle size={21} aria-hidden="true" /><strong>无法读取回收站</strong><p>{error}</p><button type="button" onClick={refresh}>重试</button></div>
      ) : items.length === 0 ? (
        <div className="trash-state"><span className="trash-empty-icon"><Recycle size={22} aria-hidden="true" /></span><strong>回收站为空</strong><p>从文件列表普通删除的文件和文件夹会显示在这里。</p></div>
      ) : (
        <>
          {attentionCount > 0 && <p className="trash-attention-summary" role="status"><AlertTriangle size={15} aria-hidden="true" />当前已载入 {attentionCount} 项需人工处理；清空时服务端会跳过这些项目。</p>}
          <ul className="trash-list" aria-label="回收站项目">
            {items.map((item) => {
              const isAttention = item.status === 'needs_attention';
              const ItemIcon = item.kind === 'directory' ? Folder : File;
              const actionBusy = busyAction?.id === item.id;
              return (
                <li className={`trash-item${isAttention ? ' is-attention' : ''}`} key={item.id}>
                  <span className="trash-item-icon"><ItemIcon size={19} aria-hidden="true" /></span>
                  <div className="trash-item-content">
                    <strong title={item.name}>{item.name}</strong>
                    <span className="trash-original-path" title={item.originalPath}>原路径：/{item.originalPath}</span>
                    <small>{kindLabel(item.kind)} · {item.kind === 'directory' ? '—' : formatBytes(item.size)} · 删除于 {formatDate(item.deletedAt)}</small>
                    {isAttention && <p className="trash-attention-note">需人工处理：服务端未能确认安全清理条件；暂不能恢复或删除，等待人工核查。</p>}
                  </div>
                  <span className={`trash-status${isAttention ? ' is-attention' : ''}`}>{isAttention ? '需人工处理' : '可恢复'}</span>
                  <div className="trash-item-actions">
                    <button type="button" onClick={() => { void restore(item); }} disabled={isAttention || Boolean(busyAction)} title={isAttention ? '此项目需人工处理，不能在此恢复' : undefined}>
                      {actionBusy && busyAction?.kind === 'restore' ? <Loader2 className="trash-spinner" size={15} aria-hidden="true" /> : <RotateCcw size={15} aria-hidden="true" />}
                      恢复
                    </button>
                    <button className="is-danger" type="button" onClick={() => openPermanentConfirmation(item)} disabled={isAttention || Boolean(busyAction)} title={isAttention ? '此项目需人工处理，不能在此永久删除' : undefined}>
                      {actionBusy && busyAction?.kind === 'permanent' ? <Loader2 className="trash-spinner" size={15} aria-hidden="true" /> : <Trash2 size={15} aria-hidden="true" />}
                      永久删除
                    </button>
                  </div>
                </li>
              );
            })}
          </ul>
          {listing?.hasMore && (
            <>
              {paginationError && <div className="trash-pagination-error" role="alert"><span>{paginationError}</span><button type="button" onClick={() => { void loadMore(); }} disabled={loadingMore}>重试</button></div>}
              <div className="trash-load-more">
                <button type="button" onClick={() => { void loadMore(); }} disabled={loadingMore || Boolean(busyAction)}>
                  {loadingMore ? <><Loader2 className="trash-spinner" size={15} aria-hidden="true" />正在读取…</> : '加载更多'}
                </button>
                <span>已载入 {items.length} 项</span>
              </div>
            </>
          )}
          {listing && !listing.hasMore && <p className="trash-list-footer">已显示当前快照的全部项目；其中 {clearableCount} 项可清理，{attentionCount} 项需人工处理。</p>}
        </>
      )}

      {confirmation && (
        <div className="trash-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) closeConfirmation(); }}>
          <section ref={dialogRef} className="trash-dialog" role="alertdialog" aria-modal="true" aria-labelledby="trash-dialog-title" aria-describedby="trash-dialog-description" tabIndex={-1}>
            <header className="trash-dialog-heading"><span><AlertTriangle size={18} aria-hidden="true" /></span><h2 id="trash-dialog-title">{confirmation.kind === 'clear' ? '清空回收站快照？' : '永久删除此项目？'}</h2></header>
            {confirmation.kind === 'clear' ? (
            <p id="trash-dialog-description">将永久删除当前快照中 {confirmation.count} 项可清理项目，此操作无法恢复。需人工处理的项目会由服务端跳过并保留。</p>
            ) : (
              <p id="trash-dialog-description">「{confirmation.item.name}」将被永久删除，之后无法恢复。确认前请核对项目名称和原路径。</p>
            )}
            {confirmError && <p className="trash-dialog-error" role="alert">{confirmError}</p>}
            <div className="trash-dialog-actions">
              <button ref={cancelButtonRef} type="button" onClick={closeConfirmation} disabled={confirmSubmitting}>取消</button>
              <button type="button" className="is-danger" onClick={() => { void confirmDestructiveAction(); }} disabled={confirmSubmitting}>
                {confirmSubmitting ? <><Loader2 className="trash-spinner" size={15} aria-hidden="true" />正在删除…</> : <><Trash2 size={15} aria-hidden="true" />{confirmation.kind === 'clear' ? '永久删除可清理项目' : '永久删除'}</>}
              </button>
            </div>
          </section>
        </div>
      )}
    </section>
  );
}
