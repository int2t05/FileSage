// 重复文件扫描只返回状态、文件路径和大小，不暴露内容摘要。
export type DuplicateScanStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled';
export type DuplicateScanPhase = 'enumerating' | 'hashing' | 'complete';

export interface DuplicateScan {
  id: string;
  status: DuplicateScanStatus;
  phase: DuplicateScanPhase;
  scopePath: string;
  entriesVisited: number;
  candidateFiles: number;
  filesHashed: number;
  bytesHashed: number;
  totalBytes: number;
  groupCount: number;
  duplicateFileCount: number;
  reclaimableBytes: number;
  errorCode: string;
  updatedAt: string;
}

export interface DuplicateScanGroup {
  id: string;
  size: number;
  duplicateCount: number;
}

export interface DuplicateScanGroupPage {
  groups: DuplicateScanGroup[];
  limit: number;
  offset: number;
  hasMore: boolean;
}

export interface DuplicateScanFile {
  path: string;
  size: number;
  modifiedAt: string;
}

export interface DuplicateScanFilePage {
  files: DuplicateScanFile[];
  limit: number;
  offset: number;
  hasMore: boolean;
}

export interface DuplicateCleanupPlanCreated {
  taskId: string;
}

export class DuplicateScanApiError extends Error {
  readonly status?: number;

  constructor(message: string, status?: number) {
    super(message);
    this.name = 'DuplicateScanApiError';
    this.status = status;
  }
}

const statuses = new Set<DuplicateScanStatus>(['queued', 'running', 'succeeded', 'failed', 'cancelled']);
const phases = new Set<DuplicateScanPhase>(['enumerating', 'hashing', 'complete']);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function nonNegativeInteger(value: unknown, field: string): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    throw new DuplicateScanApiError(`扫描服务返回了无效的${field}。`);
  }
  return value;
}

function parsePageMeta(value: Record<string, unknown>) {
  if (typeof value.limit !== 'number' || !Number.isInteger(value.limit) || value.limit < 1 || value.limit > 1000
    || typeof value.offset !== 'number' || !Number.isSafeInteger(value.offset) || value.offset < 0
    || typeof value.hasMore !== 'boolean') {
    throw new DuplicateScanApiError('扫描服务返回了无效的分页信息。');
  }
  return { limit: value.limit, offset: value.offset, hasMore: value.hasMore };
}

export function parseDuplicateScan(value: unknown): DuplicateScan {
  if (!isRecord(value)
    || typeof value.id !== 'string' || !value.id
    || typeof value.status !== 'string' || !statuses.has(value.status as DuplicateScanStatus)
    || typeof value.phase !== 'string' || !phases.has(value.phase as DuplicateScanPhase)
    || typeof value.scopePath !== 'string'
    || typeof value.errorCode !== 'string'
    || typeof value.updatedAt !== 'string') {
    throw new DuplicateScanApiError('扫描服务返回了无效的扫描状态。');
  }
  return {
    id: value.id,
    status: value.status as DuplicateScanStatus,
    phase: value.phase as DuplicateScanPhase,
    scopePath: value.scopePath,
    entriesVisited: nonNegativeInteger(value.entriesVisited, '已检查条目数'),
    candidateFiles: nonNegativeInteger(value.candidateFiles, '候选文件数'),
    filesHashed: nonNegativeInteger(value.filesHashed, '已读取文件数'),
    bytesHashed: nonNegativeInteger(value.bytesHashed, '已读取字节数'),
    totalBytes: nonNegativeInteger(value.totalBytes, '待读取字节数'),
    groupCount: nonNegativeInteger(value.groupCount, '重复组数'),
    duplicateFileCount: nonNegativeInteger(value.duplicateFileCount, '重复文件数'),
    reclaimableBytes: nonNegativeInteger(value.reclaimableBytes, '可释放空间'),
    errorCode: value.errorCode,
    updatedAt: value.updatedAt,
  };
}

export function parseDuplicateScanGroupPage(value: unknown): DuplicateScanGroupPage {
  if (!isRecord(value) || !Array.isArray(value.groups)) {
    throw new DuplicateScanApiError('扫描服务返回了无效的重复组列表。');
  }
  return {
    groups: value.groups.map((item): DuplicateScanGroup => {
      if (!isRecord(item) || typeof item.id !== 'string' || !item.id) {
        throw new DuplicateScanApiError('扫描服务返回了无效的重复组。');
      }
      return {
        id: item.id,
        size: nonNegativeInteger(item.size, '文件大小'),
        duplicateCount: nonNegativeInteger(item.duplicateCount, '重复文件数'),
      };
    }),
    ...parsePageMeta(value),
  };
}

export function parseDuplicateScanFilePage(value: unknown): DuplicateScanFilePage {
  if (!isRecord(value) || !Array.isArray(value.files)) {
    throw new DuplicateScanApiError('扫描服务返回了无效的文件列表。');
  }
  return {
    files: value.files.map((item): DuplicateScanFile => {
      if (!isRecord(item) || typeof item.path !== 'string' || !item.path || typeof item.modifiedAt !== 'string') {
        throw new DuplicateScanApiError('扫描服务返回了无效的重复文件。');
      }
      return {
        path: item.path,
        size: nonNegativeInteger(item.size, '文件大小'),
        modifiedAt: item.modifiedAt,
      };
    }),
    ...parsePageMeta(value),
  };
}

async function readResponseError(response: Response): Promise<DuplicateScanApiError> {
  const message = response.status === 404 ? '扫描记录不存在或已清理。'
    : response.status === 409 ? '扫描状态已变化，请刷新后重试。'
      : response.status === 422 ? '扫描范围或结果已失效。'
        : response.status >= 500 ? '重复文件扫描暂不可用。'
          : `扫描请求失败（HTTP ${response.status}）。`;
  return new DuplicateScanApiError(message, response.status);
}

async function requestJSON<T>(url: string, parse: (value: unknown) => T, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(url, {
      ...init,
      headers: { Accept: 'application/json', ...init?.headers },
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new DuplicateScanApiError('无法连接当前 FileSage 服务。');
  }
  if (!response.ok) throw await readResponseError(response);
  const payload = await response.json().catch(() => undefined) as unknown;
  return parse(payload);
}

function pageQuery(limit: number, offset: number): string {
  const query = new URLSearchParams({
    limit: String(Math.min(1000, Math.max(1, Math.trunc(limit)))),
    offset: String(Math.max(0, Math.trunc(offset))),
  });
  return query.toString();
}

export function getDuplicateScan(id: string, signal?: AbortSignal): Promise<DuplicateScan> {
  return requestJSON(`/api/v1/agent/duplicate-scans/${encodeURIComponent(id)}`, parseDuplicateScan, { signal });
}

// 取消请求后由状态轮询确认最终结果。
export async function cancelDuplicateScan(id: string): Promise<void> {
  let response: Response;
  try {
    response = await fetch(`/api/v1/agent/duplicate-scans/${encodeURIComponent(id)}/cancel`, {
      method: 'POST',
      headers: { Accept: 'application/json' },
    });
  } catch {
    throw new DuplicateScanApiError('无法连接当前 FileSage 服务。');
  }
  if (!response.ok) throw await readResponseError(response);
}

export function getDuplicateScanGroups(
  id: string,
  limit = 20,
  offset = 0,
  signal?: AbortSignal,
): Promise<DuplicateScanGroupPage> {
  return requestJSON(
    `/api/v1/agent/duplicate-scans/${encodeURIComponent(id)}/groups?${pageQuery(limit, offset)}`,
    parseDuplicateScanGroupPage,
    { signal },
  );
}

export function getDuplicateScanFiles(
  id: string,
  groupId: string,
  limit = 100,
  offset = 0,
  signal?: AbortSignal,
): Promise<DuplicateScanFilePage> {
  return requestJSON(
    `/api/v1/agent/duplicate-scans/${encodeURIComponent(id)}/groups/${encodeURIComponent(groupId)}/files?${pageQuery(limit, offset)}`,
    parseDuplicateScanFilePage,
    { signal },
  );
}

export function createDuplicateCleanupPlan(
  scanId: string,
  groupId: string,
  keeperPath: string,
  trashPaths: string[],
): Promise<DuplicateCleanupPlanCreated> {
  return requestJSON(
    `/api/v1/agent/duplicate-scans/${encodeURIComponent(scanId)}/cleanup-plan`,
    (value) => {
      if (!isRecord(value) || typeof value.taskId !== 'string' || !value.taskId) {
        throw new DuplicateScanApiError('整理服务返回了无效的审批任务。');
      }
      return { taskId: value.taskId };
    },
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ groupId, keeperPath, trashPaths }),
    },
  );
}
