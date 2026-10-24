// 上传队列只隐藏已完成记录，不删除服务端任务与审计。
const storageKey = 'filesage.uploads.dismissed.v1';
const maxDismissedUploads = 500;
const uploadIdPattern = /^[a-f0-9]{32}$/i;

type DismissalStorage = Pick<Storage, 'getItem' | 'setItem'>;

function browserStorage(): DismissalStorage | undefined {
  try {
    return typeof window === 'undefined' ? undefined : window.localStorage;
  } catch {
    return undefined;
  }
}

// 读取当前浏览器已清除的上传编号。
export function readDismissedUploadIds(storage = browserStorage()): Set<string> {
  if (!storage) return new Set();
  try {
    const value: unknown = JSON.parse(storage.getItem(storageKey) ?? '[]');
    if (!Array.isArray(value)) return new Set();
    return new Set(value.filter((id): id is string => typeof id === 'string' && uploadIdPattern.test(id)).map((id) => id.toLowerCase()));
  } catch {
    return new Set();
  }
}

// 保存隐藏状态，保留最近的编号以限制浏览器存储占用。
export function dismissUploadIds(ids: readonly string[], storage = browserStorage()): boolean {
  const normalized = ids.map((id) => id.toLowerCase());
  if (normalized.some((id) => !uploadIdPattern.test(id))) return false;
  if (normalized.length === 0) return true;
  if (!storage) return false;
  try {
    const dismissed = readDismissedUploadIds(storage);
    normalized.forEach((id) => dismissed.add(id));
    storage.setItem(storageKey, JSON.stringify(Array.from(dismissed).slice(-maxDismissedUploads)));
    return true;
  } catch {
    return false;
  }
}

// 恢复任务时跳过用户已清除的上传记录。
export function filterDismissedUploadRecoveries<T extends { task: { id: string } }>(
  records: T[],
  storage = browserStorage(),
): T[] {
  const dismissed = readDismissedUploadIds(storage);
  return records.filter(({ task }) => !dismissed.has(task.id.toLowerCase()));
}
