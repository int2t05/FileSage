// 选择保存目录与格式，为选中项目创建压缩包。
import { useEffect, useId, useRef, useState } from 'react';
import { ChevronRight, FileArchive, Folder, X } from 'lucide-react';
import {
  ApiError,
  archiveFileName,
  createArchive,
  getFileIndexStatus,
  listDirectory,
  type FileEntry as ApiFileEntry,
  type ArchiveFormat,
  type PathNameResult,
} from '../../api/files';

const folderPageSize = 100;

type ArchiveItem = Pick<ApiFileEntry, 'name' | 'path' | 'kind' | 'version' | 'symlink'>;

interface FolderCrumb {
  path: string;
  name: string;
  version: string;
}

interface CreateArchiveDialogProps {
  items: ArchiveItem[];
  onClose: () => void;
  onComplete: (result: PathNameResult, previousGeneration?: number) => void;
  onConflict: () => void;
}

function pathLabel(path: string): string {
  return path ? `/${path}` : '根目录 /';
}

// 分页浏览目录时按相对路径合并结果。
function mergeEntries(current: ApiFileEntry[], incoming: ApiFileEntry[]): ApiFileEntry[] {
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

export function CreateArchiveDialog({ items, onClose, onComplete, onConflict }: CreateArchiveDialogProps) {
  const root: FolderCrumb = { path: '', name: '全部文件', version: '' };
  const titleId = useId();
  const descriptionId = useId();
  const [name, setName] = useState('归档.zip');
  const [format, setFormat] = useState<ArchiveFormat>('zip');
  const [folderStack, setFolderStack] = useState<FolderCrumb[]>([root]);
  const [destination, setDestination] = useState<FolderCrumb>(root);
  const [entries, setEntries] = useState<ApiFileEntry[]>([]);
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
  const archiveName = archiveFileName(name, format);
  const visibleFolders = listingIsCurrent ? entries.filter((entry) => entry.kind === 'directory' && !entry.symlink) : [];
  const selectionIssue = items.length < 1 || items.length > 256
    ? '请选择 1 到 256 个项目。'
    : items.some((item) => !item.version || item.symlink || (item.kind !== 'file' && item.kind !== 'directory'))
      ? '仅支持有版本信息的真实文件和文件夹。'
      : '';
  const nameValue = name.trim();
  const nameIssue = !nameValue
    ? '请输入归档名称。'
    : /[\\/]/.test(archiveName) || archiveName.length > 255
      ? '请输入有效的归档文件名。'
      : '';
  const destinationIsCurrent = destination.path === currentFolder.path;
  const requestIdRef = useRef(0);
  const loadMoreRef = useRef<AbortController | null>(null);
  const submittingRef = useRef(false);
  const dialogRef = useRef<HTMLElement>(null);
  const firstActionRef = useRef<HTMLInputElement>(null);
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
      const focusable = dialog?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled)');
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

  function openFolder(entry: ApiFileEntry) {
    if (submitting || !entry.version) return;
    setFolderStack((current) => [...current, { path: entry.path, name: entry.name, version: entry.version }]);
    setLoadError('');
  }

  // 返回上级目录并重新读取其版本信息。
  function navigateToCrumb(index: number) {
    setFolderStack((current) => current.slice(0, index + 1));
    setLoadError('');
  }

  // 将当前浏览目录设为压缩包保存位置。
  function selectCurrentFolder() {
    if ((currentFolder.path && !currentFolder.version) || loading || loadError) return;
    setDestination(currentFolder);
    setOperationError('');
  }

  // 延续目录分页，避免目标文件夹被列表上限隐藏。
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

  // 先记录索引代次，再按项目与目标目录版本创建归档。
  async function submitArchive() {
    if (submittingRef.current || selectionIssue || nameIssue || !listingIsCurrent || loadError
      || (destination.path && !destination.version)) return;
    submittingRef.current = true;
    setSubmitting(true);
    setOperationError('');
    try {
      let previousGeneration: number | undefined;
      try {
        previousGeneration = (await getFileIndexStatus()).activeGeneration;
      } catch {
        // 索引状态不可用时仍允许创建归档。
      }
      const result = await createArchive(
        items.map(({ path, version }) => ({ path, version })),
        destination.path,
        destination.version,
        archiveName,
        format,
      );
      onCompleteRef.current(result, previousGeneration);
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        onConflictRef.current();
      } else {
        setOperationError(error instanceof ApiError ? error.message : '创建压缩包时发生错误，请重试。');
      }
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  }

  return (
    <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !submittingRef.current) onClose(); }}>
      <section
        ref={dialogRef}
        className="files-name-dialog files-operation-dialog files-create-archive-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={descriptionId}
        aria-busy={submitting}
        tabIndex={-1}
      >
        <header className="files-name-dialog-header">
          <div><span className="files-name-dialog-icon"><FileArchive size={17} aria-hidden="true" /></span><h2 id={titleId}>创建压缩包</h2></div>
          <button type="button" className="files-name-dialog-close" aria-label="关闭创建压缩包对话框" onClick={onClose} disabled={submitting}><X size={18} aria-hidden="true" /></button>
        </header>
        <p className="files-name-dialog-description" id={descriptionId}>压缩 {items.length} 个已选文件或文件夹，可选择保存位置。</p>

        <div className="files-name-dialog-form files-create-archive-form">
          <label htmlFor="files-create-archive-format">格式</label>
          <select
            id="files-create-archive-format"
            value={format}
            onChange={(event) => {
              const nextFormat = event.currentTarget.value as ArchiveFormat;
              setFormat(nextFormat);
              setName((current) => archiveFileName(current, nextFormat));
              setOperationError('');
            }}
            disabled={submitting}
          >
            <option value="zip">ZIP</option>
            <option value="7z">7z</option>
            <option value="tar">TAR</option>
            <option value="tar.gz">TAR.GZ</option>
          </select>
          <label htmlFor="files-create-archive-name">归档名称</label>
          <input
            ref={firstActionRef}
            id="files-create-archive-name"
            name="name"
            type="text"
            autoComplete="off"
            maxLength={255}
            value={name}
            onChange={(event) => { setName(event.currentTarget.value); setOperationError(''); }}
            placeholder="例如：项目资料"
            disabled={submitting}
            aria-invalid={Boolean(nameIssue)}
            aria-describedby={nameIssue ? 'files-create-archive-name-error' : 'files-create-archive-name-help'}
          />
          {nameIssue
            ? <p className="files-name-dialog-error" id="files-create-archive-name-error" role="alert">{nameIssue}</p>
            : <p className="files-create-archive-help" id="files-create-archive-name-help">扩展名会根据格式自动维护。</p>}
        </div>

        <div className="files-operation-summary"><div><span>保存位置</span><strong>{pathLabel(destination.path)}</strong></div></div>

        <div className="files-folder-picker">
          <nav className="files-folder-breadcrumbs" aria-label="浏览保存位置">
            {folderStack.map((crumb, index) => (
              <span className="files-folder-breadcrumb" key={crumb.path || 'root'}>
                {index > 0 && <ChevronRight size={13} aria-hidden="true" />}
                <button type="button" onClick={() => navigateToCrumb(index)} disabled={loading || index === folderStack.length - 1 || submitting}>{crumb.name}</button>
              </span>
            ))}
          </nav>
          <button
            type="button"
            className={`files-folder-select-current${destinationIsCurrent ? ' is-selected' : ''}`}
            onClick={selectCurrentFolder}
            disabled={loading || submitting || Boolean(loadError) || Boolean(currentFolder.path && !currentFolder.version)}
          >
            <Folder size={16} aria-hidden="true" />
            {destinationIsCurrent ? '已选为保存位置' : `选择此文件夹：${pathLabel(currentFolder.path)}`}
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

        {selectionIssue && <p className="files-operation-reason" role="status">{selectionIssue}</p>}
        {operationError && <p className="files-name-dialog-error" role="alert">{operationError}</p>}
        <div className="files-name-dialog-actions">
          <button type="button" className="files-name-dialog-cancel" onClick={onClose} disabled={submitting}>取消</button>
          <button type="button" className="files-name-dialog-submit" onClick={() => { void submitArchive(); }} disabled={submitting || Boolean(selectionIssue) || Boolean(nameIssue) || loading || Boolean(loadError) || Boolean(destination.path && !destination.version)}>
            {submitting ? '正在创建…' : `创建压缩包 · ${items.length} 项`}
          </button>
        </div>
      </section>
    </div>
  );
}
