// 文件页持久化用户主动选择的显示偏好。
import type { FileSortMode } from '../../api/files';

const storageKey = 'filesage.files.view.v1';
const sortModes = new Set<FileSortMode>([
  'name-asc', 'name-desc', 'size-asc', 'size-desc', 'modified-asc', 'modified-desc',
]);

export interface FileViewPreferences {
  viewMode: 'list' | 'grid';
  sortMode: FileSortMode;
  showHidden: boolean;
}

export const defaultFileViewPreferences: FileViewPreferences = {
  viewMode: 'list',
  sortMode: 'name-asc',
  showHidden: false,
};

function browserStorage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.localStorage;
  } catch {
    return null;
  }
}

// 读取并校验浏览器中的显示偏好。
export function readFileViewPreferences(storage = browserStorage()): FileViewPreferences {
  if (!storage) return defaultFileViewPreferences;
  try {
    const value: unknown = JSON.parse(storage.getItem(storageKey) ?? 'null');
    if (!value || typeof value !== 'object') return defaultFileViewPreferences;
    const saved = value as Partial<FileViewPreferences>;
    return {
      viewMode: saved.viewMode === 'grid' ? 'grid' : 'list',
      sortMode: sortModes.has(saved.sortMode as FileSortMode) ? saved.sortMode as FileSortMode : 'name-asc',
      showHidden: saved.showHidden === true,
    };
  } catch {
    return defaultFileViewPreferences;
  }
}

// 保存显示偏好；浏览器禁用存储时保持页面可用。
export function saveFileViewPreferences(value: FileViewPreferences, storage = browserStorage()): boolean {
  if (!storage) return false;
  try {
    storage.setItem(storageKey, JSON.stringify(value));
    return true;
  } catch {
    return false;
  }
}
