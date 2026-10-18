// Worker 隔离表格解析，避免大文件占用界面线程。
import { parseXlsxWorkbook } from './xlsxWorkbook.ts';

self.addEventListener('message', (event: MessageEvent<{ id: number; bytes: ArrayBuffer }>) => {
  const { id, bytes } = event.data;
  try {
    const workbook = parseXlsxWorkbook(new Uint8Array(bytes));
    self.postMessage({ id, workbook });
  } catch (error) {
    self.postMessage({ id, error: error instanceof Error ? error.message : 'XLSX 文件无法读取。' });
  }
});
