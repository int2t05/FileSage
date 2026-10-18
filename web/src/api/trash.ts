// 回收站请求只传输路径与状态元数据，不读取文件内容。
export type TrashItemStatus = 'trashed' | 'needs_attention';

export interface TrashItem {
  id: string;
  originalPath: string;
  name: string;
  kind: string;
  size: number;
  deletedAt: string;
  status: TrashItemStatus;
}

export interface TrashListing {
  items: TrashItem[];
  limit: number;
  nextCursor: string;
  hasMore: boolean;
  snapshot: number;
}

export interface TrashClearResult {
  purged: number;
  needsAttention: number;
  failed: number;
  snapshot: number;
  errors: Array<{ id: string; message: string }>;
}

export class TrashApiError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
    this.name = 'TrashApiError';
  }
}

const TRASH_ENDPOINT = '/api/v1/trash';

function isTrashItem(value: unknown): value is TrashItem {
  if (typeof value !== 'object' || value === null) return false;
  const item = value as Partial<TrashItem>;
  return typeof item.id === 'string'
    && typeof item.originalPath === 'string'
    && typeof item.name === 'string'
    && typeof item.kind === 'string'
    && typeof item.size === 'number'
    && typeof item.deletedAt === 'string'
    && (item.status === 'trashed' || item.status === 'needs_attention');
}

async function readError(response: Response, fallback: string): Promise<TrashApiError> {
  const payload = await response.json().catch(() => undefined) as unknown;
  const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
    ? payload.error
    : response.status >= 500 ? '回收站服务暂不可用。' : `${fallback}（HTTP ${response.status}）`;
  return new TrashApiError(message, response.status);
}

async function request(url: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init);
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new TrashApiError('无法连接 FileSage 回收站服务。');
  }
}

export async function listTrash(
  limit: number,
  cursor?: string,
  signal?: AbortSignal,
): Promise<TrashListing> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (cursor !== undefined) params.set('cursor', cursor);
  const response = await request(`${TRASH_ENDPOINT}?${params.toString()}`, { signal });
  if (!response.ok) throw await readError(response, '读取回收站失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  if (typeof payload !== 'object' || payload === null) throw new TrashApiError('回收站服务返回了无效列表。');
  const listing = payload as Partial<TrashListing>;
  if (!Array.isArray(listing.items) || !listing.items.every(isTrashItem)
    || !Number.isSafeInteger(listing.limit) || listing.limit !== limit
    || listing.items.length > limit
    || typeof listing.nextCursor !== 'string'
    || typeof listing.hasMore !== 'boolean'
    || listing.hasMore !== (listing.nextCursor.length > 0)
    || (listing.hasMore && listing.items.length === 0)
    || !Number.isSafeInteger(listing.snapshot) || listing.snapshot! < 0) {
    throw new TrashApiError('回收站服务返回了无效列表。');
  }
  return listing as TrashListing;
}

// 恢复结果以服务端返回的实际路径为准。
export async function restoreTrashItem(id: string): Promise<{ path: string }> {
  const response = await request(`${TRASH_ENDPOINT}/${encodeURIComponent(id)}/restore`, { method: 'POST' });
  if (!response.ok) throw await readError(response, '恢复文件失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  if (typeof payload !== 'object' || payload === null || !('path' in payload) || typeof payload.path !== 'string') {
    throw new TrashApiError('回收站服务返回了无效的恢复结果。');
  }
  return { path: payload.path };
}

// 单项永久删除必须显式携带不可逆确认。
export async function permanentlyDeleteTrashItem(id: string): Promise<void> {
  const response = await request(`${TRASH_ENDPOINT}/${encodeURIComponent(id)}?confirmPermanent=true`, { method: 'DELETE' });
  if (!response.ok) throw await readError(response, '永久删除失败');
}

function isCount(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

function isTrashClearError(value: unknown): value is { id: string; message: string } {
  if (typeof value !== 'object' || value === null) return false;
  const error = value as { id?: unknown; message?: unknown };
  return typeof error.id === 'string' && typeof error.message === 'string';
}

// 按快照清理；完整响应和快照令牌均须通过校验。
export async function clearTrashSnapshot(snapshot: number): Promise<TrashClearResult> {
  const params = new URLSearchParams({ confirmPermanent: 'true', snapshot: String(snapshot) });
  const response = await request(`${TRASH_ENDPOINT}?${params.toString()}`, { method: 'DELETE' });
  if (!response.ok) throw await readError(response, '清空回收站失败');
  if (response.status !== 200 && response.status !== 207) {
    throw new TrashApiError('回收站服务返回了无法确认的清理结果。', response.status);
  }

  let payload: unknown;
  try {
    payload = await response.json() as unknown;
  } catch {
    throw new TrashApiError('回收站服务未返回完整的清理结果。', response.status);
  }
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) {
    throw new TrashApiError('回收站服务返回了无效的清理结果。', response.status);
  }

  const result = payload as Partial<TrashClearResult>;
  if (!isCount(result.purged) || !isCount(result.needsAttention) || !isCount(result.failed)
    || !isCount(result.snapshot) || result.snapshot !== snapshot
    || !Array.isArray(result.errors) || !result.errors.every(isTrashClearError)) {
    throw new TrashApiError('清理结果字段无效或快照已变化；请刷新后核对。', response.status);
  }
  return {
    purged: result.purged,
    needsAttention: result.needsAttention,
    failed: result.failed,
    snapshot: result.snapshot,
    errors: result.errors,
  };
}
