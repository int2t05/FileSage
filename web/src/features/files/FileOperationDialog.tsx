// 单个文件复制或移动到现有目录。
import { useEffect, useRef, useState } from 'react';
import { ChevronRight, Copy, Folder, MoveRight, X } from 'lucide-react';
import {
  ApiError,
  copyDirectoryToDirectory,
  copyFileToDirectory,
  getFileIndexStatus,
  listDirectory,
  moveDirectoryToDirectory,
  moveFileToDirectory,
  type FileEntry,
  type FileOperationMode,
  type FileOperationResult,
} from '../../api/files';
import { isDirectoryTargetAllowed } from './directoryOperationRules';

const folderPageSize = 100;

interface FolderCrumb {
  path: string;
  name: string;
  version: string;
}

interface FileOperationDialogProps {
  mode: FileOperationMode;
  source: FileEntry;
  onClose: () => void;
  onComplete: (mode: FileOperationMode, result: FileOperationResult, previousGeneration?: number) => void;
}

// pathLabel 展示根目录或管理根内的相对路径。
function pathLabel(path: string): string {
  return path ? `/${path}` : '根目录 (/)';
}

// 返回文件或文件夹的父目录。
function parentPath(path: string): string {
  const separator = path.lastIndexOf('/');
  return separator < 0 ? '' : path.slice(0, separator);
}

// 判断目标是否为源文件夹本身或其子目录。
function isSameOrDescendantPath(path: string, sourcePath: string): boolean {
  return path === sourcePath || path.startsWith(`${sourcePath}/`);
}

// mergeEntries 按路径合并分页结果，避免重复目录项。
function mergeEntries(current: FileEntry[], incoming: FileEntry[]): FileEntry[] {
  const result = [...current];
  const indexes = new Map(result.map((entry, index) => [entry.path, index]));
  for (const entry of incoming) {
    const index = indexes.get(entry.path);
    if (index === undefined) {
      indexes.set(entry.path, result.length);
      result.push(entry);
    } else {
      result[index] = entry;
    }
  }
  return result;
}

// FileOperationDialog 选择目标目录并提交单项操作。
export function FileOperationDialog({ mode, source, onClose, onComplete }: FileOperationDialogProps) {
  const root: FolderCrumb = { path: '', name: '全部文件', version: '' };
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
  const isDirectoryOperation = source.kind === 'directory';
  const visibleFolders = listingIsCurrent ? entries.filter((entry) => entry.kind === 'directory' && !entry.symlink) : [];
  const sameParent = mode === 'move' && parentPath(source.path) === destination.path;
  const invalidDirectoryTarget = isDirectoryOperation && !isDirectoryTargetAllowed(mode, source.path, destination.path);
  const invalidCurrentTarget = isDirectoryOperation && !isDirectoryTargetAllowed(mode, source.path, currentFolder.path);
  const currentTargetIsSourceParent = isDirectoryOperation && mode === 'move' && parentPath(source.path) === currentFolder.path;
  const requestIdRef = useRef(0);
  const loadMoreRef = useRef<AbortController | null>(null);
  const submittingRef = useRef(false);
  const dialogRef = useRef<HTMLElement>(null);
  const firstActionRef = useRef<HTMLButtonElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

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
        if (controller.signal.aborted || (error instanceof DOMException && error.name === 'AbortError') || requestIdRef.current !== requestId) return;
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
    if (!entry.version || (isDirectoryOperation && isSameOrDescendantPath(entry.path, source.path))) return;
    setFolderStack((current) => [...current, { path: entry.path, name: entry.name, version: entry.version }]);
    setLoadError('');
  }

  function navigateToCrumb(index: number) {
    setFolderStack((current) => current.slice(0, index + 1));
    setLoadError('');
  }

  function selectCurrentFolder() {
    if ((currentFolder.path && !currentFolder.version) || invalidCurrentTarget) return;
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
      if (controller.signal.aborted || (error instanceof DOMException && error.name === 'AbortError') || requestIdRef.current !== requestId) return;
      setLoadMoreError(error instanceof ApiError ? error.message : '加载更多文件夹时发生错误。');
    } finally {
      if (loadMoreRef.current === controller) {
        loadMoreRef.current = null;
        setLoadingMore(false);
      }
    }
  }

  async function submitOperation() {
    if (submittingRef.current || sameParent || invalidDirectoryTarget || (destination.path && !destination.version)) return;
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
      const result = mode === 'copy'
        ? isDirectoryOperation
          ? await copyDirectoryToDirectory(source.path, source.version, destination.path, destination.version)
          : await copyFileToDirectory(source.path, source.version, destination.path, destination.version)
        : isDirectoryOperation
          ? await moveDirectoryToDirectory(source.path, source.version, destination.path, destination.version)
          : await moveFileToDirectory(source.path, source.version, destination.path, destination.version);
      onComplete(mode, result, previousGeneration);
    } catch (error) {
      const subject = isDirectoryOperation ? '文件夹' : '文件';
      setOperationError(error instanceof ApiError ? error.message : `${mode === 'copy' ? '复制' : '移动'}${subject}时发生错误，请重试。`);
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  }

  const OperationIcon = mode === 'copy' ? Copy : MoveRight;
  const operationLabel = mode === 'copy' ? '复制' : '移动';
  const subject = isDirectoryOperation ? '文件夹' : '文件';
  const dialogTitle = `${operationLabel}${subject}到`;

  return (
    <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !submittingRef.current) onClose(); }}>
      <section
        ref={dialogRef}
        className="files-name-dialog files-operation-dialog"
        role={mode === 'move' ? 'alertdialog' : 'dialog'}
        aria-modal="true"
        aria-labelledby="files-operation-title"
        aria-describedby="files-operation-description"
        aria-busy={submitting}
        tabIndex={-1}
      >
        <header className="files-name-dialog-header">
          <div>
            <span className="files-name-dialog-icon"><OperationIcon size={17} aria-hidden="true" /></span>
            <h2 id="files-operation-title">{dialogTitle}</h2>
          </div>
          <button type="button" className="files-name-dialog-close" aria-label={`关闭${dialogTitle}对话框`} onClick={onClose} disabled={submitting}><X size={18} aria-hidden="true" /></button>
        </header>
        <p className="files-name-dialog-description" id="files-operation-description">
          {isDirectoryOperation
            ? `选择${mode === 'copy' ? '副本' : '移动'}位置。不能选择源文件夹或它的子目录；名称冲突时系统会自动生成新名称。`
            : mode === 'copy' ? '选择一个现有文件夹作为副本位置。若名称冲突，系统会自动生成新名称。' : '选择一个现有文件夹并确认移动。若跨挂载点删除源文件失败，系统会保留源文件并说明部分结果。'}
        </p>

        <div className="files-operation-summary">
          <div><span>{isDirectoryOperation ? '源文件夹' : '源文件'}</span><strong>{`/${source.path}`}</strong></div>
          <div><span>目标文件夹</span><strong>{pathLabel(destination.path)}</strong></div>
        </div>

        <div className="files-folder-picker">
          <nav className="files-folder-breadcrumbs" aria-label="浏览目标文件夹">
            {folderStack.map((crumb, index) => (
              <span className="files-folder-breadcrumb" key={crumb.path || 'root'}>
                {index > 0 && <ChevronRight size={13} aria-hidden="true" />}
                <button type="button" onClick={() => navigateToCrumb(index)} disabled={loading || index === folderStack.length - 1}>
                  {crumb.name}
                </button>
              </span>
            ))}
          </nav>
          <button
            ref={firstActionRef}
            type="button"
            className={`files-folder-select-current${destination.path === currentFolder.path ? ' is-selected' : ''}`}
            onClick={selectCurrentFolder}
            disabled={loading || submitting || invalidCurrentTarget || Boolean(currentFolder.path && !currentFolder.version)}
          >
            <Folder size={16} aria-hidden="true" />
            {invalidCurrentTarget
              ? currentTargetIsSourceParent
                ? '源文件夹已在此目录'
                : '不能选源文件夹或其子目录'
              : destination.path === currentFolder.path ? '已选为目标文件夹' : `选择此文件夹：${pathLabel(currentFolder.path)}`}
          </button>
          <div className="files-folder-list" aria-label="现有文件夹">
            {loading && <p className="files-folder-state" role="status">正在读取文件夹…</p>}
            {!loading && loadError && (
              <div className="files-folder-state is-error" role="alert">
                <span>{loadError}</span>
                <button type="button" onClick={() => setReloadKey((current) => current + 1)}>重试</button>
              </div>
            )}
            {!loading && !loadError && visibleFolders.length === 0 && <p className="files-folder-state">此处没有可进入的文件夹。</p>}
            {!loading && !loadError && visibleFolders.map((folder) => (
              <button
                type="button"
                className="files-folder-row"
                key={folder.path}
                onClick={() => openFolder(folder)}
                disabled={submitting || !folder.version || (isDirectoryOperation && isSameOrDescendantPath(folder.path, source.path))}
                title={!folder.version
                  ? '此文件夹缺少当前版本，无法安全浏览'
                  : isDirectoryOperation && isSameOrDescendantPath(folder.path, source.path) ? '源文件夹及其子目录不可选' : undefined}
              >
                <Folder size={16} aria-hidden="true" />
                <span><strong>{folder.name}</strong><small>{!folder.version
                  ? '缺少版本，无法安全浏览'
                  : isDirectoryOperation && isSameOrDescendantPath(folder.path, source.path) ? '源文件夹及其子目录不可选' : '打开文件夹'}</small></span>
                {folder.version && <ChevronRight size={15} aria-hidden="true" />}
              </button>
            ))}
          </div>
          {listingIsCurrent && hasMore && (
            <div className="files-folder-more-area">
              {loadMoreError && <p className="files-folder-more-error" role="alert">{loadMoreError}</p>}
              <button type="button" className="files-folder-more" onClick={() => { void loadMore(); }} disabled={loadingMore || submitting}>
                {loadingMore ? '正在加载…' : loadMoreError ? '重试加载更多' : '加载更多文件夹'}
              </button>
            </div>
          )}
        </div>

        {sameParent && <p className="files-operation-reason" role="status">目标文件夹与源文件当前目录相同，不能移动到这里。请选择其他文件夹。</p>}
        {operationError && <p className="files-name-dialog-error" role="alert">{operationError}</p>}
        <div className="files-name-dialog-actions">
          <button type="button" className="files-name-dialog-cancel" onClick={onClose} disabled={submitting}>取消</button>
          <button type="button" className="files-name-dialog-submit" onClick={() => { void submitOperation(); }} disabled={submitting || sameParent || invalidDirectoryTarget || (Boolean(destination.path) && !destination.version)}>
            {submitting ? `正在${operationLabel}…` : `${operationLabel}到此文件夹`}
          </button>
        </div>
      </section>
    </div>
  );
}
