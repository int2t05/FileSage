export type FileKind = 'directory' | 'file' | 'other';
export type FileLinkStatus = 'internal' | 'dangling' | 'outside_root' | 'cycle' | 'unavailable';
export type ArchiveFormat = 'zip' | '7z' | 'tar' | 'tar.gz';

const archiveExtensions: Record<ArchiveFormat, string> = {
  zip: '.zip',
  '7z': '.7z',
  tar: '.tar',
  'tar.gz': '.tar.gz',
};

// 统一归档格式扩展名，避免切换格式时叠加旧后缀。
export function archiveFileName(value: string, format: ArchiveFormat): string {
  const baseName = value.trim().replace(/(?:\.tar\.gz|\.tar|\.7z|\.zip)+$/i, '');
  return `${baseName}${archiveExtensions[format]}`;
}

function isArchiveNameForFormat(name: string, format: ArchiveFormat): boolean {
  return name.trim().toLowerCase().endsWith(archiveExtensions[format]);
}
export type FileSortMode = 'name-asc' | 'name-desc' | 'size-asc' | 'size-desc' | 'modified-asc' | 'modified-desc';
export type PhotoSortMode = 'captured-asc' | 'captured-desc';
export type FileListingSortMode = FileSortMode | PhotoSortMode;
export type FileSearchScope = 'current' | 'recursive';
export type FileFilterType = 'image' | 'video' | 'audio' | 'document' | 'archive' | 'directory' | 'other';

export interface FileFilters {
  type?: FileFilterType;
  minSize?: number;
  maxSize?: number;
  modifiedFrom?: string;
  modifiedBefore?: string;
  capturedFrom?: string;
  capturedBefore?: string;
}

export interface FileEntry {
  name: string;
  path: string;
  kind: FileKind;
  size: number;
  modifiedAt: string;
  capturedAt?: string;
  mode: number;
  ownerUid?: number;
  groupGid?: number;
  ownershipAvailable?: boolean;
  symlink: boolean;
  linkStatus?: FileLinkStatus;
  mountUnavailable?: boolean;
  readOnly?: boolean;
  version: string;
}

export type FileMetadataChange =
  | { path: string; version: string; mode: number; ownerUid?: never; groupGid?: never }
  | { path: string; version: string; mode?: never; ownerUid?: number; groupGid?: number };

export interface FileListing {
  path: string;
  entries: FileEntry[];
  limit: number;
  hasMore: boolean;
  nextCursor: string;
  indexState: string;
}

export interface FileIndexStatus {
  activeGeneration: number;
  buildingGeneration: number;
  state: string;
}

export interface FileListOptions {
  limit: number;
  cursor: string;
  query: string;
  scope: FileSearchScope;
  sort: FileListingSortMode;
  filters?: FileFilters;
}

// 读取索引代次，供写操作完成后等待目录列表校准。
export async function getFileIndexStatus(signal?: AbortSignal): Promise<FileIndexStatus> {
  let response: Response;
  try {
    response = await fetch('/api/v1/index/status', { signal });
  } catch (error) {
    if (signal?.aborted || (error instanceof DOMException && error.name === 'AbortError')) throw error;
    throw new ApiError('无法读取文件索引状态。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : `读取文件索引状态失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)
    || !('activeGeneration' in payload) || typeof payload.activeGeneration !== 'number' || !Number.isSafeInteger(payload.activeGeneration) || payload.activeGeneration < 0
    || !('buildingGeneration' in payload) || typeof payload.buildingGeneration !== 'number' || !Number.isSafeInteger(payload.buildingGeneration) || payload.buildingGeneration < 0
    || !('state' in payload) || typeof payload.state !== 'string') {
    throw new ApiError('文件服务返回了无效的索引状态。');
  }

  return {
    activeGeneration: payload.activeGeneration,
    buildingGeneration: payload.buildingGeneration,
    state: payload.state,
  };
}

export interface PathNameResult {
  path: string;
  name: string;
}

export type FileOperationMode = 'copy' | 'move';

export type BatchFileOperationItemStatus = 'copied' | 'moved' | 'included' | 'partial' | 'failed' | 'not_started';

export interface BatchFileOperationItemResult {
  sourcePath: string;
  targetPath: string;
  status: BatchFileOperationItemStatus;
  includedIn?: string;
  message?: string;
}

export interface BatchFileOperationResult {
  status: 'complete' | 'partial';
  items: BatchFileOperationItemResult[];
}

export interface PartialMoveResult extends PathNameResult {
  status: 'partial';
  message: string;
}

export type FileOperationResult = PathNameResult | PartialMoveResult;

export interface ArchiveSelectionItem {
  path: string;
  version: string;
}

export type BatchTrashItemStatus = 'trashed' | 'included' | 'failed';

export interface BatchTrashItemResult {
  path: string;
  status: BatchTrashItemStatus;
  includedIn?: string;
  error?: string;
}

export interface BatchTrashResult {
  items: BatchTrashItemResult[];
  succeeded: number;
  failed: number;
}

export interface BatchRenameItemInput extends ArchiveSelectionItem {
  name: string;
}

export type BatchRenameItemStatus = 'renamed' | 'failed' | 'not_started';

export interface BatchRenameItemResult {
  path: string;
  targetPath: string;
  name: string;
  status: BatchRenameItemStatus;
  message?: string;
}

export interface BatchRenameResult {
  status: 'completed' | 'partial';
  items: BatchRenameItemResult[];
}

// 返回通过原件接口下载文件的地址，保留服务端的断点传输能力。
export function fileDownloadURL(path: string): string {
  return `/api/v1/files/content?${new URLSearchParams({ path }).toString()}`;
}

export class ApiError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
    this.name = 'ApiError';
  }
}

// 使用原生表单下载归档，避免把大文件读入前端内存。
export function downloadArchive(selection: ArchiveSelectionItem[]): void {
  if (selection.length === 0) throw new ApiError('至少选择一个文件或文件夹。');

  const form = document.createElement('form');
  form.method = 'POST';
  form.action = '/api/v1/files/archive';
  form.target = '_blank';
  form.enctype = 'application/x-www-form-urlencoded';
  form.hidden = true;

  const input = document.createElement('input');
  input.type = 'hidden';
  input.name = 'selection';
  input.value = JSON.stringify(selection);
  form.append(input);
  document.body.append(form);
  form.submit();
  form.remove();
}

// 按版本令牌创建归档，并返回服务端分配的最终名称。
export async function createArchive(
  items: ArchiveSelectionItem[],
  destination: string,
  destinationVersion: string,
  name: string,
  format: ArchiveFormat = 'zip',
): Promise<PathNameResult> {
  if (items.length < 1) throw new ApiError('至少选择一个文件或文件夹。');
  if (items.length > 256) throw new ApiError('一次最多压缩 256 个项目。');
  if (new Set(items.map((item) => item.path)).size !== items.length
    || items.some((item) => !item.path || !item.version)) {
    throw new ApiError('压缩项目缺少有效路径或版本。');
  }
  if ((destination === '' && destinationVersion !== '') || (destination !== '' && !destinationVersion)) {
    throw new ApiError('目标文件夹版本无效，请重新选择。');
  }
  if (!['zip', '7z', 'tar', 'tar.gz'].includes(format)
    || !name.trim() || !isArchiveNameForFormat(name, format) || /[\\/]/.test(name.trim())) {
    throw new ApiError('请输入与归档格式匹配的有效文件名。');
  }

  let response: Response;
  try {
    response = await fetch('/api/v1/files/archive/create', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items, destination, destinationVersion, name: name.trim(), format }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (response.status !== 201) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `创建压缩包失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)
    || !('path' in payload) || typeof payload.path !== 'string' || !payload.path
    || !('name' in payload) || typeof payload.name !== 'string' || !isArchiveNameForFormat(payload.name, format)) {
    throw new ApiError('文件服务返回了无效的压缩包创建结果。', response.status);
  }

  return { path: payload.path, name: payload.name };
}

// 批量移入回收站并校验逐项结果。
export async function batchMoveFilesToTrash(items: ArchiveSelectionItem[]): Promise<BatchTrashResult> {
  if (items.length === 0) throw new ApiError('至少选择一个文件或文件夹。');
  if (items.length > 256) throw new ApiError('一次最多处理 256 个项目。');

  let response: Response;
  try {
    response = await fetch('/api/v1/files/batch-trash', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (response.status !== 200 && response.status !== 207) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `批量移入回收站失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || !('items' in payload) || !Array.isArray(payload.items)
    || !('succeeded' in payload) || !Number.isSafeInteger(payload.succeeded) || (payload.succeeded as number) < 0
    || !('failed' in payload) || !Number.isSafeInteger(payload.failed) || (payload.failed as number) < 0
    || payload.items.length !== items.length) {
    throw new ApiError('文件服务返回了无效的批量移入结果。');
  }

  const requestedPaths = new Set(items.map((item) => item.path));
  const seenPaths = new Set<string>();
  const results: BatchTrashItemResult[] = [];
  for (const value of payload.items) {
    if (typeof value !== 'object' || value === null || !('path' in value) || typeof value.path !== 'string'
      || !requestedPaths.has(value.path) || seenPaths.has(value.path)
      || !('status' in value) || (value.status !== 'trashed' && value.status !== 'included' && value.status !== 'failed')
      || ('includedIn' in value && typeof value.includedIn !== 'string')
      || ('error' in value && typeof value.error !== 'string')) {
      throw new ApiError('文件服务返回了无效的批量移入结果。');
    }
    seenPaths.add(value.path);
    results.push({
      path: value.path,
      status: value.status,
      ...('includedIn' in value ? { includedIn: value.includedIn } : {}),
      ...('error' in value ? { error: value.error } : {}),
    });
  }

  const succeeded = results.filter((item) => item.status !== 'failed').length;
  const failed = results.length - succeeded;
  if (payload.succeeded !== succeeded || payload.failed !== failed
    || (response.status === 200 && failed !== 0)
    || (response.status === 207 && failed === 0)) {
    throw new ApiError('文件服务返回的批量移入结果不一致。');
  }

  return { items: results, succeeded, failed };
}

// 批量重命名并校验服务端逐项结果。
export async function batchRenameEntries(items: BatchRenameItemInput[]): Promise<BatchRenameResult> {
  if (items.length < 2) throw new ApiError('至少选择两个项目才能批量重命名。');
  if (items.length > 256) throw new ApiError('一次最多重命名 256 个项目。');
  if (new Set(items.map((item) => item.path)).size !== items.length) throw new ApiError('重命名列表中包含重复路径。');

  let response: Response;
  try {
    response = await fetch('/api/v1/files/batch-rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (response.status !== 200 && response.status !== 207) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `批量重命名失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)
    || !('status' in payload) || (payload.status !== 'completed' && payload.status !== 'partial')
    || !('items' in payload) || !Array.isArray(payload.items) || payload.items.length !== items.length) {
    throw new ApiError('文件服务返回了无效的批量重命名结果。', response.status);
  }

  const requestedPaths = new Set(items.map((item) => item.path));
  const seenPaths = new Set<string>();
  const results: BatchRenameItemResult[] = [];
  for (const value of payload.items) {
    if (typeof value !== 'object' || value === null || Array.isArray(value)
      || !('path' in value) || typeof value.path !== 'string' || !requestedPaths.has(value.path) || seenPaths.has(value.path)
      || !('targetPath' in value) || typeof value.targetPath !== 'string'
      || !('name' in value) || typeof value.name !== 'string'
      || !('status' in value) || (value.status !== 'renamed' && value.status !== 'failed' && value.status !== 'not_started')
      || ('message' in value && typeof value.message !== 'string')) {
      throw new ApiError('文件服务返回了无效的批量重命名结果。', response.status);
    }
    seenPaths.add(value.path);
    results.push({
      path: value.path,
      targetPath: value.targetPath,
      name: value.name,
      status: value.status,
      ...('message' in value ? { message: value.message } : {}),
    });
  }

  const hasFailures = results.some((item) => item.status !== 'renamed');
  if ((response.status === 200 && (payload.status !== 'completed' || hasFailures))
    || (response.status === 207 && (payload.status !== 'partial' || !hasFailures))) {
    throw new ApiError('文件服务返回的批量重命名状态不一致。', response.status);
  }

  return { status: payload.status, items: results };
}

function isFileEntry(value: unknown): value is FileEntry {
  if (typeof value !== 'object' || value === null) return false;
  const entry = value as Partial<FileEntry>;
  return typeof entry.name === 'string'
    && typeof entry.path === 'string'
    && (entry.kind === 'directory' || entry.kind === 'file' || entry.kind === 'other')
    && typeof entry.size === 'number'
    && Number.isFinite(entry.size)
    && entry.size >= 0
    && typeof entry.modifiedAt === 'string'
    && (entry.capturedAt === undefined || typeof entry.capturedAt === 'string' && !Number.isNaN(Date.parse(entry.capturedAt)))
    && typeof entry.mode === 'number'
    && Number.isSafeInteger(entry.mode)
    && (entry.ownerUid === undefined || typeof entry.ownerUid === 'number' && Number.isSafeInteger(entry.ownerUid) && entry.ownerUid >= 0)
    && (entry.groupGid === undefined || typeof entry.groupGid === 'number' && Number.isSafeInteger(entry.groupGid) && entry.groupGid >= 0)
    && (entry.ownershipAvailable === undefined || typeof entry.ownershipAvailable === 'boolean')
    && typeof entry.symlink === 'boolean'
    && (entry.linkStatus === undefined || entry.linkStatus === 'internal' || entry.linkStatus === 'dangling'
      || entry.linkStatus === 'outside_root' || entry.linkStatus === 'cycle' || entry.linkStatus === 'unavailable')
    && (entry.mountUnavailable === undefined || typeof entry.mountUnavailable === 'boolean')
    && (entry.readOnly === undefined || typeof entry.readOnly === 'boolean')
    && typeof entry.version === 'string';
}

// 按版本令牌修改权限或属主，并校验服务端返回的最新条目。
export async function updateFileMetadata(change: FileMetadataChange): Promise<FileEntry> {
  const hasMode = change.mode !== undefined;
  const hasOwner = change.ownerUid !== undefined || change.groupGid !== undefined;
  if (!isRootRelativePath(change.path, false) || !change.version || hasMode === hasOwner) {
    throw new ApiError('文件版本或修改内容无效，请刷新后重试。');
  }
  if (hasMode && (!Number.isSafeInteger(change.mode) || change.mode < 0 || change.mode > 0o777)) {
    throw new ApiError('权限必须是 0000 至 0777 的八进制值。');
  }
  if (hasOwner && (change.ownerUid === undefined && change.groupGid === undefined
    || change.ownerUid !== undefined && (!Number.isSafeInteger(change.ownerUid) || change.ownerUid < 0)
    || change.groupGid !== undefined && (!Number.isSafeInteger(change.groupGid) || change.groupGid < 0))) {
    throw new ApiError('UID 和 GID 必须是非负整数，且至少填写一项。');
  }

  let response: Response;
  try {
    response = await fetch('/api/v1/files/metadata', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(change.mode !== undefined
        ? { path: change.path, version: change.version, mode: change.mode }
        : {
          path: change.path,
          version: change.version,
          ...(change.ownerUid === undefined ? {} : { ownerUid: change.ownerUid }),
          ...(change.groupGid === undefined ? {} : { groupGid: change.groupGid }),
        }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (response.status !== 200) {
    const message = typeof payload === 'object' && payload !== null && !Array.isArray(payload)
      && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `修改文件信息失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)
    || !('entry' in payload) || !isFileEntry(payload.entry)
    || payload.entry.path !== change.path || payload.entry.symlink
    || typeof payload.entry.ownershipAvailable !== 'boolean') {
    throw new ApiError('文件服务返回了无效的文件信息。', response.status);
  }
  return payload.entry;
}

// 读取一页目录或搜索结果并校验游标契约。
export async function listDirectory(path: string, options: FileListOptions, signal?: AbortSignal): Promise<FileListing> {
  const params = new URLSearchParams({
    path,
    limit: String(options.limit),
    cursor: options.cursor,
    q: options.query,
    scope: options.scope,
    sort: options.sort,
  });
  for (const [key, value] of Object.entries(options.filters ?? {})) {
    if (value !== undefined && value !== '') params.set(key, String(value));
  }

  let response: Response;
  try {
    response = await fetch(`/api/v1/files?${params.toString()}`, { signal });
  } catch (error) {
    if (signal?.aborted || (error instanceof DOMException && error.name === 'AbortError')) throw error;
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  if (!response.ok) {
    const payload = await response.json().catch(() => undefined) as { error?: string } | undefined;
    const message = payload?.error
      ?? (response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `读取目录失败（HTTP ${response.status}）`);
    throw new ApiError(message, response.status);
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) {
    throw new ApiError('文件服务返回了无效的目录列表。');
  }
  const listing = payload as Partial<FileListing>;
  if (listing.path !== path
    || !Array.isArray(listing.entries)
    || !listing.entries.every(isFileEntry)
    || !Number.isSafeInteger(listing.limit)
    || listing.limit !== options.limit
    || listing.entries.length > options.limit
    || typeof listing.hasMore !== 'boolean'
    || typeof listing.nextCursor !== 'string'
    || listing.hasMore !== (listing.nextCursor.length > 0)
    || (listing.hasMore && listing.entries.length === 0)
    || typeof listing.indexState !== 'string') {
    throw new ApiError('文件服务返回了无效的目录列表或分页游标。');
  }

  return listing as FileListing;
}

// 创建真实目录并返回服务端分配的名称。
export async function createDirectory(parent: string, name: string): Promise<PathNameResult> {
  let response: Response;
  try {
    response = await fetch('/api/v1/files/directories', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parent, name }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `新建文件夹失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || !('path' in payload) || typeof payload.path !== 'string' || !('name' in payload) || typeof payload.name !== 'string') {
    throw new ApiError('文件服务返回了无效的新建结果。');
  }

  return { path: payload.path, name: payload.name };
}

function isRootRelativePath(path: string, allowRoot: boolean): boolean {
  if (path === '') return allowRoot;
  return !path.startsWith('/') && !path.includes('\\') && !/[\u0000-\u001f\u007f]/.test(path)
    && path.split('/').every((segment) => segment !== '' && segment !== '.' && segment !== '..');
}

function isValidSymlinkName(name: string): boolean {
  return Boolean(name) && name === name.trim() && name !== '.' && name !== '..'
    && !name.includes('/') && !name.includes('\\') && !/^[a-zA-Z]:/.test(name)
    && !/[\u0000-\u001f\u007f]/.test(name) && new TextEncoder().encode(name).byteLength <= 255;
}

// 在管理根目录内创建软链接；同名冲突由服务端自动编号。
export async function createSymlink(parent: string, name: string, target: string): Promise<PathNameResult> {
  const normalizedName = name.trim();
  if (!isValidSymlinkName(normalizedName) || normalizedName !== name) {
    throw new ApiError('请输入有效的链接名称。');
  }
  if (!isRootRelativePath(parent, true) || !isRootRelativePath(target, false)) {
    throw new ApiError('链接来源或位置无效，请刷新目录后重试。');
  }

  let response: Response;
  try {
    response = await fetch('/api/v1/files/symlinks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parent, name: normalizedName, target }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (response.status !== 201) {
    const message = typeof payload === 'object' && payload !== null && !Array.isArray(payload)
      && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status === 409 ? '软链接创建冲突，请刷新目录后重试。'
        : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `创建软链接失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)
    || !('path' in payload) || typeof payload.path !== 'string' || !payload.path
    || !('name' in payload) || typeof payload.name !== 'string' || !isValidSymlinkName(payload.name)
    || payload.path !== (parent ? `${parent}/${payload.name}` : payload.name)) {
    throw new ApiError('文件服务返回了无效的软链接创建结果。', response.status);
  }

  return { path: payload.path, name: payload.name };
}

// 按当前版本令牌重命名单个条目。
export async function renameEntry(path: string, version: string, name: string): Promise<PathNameResult> {
  let response: Response;
  try {
    response = await fetch('/api/v1/files/rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, version, name }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `重命名失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || !('path' in payload) || typeof payload.path !== 'string' || !('name' in payload) || typeof payload.name !== 'string') {
    throw new ApiError('文件服务返回了无效的重命名结果。');
  }

  return { path: payload.path, name: payload.name };
}

// 普通删除只移入回收站，不发送永久删除确认参数。
export async function moveFileToTrash(path: string, version: string): Promise<void> {
  const params = new URLSearchParams({ path, version });
  let response: Response;
  try {
    response = await fetch(`/api/v1/files?${params.toString()}`, { method: 'DELETE' });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  if (!response.ok) {
    const payload = await response.json().catch(() => undefined) as { error?: string } | undefined;
    throw new ApiError(payload?.error ?? (response.status >= 500
      ? '文件服务暂不可用。请确认后端已启动。'
      : `移入回收站失败（HTTP ${response.status}）`), response.status);
  }
}

function isPathNameResult(value: unknown): value is PathNameResult {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    && 'path' in value && typeof value.path === 'string' && value.path.length > 0
    && 'name' in value && typeof value.name === 'string' && value.name.length > 0;
}

function isCompletePathNameResult(value: unknown): value is PathNameResult {
  return isPathNameResult(value) && !('status' in value) && !('message' in value);
}

// 按文件与目标目录的版本令牌执行单文件复制或移动。
async function operateFile(mode: 'copy', path: string, version: string, destination: string, destinationVersion: string): Promise<PathNameResult>;
async function operateFile(mode: 'move', path: string, version: string, destination: string, destinationVersion: string): Promise<FileOperationResult>;
async function operateFile(
  mode: FileOperationMode,
  path: string,
  version: string,
  destination: string,
  destinationVersion: string,
): Promise<FileOperationResult> {
  let response: Response;
  try {
    response = await fetch(`/api/v1/files/${mode}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, version, destination, destinationVersion }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  const errorMessage = typeof payload === 'object' && payload !== null && !Array.isArray(payload)
    && 'error' in payload && typeof payload.error === 'string'
    ? payload.error
    : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `${mode === 'copy' ? '复制' : '移动'}文件失败（HTTP ${response.status}）`;

  if (mode === 'move' && response.status === 207) {
    if (!isPathNameResult(payload) || !('status' in payload) || payload.status !== 'partial'
      || !('message' in payload) || typeof payload.message !== 'string' || payload.message.length === 0) {
      throw new ApiError('文件服务返回了无效的部分移动结果。', response.status);
    }
    return { path: payload.path, name: payload.name, status: 'partial', message: payload.message };
  }

  const expectedStatus = mode === 'copy' ? 201 : 200;
  if (response.status !== expectedStatus) throw new ApiError(errorMessage, response.status);
  if (!isCompletePathNameResult(payload)) {
    throw new ApiError(`文件服务返回了无效的${mode === 'copy' ? '复制' : '移动'}结果。`, response.status);
  }
  return { path: payload.path, name: payload.name };
}

// 复制单个文件到已存在的目录，冲突由服务端分配新名称。
export function copyFileToDirectory(path: string, version: string, destination: string, destinationVersion: string): Promise<PathNameResult> {
  return operateFile('copy', path, version, destination, destinationVersion);
}

// 执行文件夹复制或移动，并校验完整与部分完成响应。
async function operateDirectory(mode: 'copy', path: string, version: string, destination: string, destinationVersion: string): Promise<PathNameResult>;
async function operateDirectory(mode: 'move', path: string, version: string, destination: string, destinationVersion: string): Promise<FileOperationResult>;
async function operateDirectory(
  mode: FileOperationMode,
  path: string,
  version: string,
  destination: string,
  destinationVersion: string,
): Promise<FileOperationResult> {
  let response: Response;
  try {
    response = await fetch(`/api/v1/files/${mode}-directory`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, version, destination, destinationVersion }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  const errorMessage = typeof payload === 'object' && payload !== null && !Array.isArray(payload)
    && 'error' in payload && typeof payload.error === 'string'
    ? payload.error
    : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `${mode === 'copy' ? '复制' : '移动'}文件夹失败（HTTP ${response.status}）`;

  if (mode === 'move' && response.status === 207) {
    if (!isPathNameResult(payload) || !('status' in payload) || payload.status !== 'partial'
      || !('message' in payload) || typeof payload.message !== 'string' || payload.message.length === 0) {
      throw new ApiError('文件服务返回了无效的部分移动结果。', response.status);
    }
    return { path: payload.path, name: payload.name, status: 'partial', message: payload.message };
  }

  const expectedStatus = mode === 'copy' ? 201 : 200;
  if (response.status !== expectedStatus) throw new ApiError(errorMessage, response.status);
  if (!isCompletePathNameResult(payload)) {
    throw new ApiError(`文件服务返回了无效的文件夹${mode === 'copy' ? '复制' : '移动'}结果。`, response.status);
  }
  return { path: payload.path, name: payload.name };
}

// 复制文件夹到现有目录；同名冲突由服务端分配新名称。
export function copyDirectoryToDirectory(path: string, version: string, destination: string, destinationVersion: string): Promise<PathNameResult> {
  return operateDirectory('copy', path, version, destination, destinationVersion);
}

// 移动文件夹到现有目录，并保留服务端报告的部分完成状态。
export function moveDirectoryToDirectory(path: string, version: string, destination: string, destinationVersion: string): Promise<FileOperationResult> {
  return operateDirectory('move', path, version, destination, destinationVersion);
}

// 移动单个文件；跨挂载点的部分完成会保留为明确结果。
export function moveFileToDirectory(path: string, version: string, destination: string, destinationVersion: string): Promise<FileOperationResult> {
  return operateFile('move', path, version, destination, destinationVersion);
}

// 校验批量复制与移动的版本和逐项结果。
async function batchOperateFiles(mode: FileOperationMode, items: ArchiveSelectionItem[], destination: string, destinationVersion: string): Promise<BatchFileOperationResult> {
  if (items.length < 2) throw new ApiError('至少选择两个项目才能批量操作。');
  if (items.length > 256) throw new ApiError('一次最多处理 256 个项目。');
  if (new Set(items.map((item) => item.path)).size !== items.length) throw new ApiError('批量操作列表中包含重复路径。');
  if (items.some((item) => !item.path || !item.version)) throw new ApiError('部分项目缺少路径或版本信息，请刷新后重试。');
  if (destination ? !destinationVersion : Boolean(destinationVersion)) throw new ApiError('目标文件夹版本信息无效，请重新选择目标。');

  let response: Response;
  try {
    response = await fetch(`/api/v1/files/batch-${mode}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items, destination, destinationVersion }),
    });
  } catch {
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (response.status !== 200 && response.status !== 207) {
    const message = typeof payload === 'object' && payload !== null && !Array.isArray(payload)
      && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '文件服务暂不可用。请确认后端已启动。' : `批量${mode === 'copy' ? '复制' : '移动'}失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)
    || !('status' in payload) || (payload.status !== 'complete' && payload.status !== 'partial')
    || !('items' in payload) || !Array.isArray(payload.items) || payload.items.length !== items.length) {
    throw new ApiError('文件服务返回了无效的批量操作结果。', response.status);
  }

  const requestedPaths = new Set(items.map((item) => item.path));
  const seenPaths = new Set<string>();
  const results: BatchFileOperationItemResult[] = [];
  for (const value of payload.items) {
    if (typeof value !== 'object' || value === null || Array.isArray(value)
      || !('sourcePath' in value) || typeof value.sourcePath !== 'string' || !requestedPaths.has(value.sourcePath) || seenPaths.has(value.sourcePath)
      || !('targetPath' in value) || typeof value.targetPath !== 'string'
      || !('status' in value) || !['copied', 'moved', 'included', 'partial', 'failed', 'not_started'].includes(value.status as string)
      || ('includedIn' in value && typeof value.includedIn !== 'string')
      || ('message' in value && typeof value.message !== 'string')) {
      throw new ApiError('文件服务返回了无效的批量操作结果。', response.status);
    }
    if ((mode === 'copy' && value.status === 'moved') || (mode === 'move' && value.status === 'copied')) {
      throw new ApiError('文件服务返回了与操作类型不一致的批量结果。', response.status);
    }
    seenPaths.add(value.sourcePath);
    results.push({
      sourcePath: value.sourcePath,
      targetPath: value.targetPath,
      status: value.status as BatchFileOperationItemStatus,
      ...('includedIn' in value ? { includedIn: value.includedIn as string } : {}),
      ...('message' in value ? { message: value.message as string } : {}),
    });
  }

  const hasIncompleteItems = results.some((item) => item.status === 'partial' || item.status === 'failed' || item.status === 'not_started');
  if ((response.status === 200 && (payload.status !== 'complete' || hasIncompleteItems))
    || (response.status === 207 && (payload.status !== 'partial' || !hasIncompleteItems))) {
    throw new ApiError('文件服务返回的批量操作状态不一致。', response.status);
  }

  return { status: payload.status, items: results };
}

// 批量复制已选择的文件和文件夹。
export function batchCopyFilesToDirectory(items: ArchiveSelectionItem[], destination: string, destinationVersion: string): Promise<BatchFileOperationResult> {
  return batchOperateFiles('copy', items, destination, destinationVersion);
}

// 批量移动已选择的文件和文件夹。
export function batchMoveFilesToDirectory(items: ArchiveSelectionItem[], destination: string, destinationVersion: string): Promise<BatchFileOperationResult> {
  return batchOperateFiles('move', items, destination, destinationVersion);
}
