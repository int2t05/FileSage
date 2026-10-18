// 照片页按真实拍摄日期浏览管理根目录中的图片。
import { useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties, FormEvent } from 'react';
import { createPortal } from 'react-dom';
import { AlertTriangle, ChevronLeft, ChevronRight, Image as ImageIcon, LoaderCircle, RefreshCw, ZoomIn, ZoomOut } from 'lucide-react';
import { ApiError, listDirectory, type FileEntry } from '../../api/files';
import { imagePreviewURL, requiresImageConversion } from '../reader/imagePreview';
import { ReaderPanel } from '../reader/ReaderPanel';
import './PhotosPage.css';

interface CaptureRange {
  from: string;
  through: string;
}

const pageSize = 60;

// 按服务端拍摄日生成 UTC 筛选边界，避免浏览器时区偏移。
function dateBoundary(value: string, nextDay = false): string | undefined {
  if (!value) return undefined;
  const [year, month, day] = value.split('-').map(Number);
  if (!year || !month || !day) return undefined;
  const date = new Date(0);
  date.setUTCFullYear(year, month - 1, day + (nextDay ? 1 : 0));
  date.setUTCHours(0, 0, 0, 0);
  return date.toISOString();
}

// 以拍摄日分组，缺少拍摄时间的照片统一放在最后。
function photoDay(entry: FileEntry): { key: string; label: string } {
  if (!entry.capturedAt) return { key: 'unknown', label: '拍摄日期未知' };
  const date = new Date(entry.capturedAt);
  if (Number.isNaN(date.valueOf())) return { key: 'unknown', label: '拍摄日期未知' };
  const key = [date.getUTCFullYear(), String(date.getUTCMonth() + 1).padStart(2, '0'), String(date.getUTCDate()).padStart(2, '0')].join('-');
  return { key, label: new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'long', timeZone: 'UTC' }).format(date) };
}

function previewURL(path: string): string {
  return `/api/v1/files/preview?${new URLSearchParams({ path }).toString()}`;
}

function photoPreviewURL(entry: FileEntry): string {
  return requiresImageConversion(entry.name)
    ? imagePreviewURL(entry.path, entry.version, 'thumb')
    : previewURL(entry.path);
}

function formatCaptureTime(value?: string): string {
  if (!value) return '日期未知';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '日期未知';
  return new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }).format(date);
}

// 按索引保存的 UTC 日期载体展示原始拍摄日历字段。
// 只在当前已加载的照片列表内切换。
function adjacentPhoto(entries: FileEntry[], currentPath: string, direction: -1 | 1): FileEntry | null {
  const index = entries.findIndex((entry) => entry.path === currentPath);
  return index < 0 ? null : entries[index + direction] ?? null;
}

// 将照片缩放限制在可用范围内。
function stepPhotoZoom(current: number, direction: -1 | 1): number {
  return Math.max(50, Math.min(300, current + direction * 25));
}

// 显示图片缩略预览；浏览器不支持时仍可进入文件阅读器。
function PhotoCard({ entry, onOpen }: { entry: FileEntry; onOpen: (entry: FileEntry) => void }) {
  const [failed, setFailed] = useState(false);

  return (
    <button className="photo-card" type="button" onClick={() => onOpen(entry)} aria-label={`阅读 ${entry.name}，${formatCaptureTime(entry.capturedAt)}`}>
      <span className={`photo-card-media${failed ? ' photo-card-media-failed' : ''}`}>
        {failed ? <ImageIcon size={24} aria-hidden="true" /> : (
          <img src={photoPreviewURL(entry)} alt="" loading="lazy" decoding="async" onError={() => setFailed(true)} />
        )}
      </span>
      <span className="photo-card-caption">
        <strong title={entry.name}>{entry.name}</strong>
        <time dateTime={entry.capturedAt}>{formatCaptureTime(entry.capturedAt)}</time>
      </span>
    </button>
  );
}

export function PhotosPage() {
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [nextCursor, setNextCursor] = useState('');
  const [indexState, setIndexState] = useState('');
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [range, setRange] = useState<CaptureRange>({ from: '', through: '' });
  const [draftRange, setDraftRange] = useState<CaptureRange>({ from: '', through: '' });
  const [refreshKey, setRefreshKey] = useState(0);
  const [readerTarget, setReaderTarget] = useState<FileEntry | null>(null);
  const [readerZoom, setReaderZoom] = useState(100);
  const [readerControlsHost, setReaderControlsHost] = useState<HTMLElement | null>(null);
  const loadMoreRef = useRef<AbortController | null>(null);
  const pageRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (!readerTarget) return;
    const panel = pageRef.current?.querySelector<HTMLElement>('.reader-panel');
    if (!panel) return;

    const host = document.createElement('div');
    panel.append(host);
    setReaderControlsHost(host);
    return () => {
      setReaderControlsHost(null);
      host.remove();
    };
  }, [Boolean(readerTarget)]);

  const groups = useMemo(() => {
    const grouped = new Map<string, { label: string; entries: FileEntry[] }>();
    for (const entry of entries) {
      const day = photoDay(entry);
      const group = grouped.get(day.key) ?? { label: day.label, entries: [] };
      group.entries.push(entry);
      grouped.set(day.key, group);
    }
    return Array.from(grouped, ([key, group]) => ({ key, ...group }));
  }, [entries]);

  useEffect(() => {
    const controller = new AbortController();
    loadMoreRef.current?.abort();
    setLoading(true);
    setLoadingMore(false);
    setLoadError('');
    setEntries([]);
    setHasMore(false);
    setNextCursor('');

    const filters: { type: 'image'; capturedFrom?: string; capturedBefore?: string } = { type: 'image' };
    const capturedFrom = dateBoundary(range.from);
    const capturedBefore = dateBoundary(range.through, true);
    if (capturedFrom) filters.capturedFrom = capturedFrom;
    if (capturedBefore) filters.capturedBefore = capturedBefore;

    void listDirectory('', {
      limit: pageSize,
      cursor: '',
      query: '',
      scope: 'recursive',
      sort: 'captured-desc',
      filters,
    }, controller.signal)
      .then((page) => {
        if (controller.signal.aborted) return;
        setEntries(page.entries);
        setHasMore(page.hasMore);
        setNextCursor(page.nextCursor);
        setIndexState(page.indexState);
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted || error instanceof DOMException && error.name === 'AbortError') return;
        setLoadError(error instanceof ApiError ? error.message : '读取照片失败，请重试。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });

    return () => controller.abort();
  }, [range, refreshKey]);

  // 按当前索引代次继续加载，避免把新扫描结果拼进旧游标。
  async function loadMore() {
    if (!hasMore || loadingMore || !nextCursor) return;
    const controller = new AbortController();
    loadMoreRef.current?.abort();
    loadMoreRef.current = controller;
    setLoadingMore(true);
    setLoadError('');
    const filters: { type: 'image'; capturedFrom?: string; capturedBefore?: string } = { type: 'image' };
    const capturedFrom = dateBoundary(range.from);
    const capturedBefore = dateBoundary(range.through, true);
    if (capturedFrom) filters.capturedFrom = capturedFrom;
    if (capturedBefore) filters.capturedBefore = capturedBefore;

    try {
      const page = await listDirectory('', {
        limit: pageSize,
        cursor: nextCursor,
        query: '',
        scope: 'recursive',
        sort: 'captured-desc',
        filters,
      }, controller.signal);
      if (controller.signal.aborted) return;
      setEntries((current) => [...current, ...page.entries]);
      setHasMore(page.hasMore);
      setNextCursor(page.nextCursor);
      setIndexState(page.indexState);
    } catch (error) {
      if (!(error instanceof DOMException && error.name === 'AbortError')) {
        setLoadError(error instanceof ApiError ? error.message : '读取更多照片失败，请重试。');
      }
    } finally {
      if (!controller.signal.aborted) setLoadingMore(false);
    }
  }

  function applyDateRange(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setRange(draftRange);
  }

  function clearDateRange() {
    setDraftRange({ from: '', through: '' });
    setRange({ from: '', through: '' });
  }

  function openPhoto(entry: FileEntry) {
    setReaderZoom(100);
    setReaderTarget(entry);
  }

  function navigatePhoto(direction: -1 | 1) {
    if (!readerTarget) return;
    const next = adjacentPhoto(entries, readerTarget.path, direction);
    if (!next) return;
    setReaderZoom(100);
    setReaderTarget(next);
  }

  const readerIndex = readerTarget ? entries.findIndex((entry) => entry.path === readerTarget.path) : -1;

  const indexNotice = indexState === 'ready'
    ? '按可读取的拍摄时间排序；缺少拍摄时间的照片列在最后。'
    : indexState === 'building' || indexState === 'stale' || indexState === 'not_started'
      ? '首次完整扫描后即可按拍摄时间浏览；当前索引可能尚未包含全部照片。'
      : '';

  return (
    <section
      className="photos-page"
      aria-labelledby="photos-page-title"
      ref={pageRef}
      style={{ '--photo-reader-scale': String(readerZoom / 100) } as CSSProperties}
    >
      <header className="photos-header">
        <div>
          <p className="eyebrow">真实目录视图</p>
          <h1 id="photos-page-title">照片</h1>
          <p>按拍摄日期浏览服务器中的图片，不复制或移动原件。</p>
        </div>
        <button className="photos-refresh" type="button" onClick={() => setRefreshKey((value) => value + 1)} disabled={loading}>
          <RefreshCw size={16} aria-hidden="true" />刷新
        </button>
      </header>

      <form className="photos-filters" onSubmit={applyDateRange}>
        <label>拍摄日期从<input type="date" value={draftRange.from} onChange={(event) => setDraftRange((value) => ({ ...value, from: event.target.value }))} /></label>
        <label>到<input type="date" value={draftRange.through} onChange={(event) => setDraftRange((value) => ({ ...value, through: event.target.value }))} /></label>
        <button type="submit" disabled={loading}>筛选</button>
        {(range.from || range.through) && <button type="button" className="photos-filter-clear" onClick={clearDateRange}>清除</button>}
      </form>

      {indexNotice && <p className="photos-index-note" role="status">{indexNotice}</p>}

      {loadError && (
        <div className="photos-state photos-state-error" role="alert">
          <AlertTriangle size={22} aria-hidden="true" />
          <strong>照片暂时无法读取</strong>
          <span>{loadError}</span>
          <button type="button" onClick={() => setRefreshKey((value) => value + 1)}><RefreshCw size={15} aria-hidden="true" />重试</button>
        </div>
      )}

      {loading && !loadError && <div className="photos-state" role="status"><LoaderCircle className="photos-spinner" size={22} aria-hidden="true" />正在读取照片…</div>}

      {!loading && !loadError && entries.length === 0 && (
        <div className="photos-state">
          <ImageIcon size={24} aria-hidden="true" />
          <strong>暂无照片</strong>
          <span>完成文件索引后，支持格式的图片会按拍摄日期显示在这里。</span>
        </div>
      )}

      {!loadError && groups.map((group) => (
        <section className="photo-day-group" key={group.key} aria-label={group.label}>
          <h2>{group.label}<span>{group.entries.length}</span></h2>
          <div className="photo-grid">
            {group.entries.map((entry) => <PhotoCard key={entry.path} entry={entry} onOpen={openPhoto} />)}
          </div>
        </section>
      ))}

      {hasMore && !loadError && (
        <button className="photos-load-more" type="button" onClick={() => void loadMore()} disabled={loadingMore}>
          {loadingMore ? <LoaderCircle className="photos-spinner" size={16} aria-hidden="true" /> : null}
          {loadingMore ? '加载中…' : '加载更多'}
        </button>
      )}

      {readerTarget && (
        <>
          <ReaderPanel path={readerTarget.path} name={readerTarget.name} kind="image" capturedAt={readerTarget.capturedAt} onClose={() => setReaderTarget(null)} />
          {readerControlsHost && createPortal(
            <div className="photo-reader-controls" role="group" aria-label="照片阅读控制">
              <span className="photo-reader-scope" aria-live="polite">
                第 {Math.max(readerIndex + 1, 1)} / {entries.length} 张 · 当前已加载照片
              </span>
              <div className="photo-reader-control-buttons">
                <button type="button" aria-label="上一张照片" onClick={() => navigatePhoto(-1)} disabled={readerIndex <= 0}>
                  <ChevronLeft size={18} aria-hidden="true" /><span>上一张</span>
                </button>
                <button type="button" aria-label="缩小照片" onClick={() => setReaderZoom((value) => stepPhotoZoom(value, -1))} disabled={readerZoom <= 50}>
                  <ZoomOut size={18} aria-hidden="true" /><span>缩小</span>
                </button>
                <span className="photo-reader-zoom" aria-label={`当前缩放 ${readerZoom}%`}>{readerZoom}%</span>
                <button type="button" aria-label="放大照片" onClick={() => setReaderZoom((value) => stepPhotoZoom(value, 1))} disabled={readerZoom >= 300}>
                  <ZoomIn size={18} aria-hidden="true" /><span>放大</span>
                </button>
                <button type="button" aria-label="下一张照片" onClick={() => navigatePhoto(1)} disabled={readerIndex < 0 || readerIndex >= entries.length - 1}>
                  <span>下一张</span><ChevronRight size={18} aria-hidden="true" />
                </button>
              </div>
            </div>,
            readerControlsHost,
          )}
        </>
      )}
    </section>
  );
}
