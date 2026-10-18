// 在独立线程解析 DOCX，避免阻塞阅读界面。
import { parseDocxArchive } from './docxParser';

self.addEventListener('message', (event: MessageEvent<{ buffer?: ArrayBuffer }>) => {
  if (!(event.data?.buffer instanceof ArrayBuffer)) {
    self.postMessage({ type: 'error', message: 'DOCX 数据无效。' });
    return;
  }
  try {
    const document = parseDocxArchive(event.data.buffer);
    const transfers = document.images.map((image) => image.bytes.buffer as ArrayBuffer);
    (self as unknown as { postMessage(message: unknown, transfer?: Transferable[]): void }).postMessage({ type: 'success', document }, transfers);
  } catch (error) {
    self.postMessage({ type: 'error', message: error instanceof Error ? error.message : 'DOCX 无法安全解析。' });
  }
});
