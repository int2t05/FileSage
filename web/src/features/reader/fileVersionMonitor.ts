// 只用无正文 HEAD 请求检查阅读文件是否已变化。
export interface FileVersionWatchOptions {
  path: string;
  initialVersion: string | null;
  onExpired: () => void;
  intervalMs?: number;
  fetchImpl?: typeof fetch;
}

export function watchFileVersion({
  path,
  initialVersion,
  onExpired,
  intervalMs = 3000,
  fetchImpl = fetch,
}: FileVersionWatchOptions): () => void {
  const query = new URLSearchParams({ path });
  const url = `/api/v1/files/content?${query.toString()}`;
  let expectedVersion = initialVersion?.trim() || null;
  let stopped = false;
  let checking = false;
  let expired = false;
  let requestController: AbortController | null = null;

  async function checkVersion() {
    if (stopped || checking || expired) return;
    checking = true;
    const controller = new AbortController();
    requestController = controller;
    let notifyExpired = false;
    try {
      const response = await fetchImpl(url, {
        method: 'HEAD',
        cache: 'no-store',
        signal: controller.signal,
      });
      if (stopped) return;
      if (response.status === 404 || response.status === 410) {
        expired = true;
        notifyExpired = true;
      } else if (response.ok) {
        const actualVersion = response.headers.get('X-FileSage-Version')?.trim();
        if (actualVersion && !expectedVersion) {
          expectedVersion = actualVersion;
        } else if (actualVersion && actualVersion !== expectedVersion) {
          expired = true;
          notifyExpired = true;
        }
      }
    } catch {
      // 网络暂时不可用时保留当前预览，下一轮继续检查。
    } finally {
      if (requestController === controller) requestController = null;
      checking = false;
    }
    if (notifyExpired && !stopped) onExpired();
  }

  void checkVersion();
  const timer = globalThis.setInterval(() => void checkVersion(), intervalMs);

  return () => {
    stopped = true;
    globalThis.clearInterval(timer);
    requestController?.abort();
    requestController = null;
  };
}

// 文件变化时只更新冲突状态，始终保留用户当前输入。
export function preserveDraftOnFileChange(draft: string): { draft: string; message: string } {
  return {
    draft,
    message: '文件已在其他位置修改，保存已停用；当前草稿已保留。请重载最新版本或另存为。',
  };
}
