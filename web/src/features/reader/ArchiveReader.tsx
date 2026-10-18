// 只读浏览压缩包内容，并在用户确认后解压到真实目录。
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import {
  Archive,
  ArrowDownToLine,
  ChevronLeft,
  ChevronRight,
  File,
  FileArchive,
  FileText,
  Folder,
  FolderOpen,
  LoaderCircle,
  RefreshCw,
  X,
} from 'lucide-react';
import { ArchiveApiError, archiveContentURL, extractArchive, listArchiveEntries, type ArchiveEntry, type ArchiveExtractResult } from '../../api/archive';
import { listDirectory } from '../../api/files';
import type { ReaderMediaKind } from './ReaderPanel';
import { ReaderPanel } from './ReaderPanel';
import { archivePreviewKind, formatArchiveSize, sortArchiveEntries } from './archiveReaderRules';
import './ArchiveReader.css';

interface ArchiveReaderProps {
  path: string;
  name: string;
  version: string;
  onClose: () => void;
  onExtracted: () => void;
}

interface PreviewTarget {
  path: string;
  name: string;
  kind: ReaderMediaKind;
  sourceURL: string;
}

const directoryPageSize = 100;
const directoryOptions = {
  limit: directoryPageSize,
  cursor: '',
  query: '',
  scope: 'current' as const,
  sort: 'name-asc' as const,
};

function parentDirectory(path: string): string {
  const separator = path.lastIndexOf('/');
  return separator < 0 ? '' : path.slice(0, separator);
}

// 判断已选目录是否覆盖了当前条目。
function isPathWithin(parent: string, child: string): boolean {
  return child.startsWith(`${parent}/`);
}

// 只列出真实目录，避免把链接路径作为解压目标。
function sortDirectories(entries: Array<{ name: string; path: string }>): Array<{ name: string; path: string }> {
  return [...entries].sort((left, right) => left.name.localeCompare(right.name, 'zh-CN', { numeric: true, sensitivity: 'base' }));
}

// 按压缩包版本浏览条目，审批目标后执行解压。
export function ArchiveReader({ path, name, version, onClose, onExtracted }: ArchiveReaderProps) {
  const titleId = useId();
  const pathId = useId();
  const panelRef = useRef<HTMLElement>(null);
  const destinationDialogRef = useRef<HTMLElement>(null);
  const destinationReturnFocusRef = useRef<HTMLElement | null>(null);
  const entryControllerRef = useRef<AbortController | null>(null);
  const destinationControllerRef = useRef<AbortController | null>(null);
  const extractControllerRef = useRef<AbortController | null>(null);
  const [directory, setDirectory] = useState('');
  const [entries, setEntries] = useState<ArchiveEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [loadAttempt, setLoadAttempt] = useState(0);
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [previewTarget, setPreviewTarget] = useState<PreviewTarget | null>(null);
  const [extractOpen, setExtractOpen] = useState(false);
  const [destination, setDestination] = useState('');
  const [destinationCursors, setDestinationCursors] = useState<string[]>(['']);
  const [destinationPage, setDestinationPage] = useState(0);
  const [destinationDirectories, setDestinationDirectories] = useState<Array<{ name: string; path: string }>>([]);
  const [destinationHasMore, setDestinationHasMore] = useState(false);
  const [destinationLoading, setDestinationLoading] = useState(false);
  const [destinationError, setDestinationError] = useState('');
  const [destinationAttempt, setDestinationAttempt] = useState(0);
  const [extracting, setExtracting] = useState(false);
  const [extractError, setExtractError] = useState('');
  const [extractResult, setExtractResult] = useState<ArchiveExtractResult | null>(null);

  const sortedEntries = useMemo(() => sortArchiveEntries(entries), [entries]);
  const selectedCount = selected.size;
  const destinationCursor = destinationCursors[destinationPage] ?? '';

  useEffect(() => {
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    panelRef.current?.querySelector<HTMLButtonElement>('.archive-reader-close')?.focus();
    return () => {
      document.body.style.overflow = previousOverflow;
      if (previousFocus?.isConnected) previousFocus.focus();
      entryControllerRef.current?.abort();
      destinationControllerRef.current?.abort();
      extractControllerRef.current?.abort();
    };
  }, []);

  useEffect(() => {
    if (previewTarget) return;
    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        event.preventDefault();
        if (extractOpen) setExtractOpen(false);
        else onClose();
        return;
      }
      if (event.key !== 'Tab') return;
      const panel = panelRef.current;
      if (!panel) return;
      const modal = panel.querySelector<HTMLElement>('.archive-destination-dialog') ?? panel;
      const focusable = modal.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], input:not(:disabled), [tabindex]:not([tabindex="-1"])');
      if (!focusable.length) {
        event.preventDefault();
        panel.focus();
        return;
      }
      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      if (event.shiftKey && (document.activeElement === first || !modal.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !modal.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    }
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [extractOpen, onClose, previewTarget]);

  useEffect(() => {
    if (extractOpen) destinationDialogRef.current?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
    else {
      destinationReturnFocusRef.current?.focus();
      destinationReturnFocusRef.current = null;
    }
  }, [extractOpen]);

  useEffect(() => {
    const controller = new AbortController();
    entryControllerRef.current?.abort();
    entryControllerRef.current = controller;
    setLoading(true);
    setLoadError('');
    void listArchiveEntries(path, version, directory, controller.signal)
      .then((listing) => setEntries(listing.entries))
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        setLoadError(error instanceof Error ? error.message : '读取压缩包目录失败。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [directory, loadAttempt, path, version]);

  useEffect(() => {
    if (!extractOpen) return;
    const controller = new AbortController();
    destinationControllerRef.current?.abort();
    destinationControllerRef.current = controller;
    setDestinationLoading(true);
    setDestinationError('');
    void listDirectory(destination, { ...directoryOptions, cursor: destinationCursor }, controller.signal)
      .then((listing) => {
        setDestinationDirectories(sortDirectories(listing.entries
          .filter((entry) => entry.kind === 'directory' && !entry.symlink)
          .map(({ name: folderName, path: folderPath }) => ({ name: folderName, path: folderPath }))));
        setDestinationHasMore(listing.hasMore);
        if (listing.hasMore) {
          setDestinationCursors((current) => current.length <= destinationPage + 1 ? [...current, listing.nextCursor] : current);
        }
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) setDestinationError(error instanceof Error ? error.message : '读取目标目录失败。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setDestinationLoading(false);
      });
    return () => controller.abort();
  }, [destination, destinationAttempt, destinationCursor, destinationPage, extractOpen]);

  function toggleSelection(entry: ArchiveEntry) {
    if (entry.kind === 'unsupported') return;
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(entry.path)) {
        next.delete(entry.path);
        return next;
      }
      for (const selectedPath of current) {
        if (isPathWithin(selectedPath, entry.path)) next.delete(selectedPath);
        else if (isPathWithin(entry.path, selectedPath)) next.delete(selectedPath);
      }
      next.add(entry.path);
      return next;
    });
    setExtractResult(null);
    setExtractError('');
  }

  // 每次打开解压流程都从管理根目录开始选择目标。
  function openDestinationPicker() {
    destinationReturnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setDestination('');
    setDestinationCursors(['']);
    setDestinationPage(0);
    setDestinationAttempt((value) => value + 1);
    setDestinationError('');
    setExtractError('');
    setExtractOpen(true);
  }

  function enterDestination(pathname: string) {
    setDestination(pathname);
    setDestinationCursors(['']);
    setDestinationPage(0);
  }

  async function confirmExtraction() {
    setExtracting(true);
    setExtractError('');
    setExtractResult(null);
    const controller = new AbortController();
    extractControllerRef.current?.abort();
    extractControllerRef.current = controller;
    try {
      const result = await extractArchive(path, version, destination, [...selected].sort(), controller.signal);
      setExtractResult(result);
      setExtractOpen(false);
      setSelected(result.status === 'partial' ? new Set(result.errors.map(({ entry }) => entry)) : new Set());
      if (result.paths.length > 0) onExtracted();
    } catch (error) {
      if (controller.signal.aborted) return;
      setExtractError(error instanceof ArchiveApiError || error instanceof Error ? error.message : '解压失败。');
    } finally {
      if (extractControllerRef.current === controller) extractControllerRef.current = null;
      if (!controller.signal.aborted) setExtracting(false);
    }
  }

  function openPreview(entry: ArchiveEntry) {
    const kind = archivePreviewKind(entry);
    if (!kind) return;
    setPreviewTarget({ path: entry.path, name: entry.name, kind, sourceURL: archiveContentURL(path, version, entry.path) });
  }

  function changeDestinationPage(direction: -1 | 1) {
    setDestinationPage((page) => Math.max(0, Math.min(destinationCursors.length - 1, page + direction)));
  }

  const destinationLabel = destination ? `/${destination}` : '/ 管理根目录';

  return (
    <div className="archive-reader-backdrop">
      <section ref={panelRef} className="archive-reader-panel" role="dialog" aria-modal="true" aria-labelledby={titleId} aria-describedby={pathId} tabIndex={-1}>
        <header className="archive-reader-header">
          <div className="archive-reader-heading">
            <span className="archive-reader-eyebrow"><Archive size={14} aria-hidden="true" />压缩包 · 只读</span>
            <h2 id={titleId} title={name}>{name}</h2>
            <p id={pathId} title={path}>/{path}</p>
          </div>
          <button className="archive-reader-close" type="button" onClick={onClose} aria-label={`关闭压缩包：${name}`}><X size={19} aria-hidden="true" /></button>
        </header>

        <div className="archive-reader-toolbar">
          <div className="archive-reader-breadcrumb">
            <button type="button" className="archive-reader-up" onClick={() => setDirectory(parentDirectory(directory))} disabled={!directory || loading} aria-label="返回上级目录"><ChevronLeft size={17} aria-hidden="true" />上一级</button>
            <span title={directory || '/'}>{directory ? `/${directory}` : '/ 压缩包根目录'}</span>
          </div>
          <button type="button" className="archive-reader-extract" onClick={openDestinationPicker} disabled={loading || Boolean(loadError) || extracting}>
            <ArrowDownToLine size={16} aria-hidden="true" />{selectedCount ? `解压所选（${selectedCount}）` : '解压全部'}
          </button>
        </div>

        {loadError && (
          <div className="archive-reader-state archive-reader-error" role="alert">
            <strong>无法读取压缩包</strong><p>{loadError}</p>
            <button type="button" onClick={() => setLoadAttempt((value) => value + 1)}><RefreshCw size={15} aria-hidden="true" />重试</button>
          </div>
        )}
        {loading && !loadError && <div className="archive-reader-state" role="status"><LoaderCircle className="archive-reader-spinner" size={22} aria-hidden="true" />正在读取目录…</div>}
        {!loading && !loadError && sortedEntries.length === 0 && <div className="archive-reader-state" role="status">此目录为空。</div>}
        {!loading && !loadError && sortedEntries.length > 0 && (
          <main className="archive-reader-content" aria-label="压缩包条目">
            <div className="archive-reader-list-heading"><span>名称</span><span>大小</span></div>
            <ul className="archive-reader-list">
              {sortedEntries.map((entry) => {
                const previewKind = archivePreviewKind(entry);
                const Icon = entry.kind === 'directory' ? Folder : entry.kind === 'unsupported' ? FileArchive : previewKind === 'pdf' || previewKind === 'text' || previewKind === 'xlsx' || previewKind === 'epub' ? FileText : File;
                return (
                  <li className={`archive-reader-row archive-entry-${entry.kind}`} key={entry.path}>
                    <input
                      type="checkbox"
                      checked={selected.has(entry.path)}
                      disabled={entry.kind === 'unsupported' || extracting}
                      onChange={() => toggleSelection(entry)}
                      aria-label={`选择 ${entry.name} 解压`}
                    />
                    {entry.kind === 'directory' ? (
                      <button type="button" className="archive-reader-entry-name" onClick={() => setDirectory(entry.path)} disabled={loading}>
                        <Icon size={17} aria-hidden="true" /><span>{entry.name}</span><small>文件夹</small>
                      </button>
                    ) : previewKind ? (
                      <button type="button" className="archive-reader-entry-name" onClick={() => openPreview(entry)}>
                        <Icon size={17} aria-hidden="true" /><span>{entry.name}</span><small>预览</small>
                      </button>
                    ) : (
                      <span className="archive-reader-entry-name archive-reader-entry-disabled" title={entry.kind === 'unsupported' ? '此条目不支持安全解压' : '此文件格式暂不支持预览'}>
                        <Icon size={17} aria-hidden="true" /><span>{entry.name}</span><small>{entry.kind === 'unsupported' ? '不支持' : '文件'}</small>
                      </span>
                    )}
                    <span className="archive-reader-entry-size">{entry.kind === 'directory' ? '—' : formatArchiveSize(entry.size)}</span>
                  </li>
                );
              })}
            </ul>
          </main>
        )}

        {extractResult && (
          <div className={`archive-reader-result${extractResult.status === 'partial' ? ' is-partial' : ''}`} role={extractResult.status === 'partial' ? 'alert' : 'status'}>
            <strong>{extractResult.status === 'partial' ? `已解压 ${extractResult.paths.length} 项，${extractResult.errors.length} 项失败` : `已解压 ${extractResult.paths.length} 项`}</strong>
            {extractResult.errors.length > 0 && <ul>{extractResult.errors.map((error) => <li key={`${error.entry}:${error.message}`}><span>{error.entry}</span>：{error.message}</li>)}</ul>}
          </div>
        )}
        {extractError && <p className="archive-reader-extract-error" role="alert">{extractError}</p>}

        {extractOpen && (
          <div className="archive-destination-backdrop">
            <section ref={destinationDialogRef} className="archive-destination-dialog" role="dialog" aria-modal="true" aria-labelledby={`${titleId}-destination`}>
              <header><div><span className="archive-reader-eyebrow"><FolderOpen size={14} aria-hidden="true" />解压目标</span><h3 id={`${titleId}-destination`}>选择目标目录</h3></div><button type="button" className="archive-reader-close" onClick={() => setExtractOpen(false)} aria-label="关闭目标目录选择"><X size={18} aria-hidden="true" /></button></header>
              <div className="archive-destination-current"><span>将解压到</span><strong>{destinationLabel}</strong></div>
              <div className="archive-destination-navigation">
                <button type="button" onClick={() => enterDestination(parentDirectory(destination))} disabled={!destination || destinationLoading}><ChevronLeft size={16} aria-hidden="true" />上一级</button>
                <span>{destination ? `/${destination}` : '/ 管理根目录'}</span>
              </div>
              {destinationError && <div className="archive-destination-error" role="alert"><p>{destinationError}</p><button type="button" onClick={() => setDestinationAttempt((value) => value + 1)}><RefreshCw size={14} aria-hidden="true" />重试</button></div>}
              {extractError && <p className="archive-reader-extract-error" role="alert">{extractError}</p>}
              {destinationLoading && <div className="archive-destination-empty" role="status"><LoaderCircle className="archive-reader-spinner" size={18} aria-hidden="true" />正在读取文件夹…</div>}
              {!destinationLoading && !destinationError && destinationDirectories.length === 0 && <div className="archive-destination-empty" role="status">当前目录没有子文件夹。</div>}
              {!destinationLoading && !destinationError && destinationDirectories.length > 0 && (
                <ul className="archive-destination-list">
                  {destinationDirectories.map((folder) => <li key={folder.path}><button type="button" onClick={() => enterDestination(folder.path)}><Folder size={16} aria-hidden="true" /><span>{folder.name}</span><ChevronRight size={15} aria-hidden="true" /></button></li>)}
                </ul>
              )}
              {!destinationLoading && !destinationError && (destinationPage > 0 || destinationHasMore) && (
                <div className="archive-destination-pages">
                  <button type="button" onClick={() => changeDestinationPage(-1)} disabled={destinationPage === 0}><ChevronLeft size={15} aria-hidden="true" />上一页</button>
                  <span>第 {destinationPage + 1} 页</span>
                  <button type="button" onClick={() => changeDestinationPage(1)} disabled={!destinationHasMore}>下一页<ChevronRight size={15} aria-hidden="true" /></button>
                </div>
              )}
              <footer>
                <button type="button" className="archive-destination-cancel" onClick={() => setExtractOpen(false)} disabled={extracting}>取消</button>
                <button type="button" className="archive-destination-confirm" onClick={() => void confirmExtraction()} disabled={extracting || destinationLoading || Boolean(destinationError)}>
                  {extracting ? <LoaderCircle className="archive-reader-spinner" size={16} aria-hidden="true" /> : <ArrowDownToLine size={16} aria-hidden="true" />}
                  {extracting ? '正在解压…' : '确认解压'}
                </button>
              </footer>
            </section>
          </div>
        )}

        {previewTarget && <ReaderPanel key={previewTarget.sourceURL} {...previewTarget} onClose={() => setPreviewTarget(null)} />}
      </section>
    </div>
  );
}
