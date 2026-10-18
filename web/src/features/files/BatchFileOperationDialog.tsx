// 为多选项目选择现有目录，确认后展示服务端逐项结果。
import { useEffect, useId, useRef, useState } from 'react';
import { ChevronRight, Copy, Folder, MoveRight, X } from 'lucide-react';
import {
  ApiError,
  batchCopyFilesToDirectory,
  batchMoveFilesToDirectory,
  getFileIndexStatus,
  listDirectory,
  type BatchFileOperationResult,
  type FileEntry,
  type FileOperationMode,
} from '../../api/files';
import { batchOperationTargetError, batchSelectionInclusions } from './batchFileOperationRules';

const folderPageSize = 100;

interface FolderCrumb {
  path: string;
  name: string;
  version: string;
}

interface BatchFileOperationDialogProps {
  mode: FileOperationMode;
  items: FileEntry[];
  onClose: () => void;
  onComplete: (result: BatchFileOperationResult, previousGeneration?: number) => void;
  onConflict: () => void;
}

function pathLabel(path: string): string {
  return path ? `/${path}` : '根目录 (/)';
}

function mergeEntries(current: FileEntry[], incoming: FileEntry[]): FileEntry[] {
  const result = [...current];
  const indexes = new Map(result.map((entry, index) => [entry.path, index]));
  for (const entry of incoming) {
    const index = indexes.get(entry.path);
    if (index === undefined) {
      indexes.set(entry.path, result.length);
      result.push(entry);
    } else result[index] = entry;
  }
  return result;
}

// 目录浏览、版本检查和批量操作由同一对话框管理。
export function BatchFileOperationDialog({ mode, items, onClose, onComplete, onConflict }: BatchFileOperationDialogProps) {
  const root: FolderCrumb = { path: '', name: '全部文件', version: '' };
  const titleId = useId();
  const descriptionId = useId();
  const [folderStack, setFolderStack] = useState<FolderCrumb[]>([root]);
  const [destination, setDestination] = useState<FolderCrumb>(root);
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [loadedPath, setLoadedPath] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState('');
  const [hasMore, setHasMore] = useState(false);
  const [nextCursor, setNextCursor] = useState('');
  const [reloadKey, setReloadKey] = useState(0);
  const [operationError, setOperationError] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const currentFolder = folderStack.at(-1) ?? root;
  const currentPath = currentFolder.path;
  const listingIsCurrent = loadedPath === currentPath;
  const visibleFolders = listingIsCurrent ? entries.filter((entry) => entry.kind === 'directory' && !entry.symlink) : [];
  const inclusionRelations = batchSelectionInclusions(items.map(({ path, kind }) => ({ path, kind: kind as 'file' | 'directory' })));
  const targetError = batchOperationTargetError(mode, items.map(({ path, kind }) => ({ path, kind: kind as 'file' | 'directory' })), currentPath);
  const destinationError = batchOperationTargetError(mode, items.map(({ path, kind }) => ({ path, kind: kind as 'file' | 'directory' })), destination.path);
  const requestIdRef = useRef(0);
  const loadMoreRef = useRef<AbortController | null>(null);
  const submittingRef = useRef(false);
  const dialogRef = useRef<HTMLElement>(null);
  const firstActionRef = useRef<HTMLButtonElement>(null);
  const onCloseRef = useRef(onClose);
  const onConflictRef = useRef(onConflict);
  const onCompleteRef = useRef(onComplete);
  onCloseRef.current = onClose;
  onConflictRef.current = onConflict;
  onCompleteRef.current = onComplete;

  useEffect(() => {
    const requestId = ++requestIdRef.current;
    const controller = new AbortController();
    loadMoreRef.current?.abort();
    loadMoreRef.current = null;
    setLoadedPath(null);
    setEntries([]);
    setLoading(true);
    setLoadError('');
    setLoadingMore(false);
    setLoadMoreError('');
    setHasMore(false);
    setNextCursor('');
    void listDirectory(currentPath, {
      limit: folderPageSize,
      cursor: '',
      query: '',
      scope: 'current',
      sort: 'name-asc',
    }, controller.signal)
      .then((listing) => {
        if (controller.signal.aborted || requestIdRef.current !== requestId) return;
        setEntries(listing.entries);
        setHasMore(listing.hasMore);
        setNextCursor(listing.nextCursor);
        setLoadedPath(currentPath);
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted || error instanceof DOMException && error.name === 'AbortError' || requestIdRef.current !== requestId) return;
        setLoadError(error instanceof ApiError ? error.message : '读取文件夹时发生错误。');
      })
      .finally(() => {
        if (!controller.signal.aborted && requestIdRef.current === requestId) setLoading(false);
      });
    return () => {
      controller.abort();
      loadMoreRef.current?.abort();
      loadMoreRef.current = null;
    };
  }, [currentPath, reloadKey]);

  useEffect(() => {
    const previousFocus = document.activeElement;
    firstActionRef.current?.focus();
    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        event.preventDefault();
        if (!submittingRef.current) onCloseRef.current();
        return;
      }
      if (event.key !== 'Tab') return;
      const dialog = dialogRef.current;
      const focusable = dialog?.querySelectorAll<HTMLElement>('button:not(:disabled)');
      if (!focusable?.length) {
        event.preventDefault();
        dialog?.focus();
        return;
      }
      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      if (event.shiftKey && (document.activeElement === first || !dialog?.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !dialog?.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    }
    window.addEventListener('keydown', handleKeyDown);
    return () => {
      window.removeEventListener('keydown', handleKeyDown);
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) previousFocus.focus();
    };
  }, []);

  function openFolder(entry: FileEntry) {
    if (!entry.version) return;
    setFolderStack((current) => [...current, { path: entry.path, name: entry.name, version: entry.version }]);
    setLoadError('');
  }

  function navigateToCrumb(index: number) {
    setFolderStack((current) => current.slice(0, index + 1));
    setLoadError('');
  }

  function selectCurrentFolder() {
    if ((currentFolder.path && !currentFolder.version) || targetError) return;
    setDestination(currentFolder);
    setOperationError('');
  }

  async function loadMore() {
    if (!listingIsCurrent || !hasMore || !nextCursor || loadingMore || loadMoreRef.current) return;
    const controller = new AbortController();
    const requestId = requestIdRef.current;
    loadMoreRef.current = controller;
    setLoadingMore(true);
    setLoadMoreError('');
    try {
      const listing = await listDirectory(currentPath, {
        limit: folderPageSize,
        cursor: nextCursor,
        query: '',
        scope: 'current',
        sort: 'name-asc',
      }, controller.signal);
      if (controller.signal.aborted || requestIdRef.current !== requestId) return;
      setEntries((current) => mergeEntries(current, listing.entries));
      setHasMore(listing.hasMore);
      setNextCursor(listing.nextCursor);
    } catch (error) {
      if (controller.signal.aborted || error instanceof DOMException && error.name === 'AbortError' || requestIdRef.current !== requestId) return;
      setLoadMoreError(error instanceof ApiError ? error.message : '加载更多文件夹时发生错误。');
    } finally {
      if (loadMoreRef.current === controller) {
        loadMoreRef.current = null;
        setLoadingMore(false);
      }
    }
  }

  async function submitOperation() {
    if (submittingRef.current || destinationError || (destination.path && !destination.version)) return;
    submittingRef.current = true;
    setSubmitting(true);
    setOperationError('');
    try {
      let previousGeneration: number | undefined;
      try {
        previousGeneration = (await getFileIndexStatus()).activeGeneration;
      } catch {
        // 索引状态不可用时仍允许文件操作完成。
      }
      const requestItems = items.map(({ path, version }) => ({ path, version }));
      const result = mode === 'copy'
        ? await batchCopyFilesToDirectory(requestItems, destination.path, destination.version)
        : await batchMoveFilesToDirectory(requestItems, destination.path, destination.version);
      onCompleteRef.current(result, previousGeneration);
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        onConflictRef.current();
      } else {
        setOperationError(error instanceof ApiError ? error.message : `批量${mode === 'copy' ? '复制' : '移动'}时发生错误，请重试。`);
      }
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  }

  const OperationIcon = mode === 'copy' ? Copy : MoveRight;
  const operationLabel = mode === 'copy' ? '复制' : '移动';
  const destinationIsCurrent = destination.path === currentFolder.path;

  return (
    <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !submittingRef.current) onClose(); }}>
      <section
        ref={dialogRef}
        className="files-name-dialog files-operation-dialog files-batch-operation-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={descriptionId}
        aria-busy={submitting}
        tabIndex={-1}
      >
        <header className="files-name-dialog-header">
          <div><span className="files-name-dialog-icon"><OperationIcon size={17} aria-hidden="true" /></span><h2 id={titleId}>批量{operationLabel}到</h2></div>
          <button type="button" className="files-name-dialog-close" aria-label={`关闭批量${operationLabel}对话框`} onClick={onClose} disabled={submitting}><X size={18} aria-hidden="true" /></button>
        </header>
        <p className="files-name-dialog-description" id={descriptionId}>
          选择目标文件夹。名称冲突时会自动编号。{inclusionRelations.length > 0 && '父目录和子项同时选择时，子项由父目录包含；最终以服务端结果为准。'}
        </p>

        <div className="files-operation-summary">
          <div><span>项目数</span><strong>{items.length} 项</strong></div>
          <div><span>目标文件夹</span><strong>{pathLabel(destination.path)}</strong></div>
        </div>
        <details className="files-batch-operation-sources">
          <summary>查看所选项目</summary>
          <ul>{items.map((item) => <li key={item.path}>{item.kind === 'directory' ? '文件夹' : '文件'}：/{item.path}</li>)}</ul>
        </details>

        <div className="files-folder-picker">
          <nav className="files-folder-breadcrumbs" aria-label="浏览目标文件夹">
            {folderStack.map((crumb, index) => (
              <span className="files-folder-breadcrumb" key={crumb.path || 'root'}>
                {index > 0 && <ChevronRight size={13} aria-hidden="true" />}
                <button type="button" onClick={() => navigateToCrumb(index)} disabled={loading || index === folderStack.length - 1}>{crumb.name}</button>
              </span>
            ))}
          </nav>
          <button
            ref={firstActionRef}
            type="button"
            className={`files-folder-select-current${destinationIsCurrent ? ' is-selected' : ''}`}
            onClick={selectCurrentFolder}
            disabled={loading || submitting || Boolean(targetError) || Boolean(currentFolder.path && !currentFolder.version)}
          >
            <Folder size={16} aria-hidden="true" />
            {targetError || (destinationIsCurrent ? '已选为目标文件夹' : `选择此文件夹：${pathLabel(currentFolder.path)}`)}
          </button>
          <div className="files-folder-list" aria-label="现有文件夹">
            {loading && <p className="files-folder-state" role="status">正在读取文件夹…</p>}
            {!loading && loadError && <div className="files-folder-state is-error" role="alert"><span>{loadError}</span><button type="button" onClick={() => setReloadKey((current) => current + 1)}>重试</button></div>}
            {!loading && !loadError && visibleFolders.length === 0 && <p className="files-folder-state">此处没有可进入的文件夹。</p>}
            {!loading && !loadError && visibleFolders.map((folder) => (
              <button type="button" className="files-folder-row" key={folder.path} onClick={() => openFolder(folder)} disabled={submitting || !folder.version} title={!folder.version ? '此文件夹缺少当前版本，无法安全浏览' : undefined}>
                <Folder size={16} aria-hidden="true" />
                <span><strong>{folder.name}</strong><small>{folder.version ? '打开文件夹' : '缺少版本，无法安全浏览'}</small></span>
                {folder.version && <ChevronRight size={15} aria-hidden="true" />}
              </button>
            ))}
          </div>
          {listingIsCurrent && hasMore && <div className="files-folder-more-area">{loadMoreError && <p className="files-folder-more-error" role="alert">{loadMoreError}</p>}<button type="button" className="files-folder-more" onClick={() => { void loadMore(); }} disabled={loadingMore || submitting}>{loadingMore ? '正在加载…' : loadMoreError ? '重试加载更多' : '加载更多文件夹'}</button></div>}
        </div>

        {destinationError && <p className="files-operation-reason" role="status">{destinationError}</p>}
        {operationError && <p className="files-name-dialog-error" role="alert">{operationError}</p>}
        <div className="files-name-dialog-actions">
          <button type="button" className="files-name-dialog-cancel" onClick={onClose} disabled={submitting}>取消</button>
          <button type="button" className="files-name-dialog-submit" onClick={() => { void submitOperation(); }} disabled={submitting || Boolean(destinationError) || (Boolean(destination.path) && !destination.version)}>
            {submitting ? `正在${operationLabel}…` : `确认${operationLabel} ${items.length} 项`}
          </button>
        </div>
      </section>
    </div>
  );
}
