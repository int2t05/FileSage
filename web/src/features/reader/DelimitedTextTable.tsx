// 本组件分页展示 CSV/TSV，并将单元格作为纯文本渲染。
import { useEffect, useMemo, useState } from 'react';
import { ChevronLeft, ChevronRight } from 'lucide-react';
import { parseDelimitedText, type TableDelimiter } from './delimitedText';

const PAGE_SIZE = 100;

interface DelimitedTextTableProps {
  content: string;
  delimiter: TableDelimiter;
  formatName: 'CSV' | 'TSV';
  fileName: string;
}

// 解析表格并展示当前页；格式或规模不支持时说明原因。
export function DelimitedTextTable({ content, delimiter, formatName, fileName }: DelimitedTextTableProps) {
  const [page, setPage] = useState(0);
  useEffect(() => setPage(0), [content, delimiter]);
  const parsed = useMemo(() => {
    try {
      const records = parseDelimitedText(content, delimiter);
      const headers = records[0] ?? [];
      const rows = records.slice(1);
      const columnCount = records.reduce((count, record) => Math.max(count, record.length), 0);
      return { headers, rows, columnCount, error: '' };
    } catch (error) {
      return {
        headers: [],
        rows: [],
        columnCount: 0,
        error: error instanceof Error ? error.message : `${formatName} 格式错误。`,
      };
    }
  }, [content, delimiter, formatName]);

  const pageCount = Math.ceil(parsed.rows.length / PAGE_SIZE);
  const currentPage = Math.min(page, Math.max(0, pageCount - 1));
  const firstRow = currentPage * PAGE_SIZE;
  const visibleRows = parsed.rows.slice(firstRow, firstRow + PAGE_SIZE);

  if (parsed.error) {
    return (
      <div className="reader-table-error" role="alert">
        <strong>无法解析 {formatName} 文件</strong>
        <p>{parsed.error}</p>
        <span>可下载原件核对分隔符和引号格式。</span>
      </div>
    );
  }

  if (parsed.columnCount === 0) {
    return <div className="reader-table-empty-state">文件中没有表格数据。</div>;
  }

  return (
    <div className="reader-table-wrap">
      {pageCount > 1 && (
        <nav className="reader-table-pagination" aria-label={`${fileName} 表格分页`}>
          <span>{parsed.rows.length.toLocaleString('zh-CN')} 行</span>
          <div>
            <button
              className="reader-action-button"
              type="button"
              aria-label="上一页"
              onClick={() => setPage(Math.max(0, currentPage - 1))}
              disabled={currentPage === 0}
            >
              <ChevronLeft size={16} aria-hidden="true" />上一页
            </button>
            <span aria-live="polite">{currentPage + 1} / {pageCount}</span>
            <button
              className="reader-action-button"
              type="button"
              aria-label="下一页"
              onClick={() => setPage(Math.min(pageCount - 1, currentPage + 1))}
              disabled={currentPage >= pageCount - 1}
            >
              下一页<ChevronRight size={16} aria-hidden="true" />
            </button>
          </div>
        </nav>
      )}
      <div className="reader-table-scroll" role="region" aria-label={`${fileName} 表格，可横向滚动`} tabIndex={0}>
        <table className="reader-table">
          <thead>
            <tr>
              <th className="reader-table-row-number" scope="col">行</th>
              {Array.from({ length: parsed.columnCount }, (_, columnIndex) => (
                <th scope="col" key={columnIndex}>
                  {parsed.headers[columnIndex]?.trim() || `列 ${columnIndex + 1}`}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {visibleRows.map((row, rowIndex) => (
              <tr key={firstRow + rowIndex}>
                <th className="reader-table-row-number" scope="row">{firstRow + rowIndex + 2}</th>
                {Array.from({ length: parsed.columnCount }, (_, columnIndex) => {
                  const value = row[columnIndex] ?? '';
                  return (
                    <td key={columnIndex}>
                      {value === '' ? <span className="reader-table-empty-cell">（空）</span> : value}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="reader-table-note">首行作为列标题；阅读上限为 256 列或 50,000 个单元格。</p>
    </div>
  );
}
