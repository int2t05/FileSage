// EPUB 的解压与 XML 解析在独立线程运行，避免阻塞页面交互。
import { parseEpubArchive } from './epubParser';

self.addEventListener('message', (event: MessageEvent<{ buffer?: ArrayBuffer }>) => {
  if (!(event.data?.buffer instanceof ArrayBuffer)) {
    self.postMessage({ type: 'error', message: 'EPUB 数据无效。' });
    return;
  }

  try {
    const book = parseEpubArchive(event.data.buffer);
    self.postMessage({ type: 'success', book });
  } catch (error) {
    const message = error instanceof Error ? error.message : 'EPUB 无法安全解析。';
    self.postMessage({ type: 'error', message });
  }
});
