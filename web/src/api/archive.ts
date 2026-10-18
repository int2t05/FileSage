// 提供版本保护的压缩包浏览、读取与解压请求。
export type ArchiveEntryKind = 'file' | 'directory' | 'unsupported';

export interface ArchiveEntry {
  name: string;
  path: string;
  kind: ArchiveEntryKind;
  size: number;
  modifiedAt: string;
}

export interface ArchiveListing {
  archivePath: string;
  version: string;
  directory: string;
  entries: ArchiveEntry[];
}

export interface ArchiveExtractResult {
  status: 'complete' | 'partial';
  paths: string[];
  errors: Array<{ entry: string; message: string }>;
}

export class ArchiveApiError extends Error {
  readonly status: number | undefined;

  constructor(message: string, status?: number) {
    super(message);
    this.name = 'ArchiveApiError';
    this.status = status;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isArchiveEntry(value: unknown): value is ArchiveEntry {
  if (!isRecord(value)) return false;
  return typeof value.name === 'string'
    && value.name.length > 0
    && typeof value.path === 'string'
    && value.path.length > 0
    && (value.kind === 'file' || value.kind === 'directory' || value.kind === 'unsupported')
    && typeof value.size === 'number'
    && Number.isSafeInteger(value.size)
    && value.size >= 0
    && typeof value.modifiedAt === 'string';
}

async function readError(response: Response, action: string): Promise<ArchiveApiError> {
  const payload = await response.json().catch(() => undefined) as unknown;
  const message = isRecord(payload) && typeof payload.error === 'string'
    ? payload.error
    : `${action}失败（HTTP ${response.status}）`;
  return new ArchiveApiError(message, response.status);
}

function throwConnectionError(error: unknown): never {
  if (error instanceof DOMException && error.name === 'AbortError') throw error;
  throw new ArchiveApiError('无法连接 FileSage 服务。请确认后端已启动。');
}

export function archiveEntriesURL(path: string, version: string, directory = ''): string {
  const query = new URLSearchParams({ path, version, directory });
  return `/api/v1/files/archive/entries?${query.toString()}`;
}

export function archiveContentURL(path: string, version: string, entry: string): string {
  const query = new URLSearchParams({ path, version, entry });
  return `/api/v1/files/archive/content?${query.toString()}`;
}

export async function listArchiveEntries(
  path: string,
  version: string,
  directory = '',
  signal?: AbortSignal,
): Promise<ArchiveListing> {
  // 版本令牌确保压缩包变化后旧目录不会被继续使用。
  let response: Response;
  try {
    response = await fetch(archiveEntriesURL(path, version, directory), { signal, cache: 'no-store' });
  } catch (error) {
    if (signal?.aborted || error instanceof DOMException && error.name === 'AbortError') throw error;
    throwConnectionError(error);
  }
  if (!response.ok) throw await readError(response, '读取压缩包目录');

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!isRecord(payload)
    || payload.archivePath !== path
    || payload.version !== version
    || payload.directory !== directory
    || !Array.isArray(payload.entries)
    || !payload.entries.every(isArchiveEntry)) {
    throw new ArchiveApiError('文件服务返回了无效的压缩包目录。');
  }

  return payload as unknown as ArchiveListing;
}

function isArchiveExtractResult(value: unknown): value is ArchiveExtractResult {
  return isRecord(value)
    && (value.status === 'complete' || value.status === 'partial')
    && Array.isArray(value.paths)
    && value.paths.every((path) => typeof path === 'string')
    && Array.isArray(value.errors)
    && value.errors.every((error) => isRecord(error)
      && typeof error.entry === 'string'
      && typeof error.message === 'string');
}

export async function extractArchive(
  path: string,
  version: string,
  destination: string,
  entries: string[],
  signal?: AbortSignal,
): Promise<ArchiveExtractResult> {
  // 保留完整与部分结果，避免把部分解压误报为成功。
  let response: Response;
  try {
    response = await fetch('/api/v1/files/archive/extract', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, version, destination, entries }),
      signal,
      cache: 'no-store',
    });
  } catch (error) {
    if (signal?.aborted || error instanceof DOMException && error.name === 'AbortError') throw error;
    throwConnectionError(error);
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    if (response.status !== 207) throw new ArchiveApiError(
      isRecord(payload) && typeof payload.error === 'string' ? payload.error : `解压失败（HTTP ${response.status}）`,
      response.status,
    );
  }
  if (!isArchiveExtractResult(payload)
    || response.status === 207 && payload.status !== 'partial'
    || response.status !== 207 && payload.status !== 'complete') {
    throw new ArchiveApiError('文件服务返回了无效的解压结果。', response.status);
  }
  return payload;
}
