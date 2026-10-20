// 文件页展示服务端目录，并处理受版本保护的单项操作。
import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import {
  ChevronRight,
  Copy,
  Download,
  File,
  FileArchive,
  FileImage,
  FileSpreadsheet,
  FileText,
  FileVideo,
  Folder,
  FolderOpen,
  FolderPlus,
  HardDrive,
  Info,
  LayoutGrid,
  Link2,
  List,
  MoveRight,
  Pencil,
  Recycle,
  Search,
  Shield,
  SlidersHorizontal,
  Sparkles,
  Trash2,
  Upload,
  Users,
  X,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import { ApiError, batchMoveFilesToTrash, createDirectory, downloadArchive, fileDownloadURL, getFileIndexStatus, listDirectory, moveFileToTrash, renameEntry, type BatchFileOperationItemResult, type BatchFileOperationResult, type BatchRenameResult, type BatchTrashResult, type FileEntry as ApiFileEntry, type FileFilters, type FileFilterType, type FileOperationMode, type FileOperationResult, type FileSearchScope, type FileSortMode } from '../../api/files';
import { BatchRenameDialog } from './BatchRenameDialog';
import { BatchFileOperationDialog } from './BatchFileOperationDialog';
import { CreateArchiveDialog } from './CreateArchiveDialog';
import { CreateSymlinkDialog } from './CreateSymlinkDialog';
import { incompleteBatchOperationPaths } from './batchFileOperationRules';
import { FileOperationDialog } from './FileOperationDialog';
import { FileMetadataDialog, type FileMetadataEditMode } from './FileMetadataDialog';
import { canEditFileMetadata, formatOwnerId, formatPermissionMode } from './fileMetadataRules';
import { ArchiveReader } from '../reader/ArchiveReader';
import { ReaderPanel, type ReaderMediaKind } from '../reader/ReaderPanel';
import { isSupportedArchiveName } from '../reader/archiveReaderRules';
import { UploadPanel } from '../uploads/UploadPanel';
import type { AgentLaunchContext } from '../agent/AgentPage';
import './FilesPage.css';

type EntryKind = 'folder' | 'image' | 'audio' | 'pdf' | 'text' | 'office' | 'spreadsheet' | 'archive' | 'video' | 'other';
type ViewMode = 'list' | 'grid';
const pageSize = 100;
const mebibyte = 1024 * 1024;

interface FilesPageProps {
  onOpenTrash: () => void;
  onOpenAgent: (context: AgentLaunchContext) => void;
}

interface FileFilterInputs {
  type: FileFilterType | '';
  minSizeMiB: string;
  maxSizeMiB: string;
  modifiedFrom: string;
  modifiedThrough: string;
  capturedFrom: string;
  capturedThrough: string;
}

function emptyFileFilterInputs(): FileFilterInputs {
  return {
    type: '',
    minSizeMiB: '',
    maxSizeMiB: '',
    modifiedFrom: '',
    modifiedThrough: '',
    capturedFrom: '',
    capturedThrough: '',
  };
}

function sizeInBytes(value: string): number | null | undefined {
  if (!value.trim()) return undefined;
  const mebibytes = Number(value);
  if (!Number.isFinite(mebibytes) || mebibytes < 0 || mebibytes > Number.MAX_SAFE_INTEGER / mebibyte) return null;
  const bytes = Math.round(mebibytes * mebibyte);
  return Number.isSafeInteger(bytes) ? bytes : null;
}

function fileFilterError(inputs: FileFilterInputs): string {
  const minSize = sizeInBytes(inputs.minSizeMiB);
  const maxSize = sizeInBytes(inputs.maxSizeMiB);
  if (minSize === null || maxSize === null) return '大小必须是 0 或更大的有效 MiB 数值。';
  if (inputs.minSizeMiB && inputs.maxSizeMiB && Number(inputs.minSizeMiB) > Number(inputs.maxSizeMiB)) return '最小大小不能大于最大大小。';
  if (inputs.modifiedFrom && inputs.modifiedThrough && inputs.modifiedFrom > inputs.modifiedThrough) return '修改日期的起始日不能晚于结束日。';
  if (inputs.capturedFrom && inputs.capturedThrough && inputs.capturedFrom > inputs.capturedThrough) return '拍摄日期的起始日不能晚于结束日。';
  return '';
}

// 按浏览器本地日界转换；结束日取次日以符合 Before 不含边界。
function localDateBoundary(value: string, endOfSelectedDay = false): string | undefined {
  if (!value) return undefined;
  const [year, month, day] = value.split('-').map(Number);
  if (!year || !month || !day) return undefined;
  const date = new Date(0);
  date.setFullYear(year, month - 1, day + (endOfSelectedDay ? 1 : 0));
  date.setHours(0, 0, 0, 0);
  return date.toISOString();
}

function toFileFilters(inputs: FileFilterInputs): FileFilters {
  const filters: FileFilters = {};
  const minSize = sizeInBytes(inputs.minSizeMiB);
  const maxSize = sizeInBytes(inputs.maxSizeMiB);
  if (inputs.type) filters.type = inputs.type;
  if (minSize !== undefined && minSize !== null) filters.minSize = minSize;
  if (maxSize !== undefined && maxSize !== null) filters.maxSize = maxSize;
  const modifiedFrom = localDateBoundary(inputs.modifiedFrom);
  const modifiedBefore = localDateBoundary(inputs.modifiedThrough, true);
  const capturedFrom = localDateBoundary(inputs.capturedFrom);
  const capturedBefore = localDateBoundary(inputs.capturedThrough, true);
  if (modifiedFrom) filters.modifiedFrom = modifiedFrom;
  if (modifiedBefore) filters.modifiedBefore = modifiedBefore;
  if (capturedFrom) filters.capturedFrom = capturedFrom;
  if (capturedBefore) filters.capturedBefore = capturedBefore;
  return filters;
}

interface FileEntry extends ApiFileEntry {
  id: string;
  displayKind: EntryKind;
  displayPath: string;
}

function displayKind(entry: ApiFileEntry): EntryKind {
  if (entry.kind === 'directory') return 'folder';
  if (entry.kind === 'other') return 'other';
  const extension = entry.name.split('.').at(-1)?.toLocaleLowerCase() ?? '';
  if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp', 'heic', 'heif', 'cr2', 'cr3', 'nef', 'arw', 'dng'].includes(extension)) return 'image';
  if (['aac', 'flac', 'm4a', 'mp3', 'oga', 'ogg', 'opus', 'wav'].includes(extension)) return 'audio';
  if (extension === 'pdf') return 'pdf';
  if (['xls', 'xlsx', 'ods', 'csv', 'tsv'].includes(extension)) return 'spreadsheet';
  if (['docx', 'pptx'].includes(extension)) return 'office';
  if (isSupportedArchiveName(entry.name)) return 'archive';
  if (['mp4', 'mov', 'mkv', 'webm', 'avi', 'm4v'].includes(extension)) return 'video';
  if (['txt', 'md', 'markdown', 'json', 'yaml', 'yml', 'xml', 'html', 'css', 'js', 'ts', 'go', 'py', 'log'].includes(extension)) return 'text';
  return 'other';
}

function iconFor(kind: EntryKind): LucideIcon {
  switch (kind) {
    case 'folder': return Folder;
    case 'image': return FileImage;
    case 'pdf': return FileText;
    case 'text': return FileText;
    case 'office': return FileText;
    case 'spreadsheet': return FileSpreadsheet;
    case 'archive': return FileArchive;
    case 'video': return FileVideo;
    default: return File;
  }
}

function videoPosterUrl(path: string, version: string): string {
  return `/api/v1/files/video-poster?${new URLSearchParams({ path, version }).toString()}`;
}

function VideoPosterThumbnail({ path, version, Icon }: { path: string; version: string; Icon: LucideIcon }) {
  const [failed, setFailed] = useState(false);
  if (!version || failed) return <span className="files-grid-video-fallback"><Icon size={25} strokeWidth={1.7} /></span>;
  return <img
    className="files-grid-video-poster"
    src={videoPosterUrl(path, version)}
    alt=""
    loading="lazy"
    decoding="async"
    onError={() => setFailed(true)}
  />;
}

// linkStatusLabel 在文件列表中说明软链接状态。
function linkStatusLabel(status: ApiFileEntry['linkStatus']): string {
  switch (status) {
    case 'internal': return '根目录内';
    case 'dangling': return '目标不存在';
    case 'outside_root': return '目标越界';
    case 'cycle': return '链接循环';
    case 'unavailable': return '状态不可用';
    default: return '状态未知';
  }
}

// linkStatusDescription 说明软链接能否跟随及原因。
function linkStatusDescription(status: ApiFileEntry['linkStatus']): string {
  switch (status) {
    case 'internal': return '这是软链接，目标位于管理根目录内。';
    case 'dangling': return '软链接目标不存在，无法访问。';
    case 'outside_root': return '软链接目标在管理根目录之外，已禁止跟随。';
    case 'cycle': return '软链接形成循环，已禁止跟随。';
    case 'unavailable': return '无法确认软链接状态，已禁止跟随。';
    default: return '软链接状态未知，已禁止跟随。';
  }
}

function kindLabel(entry: FileEntry): string {
  if (entry.mountUnavailable) return '磁盘不可用';
  if (entry.readOnly) return entry.kind === 'directory' ? '文件夹 · 只读' : '文件 · 只读';
  if (entry.symlink) {
    const kind = entry.displayKind === 'folder' ? '文件夹软链接' : '文件软链接';
    return `${kind} · ${linkStatusLabel(entry.linkStatus)}`;
  }
  if (entry.displayKind === 'folder') return '文件夹';
  if (entry.displayKind === 'image') return '图片';
  if (entry.displayKind === 'audio') return '音频';
  if (entry.displayKind === 'pdf') return 'PDF 文档';
  if (entry.displayKind === 'office') return 'Office 文档';
  if (entry.displayKind === 'text') return '文本与 Markdown';
  if (entry.displayKind === 'spreadsheet') return '电子表格';
  if (entry.displayKind === 'archive') return '压缩包';
  if (entry.displayKind === 'video') return '视频';
  if (entry.kind === 'other') return '特殊文件';
  return '文件';
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unitIndex = -1;
  do {
    value /= 1024;
    unitIndex += 1;
  } while (value >= 1024 && unitIndex < units.length - 1);
  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${units[unitIndex]}`;
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '—';
  return new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(date);
}

function batchFileOperationItemMessage(item: BatchFileOperationItemResult): string {
  const target = item.targetPath ? `/${item.targetPath}` : '/';
  switch (item.status) {
    case 'copied': return `已复制到 ${target}`;
    case 'moved': return `已移动到 ${target}`;
    case 'included': return `已包含于 /${item.includedIn || '所选目录'}${item.targetPath ? `；目标 ${target}` : ''}`;
    case 'partial': return `部分完成${item.targetPath ? `；目标 ${target}` : ''}${item.message ? `：${item.message}` : ''}`;
    case 'failed': return `失败${item.targetPath ? `；目标 ${target}` : ''}${item.message ? `：${item.message}` : ''}`;
    case 'not_started': return `未开始${item.message ? `：${item.message}` : ''}`;
  }
}

function toFileEntry(entry: ApiFileEntry): FileEntry {
  const relativePath = entry.path;
  return {
    ...entry,
    id: relativePath,
    displayPath: relativePath ? `/${relativePath}` : '/',
    displayKind: displayKind(entry),
  };
}

// isHiddenEntry 判断路径是否包含点开头的隐藏项目。
function isHiddenEntry(entry: FileEntry): boolean {
  return entry.path.split('/').some((component) => component.startsWith('.'));
}

function appendUniqueEntries(existing: FileEntry[], incoming: FileEntry[]): FileEntry[] {
  const merged = [...existing];
  const indexes = new Map(merged.map((entry, index) => [entry.path, index]));
  for (const entry of incoming) {
    const existingIndex = indexes.get(entry.path);
    if (existingIndex === undefined) {
      indexes.set(entry.path, merged.length);
      merged.push(entry);
    } else {
      merged[existingIndex] = entry;
    }
  }
  return merged;
}

function indexStateMessage(state: string): string {
  switch (state) {
    case 'ready': return '';
    case 'not_started': return '目录索引正在准备，递归搜索暂不可用。';
    case 'building': return '目录索引正在更新，文件列表可能暂未包含最新文件。';
    case 'stale': return '目录索引可能未包含最新文件，系统会自动校准。';
    case 'failed': return '目录索引构建失败，部分文件暂不可见。';
    case 'unavailable': return '目录索引暂不可用。';
    default: return state ? `搜索索引状态：${state}` : '';
  }
}

export function FilesPage({ onOpenTrash, onOpenAgent }: FilesPageProps) {
  const [path, setPath] = useState<string[]>([]);
  const [query, setQuery] = useState('');
  const [searchScope, setSearchScope] = useState<FileSearchScope>('current');
  const [filterInputs, setFilterInputs] = useState<FileFilterInputs>(emptyFileFilterInputs);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const [showHidden, setShowHidden] = useState(false);
  const [viewMode, setViewMode] = useState<ViewMode>('list');
  const [sortMode, setSortMode] = useState<FileSortMode>('name-asc');
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());
  const [activeEntry, setActiveEntry] = useState<FileEntry | null>(null);
  const [readerTarget, setReaderTarget] = useState<{ path: string; name: string; kind: ReaderMediaKind; capturedAt?: string } | null>(null);
  const [directoryDrawerOpen, setDirectoryDrawerOpen] = useState(false);
  const [symlinkTarget, setSymlinkTarget] = useState<FileEntry | null>(null);
  const [archiveTarget, setArchiveTarget] = useState<{ path: string; name: string; version: string } | null>(null);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [detailOpen, setDetailOpen] = useState(false);
  const [metadataEditMode, setMetadataEditMode] = useState<FileMetadataEditMode | null>(null);
  const [notice, setNotice] = useState('');
  const [noticeFocusKey, setNoticeFocusKey] = useState(0);
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState('');
  const [hasMore, setHasMore] = useState(false);
  const [nextCursor, setNextCursor] = useState('');
  const [indexState, setIndexState] = useState('');
  const [refreshKey, setRefreshKey] = useState(0);
  const [pendingIndexRefreshGeneration, setPendingIndexRefreshGeneration] = useState<number | null>(null);
  const [folderDialogOpen, setFolderDialogOpen] = useState(false);
  const [folderName, setFolderName] = useState('');
  const [folderError, setFolderError] = useState('');
  const [folderSubmitting, setFolderSubmitting] = useState(false);
  const [renameTarget, setRenameTarget] = useState<FileEntry | null>(null);
  const [renameName, setRenameName] = useState('');
  const [renameError, setRenameError] = useState('');
  const [renameSubmitting, setRenameSubmitting] = useState(false);
  const [trashTarget, setTrashTarget] = useState<FileEntry | null>(null);
  const [trashError, setTrashError] = useState('');
  const [trashSubmitting, setTrashSubmitting] = useState(false);
  const [batchTrashOpen, setBatchTrashOpen] = useState(false);
  const [batchTrashError, setBatchTrashError] = useState('');
  const [batchTrashSubmitting, setBatchTrashSubmitting] = useState(false);
  const [batchTrashResult, setBatchTrashResult] = useState<BatchTrashResult | null>(null);
  const [batchRenameOpen, setBatchRenameOpen] = useState(false);
  const [createArchiveOpen, setCreateArchiveOpen] = useState(false);
  const [fileOperation, setFileOperation] = useState<{ mode: FileOperationMode; source: FileEntry } | null>(null);
  const [batchFileOperation, setBatchFileOperation] = useState<FileOperationMode | null>(null);
  const [batchFileOperationResult, setBatchFileOperationResult] = useState<{ mode: FileOperationMode; result: BatchFileOperationResult } | null>(null);
  const selectAllRef = useRef<HTMLInputElement>(null);
  const noticeRef = useRef<HTMLDivElement>(null);
  const focusNoticeRef = useRef(false);
  const folderNameRef = useRef<HTMLInputElement>(null);
  const renameNameRef = useRef<HTMLInputElement>(null);
  const directoryDrawerRef = useRef<HTMLElement>(null);
  const directoryDrawerTriggerRef = useRef<HTMLButtonElement>(null);
  const directoryDrawerCloseRef = useRef<HTMLButtonElement>(null);
  const trashCancelRef = useRef<HTMLButtonElement>(null);
  const batchTrashCancelRef = useRef<HTMLButtonElement>(null);
  const folderSubmittingRef = useRef(false);
  const renameSubmittingRef = useRef(false);
  const trashSubmittingRef = useRef(false);
  const batchTrashSubmittingRef = useRef(false);
  const batchRenameRefreshPendingRef = useRef(false);
  const preserveSelectionOnRefreshRef = useRef<Set<string> | null>(null);
  const requestIdRef = useRef(0);
  const loadedListingKeyRef = useRef('');
  const previousViewKeyRef = useRef('');
  const initialRequestRef = useRef<AbortController | null>(null);
  const loadMoreRequestRef = useRef<AbortController | null>(null);
  const relativePath = path.join('/');
  const queryTerm = query.trim();
  const effectiveScope = queryTerm ? searchScope : 'current';
  const filterValidationError = fileFilterError(filterInputs);
  const filterKey = JSON.stringify(filterInputs);
  const apiFilters = toFileFilters(filterInputs);
  const activeFilterCount = Object.values(filterInputs).filter((value) => value !== '').length;
  const hasListingCriteria = Boolean(queryTerm || activeFilterCount);
  const viewKey = JSON.stringify([relativePath, queryTerm, effectiveScope, sortMode, filterKey]);
  const listingKey = JSON.stringify([relativePath, queryTerm, effectiveScope, sortMode, filterKey, refreshKey]);

  useEffect(() => {
    const requestId = ++requestIdRef.current;
    const viewChanged = previousViewKeyRef.current !== viewKey;
    previousViewKeyRef.current = viewKey;
    const controller = new AbortController();
    initialRequestRef.current?.abort();
    loadMoreRequestRef.current?.abort();
    loadMoreRequestRef.current = null;
    initialRequestRef.current = controller;
    loadedListingKeyRef.current = '';
    setLoading(true);
    setLoadError('');
    setLoadMoreError('');
    setLoadingMore(false);
    setEntries([]);
    setHasMore(false);
    setNextCursor('');
    setIndexState('');
    const preservedSelection = preserveSelectionOnRefreshRef.current;
    preserveSelectionOnRefreshRef.current = null;
    setSelectedIds(preservedSelection ?? new Set());
    if (viewChanged) {
      setActiveEntry(null);
      setDetailOpen(false);
      setMetadataEditMode(null);
      setBatchTrashResult(null);
      setBatchFileOperationResult(null);
    }

    if (filterValidationError) {
      loadedListingKeyRef.current = listingKey;
      setLoading(false);
      return () => {
        controller.abort();
        if (initialRequestRef.current === controller) initialRequestRef.current = null;
      };
    }

    const timer = window.setTimeout(() => {
      void listDirectory(relativePath, {
        limit: pageSize,
        cursor: '',
        query: queryTerm,
        scope: effectiveScope,
        sort: sortMode,
        filters: apiFilters,
      }, controller.signal)
        .then((listing) => {
          if (controller.signal.aborted || requestIdRef.current !== requestId) return;
          const nextEntries = listing.entries.map(toFileEntry);
          setEntries(appendUniqueEntries([], nextEntries));
          setHasMore(listing.hasMore);
          setNextCursor(listing.nextCursor);
          setIndexState(listing.indexState);
          setActiveEntry((current) => current ? nextEntries.find((entry) => entry.path === current.path) ?? current : null);
          loadedListingKeyRef.current = listingKey;
        })
        .catch((error: unknown) => {
          if (controller.signal.aborted || (error instanceof DOMException && error.name === 'AbortError') || requestIdRef.current !== requestId) return;
          loadedListingKeyRef.current = listingKey;
          setEntries([]);
          setLoadError(error instanceof ApiError ? error.message : '读取目录时发生错误。');
        })
        .finally(() => {
          if (!controller.signal.aborted && requestIdRef.current === requestId) setLoading(false);
        });
    }, queryTerm ? 300 : 0);

    return () => {
      window.clearTimeout(timer);
      controller.abort();
      loadMoreRequestRef.current?.abort();
      loadMoreRequestRef.current = null;
      if (initialRequestRef.current === controller) initialRequestRef.current = null;
    };
  }, [relativePath, queryTerm, effectiveScope, sortMode, refreshKey, viewKey, listingKey, filterValidationError]);

  // 等待写操作触发的新索引代次，再刷新当前目录。
  useEffect(() => {
    if (pendingIndexRefreshGeneration === null) return;
    let cancelled = false;
    let timer: number | undefined;
    let attempts = 0;
    let failures = 0;
    let observedGeneration = -1;
    let stableGenerationChecks = 0;
    const controller = new AbortController();

    const checkIndex = async () => {
      try {
        const status = await getFileIndexStatus(controller.signal);
        if (cancelled) return;
        failures = 0;
        if (status.activeGeneration > pendingIndexRefreshGeneration && status.buildingGeneration === 0) {
          stableGenerationChecks = status.activeGeneration === observedGeneration ? stableGenerationChecks + 1 : 1;
          observedGeneration = status.activeGeneration;
          if (stableGenerationChecks >= 2) {
            setPendingIndexRefreshGeneration(null);
            setRefreshKey((current) => current + 1);
            return;
          }
        } else {
          observedGeneration = -1;
          stableGenerationChecks = 0;
        }
        if (status.state === 'failed' || status.state === 'unavailable') {
          setPendingIndexRefreshGeneration(null);
          return;
        }
      } catch {
        if (cancelled) return;
        failures += 1;
        if (failures >= 3) {
          setPendingIndexRefreshGeneration(null);
          return;
        }
      }

      attempts += 1;
      if (attempts >= 120) {
        setPendingIndexRefreshGeneration(null);
        return;
      }
      timer = window.setTimeout(() => { void checkIndex(); }, 1000);
    };

    void checkIndex();
    return () => {
      cancelled = true;
      controller.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [pendingIndexRefreshGeneration]);

  const listingIsCurrent = loadedListingKeyRef.current === listingKey;
  const displayLoading = loading || !listingIsCurrent;
  const pageHasMore = listingIsCurrent && hasMore;
  const visibleEntries = listingIsCurrent
    ? showHidden ? entries : entries.filter((entry) => !isHiddenEntry(entry))
    : [];
  const hiddenEntriesAvailable = !showHidden && listingIsCurrent && entries.some(isHiddenEntry);
  const selectableEntries = visibleEntries.filter((entry) => !entry.mountUnavailable);
  const allLoadedSelected = selectableEntries.length > 0 && selectableEntries.every((entry) => selectedIds.has(entry.id));
  const visibleSelectedCount = selectableEntries.filter((entry) => selectedIds.has(entry.id)).length;
  const selectedEntries = listingIsCurrent ? entries.filter((entry) => selectedIds.has(entry.id)) : [];
  const selectionHasMissingEntries = selectedEntries.length !== selectedIds.size;
  const selectionHasUnavailableEntries = selectedEntries.some((entry) => entry.mountUnavailable);
  const selectionHasMissingVersion = !selectionHasMissingEntries && selectedEntries.some((entry) => !entry.version);
  const canDownloadSelection = !displayLoading && selectedIds.size > 0 && !selectionHasMissingEntries
    && !selectionHasUnavailableEntries && !selectionHasMissingVersion;
  const selectionIssue = displayLoading
    ? '目录加载中，暂不可下载。'
    : selectionHasMissingEntries
      ? '部分选择已不在当前已加载项目中，请清除选择后重新选取。'
      : selectionHasUnavailableEntries
        ? '磁盘不可用，无法执行文件操作；请清除选择。'
        : selectionHasMissingVersion
          ? '部分项目缺少版本信息，请刷新目录后重新选择。'
          : '';
  const batchTrashIssue = displayLoading
    ? '目录加载中，暂不可操作。'
    : selectionHasMissingEntries
      ? '部分选择已不在当前已加载项目中，请清除选择后重新选取。'
      : selectionHasUnavailableEntries
        ? selectionIssue
        : selectedEntries.some((entry) => entry.readOnly)
          ? '只读项目不能移入回收站。'
          : selectionHasMissingVersion
            ? '部分项目缺少版本信息，请刷新目录后重新选择。'
            : selectedEntries.some((entry) => !canMoveEntryToTrash(entry))
              ? '选择中包含无法移入回收站的特殊文件。'
              : selectedIds.size > 256
                ? '一次最多移入 256 项，请减少选择数量。'
                : '';
  const canSubmitBatchTrash = selectedIds.size > 1 && !batchTrashIssue;
  const selectedPaths = new Set(selectedEntries.map((entry) => entry.path));
  const hasParentAndChildSelection = selectedEntries.some((entry) => {
    const parents = entry.path.split('/');
    parents.pop();
    while (parents.length > 0) {
      if (selectedPaths.has(parents.join('/'))) return true;
      parents.pop();
    }
    return false;
  });
  const batchRenameIssue = displayLoading
    ? '目录加载中，暂不可操作。'
    : selectionHasMissingEntries
      ? '部分选择已不在当前已加载项目中，请清除选择后重新选取。'
      : selectionHasUnavailableEntries
        ? selectionIssue
        : selectedEntries.some((entry) => entry.readOnly)
          ? '只读项目不能批量重命名。'
          : selectionHasMissingVersion
            ? '部分项目缺少版本信息，请刷新目录后重新选择。'
            : selectedIds.size > 256
              ? '一次最多重命名 256 项，请减少选择数量。'
              : hasParentAndChildSelection
                ? '不能同时重命名父目录和其中的项目，请分开操作。'
                : '';
  const canSubmitBatchRename = selectedIds.size > 1 && !batchRenameIssue;
  const batchFileOperationIssue = displayLoading
    ? '目录加载中，暂不可操作。'
    : selectionHasMissingEntries
      ? '部分选择已不在当前已加载项目中，请清除选择后重新选取。'
      : selectionHasUnavailableEntries
        ? selectionIssue
        : selectionHasMissingVersion
          ? '部分项目缺少版本信息，请刷新目录后重新选择。'
          : selectedEntries.some((entry) => entry.symlink || (entry.kind !== 'file' && entry.kind !== 'directory'))
            ? '批量复制和移动仅支持真实文件与文件夹。'
            : selectedIds.size > 256
              ? '一次最多处理 256 项，请减少选择数量。'
              : '';
  const canSubmitBatchFileOperation = selectedIds.size > 1 && !batchFileOperationIssue;
  const batchMoveIssue = batchFileOperationIssue || (selectedEntries.some((entry) => entry.readOnly) ? '只读项目不能移动。' : '');
  const canSubmitBatchMove = canSubmitBatchFileOperation && !selectedEntries.some((entry) => entry.readOnly);
  const createArchiveIssue = displayLoading
    ? '目录加载中，暂不可创建压缩包。'
    : selectionHasMissingEntries
      ? '部分选择已不在当前已加载项目中，请清除选择后重新选取。'
      : selectionHasUnavailableEntries
        ? selectionIssue
        : selectedEntries.some((entry) => entry.readOnly)
          ? '只读项目不能创建压缩包。'
          : selectionHasMissingVersion
            ? '部分项目缺少版本信息，请刷新目录后重新选择。'
            : selectedEntries.some((entry) => entry.symlink || (entry.kind !== 'file' && entry.kind !== 'directory'))
              ? '创建压缩包仅支持真实文件和文件夹。'
              : selectedIds.size > 256
                ? '一次最多压缩 256 项，请减少选择数量。'
                : '';
  const canCreateArchive = selectedIds.size > 0 && !createArchiveIssue;
  const currentDirectoryLabel = path.at(-1) ?? '全部文件';
  const crumbEntries = path.map((name, index) => ({ name, path: path.slice(0, index + 1) }));
  const directChildFolders = visibleEntries.filter((entry) => {
    if (entry.kind !== 'directory') return false;
    const separator = entry.path.lastIndexOf('/');
    const parentPath = separator < 0 ? '' : entry.path.slice(0, separator);
    return parentPath === relativePath;
  });
  const singleSelectedEntry = selectedIds.size === 1 ? entries.find((entry) => selectedIds.has(entry.id)) : undefined;
  const canCopySelected = !displayLoading && selectedIds.size === 1 && Boolean(singleSelectedEntry
    && (singleSelectedEntry.kind === 'file' || singleSelectedEntry.kind === 'directory')
    && !singleSelectedEntry.symlink && !singleSelectedEntry.mountUnavailable && singleSelectedEntry.version);
  const canMoveSelected = canCopySelected && !singleSelectedEntry?.readOnly
    && (singleSelectedEntry?.kind === 'file' || singleSelectedEntry?.kind === 'directory');
  const indexNotice = listingIsCurrent ? indexStateMessage(indexState) : '';

  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = visibleSelectedCount > 0 && !allLoadedSelected;
    }
  }, [allLoadedSelected, visibleSelectedCount]);

  useEffect(() => {
    if (!focusNoticeRef.current || !notice) return;
    noticeRef.current?.focus();
    focusNoticeRef.current = false;
  }, [notice, noticeFocusKey]);

  useEffect(() => {
    if (!folderDialogOpen && !renameTarget && !trashTarget && !batchTrashOpen) return;

    const previousFocus = document.activeElement;
    const activeInput = folderDialogOpen ? folderNameRef.current : renameTarget ? renameNameRef.current : trashTarget ? trashCancelRef.current : batchTrashCancelRef.current;
    activeInput?.focus();
    if (renameTarget) renameNameRef.current?.select();

    function handleDialogKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        event.preventDefault();
        if (renameTarget) closeRenameDialog();
        else if (trashTarget) closeTrashDialog();
        else if (batchTrashOpen) closeBatchTrashDialog();
        else closeFolderDialog();
        return;
      }
      if (event.key !== 'Tab') return;

      const dialog = document.querySelector<HTMLElement>('.files-name-dialog');
      if (!dialog) return;
      const focusable = dialog.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled)');
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

    window.addEventListener('keydown', handleDialogKeyDown);
    return () => {
      window.removeEventListener('keydown', handleDialogKeyDown);
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) previousFocus.focus();
    };
  }, [folderDialogOpen, renameTarget, trashTarget, batchTrashOpen]);

  useEffect(() => {
    if (!directoryDrawerOpen) return;

    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    directoryDrawerCloseRef.current?.focus();

    function handleKeyDown(event: KeyboardEvent) {
      const drawer = directoryDrawerRef.current;
      if (!drawer) return;
      if (event.key === 'Escape') {
        event.preventDefault();
        setDirectoryDrawerOpen(false);
        return;
      }
      if (event.key !== 'Tab') return;

      const focusable = drawer.querySelectorAll<HTMLElement>('button:not(:disabled)');
      if (focusable.length === 0) {
        event.preventDefault();
        drawer.focus();
        return;
      }
      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      if (event.shiftKey && (document.activeElement === first || !drawer.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !drawer.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    }

    const desktopLayout = window.matchMedia('(min-width: 1201px)');
    const closeOnDesktop = () => {
      if (desktopLayout.matches) setDirectoryDrawerOpen(false);
    };
    window.addEventListener('keydown', handleKeyDown);
    desktopLayout.addEventListener('change', closeOnDesktop);
    return () => {
      window.removeEventListener('keydown', handleKeyDown);
      desktopLayout.removeEventListener('change', closeOnDesktop);
      document.body.style.overflow = previousOverflow;
      const focusTarget = previousFocus && previousFocus !== document.body
        ? previousFocus
        : directoryDrawerTriggerRef.current;
      if (focusTarget?.isConnected) focusTarget.focus();
    };
  }, [directoryDrawerOpen]);

  function navigate(nextPath: string[]) {
    setPath(nextPath);
    setDirectoryDrawerOpen(false);
    setSelectedIds(new Set());
    setActiveEntry(null);
    setDetailOpen(false);
    setQuery('');
    setSearchScope('current');
  }

  function replaceFilterInputs(update: (current: FileFilterInputs) => FileFilterInputs) {
    initialRequestRef.current?.abort();
    loadMoreRequestRef.current?.abort();
    initialRequestRef.current = null;
    loadMoreRequestRef.current = null;
    requestIdRef.current += 1;
    loadedListingKeyRef.current = '';
    setEntries([]);
    setHasMore(false);
    setNextCursor('');
    setLoading(true);
    setLoadingMore(false);
    setLoadError('');
    setLoadMoreError('');
    setSelectedIds(new Set());
    setFilterInputs(update);
  }

  function updateFilter<K extends keyof FileFilterInputs>(key: K, value: FileFilterInputs[K]) {
    replaceFilterInputs((current) => ({ ...current, [key]: value }));
  }

  function clearFilters() {
    replaceFilterInputs(() => emptyFileFilterInputs());
  }

  function activateEntry(entry: FileEntry) {
    setNotice('');
    if (entry.mountUnavailable) return;
    if (entry.kind === 'directory') {
      navigate(entry.path ? entry.path.split('/').filter(Boolean) : []);
      return;
    }
    if (entry.displayKind === 'archive') {
      if (!entry.version) {
        setNotice('服务未提供压缩包版本信息，无法安全打开。');
        return;
      }
      setArchiveTarget({ path: entry.path, name: entry.name, version: entry.version });
      setActiveEntry(null);
      setDetailOpen(false);
      return;
    }
    const extension = entry.name.split('.').at(-1)?.toLocaleLowerCase() ?? '';
    const isDelimitedText = entry.displayKind === 'spreadsheet' && ['csv', 'tsv'].includes(extension);
    const isXlsx = entry.displayKind === 'spreadsheet' && extension === 'xlsx';
    const isEpub = extension === 'epub';
    const isOffice = ['docx', 'pptx'].includes(extension);
    let readerKind: ReaderMediaKind | null = null;
    if (entry.displayKind === 'image' || entry.displayKind === 'audio' || entry.displayKind === 'text' || entry.displayKind === 'video' || entry.displayKind === 'pdf') {
      readerKind = entry.displayKind;
    } else if (isDelimitedText) {
      readerKind = 'text';
    } else if (isXlsx) {
      readerKind = 'xlsx';
    } else if (isEpub) {
      readerKind = 'epub';
    } else if (isOffice) {
      readerKind = 'office';
    }
    if (readerKind) {
      // 支持格式交由对应阅读器展示。
      setReaderTarget({ path: entry.id, name: entry.name, kind: readerKind, capturedAt: entry.capturedAt });
      setActiveEntry(null);
      setDetailOpen(false);
      return;
    }
    setActiveEntry(entry);
    setDetailOpen(true);
  }

  function showDetails(entry: FileEntry) {
    setMetadataEditMode(null);
    setActiveEntry(entry);
    setDetailOpen(true);
  }

  function completeMetadataChange(entry: ApiFileEntry) {
    const updated = toFileEntry(entry);
    setActiveEntry(updated);
    setEntries((current) => current.map((item) => item.path === updated.path ? updated : item));
    setMetadataEditMode(null);
    setNotice(`已更新文件信息：${updated.name}`);
    setRefreshKey((current) => current + 1);
  }

  function toggleSelection(id: string) {
    if (entries.find((entry) => entry.id === id)?.mountUnavailable) return;
    setSelectedIds((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function toggleAllVisible() {
    setSelectedIds((current) => {
      const next = new Set(current);
      visibleEntries.filter((entry) => entry.mountUnavailable).forEach((entry) => next.delete(entry.id));
      if (allLoadedSelected) selectableEntries.forEach((entry) => next.delete(entry.id));
      else selectableEntries.forEach((entry) => next.add(entry.id));
      return next;
    });
  }

  async function loadMore() {
    if (!hasMore || !nextCursor || loadingMore || loadedListingKeyRef.current !== listingKey) return;
    const requestId = requestIdRef.current;
    const controller = new AbortController();
    loadMoreRequestRef.current?.abort();
    loadMoreRequestRef.current = controller;
    setLoadingMore(true);
    setLoadMoreError('');
    try {
      const listing = await listDirectory(relativePath, {
        limit: pageSize,
        cursor: nextCursor,
        query: queryTerm,
        scope: effectiveScope,
        sort: sortMode,
        filters: apiFilters,
      }, controller.signal);
      if (controller.signal.aborted || requestIdRef.current !== requestId || loadedListingKeyRef.current !== listingKey) return;
      setEntries((current) => appendUniqueEntries(current, listing.entries.map(toFileEntry)));
      setHasMore(listing.hasMore);
      setNextCursor(listing.nextCursor);
      setIndexState(listing.indexState);
    } catch (error) {
      if (controller.signal.aborted || (error instanceof DOMException && error.name === 'AbortError') || requestIdRef.current !== requestId) return;
      if (error instanceof ApiError && error.status === 409) {
        setRefreshKey((current) => current + 1);
        return;
      }
      setLoadMoreError(error instanceof ApiError ? error.message : '加载更多项目时发生错误。');
    } finally {
      if (loadMoreRequestRef.current === controller) {
        loadMoreRequestRef.current = null;
        setLoadingMore(false);
      }
    }
  }

  function openFolderDialog() {
    setNotice('');
    setFolderName('');
    setFolderError('');
    setFolderDialogOpen(true);
  }

  function closeFolderDialog() {
    if (folderSubmittingRef.current) return;
    setFolderDialogOpen(false);
    setFolderError('');
  }

  // 创建真实目录并按服务端分配的名称刷新当前列表。
  async function submitCreateFolder(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const name = folderName.trim();
    if (!name) {
      setFolderError('请输入文件夹名称。');
      folderNameRef.current?.focus();
      return;
    }
    if (name === '.' || name === '..' || /[\\/\0]/.test(name)) {
      setFolderError('名称不能是 . 或 ..，也不能包含斜杠。');
      folderNameRef.current?.focus();
      return;
    }
    if (new TextEncoder().encode(name).byteLength > 255) {
      setFolderError('名称不能超过 255 个 UTF-8 字节。');
      folderNameRef.current?.focus();
      return;
    }

    folderSubmittingRef.current = true;
    setFolderSubmitting(true);
    setFolderError('');
    try {
      const created = await createDirectory(relativePath, name);
      setFolderDialogOpen(false);
      setFolderName('');
      setNotice(`已创建文件夹「${created.name}」。`);
      setRefreshKey((current) => current + 1);
    } catch (error) {
      setFolderError(error instanceof ApiError ? error.message : '新建文件夹时发生错误，请重试。');
    } finally {
      folderSubmittingRef.current = false;
      setFolderSubmitting(false);
    }
  }

  function openRenameDialog(entry: FileEntry) {
    setNotice('');
    setRenameTarget(entry);
    setRenameName(entry.name);
    setRenameError('');
  }

  function closeRenameDialog() {
    if (renameSubmittingRef.current) return;
    setRenameTarget(null);
    setRenameError('');
  }

  // 提交版本保护的重命名；冲突后刷新并提示用户核对。
  async function submitRename(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const target = renameTarget;
    if (!target) return;

    const name = renameName.trim();
    if (!name) {
      setRenameError('请输入新名称。');
      renameNameRef.current?.focus();
      return;
    }
    if (name === '.' || name === '..' || /[\\/\0]/.test(name)) {
      setRenameError('名称不能是 . 或 ..，也不能包含斜杠。');
      renameNameRef.current?.focus();
      return;
    }
    if (new TextEncoder().encode(name).byteLength > 255) {
      setRenameError('名称不能超过 255 个 UTF-8 字节。');
      renameNameRef.current?.focus();
      return;
    }
    if (name === target.name) {
      setRenameError('新名称与当前名称相同。');
      renameNameRef.current?.focus();
      return;
    }

    renameSubmittingRef.current = true;
    setRenameSubmitting(true);
    setRenameError('');
    try {
      const result = await renameEntry(target.path, target.version, name);
      setRenameTarget(null);
      // 响应不含新版本；刷新完成前禁用后续重命名提交。
      setActiveEntry({ ...target, name: result.name, path: result.path, id: result.path, displayPath: `/${result.path}`, version: '' });
      setSelectedIds(new Set());
      setNotice(`已重命名为「${result.name}」。`);
      setRefreshKey((current) => current + 1);
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setRenameTarget(null);
        setActiveEntry(null);
        setDetailOpen(false);
        focusNoticeRef.current = true;
        setNoticeFocusKey((current) => current + 1);
        setNotice('重命名未执行：文件或文件夹状态已变化、目标名称已存在，或文件系统无法安全修改。未覆盖任何项目；目录正在刷新，请核对后重试。');
        setRefreshKey((current) => current + 1);
      } else {
        setRenameError(error instanceof ApiError ? error.message : '重命名时发生错误，请重试。');
      }
    } finally {
      renameSubmittingRef.current = false;
      setRenameSubmitting(false);
    }
  }

  function clearSelection() {
    setSelectedIds(new Set());
  }

  // 打开单项复制或移动的目录选择器。
  function openFileOperation(mode: FileOperationMode) {
    if (!singleSelectedEntry || (mode === 'copy' ? !canCopySelected : !canMoveSelected)) return;
    setNotice('');
    setFileOperation({ mode, source: singleSelectedEntry });
  }

  function openBatchFileOperation(mode: FileOperationMode) {
    if (!canSubmitBatchFileOperation || mode === 'move' && selectedEntries.some((entry) => entry.readOnly)) return;
    setNotice('');
    setBatchFileOperationResult(null);
    setBatchFileOperation(mode);
  }

  function openCreateArchiveDialog() {
    if (!canCreateArchive) return;
    setNotice('');
    setCreateArchiveOpen(true);
  }

  function completeCreateArchive(result: { path: string; name: string }, previousGeneration?: number) {
    setCreateArchiveOpen(false);
    preserveSelectionOnRefreshRef.current = new Set();
    setSelectedIds(new Set());
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    setNotice(`已创建压缩包「${result.name}」。`);
    setPendingIndexRefreshGeneration(previousGeneration ?? null);
    setRefreshKey((current) => current + 1);
  }

  function handleCreateArchiveConflict() {
    setCreateArchiveOpen(false);
    preserveSelectionOnRefreshRef.current = new Set(selectedIds);
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    setNotice('文件状态已变化，未创建压缩包。目录正在刷新，请核对所选项目后重试。');
    setRefreshKey((current) => current + 1);
  }

  // 展示操作结果，并刷新当前目录。
  function completeFileOperation(mode: FileOperationMode, result: FileOperationResult, previousGeneration?: number) {
    const source = fileOperation?.source;
    setFileOperation(null);
    setSelectedIds(new Set());
    const partialMove = mode === 'move' && 'status' in result && result.status === 'partial';
    if (mode === 'move' && !partialMove) {
      setActiveEntry((current) => current?.path === fileOperation?.source.path ? null : current);
      if (activeEntry?.path === fileOperation?.source.path) setDetailOpen(false);
    }
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    if (partialMove && 'message' in result) {
      const renamed = source && result.name !== source.name ? `目标已命名为「${result.name}」，` : '';
      setNotice(`部分完成：${renamed}目标位于 /${result.path}。${result.message}`);
    } else if (source && result.name !== source.name) {
      const target = mode === 'copy' ? '副本' : '项目';
      setNotice(`目标已有同名项目，已自动命名为「${result.name}」；${target}位于 /${result.path}。`);
    } else {
      setNotice(`已${mode === 'copy' ? '复制' : '移动'}到 /${result.path}。`);
    }
    setPendingIndexRefreshGeneration(previousGeneration ?? null);
    setRefreshKey((current) => current + 1);
  }

  // 展示逐项结果，并保留失败项目供用户重试。
  function completeBatchFileOperation(mode: FileOperationMode, result: BatchFileOperationResult, previousGeneration?: number) {
    setBatchFileOperation(null);
    const incompletePaths = incompleteBatchOperationPaths(result.items);
    const remaining = new Set(selectedEntries.filter((entry) => incompletePaths.has(entry.path)).map((entry) => entry.id));
    preserveSelectionOnRefreshRef.current = remaining;
    setSelectedIds(remaining);
    setBatchFileOperationResult({ mode, result });

    if (mode === 'move') {
      const movedDirectories = result.items
        .filter((item) => item.status === 'moved' && selectedEntries.some((entry) => entry.path === item.sourcePath && entry.kind === 'directory'))
        .map((item) => item.sourcePath);
      const movedPaths = new Set(result.items.filter((item) => item.status === 'moved').map((item) => item.sourcePath));
      const activeWasMoved = activeEntry && (movedPaths.has(activeEntry.path)
        || movedDirectories.some((directory) => activeEntry.path.startsWith(`${directory}/`)));
      if (activeWasMoved) {
        setActiveEntry(null);
        setDetailOpen(false);
      }
    }

    const failedCount = incompletePaths.size;
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    setNotice(failedCount
      ? `批量${mode === 'copy' ? '复制' : '移动'}部分完成：${result.items.length - failedCount} 项已处理，${failedCount} 项保留选择以便重试。`
      : `已完成批量${mode === 'copy' ? '复制' : '移动'}，共 ${result.items.length} 项。`);
    setPendingIndexRefreshGeneration(previousGeneration ?? null);
    setRefreshKey((current) => current + 1);
  }

  function handleBatchFileOperationConflict() {
    setBatchFileOperation(null);
    preserveSelectionOnRefreshRef.current = new Set(selectedIds);
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    setNotice('文件状态已变化，未执行批量操作。目录正在刷新，请核对所选项目后重试。');
    setRefreshKey((current) => current + 1);
  }

  function canMoveEntryToTrash(entry: FileEntry): boolean {
    return Boolean(entry.version) && !entry.readOnly && !entry.mountUnavailable
      && (entry.kind !== 'other' || entry.symlink);
  }

  function openTrashDialog(entry: FileEntry) {
    if (!canMoveEntryToTrash(entry) || trashSubmittingRef.current) return;
    setNotice('');
    setTrashError('');
    setTrashTarget(entry);
  }

  function closeTrashDialog() {
    if (trashSubmittingRef.current) return;
    setTrashTarget(null);
    setTrashError('');
  }

  function openBatchTrashDialog() {
    if (!canSubmitBatchTrash || batchTrashSubmittingRef.current) return;
    setNotice('');
    setBatchTrashError('');
    setBatchTrashResult(null);
    setBatchTrashOpen(true);
  }

  function closeBatchTrashDialog() {
    if (batchTrashSubmittingRef.current) return;
    setBatchTrashOpen(false);
    setBatchTrashError('');
  }

  function openBatchRenameDialog() {
    if (!canSubmitBatchRename) return;
    batchRenameRefreshPendingRef.current = false;
    setNotice('');
    setBatchRenameOpen(true);
  }

  function closeBatchRenameDialog() {
    setBatchRenameOpen(false);
    if (!batchRenameRefreshPendingRef.current) return;
    batchRenameRefreshPendingRef.current = false;
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    setRefreshKey((current) => current + 1);
  }

  function handleBatchRenameConflict() {
    batchRenameRefreshPendingRef.current = false;
    setBatchRenameOpen(false);
    focusNoticeRef.current = true;
    setNoticeFocusKey((current) => current + 1);
    setNotice('重命名未执行：文件状态已变化或目标名称已占用。目录正在刷新，请核对后重试。');
    setRefreshKey((current) => current + 1);
  }

  function handleBatchRenameComplete(result: BatchRenameResult) {
    const renamedPaths = new Set(result.items.filter((item) => item.status === 'renamed').map((item) => item.path));
    const remainingSelection = selectedEntries
      .filter((entry) => !renamedPaths.has(entry.path))
      .map((entry) => entry.id);
    preserveSelectionOnRefreshRef.current = new Set(remainingSelection);
    setSelectedIds(new Set(remainingSelection));

    if (readerTarget && renamedPaths.has(readerTarget.path)) setReaderTarget(null);
    if (activeEntry && [...renamedPaths].some((renamedPath) => activeEntry.path === renamedPath || activeEntry.path.startsWith(`${renamedPath}/`))) {
      setActiveEntry(null);
      setDetailOpen(false);
    }

    batchRenameRefreshPendingRef.current = true;
    const renamedCount = renamedPaths.size;
    setNotice(result.status === 'completed'
      ? `已重命名 ${renamedCount} 项。`
      : '批量重命名部分完成；失败和未执行项目仍保留在选择中。');
  }

  async function submitBatchMoveToTrash() {
    if (!canSubmitBatchTrash || batchTrashSubmittingRef.current) return;

    batchTrashSubmittingRef.current = true;
    setBatchTrashSubmitting(true);
    setBatchTrashError('');
    try {
      const result = await batchMoveFilesToTrash(selectedEntries.map(({ path: entryPath, version }) => ({ path: entryPath, version })));
      const succeededPaths = result.items.filter((item) => item.status !== 'failed').map((item) => item.path);
      const failedPaths = result.items.filter((item) => item.status === 'failed').map((item) => item.path);
      const failedIds = new Set(failedPaths);
      preserveSelectionOnRefreshRef.current = failedIds;
      setSelectedIds(failedIds);
      if (activeEntry && succeededPaths.some((entryPath) => activeEntry.path === entryPath || activeEntry.path.startsWith(`${entryPath}/`))) {
        setActiveEntry(null);
        setDetailOpen(false);
      }
      setBatchTrashResult(result);
      setBatchTrashOpen(false);
      setRefreshKey((current) => current + 1);
    } catch (error) {
      setBatchTrashError(error instanceof ApiError ? error.message : '批量移入回收站时发生错误，请重试。');
    } finally {
      batchTrashSubmittingRef.current = false;
      setBatchTrashSubmitting(false);
    }
  }

  // 按当前版本令牌将单个项目移入可恢复的回收站。
  async function submitMoveToTrash() {
    const target = trashTarget;
    if (!target) return;
    const removingActiveEntry = activeEntry?.path === target.path;
    if (!target.version) {
      setTrashError('文件状态已变化，正在刷新目录；请核对后重试。');
      setRefreshKey((current) => current + 1);
      return;
    }

    trashSubmittingRef.current = true;
    setTrashSubmitting(true);
    setTrashError('');
    try {
      await moveFileToTrash(target.path, target.version);
      setTrashTarget(null);
      setActiveEntry((current) => current?.path === target.path ? null : current);
      if (removingActiveEntry) setDetailOpen(false);
      setSelectedIds((current) => {
        const next = new Set(current);
        next.delete(target.id);
        return next;
      });
      focusNoticeRef.current = true;
      setNoticeFocusKey((current) => current + 1);
      setNotice(`已将「${target.name}」移入回收站，可在回收站恢复。`);
      setRefreshKey((current) => current + 1);
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        setTrashTarget(null);
        setActiveEntry(null);
        setDetailOpen(false);
        focusNoticeRef.current = true;
        setNoticeFocusKey((current) => current + 1);
        setNotice('删除未执行：文件或文件夹状态已变化，或当前操作无法安全完成。目录正在刷新，请核对后重试。');
        setRefreshKey((current) => current + 1);
      } else {
        setTrashError(error instanceof ApiError ? error.message : '移入回收站时发生错误，请重试。');
      }
    } finally {
      trashSubmittingRef.current = false;
      setTrashSubmitting(false);
    }
  }

  const countLabel = `已加载 ${visibleEntries.length} 项`;

  return (
    <section className="files-page" aria-labelledby="files-page-title">
      <header className="files-page-heading">
        <div>
          <h1 id="files-page-title">我的文件</h1>
        </div>
        <div className="files-heading-actions">
          <button className="files-button files-button-secondary" type="button" onClick={onOpenTrash}>
            <Recycle size={17} aria-hidden="true" />回收站
          </button>
          <button className="files-button files-button-secondary" type="button" onClick={() => setUploadOpen(true)}>
            <Upload size={17} aria-hidden="true" />上传
          </button>
          <button className="files-button files-button-primary" type="button" onClick={openFolderDialog}>
            <FolderPlus size={17} aria-hidden="true" />新建文件夹
          </button>
        </div>
      </header>

      <div className={`files-browser-shell${activeEntry ? ' has-details' : ''}`}>
        <aside className="files-directory-panel" aria-label="目录导航">
          <div className="files-panel-heading"><span>目录</span><span className="files-realtime-label">实时</span></div>
          <nav className="files-directory-list" aria-label="管理根目录">
            <button
              className={`files-directory-item${path.length === 0 ? ' is-active' : ''}`}
              type="button"
              aria-current={path.length === 0 ? 'location' : undefined}
              onClick={() => navigate([])}
            >
              <HardDrive size={17} strokeWidth={1.8} aria-hidden="true" />
              <span>全部文件</span>
            </button>
          </nav>
        </aside>

        <div className="files-main-column">
          <div className="files-path-row">
            <button
              ref={directoryDrawerTriggerRef}
              className="files-mobile-directory-toggle"
              type="button"
              aria-label="打开目录导航"
              aria-haspopup="dialog"
              aria-expanded={directoryDrawerOpen}
              aria-controls="files-directory-drawer"
              onClick={() => setDirectoryDrawerOpen(true)}
            >
              <Folder size={16} aria-hidden="true" />目录
            </button>
            <nav className="files-breadcrumbs" aria-label="当前位置">
              <button type="button" onClick={() => navigate([])} aria-current={path.length === 0 ? 'page' : undefined}>全部文件</button>
              {crumbEntries.map((crumb) => (
                <span className="files-breadcrumb-part" key={crumb.path.join('/')}>
                  <ChevronRight size={14} aria-hidden="true" />
                  <button type="button" onClick={() => navigate(crumb.path)} aria-current={crumb.path.length === path.length ? 'page' : undefined}>{crumb.name}</button>
                </span>
              ))}
            </nav>
            <span className="files-item-count">{countLabel}</span>
          </div>

          <div className="files-toolbar">
            <div className="files-search-controls">
              <label className="files-search">
                <Search size={17} aria-hidden="true" />
                <input
                  type="search"
                  value={query}
                  onChange={(event) => {
                    setQuery(event.currentTarget.value);
                    if (!event.currentTarget.value.trim()) setSearchScope('current');
                  }}
                  placeholder="搜索文件名或路径"
                  aria-label="搜索名称或相对路径"
                />
                {query && <button type="button" className="files-search-clear" aria-label="清空搜索" onClick={() => { setQuery(''); setSearchScope('current'); }}><X size={15} /></button>}
              </label>
              {queryTerm && (
                <label className="files-search-scope-label">
                  <span className="files-visually-hidden">搜索范围</span>
                  <select className="files-search-scope" aria-label="搜索范围" value={searchScope} onChange={(event) => setSearchScope(event.currentTarget.value as FileSearchScope)}>
                    <option value="current">当前文件夹</option>
                    <option value="recursive">包含子文件夹</option>
                  </select>
                </label>
              )}
            </div>
            <select className="files-sort-select" aria-label="排序方式" value={sortMode} onChange={(event) => setSortMode(event.currentTarget.value as FileSortMode)}>
              <option value="name-asc">名称升序</option>
              <option value="name-desc">名称降序</option>
              <option value="size-desc">大小从大到小</option>
              <option value="size-asc">大小从小到大</option>
              <option value="modified-desc">修改时间最新</option>
              <option value="modified-asc">修改时间最早</option>
              <option value="captured-desc">拍摄时间最新</option>
              <option value="captured-asc">拍摄时间最早</option>
            </select>
            <div className="files-view-switch" role="group" aria-label="显示方式">
              <button type="button" className={viewMode === 'list' ? 'is-active' : ''} aria-pressed={viewMode === 'list'} aria-label="列表视图" onClick={() => setViewMode('list')}>
                <List size={17} aria-hidden="true" /><span>列表</span>
              </button>
              <button type="button" className={viewMode === 'grid' ? 'is-active' : ''} aria-pressed={viewMode === 'grid'} aria-label="网格视图" onClick={() => setViewMode('grid')}>
                <LayoutGrid size={16} aria-hidden="true" /><span>网格</span>
              </button>
            </div>
          </div>

          <div className="files-filter-bar">
            <button
              type="button"
              className={`files-filter-toggle${filtersOpen ? ' is-open' : ''}`}
              aria-expanded={filtersOpen}
              aria-controls="files-filter-panel"
              onClick={() => setFiltersOpen((open) => !open)}
            >
              <SlidersHorizontal size={15} aria-hidden="true" />
              <span>筛选</span>
              {activeFilterCount > 0 && <span className="files-filter-count">{activeFilterCount}</span>}
            </button>
            <label className="files-hidden-toggle" title="默认隐藏名称以点开头的文件和文件夹">
              <input
                type="checkbox"
                checked={showHidden}
                onChange={(event) => {
                  setShowHidden(event.currentTarget.checked);
                  clearSelection();
                }}
              />
              <span>显示隐藏文件</span>
            </label>
            {activeFilterCount > 0 && (
              <>
                <span className="files-filter-summary">已启用 {activeFilterCount} 项</span>
                <button type="button" className="files-filter-clear" onClick={clearFilters}>清除筛选</button>
              </>
            )}
          </div>

          {filtersOpen && (
            <section id="files-filter-panel" className="files-filter-panel" aria-label="文件筛选条件">
              <div className="files-filter-grid">
                <label className="files-filter-field">
                  <span>文件类型</span>
                  <select value={filterInputs.type} onChange={(event) => updateFilter('type', event.currentTarget.value as FileFilterType | '')}>
                    <option value="">全部类型</option>
                    <option value="image">图片</option>
                    <option value="video">视频</option>
                    <option value="audio">音频</option>
                    <option value="document">文档</option>
                    <option value="archive">压缩包</option>
                    <option value="directory">文件夹</option>
                    <option value="other">其他</option>
                  </select>
                </label>
                <label className="files-filter-field">
                  <span>最小大小（MiB）</span>
                  <input type="number" min="0" step="any" inputMode="decimal" value={filterInputs.minSizeMiB} onChange={(event) => updateFilter('minSizeMiB', event.currentTarget.value)} />
                </label>
                <label className="files-filter-field">
                  <span>最大大小（MiB）</span>
                  <input type="number" min="0" step="any" inputMode="decimal" value={filterInputs.maxSizeMiB} onChange={(event) => updateFilter('maxSizeMiB', event.currentTarget.value)} />
                </label>
                <label className="files-filter-field">
                  <span>修改日期起（含）</span>
                  <input type="date" value={filterInputs.modifiedFrom} onChange={(event) => updateFilter('modifiedFrom', event.currentTarget.value)} />
                </label>
                <label className="files-filter-field">
                  <span>修改日期止（含）</span>
                  <input type="date" value={filterInputs.modifiedThrough} onChange={(event) => updateFilter('modifiedThrough', event.currentTarget.value)} />
                </label>
                <label className="files-filter-field">
                  <span>拍摄日期起（含）</span>
                  <input type="date" value={filterInputs.capturedFrom} onChange={(event) => updateFilter('capturedFrom', event.currentTarget.value)} />
                </label>
                <label className="files-filter-field">
                  <span>拍摄日期止（含）</span>
                  <input type="date" value={filterInputs.capturedThrough} onChange={(event) => updateFilter('capturedThrough', event.currentTarget.value)} />
                </label>
              </div>
              <p className="files-filter-help">日期按浏览器本地时区计算，结束日包含当天。</p>
              {filterValidationError && <p className="files-filter-error" role="alert">{filterValidationError}</p>}
            </section>
          )}

            {viewMode === 'grid' && selectableEntries.length > 0 && !displayLoading && !loadError && (
            <div className="files-loaded-select">
              <label>
                <input ref={selectAllRef} type="checkbox" checked={allLoadedSelected} onChange={toggleAllVisible} aria-label="全选已加载项目" />
                <span>全选已加载</span>
              </label>
            </div>
          )}

          {selectedIds.size > 0 && (
            <div className="files-selection-bar" role="status" aria-live="polite">
              <span><strong>已选择 {selectedIds.size} 项</strong><small>{queryTerm && effectiveScope === 'recursive' ? '递归搜索结果' : currentDirectoryLabel} · {singleSelectedEntry ? '可执行单项操作' : '批量操作'}</small></span>
              <button
                type="button"
                className="files-selection-download"
                onClick={() => downloadArchive(selectedEntries.map(({ path: entryPath, version }) => ({ path: entryPath, version })))}
                disabled={!canDownloadSelection}
                aria-describedby={selectionIssue ? 'files-selection-download-help' : undefined}
              >
                <Download size={15} aria-hidden="true" />下载所选
              </button>
              <button
                type="button"
                className="files-selection-archive"
                onClick={openCreateArchiveDialog}
                disabled={!canCreateArchive}
                aria-describedby={createArchiveIssue ? (createArchiveIssue === selectionIssue ? 'files-selection-download-help' : 'files-selection-archive-help') : undefined}
              >
                <FileArchive size={15} aria-hidden="true" />创建压缩包
              </button>
              {canCopySelected && singleSelectedEntry && (
                <>
                  <button type="button" className="files-selection-copy" onClick={() => openFileOperation('copy')}>
                    <Copy size={15} aria-hidden="true" />复制到
                  </button>
                  {canMoveSelected && (
                    <button type="button" className="files-selection-move" onClick={() => openFileOperation('move')}>
                      <MoveRight size={15} aria-hidden="true" />移动到
                    </button>
                  )}
                </>
              )}
              {selectedIds.size > 1 && (
                <>
                  <button type="button" className="files-selection-copy" onClick={() => openBatchFileOperation('copy')} disabled={!canSubmitBatchFileOperation} aria-describedby={batchFileOperationIssue ? (batchFileOperationIssue === selectionIssue ? 'files-selection-download-help' : 'files-selection-file-operation-help') : undefined}>
                    <Copy size={15} aria-hidden="true" />复制到
                  </button>
                  <button type="button" className="files-selection-move" onClick={() => openBatchFileOperation('move')} disabled={!canSubmitBatchMove} aria-describedby={batchMoveIssue ? (batchMoveIssue === batchFileOperationIssue ? (batchFileOperationIssue === selectionIssue ? 'files-selection-download-help' : 'files-selection-file-operation-help') : 'files-selection-move-help') : undefined}>
                    <MoveRight size={15} aria-hidden="true" />移动到
                  </button>
                </>
              )}
              {selectedIds.size > 1 && (
                <button
                  type="button"
                  className="files-selection-rename"
                  onClick={openBatchRenameDialog}
                  disabled={!canSubmitBatchRename}
                  aria-describedby={batchRenameIssue ? (batchRenameIssue === selectionIssue ? 'files-selection-download-help' : 'files-selection-rename-help') : undefined}
                >
                  <Pencil size={15} aria-hidden="true" />批量重命名
                </button>
              )}
              {singleSelectedEntry && <button type="button" className="files-selection-trash" onClick={() => openTrashDialog(singleSelectedEntry)} disabled={!canMoveEntryToTrash(singleSelectedEntry)}><Trash2 size={15} aria-hidden="true" />移入回收站</button>}
              {selectedIds.size > 1 && (
                <button
                  type="button"
                  className="files-selection-trash"
                  onClick={openBatchTrashDialog}
                  disabled={!canSubmitBatchTrash}
                  aria-describedby={batchTrashIssue ? (batchTrashIssue === selectionIssue ? 'files-selection-download-help' : 'files-selection-trash-help') : undefined}
                >
                  <Trash2 size={15} aria-hidden="true" />移入回收站
                </button>
              )}
              <button type="button" onClick={clearSelection}><X size={15} aria-hidden="true" />清除选择</button>
              {selectionIssue && <small id="files-selection-download-help" className="files-selection-help">{selectionIssue}</small>}
              {selectedIds.size > 1 && batchRenameIssue && batchRenameIssue !== selectionIssue && <small id="files-selection-rename-help" className="files-selection-help">{batchRenameIssue}</small>}
              {selectedIds.size > 1 && batchTrashIssue && batchTrashIssue !== selectionIssue && <small id="files-selection-trash-help" className="files-selection-help">{batchTrashIssue}</small>}
              {selectedIds.size > 1 && batchFileOperationIssue && batchFileOperationIssue !== selectionIssue && <small id="files-selection-file-operation-help" className="files-selection-help">{batchFileOperationIssue}</small>}
              {selectedIds.size > 1 && batchMoveIssue && batchMoveIssue !== batchFileOperationIssue && <small id="files-selection-move-help" className="files-selection-help">{batchMoveIssue}</small>}
              {createArchiveIssue && createArchiveIssue !== selectionIssue && <small id="files-selection-archive-help" className="files-selection-help">{createArchiveIssue}</small>}
            </div>
          )}

          {indexNotice && <p className="files-index-note" role="status">{indexNotice}</p>}

          {notice && (
            <div ref={noticeRef} className="files-action-notice" role="status" tabIndex={-1}>
              <span>{notice}</span>
              <button type="button" aria-label="关闭提示" onClick={() => setNotice('')}><X size={16} /></button>
            </div>
          )}

          {batchTrashResult && (
            <section className="files-batch-trash-report" aria-labelledby="files-batch-trash-report-title">
              <header>
                <div>
                  <strong id="files-batch-trash-report-title" role="status" aria-live="polite">批量移入结果：{batchTrashResult.succeeded} 项已处理，{batchTrashResult.failed} 项失败</strong>
                  <small>已处理项目可从回收站恢复；失败项目仍保留在选择中。</small>
                </div>
                <button type="button" aria-label="关闭批量移入结果" onClick={() => setBatchTrashResult(null)}><X size={16} aria-hidden="true" /></button>
              </header>
              <details open={batchTrashResult.failed > 0}>
                <summary>查看 {batchTrashResult.items.length} 项结果</summary>
                <ul>
                  {batchTrashResult.items.map((item) => (
                    <li key={item.path}>
                      <span>/{item.path}</span>
                      <small>{item.status === 'trashed'
                        ? '已移入回收站'
                        : item.status === 'included'
                          ? `已包含于 /${item.includedIn ?? '所选目录'}`
                          : `失败：${item.error || '未能移入回收站'}`}</small>
                    </li>
                  ))}
                </ul>
              </details>
            </section>
          )}

          {batchFileOperationResult && (
            <section className="files-batch-file-operation-report" aria-labelledby="files-batch-file-operation-report-title">
              <header>
                <div>
                  <strong id="files-batch-file-operation-report-title" role="status" aria-live="polite">
                    批量{batchFileOperationResult.mode === 'copy' ? '复制' : '移动'}：{batchFileOperationResult.result.status === 'complete' ? '已完成' : '部分完成'}
                  </strong>
                  <small>{incompleteBatchOperationPaths(batchFileOperationResult.result.items).size > 0 ? '失败、未开始或部分完成的项目仍保留选择，可核对后重试。' : '逐项目标以文件服务返回结果为准。'}</small>
                </div>
                <button type="button" aria-label="关闭批量文件操作结果" onClick={() => setBatchFileOperationResult(null)}><X size={16} aria-hidden="true" /></button>
              </header>
              <details open={batchFileOperationResult.result.status === 'partial'}>
                <summary>查看 {batchFileOperationResult.result.items.length} 项结果</summary>
                <ul>
                  {batchFileOperationResult.result.items.map((item) => (
                    <li key={item.sourcePath}>
                      <span>/{item.sourcePath}</span>
                      <small>{batchFileOperationItemMessage(item)}</small>
                    </li>
                  ))}
                </ul>
              </details>
            </section>
          )}

          {displayLoading ? (
            <div className="files-empty-state" role="status"><strong>正在读取目录</strong><p>正在从服务器获取当前文件夹内容。</p></div>
          ) : loadError ? (
            <div className="files-empty-state" role="alert">
              <span className="files-empty-icon"><HardDrive size={21} aria-hidden="true" /></span>
              <strong>无法读取管理根目录</strong>
              <p>{loadError}</p>
              <button type="button" onClick={() => setRefreshKey((current) => current + 1)}>重试</button>
            </div>
          ) : filterValidationError ? (
            <div className="files-empty-state" role="status">
              <span className="files-empty-icon"><SlidersHorizontal size={21} aria-hidden="true" /></span>
              <strong>请修正筛选条件</strong>
              <p>{filterValidationError}</p>
            </div>
          ) : visibleEntries.length === 0 ? (
            <div className="files-empty-state">
              <span className="files-empty-icon"><Search size={21} aria-hidden="true" /></span>
              <strong>{hiddenEntriesAvailable ? '隐藏项目已收起' : hasListingCriteria ? '没有找到匹配项目' : '这个文件夹目前为空'}</strong>
              <p>{hiddenEntriesAvailable
                ? '开启“显示隐藏文件”即可查看当前结果中的隐藏项目。'
                : queryTerm
                ? `在${effectiveScope === 'recursive' ? '当前文件夹及子文件夹' : '当前文件夹'}中，没有找到名称或路径匹配「${queryTerm}」的项目。`
                : activeFilterCount > 0
                  ? '当前筛选条件下没有可展示的项目。'
                : '此目录没有可展示的文件。'}</p>
              {queryTerm && <button type="button" onClick={() => { setQuery(''); setSearchScope('current'); }}>清除搜索</button>}
              {!queryTerm && activeFilterCount > 0 && <button type="button" onClick={clearFilters}>清除筛选</button>}
            </div>
          ) : viewMode === 'list' ? (
            <div className="files-list-wrap">
              <div className="files-list-header">
                <label className="files-select-all">
                  <input ref={selectAllRef} type="checkbox" checked={allLoadedSelected} onChange={toggleAllVisible} aria-label="全选已加载项目" />
                  <span className="files-visually-hidden">全选已加载</span>
                </label>
                <span>名称</span><span>大小</span><span>修改时间</span><span />
              </div>
              <div className="files-list" role="list" aria-label={`${currentDirectoryLabel}中的文件`}>
                {visibleEntries.map((entry) => {
                  const EntryIcon = iconFor(entry.displayKind);
                  const checked = selectedIds.has(entry.id);
                  const isActive = activeEntry?.id === entry.id;
                  return (
                    <div className={`files-list-row${checked ? ' is-checked' : ''}${isActive ? ' is-current' : ''}`} role="listitem" key={entry.id}>
                      <label className="files-row-check"><input type="checkbox" checked={checked} disabled={entry.mountUnavailable} onChange={() => toggleSelection(entry.id)} aria-label={`选择 ${entry.name}`} /></label>
                      <button className="files-row-main" type="button" onClick={() => activateEntry(entry)} disabled={entry.mountUnavailable}>
                        <span className={`files-entry-icon kind-${entry.displayKind}`}><EntryIcon size={19} strokeWidth={1.8} aria-hidden="true" /></span>
                        <span className="files-entry-copy">
                          <strong>{entry.name}</strong>
                          <small>{entry.displayPath === '/' ? '根目录' : entry.displayPath} <span>·</span> {kindLabel(entry)}</small>
                        </span>
                      </button>
                      <span className="files-row-size">{entry.kind === 'directory' ? '—' : formatBytes(entry.size)}</span>
                      <time className="files-row-modified">{formatDate(entry.modifiedAt)}</time>
                      <button className="files-row-details" type="button" aria-label={`查看 ${entry.name} 详情`} title="查看详情" onClick={() => showDetails(entry)}><Info size={17} aria-hidden="true" /></button>
                    </div>
                  );
                })}
              </div>
            </div>
          ) : (
            <div className="files-grid" aria-label={`${currentDirectoryLabel}中的文件`}>
              {visibleEntries.map((entry) => {
                const EntryIcon = iconFor(entry.displayKind);
                const checked = selectedIds.has(entry.id);
                return (
                  <article className={`files-grid-card${checked ? ' is-checked' : ''}`} key={entry.id}>
                    <label className="files-grid-check"><input type="checkbox" checked={checked} disabled={entry.mountUnavailable} onChange={() => toggleSelection(entry.id)} aria-label={`选择 ${entry.name}`} /></label>
                    <button className="files-grid-open" type="button" onClick={() => activateEntry(entry)} disabled={entry.mountUnavailable}>
                      <span className={`files-grid-art kind-${entry.displayKind}${entry.displayKind === 'video' ? ' is-video-poster' : ''}`} aria-hidden="true">
                        {entry.displayKind === 'video'
                          ? <VideoPosterThumbnail key={`${entry.path}:${entry.version}`} path={entry.path} version={entry.version} Icon={EntryIcon} />
                          : <EntryIcon size={25} strokeWidth={1.7} />}
                      </span>
                      <strong>{entry.name}</strong>
                      <small>{entry.mountUnavailable ? '磁盘不可用' : `${entry.kind === 'directory' ? '文件夹' : formatBytes(entry.size)} · ${formatDate(entry.modifiedAt)}`}</small>
                      {queryTerm && effectiveScope === 'recursive' && <small className="files-grid-path">{entry.displayPath}</small>}
                    </button>
                    <button className="files-grid-info" type="button" aria-label={`查看 ${entry.name} 详情`} title="查看详情" onClick={() => showDetails(entry)}><Info size={16} aria-hidden="true" /></button>
                  </article>
                );
              })}
            </div>
          )}

          {pageHasMore && !loadError && (
            <div className="files-load-more-area">
              {loadMoreError && <p className="files-load-more-error" role="alert">{loadMoreError}</p>}
              <button className="files-load-more" type="button" onClick={() => { void loadMore(); }} disabled={loadingMore}>
                {loadingMore ? '正在加载…' : loadMoreError ? '重试加载' : '加载更多'}
              </button>
            </div>
          )}

          <button className="files-agent-link" type="button" onClick={() => onOpenAgent(
              selectedEntries.length > 0 && !selectionHasMissingEntries && !selectionHasUnavailableEntries
              ? { kind: 'selection', paths: selectedEntries.map((entry) => entry.path) }
              : { kind: 'directory', path: relativePath },
          )}>
            <span className="files-agent-symbol"><Sparkles size={17} aria-hidden="true" /></span>
            <span><strong>让 Agent 帮我整理</strong><small>描述目标，在对话中审核方案</small></span>
            <ChevronRight size={17} aria-hidden="true" />
          </button>
        </div>

        {activeEntry && (() => {
          const entry = activeEntry;
          const DetailIcon = iconFor(entry.displayKind);
          const detailNotice = entry.mountUnavailable
            ? '磁盘不可用，无法进入。'
            : entry.readOnly ? '此项目只读，不能执行文件变更。'
              : entry.symlink ? linkStatusDescription(entry.linkStatus) : '';
          return (
            <aside className={`files-detail-panel${detailOpen ? ' is-open' : ''}`} aria-label="文件详情">
              <div className="files-detail-header">
                <strong>文件详情</strong>
                <button type="button" className="files-detail-close" aria-label="关闭详情" onClick={() => setDetailOpen(false)}><X size={17} /></button>
              </div>
              <div className="files-detail-content">
                <div className={`files-detail-art kind-${entry.displayKind}`}><DetailIcon size={31} strokeWidth={1.6} aria-hidden="true" /></div>
                <span className="files-detail-kind">{kindLabel(entry)}</span>
                <h2>{entry.name}</h2>
                {detailNotice && <p className="files-detail-description">{detailNotice}</p>}
                <dl className="files-detail-metadata">
                  <div><dt>所在位置</dt><dd>{entry.displayPath === '/' ? '根目录' : entry.displayPath}</dd></div>
                  <div><dt>大小</dt><dd>{entry.kind === 'directory' ? '—' : formatBytes(entry.size)}</dd></div>
                  <div><dt>修改时间</dt><dd>{formatDate(entry.modifiedAt)}</dd></div>
                </dl>
                <details className="files-detail-advanced">
                  <summary>权限与属主</summary>
                  <dl className="files-detail-metadata">
                    <div><dt>权限模式</dt><dd>{formatPermissionMode(entry.mode)}</dd></div>
                    <div><dt>属主 UID</dt><dd>{formatOwnerId(entry.ownerUid, entry.ownershipAvailable)}</dd></div>
                    <div><dt>属组 GID</dt><dd>{formatOwnerId(entry.groupGid, entry.ownershipAvailable)}</dd></div>
                  </dl>
                </details>
                {!entry.mountUnavailable && (
                  <>
                    {entry.kind === 'file' && (
                      <a className="files-detail-download" href={fileDownloadURL(entry.path)} download={entry.name}>
                        <Download size={15} aria-hidden="true" />下载原件
                      </a>
                    )}
                    {(entry.kind === 'file' || entry.kind === 'directory') && !entry.readOnly && (
                      <button className="files-detail-open-folder" type="button" onClick={() => setSymlinkTarget(entry)}>
                        <Link2 size={16} aria-hidden="true" />创建软链接
                      </button>
                    )}
                    {!entry.readOnly && canEditFileMetadata(entry) && (
                      <div className="files-detail-metadata-actions">
                        <button className="files-detail-open-folder" type="button" onClick={() => setMetadataEditMode('mode')} disabled={!entry.version}>
                          <Shield size={16} aria-hidden="true" />修改权限
                        </button>
                        <button
                          className="files-detail-open-folder"
                          type="button"
                          onClick={() => setMetadataEditMode('owner')}
                          disabled={!entry.version || entry.ownershipAvailable !== true}
                          title={entry.ownershipAvailable === true ? undefined : '服务器未提供属主信息'}
                        >
                          <Users size={16} aria-hidden="true" />修改属主
                        </button>
                        {entry.ownershipAvailable !== true && <p className="files-detail-ownership-note">服务器未提供属主信息，暂不能修改。</p>}
                      </div>
                    )}
                    <button className="files-detail-rename" type="button" onClick={() => openRenameDialog(entry)} disabled={!entry.version || entry.readOnly}>
                      <Pencil size={15} aria-hidden="true" />重命名
                    </button>
                    <button className="files-detail-trash" type="button" onClick={() => openTrashDialog(entry)} disabled={!canMoveEntryToTrash(entry)} title={entry.kind === 'other' && !entry.symlink ? '特殊文件不能通过此接口删除' : undefined}>
                      <Trash2 size={15} aria-hidden="true" />移入回收站
                    </button>
                    {entry.kind === 'directory' && (
                      <button className="files-detail-open-folder" type="button" onClick={() => activateEntry(entry)} disabled={entry.mountUnavailable}><FolderOpen size={16} />打开此文件夹</button>
                    )}
                  </>
                )}
              </div>
            </aside>
          );
        })()}
      </div>

      {directoryDrawerOpen && (
        <div className="files-directory-drawer-layer">
          <button
            className="files-directory-drawer-backdrop"
            type="button"
            aria-label="关闭目录导航"
            onClick={() => setDirectoryDrawerOpen(false)}
          />
          <aside
            ref={directoryDrawerRef}
            id="files-directory-drawer"
            className="files-directory-drawer"
            role="dialog"
            aria-modal="true"
            aria-labelledby="files-directory-drawer-title"
            tabIndex={-1}
          >
            <header className="files-directory-drawer-header">
              <div>
                <p>目录导航</p>
                <h2 id="files-directory-drawer-title">{currentDirectoryLabel}</h2>
              </div>
              <button
                ref={directoryDrawerCloseRef}
                className="files-directory-drawer-close"
                type="button"
                aria-label="关闭目录导航"
                onClick={() => setDirectoryDrawerOpen(false)}
              ><X size={18} aria-hidden="true" /></button>
            </header>
            <nav className="files-directory-drawer-nav" aria-label="管理根目录与当前位置">
              <section className="files-directory-drawer-section" aria-labelledby="files-directory-drawer-location">
                <h3 id="files-directory-drawer-location">当前位置</h3>
                <button
                  className="files-directory-drawer-item"
                  type="button"
                  aria-current={path.length === 0 ? 'location' : undefined}
                  onClick={() => navigate([])}
                >
                  <HardDrive size={17} aria-hidden="true" />
                  <span>全部文件</span>
                </button>
                {crumbEntries.map((crumb) => (
                  <button
                    key={crumb.path.join('/')}
                    className="files-directory-drawer-item"
                    type="button"
                    aria-current={crumb.path.length === path.length ? 'location' : undefined}
                    onClick={() => navigate(crumb.path)}
                  >
                    <Folder size={17} aria-hidden="true" />
                    <span>{crumb.name}</span>
                  </button>
                ))}
              </section>
              <section className="files-directory-drawer-section" aria-labelledby="files-directory-drawer-children">
                <h3 id="files-directory-drawer-children">当前目录的子文件夹</h3>
                {displayLoading ? (
                  <p className="files-directory-drawer-message" role="status">正在读取当前目录…</p>
                ) : loadError ? (
                  <p className="files-directory-drawer-message" role="alert">无法读取当前目录：{loadError}</p>
                ) : directChildFolders.length > 0 ? (
                  <div className="files-directory-drawer-list">
                    {directChildFolders.map((entry) => (
                      <button
                        key={entry.path}
                        className="files-directory-drawer-item"
                        type="button"
                        disabled={entry.mountUnavailable}
                        onClick={() => navigate(entry.path.split('/').filter(Boolean))}
                      >
                        <Folder size={17} aria-hidden="true" />
                        <span>{entry.mountUnavailable ? `${entry.name} · 磁盘不可用` : entry.name}</span>
                      </button>
                    ))}
                  </div>
                ) : (
                  <p className="files-directory-drawer-message" role="status">
                    {hasListingCriteria
                      ? '当前搜索或筛选结果中没有直接子文件夹。'
                      : '当前目录没有已加载的直接子文件夹。'}
                    {hiddenEntriesAvailable && ' 可开启显示隐藏文件查看隐藏文件夹。'}
                  </p>
                )}
                {pageHasMore && !loadError && (
                  <button
                    className="files-directory-drawer-more"
                    type="button"
                    onClick={() => { void loadMore(); }}
                    disabled={loadingMore}
                  >
                    {loadingMore ? '正在加载…' : '加载更多项目'}
                  </button>
                )}
                {loadMoreError && <p className="files-directory-drawer-message" role="alert">{loadMoreError}</p>}
              </section>
            </nav>
          </aside>
        </div>
      )}

      {detailOpen && <button className="files-detail-backdrop" type="button" aria-label="关闭详情" onClick={() => setDetailOpen(false)} />}
      {metadataEditMode && activeEntry && (
        <FileMetadataDialog
          key={`${metadataEditMode}:${activeEntry.path}`}
          entry={activeEntry}
          mode={metadataEditMode}
          onClose={() => setMetadataEditMode(null)}
          onComplete={completeMetadataChange}
          onConflict={() => setRefreshKey((current) => current + 1)}
        />
      )}
      {readerTarget && <ReaderPanel key={`${readerTarget.path}:${readerTarget.kind}`} {...readerTarget} onClose={() => setReaderTarget(null)} onFileCreated={() => setRefreshKey((current) => current + 1)} />}
      {symlinkTarget && <CreateSymlinkDialog
        targetPath={symlinkTarget.path}
        targetName={symlinkTarget.name}
        parentPath={relativePath}
        onClose={() => setSymlinkTarget(null)}
        onConflict={() => setRefreshKey((current) => current + 1)}
        onComplete={(result) => {
          setSymlinkTarget(null);
          setNotice(`已创建软链接：/${result.path}`);
          setRefreshKey((current) => current + 1);
        }}
      />}
      {archiveTarget && <ArchiveReader key={`${archiveTarget.path}:${archiveTarget.version}`} {...archiveTarget} onClose={() => setArchiveTarget(null)} onExtracted={() => setRefreshKey((current) => current + 1)} />}
      <UploadPanel
        open={uploadOpen}
        targetPath={relativePath}
        onClose={() => setUploadOpen(false)}
        onPublished={() => setRefreshKey((current) => current + 1)}
      />
      {fileOperation && (
        <FileOperationDialog
          mode={fileOperation.mode}
          source={fileOperation.source}
          onClose={() => setFileOperation(null)}
          onComplete={completeFileOperation}
        />
      )}
      {batchFileOperation && (
        <BatchFileOperationDialog
          key={`${batchFileOperation}:${selectedEntries.map((entry) => entry.path).join('|')}`}
          mode={batchFileOperation}
          items={selectedEntries}
          onClose={() => setBatchFileOperation(null)}
          onComplete={(result, previousGeneration) => completeBatchFileOperation(batchFileOperation, result, previousGeneration)}
          onConflict={handleBatchFileOperationConflict}
        />
      )}
      {createArchiveOpen && (
        <CreateArchiveDialog
          key={selectedEntries.map((entry) => entry.path).join('|')}
          items={selectedEntries}
          onClose={() => setCreateArchiveOpen(false)}
          onComplete={completeCreateArchive}
          onConflict={handleCreateArchiveConflict}
        />
      )}
      {folderDialogOpen && (
        <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) closeFolderDialog(); }}>
          <section className="files-name-dialog" role="dialog" aria-modal="true" aria-labelledby="files-folder-title" aria-describedby="files-folder-description" tabIndex={-1}>
            <header className="files-name-dialog-header">
              <div>
                <span className="files-name-dialog-icon"><FolderPlus size={18} aria-hidden="true" /></span>
                <h2 id="files-folder-title">新建文件夹</h2>
              </div>
              <button type="button" className="files-name-dialog-close" aria-label="关闭新建文件夹对话框" onClick={closeFolderDialog} disabled={folderSubmitting}><X size={18} aria-hidden="true" /></button>
            </header>
            <p className="files-name-dialog-description" id="files-folder-description">将在当前位置创建一个真实文件夹。</p>
            <form className="files-name-dialog-form" onSubmit={(event) => { void submitCreateFolder(event); }}>
              <label htmlFor="files-folder-name">文件夹名称</label>
              <input
                ref={folderNameRef}
                id="files-folder-name"
                name="name"
                type="text"
                autoComplete="off"
                maxLength={255}
                value={folderName}
                onChange={(event) => { setFolderName(event.currentTarget.value); setFolderError(''); }}
                placeholder="例如：旅行照片"
                disabled={folderSubmitting}
                aria-invalid={Boolean(folderError)}
                aria-describedby={folderError ? 'files-folder-error' : undefined}
              />
              {folderError && <p className="files-name-dialog-error" id="files-folder-error" role="alert">{folderError}</p>}
              <div className="files-name-dialog-actions">
                <button type="button" className="files-name-dialog-cancel" onClick={closeFolderDialog} disabled={folderSubmitting}>取消</button>
                <button type="submit" className="files-name-dialog-submit" disabled={folderSubmitting || !folderName.trim()}>
                  {folderSubmitting ? '正在创建…' : '创建文件夹'}
                </button>
              </div>
            </form>
          </section>
        </div>
      )}
      {renameTarget && (
        <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) closeRenameDialog(); }}>
          <section className="files-name-dialog" role="dialog" aria-modal="true" aria-labelledby="files-rename-title" aria-describedby="files-rename-description" tabIndex={-1}>
            <header className="files-name-dialog-header">
              <div>
                <span className="files-name-dialog-icon"><Pencil size={17} aria-hidden="true" /></span>
                <h2 id="files-rename-title">重命名</h2>
              </div>
              <button type="button" className="files-name-dialog-close" aria-label="关闭重命名对话框" onClick={closeRenameDialog} disabled={renameSubmitting}><X size={18} aria-hidden="true" /></button>
            </header>
            <p className="files-name-dialog-description" id="files-rename-description">重命名单个文件或文件夹。若目标名称已存在，不会覆盖现有项目。</p>
            <form className="files-name-dialog-form" onSubmit={(event) => { void submitRename(event); }}>
              <label htmlFor="files-rename-name">新名称</label>
              <input
                ref={renameNameRef}
                id="files-rename-name"
                name="name"
                type="text"
                autoComplete="off"
                maxLength={255}
                value={renameName}
                onChange={(event) => { setRenameName(event.currentTarget.value); setRenameError(''); }}
                disabled={renameSubmitting}
                aria-invalid={Boolean(renameError)}
                aria-describedby={renameError ? 'files-rename-error' : undefined}
              />
              {renameError && <p className="files-name-dialog-error" id="files-rename-error" role="alert">{renameError}</p>}
              <div className="files-name-dialog-actions">
                <button type="button" className="files-name-dialog-cancel" onClick={closeRenameDialog} disabled={renameSubmitting}>取消</button>
                <button type="submit" className="files-name-dialog-submit" disabled={renameSubmitting || !renameName.trim() || renameName.trim() === renameTarget.name}>
                  {renameSubmitting ? '正在重命名…' : '保存名称'}
                </button>
              </div>
            </form>
          </section>
        </div>
      )}
      {trashTarget && (
        <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) closeTrashDialog(); }}>
          <section className="files-name-dialog" role="alertdialog" aria-modal="true" aria-labelledby="files-trash-title" aria-describedby="files-trash-description" tabIndex={-1}>
            <header className="files-name-dialog-header">
              <div>
                <span className="files-name-dialog-icon is-danger"><Trash2 size={17} aria-hidden="true" /></span>
                <h2 id="files-trash-title">移入回收站</h2>
              </div>
              <button type="button" className="files-name-dialog-close" aria-label="关闭移入回收站对话框" onClick={closeTrashDialog} disabled={trashSubmitting}><X size={18} aria-hidden="true" /></button>
            </header>
            <p className="files-name-dialog-description" id="files-trash-description">将「{trashTarget.name}」移入回收站。此操作可恢复，不会立即永久删除。</p>
            <p className="files-trash-source">原路径：/{trashTarget.path}</p>
            {trashError && <p className="files-name-dialog-error" role="alert">{trashError}</p>}
            <div className="files-name-dialog-actions">
              <button ref={trashCancelRef} type="button" className="files-name-dialog-cancel" onClick={closeTrashDialog} disabled={trashSubmitting}>取消</button>
              <button type="button" className="files-name-dialog-delete" onClick={() => { void submitMoveToTrash(); }} disabled={trashSubmitting || !trashTarget.version}>
                {trashSubmitting ? '正在移入…' : '移入回收站'}
              </button>
            </div>
          </section>
        </div>
      )}
      {batchTrashOpen && (
        <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) closeBatchTrashDialog(); }}>
          <section className="files-name-dialog" role="alertdialog" aria-modal="true" aria-labelledby="files-batch-trash-title" aria-describedby="files-batch-trash-description" tabIndex={-1} aria-busy={batchTrashSubmitting}>
            <header className="files-name-dialog-header">
              <div>
                <span className="files-name-dialog-icon is-danger"><Trash2 size={17} aria-hidden="true" /></span>
                <h2 id="files-batch-trash-title">批量移入回收站</h2>
              </div>
              <button type="button" className="files-name-dialog-close" aria-label="关闭批量移入回收站对话框" onClick={closeBatchTrashDialog} disabled={batchTrashSubmitting}><X size={18} aria-hidden="true" /></button>
            </header>
            <p className="files-name-dialog-description" id="files-batch-trash-description">
              将 {selectedEntries.length} 个已选项目移入可恢复回收站。父目录和子项同时选择时，服务端会合并处理并说明包含关系。此操作不会永久删除。
            </p>
            {batchTrashError && <p className="files-name-dialog-error" role="alert">{batchTrashError}</p>}
            <div className="files-name-dialog-actions">
              <button ref={batchTrashCancelRef} type="button" className="files-name-dialog-cancel" onClick={closeBatchTrashDialog} disabled={batchTrashSubmitting}>取消</button>
              <button type="button" className="files-name-dialog-delete" onClick={() => { void submitBatchMoveToTrash(); }} disabled={!canSubmitBatchTrash || batchTrashSubmitting}>
                {batchTrashSubmitting ? '正在处理…' : `移入 ${selectedEntries.length} 项`}
              </button>
            </div>
          </section>
        </div>
      )}
      {batchRenameOpen && (
        <BatchRenameDialog
          items={selectedEntries.map(({ path: entryPath, name, version }) => ({ path: entryPath, name, version }))}
          onClose={closeBatchRenameDialog}
          onConflict={handleBatchRenameConflict}
          onComplete={handleBatchRenameComplete}
        />
      )}
    </section>
  );
}
