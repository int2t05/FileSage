import { ApiError } from './files';

export type TaskStatus =
  | 'queued'
  | 'analyzing'
  | 'awaiting_approval'
  | 'running'
  | 'paused'
  | 'succeeded'
  | 'partial'
  | 'failed'
  | 'cancelled';

export interface TaskSummary {
  id: string;
  kind: string;
  status: TaskStatus;
  scopePath: string;
  createdAt: string;
  updatedAt: string;
}

export interface TaskPage {
  tasks: TaskSummary[];
  limit: number;
  offset: number;
  hasMore: boolean;
}

export interface TaskAuditEvent {
  id: number;
  eventType: string;
  actor: string;
  outcome: string;
  message?: string;
  sourcePath?: string;
  targetPath?: string;
  createdAt: string;
}

export interface TaskAuditPage {
  events: TaskAuditEvent[];
  limit: number;
  offset: number;
  hasMore: boolean;
}

export interface UploadStatus {
  uploadId: string;
  status: string;
  size: number;
  offset: number;
  targetPath: string;
  candidatePath?: string;
  publishedPath?: string;
  error?: string;
  updatedAt: string;
}

export interface TaskFilters {
  kind?: string;
  status?: TaskStatus | '';
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  let response: Response;
  try {
    response = await fetch(url, { headers: { Accept: 'application/json' }, signal });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500 ? '任务服务暂不可用。请确认后端已启动。' : `读取任务信息失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  if (typeof payload !== 'object' || payload === null) {
    throw new ApiError('任务服务返回了无效响应。');
  }
  return payload as T;
}

export function listTasks(filters: TaskFilters, limit = 50, offset = 0, signal?: AbortSignal): Promise<TaskPage> {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (filters.kind) params.set('kind', filters.kind);
  if (filters.status) params.set('status', filters.status);
  return getJSON<TaskPage>(`/api/v1/tasks?${params.toString()}`, signal);
}

export function getTaskAudit(taskId: string, limit = 50, offset = 0, signal?: AbortSignal): Promise<TaskAuditPage> {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  return getJSON<TaskAuditPage>(`/api/v1/tasks/${encodeURIComponent(taskId)}/audit?${params.toString()}`, signal);
}

export function getUploadStatus(uploadId: string, signal?: AbortSignal): Promise<UploadStatus> {
  return getJSON<UploadStatus>(`/api/v1/uploads/status/${encodeURIComponent(uploadId)}`, signal);
}
