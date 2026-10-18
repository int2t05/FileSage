import { listTasks, type TaskSummary } from './tasks';

export type PublicationStatus =
  | 'receiving'
  | 'queued'
  | 'publishing'
  | 'published'
  | 'partial'
  | 'failed'
  | 'cancelling'
  | 'cancelled';

export interface UploadPublication {
  uploadId: string;
  status: PublicationStatus;
  size: number;
  offset: number;
  targetPath: string;
  candidatePath?: string;
  publishedPath?: string;
  error?: string;
  updatedAt: string;
}

export interface UploadRecoveryRecord {
  task: TaskSummary;
  publication?: UploadPublication;
  error?: string;
  errorStatus?: number;
}

export class UploadApiError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
    this.name = 'UploadApiError';
  }
}

const publicationStatuses = new Set<PublicationStatus>([
  'receiving', 'queued', 'publishing', 'published', 'partial', 'failed', 'cancelling', 'cancelled',
]);

function isPublication(value: unknown): value is UploadPublication {
  if (typeof value !== 'object' || value === null) return false;
  const result = value as Partial<UploadPublication>;
  return typeof result.uploadId === 'string'
    && typeof result.status === 'string'
    && publicationStatuses.has(result.status as PublicationStatus)
    && typeof result.size === 'number'
    && typeof result.offset === 'number'
    && typeof result.targetPath === 'string'
    && typeof result.updatedAt === 'string';
}

async function requestPublication(url: string, init?: RequestInit): Promise<UploadPublication> {
  let response: Response;
  try {
    response = await fetch(url, init);
  } catch (error) {
    if (init?.signal?.aborted || (error instanceof DOMException && error.name === 'AbortError')) throw error;
    throw new UploadApiError('无法连接 FileSage 上传服务。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error
      : `读取上传发布状态失败（HTTP ${response.status}）。`;
    throw new UploadApiError(message, response.status);
  }
  if (!isPublication(payload)) throw new UploadApiError('上传服务返回了无效的发布状态。');
  return payload;
}

export function getUploadPublication(uploadId: string, signal?: AbortSignal): Promise<UploadPublication> {
  return requestPublication(`/api/v1/uploads/status/${encodeURIComponent(uploadId)}`, { signal });
}

export function retryUploadPublication(uploadId: string): Promise<UploadPublication> {
  return requestPublication(`/api/v1/uploads/status/${encodeURIComponent(uploadId)}/retry`, { method: 'POST' });
}

// 读取最近上传任务及发布状态，限制并发以免刷新时集中请求服务端。
export async function listUploadRecoveries(signal?: AbortSignal): Promise<UploadRecoveryRecord[]> {
  const page = await listTasks({ kind: 'upload' }, 50, 0, signal);
  const records: UploadRecoveryRecord[] = new Array(page.tasks.length);
  let next = 0;
  const workers = Array.from({ length: Math.min(5, page.tasks.length) }, async () => {
    while (true) {
      if (signal?.aborted) throw new DOMException('Upload recovery cancelled', 'AbortError');
      const index = next++;
      if (index >= page.tasks.length) return;
      const task = page.tasks[index];
      if (!task) continue;
      try {
        records[index] = { task, publication: await getUploadPublication(task.id, signal) };
      } catch (error) {
        if (signal?.aborted || (error instanceof DOMException && error.name === 'AbortError')) throw error;
        records[index] = {
          task,
          error: error instanceof UploadApiError ? error.message : '无法读取该上传的服务端状态。',
          errorStatus: error instanceof UploadApiError ? error.status : undefined,
        };
      }
    }
  });
  await Promise.all(workers);
  return records;
}
