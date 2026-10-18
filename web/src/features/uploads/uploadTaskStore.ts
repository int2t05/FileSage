// 在路由切换期间保留上传任务和 tus 实例。
export interface UploadTaskIdentity {
  id: string;
}

type TaskUpdate<T> = T[] | ((current: T[]) => T[]);

export function createUploadTaskStore<T extends UploadTaskIdentity>() {
  let tasks: T[] = [];
  const listeners = new Set<() => void>();

  return {
    getSnapshot: () => tasks,
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    set(update: TaskUpdate<T>) {
      const next = typeof update === 'function' ? update(tasks) : update;
      if (next === tasks) return;
      tasks = next;
      listeners.forEach((listener) => listener());
    },
  };
}

export const uploadCancellationRequests = new Set<string>();
