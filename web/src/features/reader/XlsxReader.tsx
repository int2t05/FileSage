// 本组件在独立 Worker 中解析 XLSX，并分页展示只读单元格。
import { useEffect, useMemo, useState } from 'react';
import { AlertTriangle, ChevronLeft, ChevronRight, LoaderCircle, RefreshCw } from 'lucide-react';
import { XLSX_LIMITS } from './xlsxLimits.ts';
import type { XlsxCellValue, XlsxSheetValue, XlsxWorkbookValue } from './xlsxWorkbook.ts';

const PAGE_ROWS = 40;
const WORKER_TIMEOUT_MS = 15_000;

interface XlsxReaderProps {
  url: string;
  fileName: string;
  onRetry: () => void;
}

type ReaderState =
  | { status: 'loading' }
  | { status: 'ready'; workbook: XlsxWorkbookValue }
  | { status: 'error'; message: string };

// 按流读取文件，并在下载阶段限制压缩字节数。
async function readBoundedResponse(response: Response): Promise<Uint8Array> {
  const declaredLength = Number(response.headers.get('content-length'));
  if (Number.isFinite(declaredLength) && declaredLength > XLSX_LIMITS.compressedBytes) {
    await response.body?.cancel();
    throw new Error('XLSX 文件超过 10 MiB 阅读上限。可下载原件后使用本机表格软件打开。');
  }

  const reader = response.body?.getReader();
  if (!reader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength > XLSX_LIMITS.compressedBytes) {
      throw new Error('XLSX 文件超过 10 MiB 阅读上限。可下载原件后使用本机表格软件打开。');
    }
    return bytes;
  }

  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > XLSX_LIMITS.compressedBytes) {
        await reader.cancel().catch(() => undefined);
        throw new Error('XLSX 文件超过 10 MiB 阅读上限。可下载原件后使用本机表格软件打开。');
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

// 转移数据到 Worker，并在超时或页面关闭时终止解析。
function parseInWorker(bytes: Uint8Array, signal: AbortSignal): Promise<XlsxWorkbookValue> {
  return new Promise((resolve, reject) => {
    const worker = new Worker(new URL('./xlsx.worker.ts', import.meta.url), { type: 'module' });
    const timeout = window.setTimeout(() => {
      worker.terminate();
      reject(new Error('XLSX 解析超过 15 秒，已停止以保护页面。可下载原件后使用本机表格软件打开。'));
    }, WORKER_TIMEOUT_MS);
    const finish = () => {
      window.clearTimeout(timeout);
      signal.removeEventListener('abort', abort);
      worker.terminate();
    };
    const abort = () => {
      finish();
      reject(new DOMException('读取已取消。', 'AbortError'));
    };

    worker.onmessage = (event: MessageEvent<{ id: number; workbook?: XlsxWorkbookValue; error?: string }>) => {
      if (event.data.id !== 1) return;
      finish();
      if (event.data.workbook) resolve(event.data.workbook);
      else reject(new Error(event.data.error || 'XLSX 文件无法读取。'));
    };
    worker.onerror = () => {
      finish();
      reject(new Error('XLSX 阅读器启动失败，请重新加载或下载原件。'));
    };
    signal.addEventListener('abort', abort, { once: true });
    if (signal.aborted) {
      abort();
      return;
    }
    const transferable = bytes.slice().buffer;
    worker.postMessage({ id: 1, bytes: transferable }, [transferable]);
  });
}

// 将从零开始的列索引转换为工作表列标。
function columnName(index: number): string {
  let value = index + 1;
  let label = '';
  while (value > 0) {
    value -= 1;
    label = String.fromCharCode(65 + value % 26) + label;
    value = Math.floor(value / 26);
  }
  return label;
}

// 给数值、日期、空值和公式缓存提供简洁说明。
function valueKindLabel(cell: XlsxCellValue): string {
  if (cell.formulaCache === 'missing') return '公式未缓存';
  const kindLabels = {
    text: '文本',
    number: '数值',
    date: '日期',
    boolean: '布尔值',
    error: '错误值',
    empty: '空值',
  } as const;
  const label = kindLabels[cell.kind];
  return cell.formulaCache === 'present' ? `公式缓存 · ${label}` : label;
}

// 仅对需要辨识的类型显示简短标签。
function valueTypeLabel(cell: XlsxCellValue): string {
  if (cell.kind === 'number') return '数值';
  if (cell.kind === 'date') return '日期';
  if (cell.kind === 'boolean') return '布尔值';
  if (cell.kind === 'error') return '错误值';
  return '';
}

// 按真实单元格地址分页渲染工作表，空白单元格只作为空值显示。
function SheetTable({ sheet, fileName }: { sheet: XlsxSheetValue; fileName: string }) {
  const [page, setPage] = useState(0);
  useEffect(() => setPage(0), [sheet.name]);
  const rowCount = Math.max(0, sheet.endRow - sheet.startRow + 1);
  const columnCount = Math.max(0, sheet.endColumn - sheet.startColumn + 1);
  const pageCount = Math.ceil(rowCount / PAGE_ROWS);
  const currentPage = Math.min(page, Math.max(0, pageCount - 1));
  const firstRow = sheet.startRow + currentPage * PAGE_ROWS;
  const visibleRowCount = Math.max(0, Math.min(PAGE_ROWS, sheet.endRow - firstRow + 1));
  const cellsByPosition = useMemo(() => {
    const cells = new Map<string, XlsxCellValue>();
    for (const cell of sheet.cells) cells.set(`${cell.row}:${cell.column}`, cell);
    return cells;
  }, [sheet.cells]);

  if (rowCount === 0 || columnCount === 0) {
    return <p className="reader-xlsx-empty">此工作表没有可展示的单元格。</p>;
  }

  return (
    <div className="reader-xlsx-sheet">
      <div className="reader-xlsx-summary">
        <span>{rowCount.toLocaleString('zh-CN')} 行 × {columnCount.toLocaleString('zh-CN')} 列</span>
        <span>{sheet.cells.length.toLocaleString('zh-CN')} 个有内容的单元格</span>
      </div>
      {pageCount > 1 && (
        <nav className="reader-table-pagination" aria-label={`${fileName} · ${sheet.name} 分页`}>
          <span>{rowCount.toLocaleString('zh-CN')} 行</span>
          <div>
            <button className="reader-action-button" type="button" aria-label="上一页" onClick={() => setPage(Math.max(0, currentPage - 1))} disabled={currentPage === 0}>
              <ChevronLeft size={16} aria-hidden="true" />上一页
            </button>
            <span aria-live="polite">{currentPage + 1} / {pageCount}</span>
            <button className="reader-action-button" type="button" aria-label="下一页" onClick={() => setPage(Math.min(pageCount - 1, currentPage + 1))} disabled={currentPage >= pageCount - 1}>
              下一页<ChevronRight size={16} aria-hidden="true" />
            </button>
          </div>
        </nav>
      )}
      <div className="reader-table-scroll" role="region" aria-label={`${fileName} · ${sheet.name} 工作表，可横向滚动`} tabIndex={0}>
        <table className="reader-table reader-xlsx-table">
          <thead>
            <tr>
              <th className="reader-table-row-number" scope="col">行</th>
              {Array.from({ length: columnCount }, (_, index) => (
                <th scope="col" key={sheet.startColumn + index}>{columnName(sheet.startColumn + index)}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {Array.from({ length: visibleRowCount }, (_, rowIndex) => {
              const row = firstRow + rowIndex;
              return (
                <tr key={row}>
                  <th className="reader-table-row-number" scope="row">{row}</th>
                  {Array.from({ length: columnCount }, (_, columnIndex) => {
                    const column = sheet.startColumn + columnIndex;
                    const address = `${columnName(column)}${row}`;
                    const cell = cellsByPosition.get(`${row - 1}:${column}`);
                    const label = cell ? valueKindLabel(cell) : '空值';
                    const typeLabel = cell ? valueTypeLabel(cell) : '';
                    const title = cell?.formula ? `${label} · ${cell.formula}` : label;
                    const empty = !cell || (cell.value === '' && cell.formulaCache !== 'missing');
                    const valueHint = empty ? '，空值' : cell?.value ? `，${cell.value}` : '';
                    return (
                      <td key={address} title={`${address} · ${title}`} aria-label={`${address}：${label}${valueHint}`}>
                        {empty && <span className="reader-table-empty-cell">（空）</span>}
                        {cell && !empty && <span>{cell.value}</span>}
                        {typeLabel && <small className="reader-xlsx-value-kind">{typeLabel}</small>}
                        {cell?.formulaCache && <small className="reader-xlsx-formula-state">{cell.formulaCache === 'present' ? '公式缓存' : '未缓存'}</small>}
                      </td>
                    );
                  })}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <p className="reader-table-note">只读显示原件中的值；公式不计算，宏、链接和外部数据不会执行或加载。</p>
    </div>
  );
}

// 获取工作簿并提供键盘可操作的工作表切换。
export function XlsxReader({ url, fileName, onRetry }: XlsxReaderProps) {
  const [state, setState] = useState<ReaderState>({ status: 'loading' });

  useEffect(() => {
    const controller = new AbortController();
    let current = true;
    setState({ status: 'loading' });

    void (async () => {
      try {
        const response = await fetch(url, { signal: controller.signal, cache: 'no-store' });
        if (!response.ok) throw new Error(`XLSX 读取失败（HTTP ${response.status}）。请检查文件是否仍存在。`);
        const bytes = await readBoundedResponse(response);
        const workbook = await parseInWorker(bytes, controller.signal);
        if (current) setState({ status: 'ready', workbook });
      } catch (error) {
        if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
        setState({ status: 'error', message: error instanceof Error ? error.message : 'XLSX 文件无法读取。可下载原件后使用本机表格软件打开。' });
      }
    })();

    return () => {
      current = false;
      controller.abort();
    };
  }, [url]);

  const [selectedSheet, setSelectedSheet] = useState('');
  const sheet = state.status === 'ready'
    ? state.workbook.sheets.find((item) => item.name === selectedSheet) ?? state.workbook.sheets[0]
    : undefined;
  useEffect(() => {
    if (state.status === 'ready') setSelectedSheet(state.workbook.sheets[0]?.name ?? '');
  }, [state]);

  if (state.status === 'loading') {
    return (
      <div className="reader-xlsx-state" role="status">
        <LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />
        <strong>正在安全读取工作簿…</strong>
        <span>只读取工作表值，不执行公式或宏。</span>
      </div>
    );
  }

  if (state.status === 'error') {
    return (
      <div className="reader-xlsx-state reader-xlsx-error" role="alert">
        <AlertTriangle size={23} aria-hidden="true" />
        <strong>无法预览 XLSX 文件</strong>
        <p>{state.message}</p>
        <button className="reader-action-button" type="button" onClick={onRetry}>
          <RefreshCw size={16} aria-hidden="true" />重新加载
        </button>
        <span>原件仍可通过页面上方“下载原件”获取。</span>
      </div>
    );
  }

  if (!sheet) return <p className="reader-xlsx-empty">工作簿中没有可展示的工作表。</p>;

  return (
    <div className="reader-xlsx-wrap">
      <div className="reader-xlsx-controls">
        <label htmlFor="reader-xlsx-sheet">工作表</label>
        <select id="reader-xlsx-sheet" value={sheet.name} onChange={(event) => setSelectedSheet(event.currentTarget.value)}>
          {state.workbook.sheets.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
        </select>
        <span>{state.workbook.sheets.length} 张工作表 · 只读</span>
      </div>
      <SheetTable sheet={sheet} fileName={fileName} />
    </div>
  );
}
