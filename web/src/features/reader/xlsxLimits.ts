// XLSX 上限同时用于下载、解析与页面渲染。
export const XLSX_LIMITS = {
  compressedBytes: 10 * 1024 * 1024,
  archiveEntries: 1_000,
  entryBytes: 25 * 1024 * 1024,
  expandedBytes: 50 * 1024 * 1024,
  compressionRatio: 100,
  sheets: 20,
  rowsPerSheet: 2_000,
  columnsPerSheet: 100,
  totalCells: 50_000,
  cellTextCharacters: 10_000,
  workbookTextCharacters: 2_000_000,
} as const;
