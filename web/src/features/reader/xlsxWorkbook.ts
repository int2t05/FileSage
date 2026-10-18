// 本模块校验 XLSX 压缩包并提取只读单元格数据。
import * as XLSX from 'xlsx';
import { Inflate } from 'fflate';
import { XLSX_LIMITS } from './xlsxLimits.ts';

export type XlsxValueKind = 'text' | 'number' | 'date' | 'boolean' | 'error' | 'empty';

export interface XlsxCellValue {
  address: string;
  row: number;
  column: number;
  kind: XlsxValueKind;
  value: string;
  formula?: string;
  formulaCache?: 'present' | 'missing';
}

export interface XlsxSheetValue {
  name: string;
  startRow: number;
  endRow: number;
  startColumn: number;
  endColumn: number;
  cells: XlsxCellValue[];
}

export interface XlsxWorkbookValue {
  sheets: XlsxSheetValue[];
}

class XlsxReadError extends Error {}

interface ZipEntryBoundary {
  method: number;
  crc32: number;
  compressedBytes: number;
  expandedBytes: number;
  localOffset: number;
  dataOffset: number;
  dataEnd: number;
  hasDescriptor: boolean;
}

const crc32Table = Uint32Array.from({ length: 256 }, (_, index) => {
  let value = index;
  for (let bit = 0; bit < 8; bit += 1) value = (value >>> 1) ^ (value & 1 ? 0xedb88320 : 0);
  return value >>> 0;
});

// 增量计算 ZIP 校验和，不保留解压后的文件内容。
function updateCrc32(crc: number, bytes: Uint8Array): number {
  let value = crc;
  for (const byte of bytes) value = (value >>> 8) ^ crc32Table[(value ^ byte) & 0xff]!;
  return value;
}

// 拒绝 ZIP64 扩展，避免解析器从本地扩展字段覆盖已校验的大小。
function validateExtraFields(bytes: Uint8Array, start: number, length: number): void {
  const end = start + length;
  if (end > bytes.byteLength) fail('XLSX 扩展字段损坏。');
  for (let offset = start; offset < end;) {
    if (offset + 4 > end) fail('XLSX 扩展字段损坏。');
    const identifier = bytes[offset]! | (bytes[offset + 1]! << 8);
    const fieldBytes = bytes[offset + 2]! | (bytes[offset + 3]! << 8);
    offset += 4;
    if (offset + fieldBytes > end) fail('XLSX 扩展字段损坏。');
    if (identifier === 0x0001) fail('不支持 ZIP64 扩展格式的 XLSX 文件。');
    offset += fieldBytes;
  }
}

// 统一生成可展示给用户的解析错误。
function fail(message: string): never {
  throw new XlsxReadError(message);
}

// 读取 ZIP 目录边界；每个条目通过有界流式解压核对真实大小和校验和。
function readZipEntries(bytes: Uint8Array): void {
  if (bytes.byteLength > XLSX_LIMITS.compressedBytes) {
    fail('XLSX 文件超过 10 MiB 阅读上限。可下载原件后使用本机表格软件打开。');
  }
  if (bytes.byteLength < 22) fail('文件不是有效的 XLSX 压缩包。');

  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const firstCandidate = Math.max(0, bytes.byteLength - 22 - 65_535);
  let endOffset = -1;
  for (let offset = bytes.byteLength - 22; offset >= firstCandidate; offset -= 1) {
    if (view.getUint32(offset, true) !== 0x06054b50) continue;
    const commentBytes = view.getUint16(offset + 20, true);
    if (offset + 22 + commentBytes === bytes.byteLength) {
      endOffset = offset;
      break;
    }
  }
  if (endOffset < 0) fail('XLSX 文件尾部目录缺失或损坏。');

  const diskNumber = view.getUint16(endOffset + 4, true);
  const directoryDisk = view.getUint16(endOffset + 6, true);
  const diskEntries = view.getUint16(endOffset + 8, true);
  const entryCount = view.getUint16(endOffset + 10, true);
  const directoryBytes = view.getUint32(endOffset + 12, true);
  const directoryOffset = view.getUint32(endOffset + 16, true);
  if (diskNumber !== 0 || directoryDisk !== 0 || diskEntries !== entryCount) {
    fail('不支持分卷 XLSX 文件。');
  }
  if (entryCount === 0xffff || directoryBytes === 0xffffffff || directoryOffset === 0xffffffff) {
    fail('不支持 ZIP64 格式的 XLSX 文件。');
  }
  if (entryCount > XLSX_LIMITS.archiveEntries) {
    fail(`XLSX 文件条目超过 ${XLSX_LIMITS.archiveEntries} 个的安全上限。`);
  }
  if (directoryOffset + directoryBytes > endOffset) fail('XLSX 中央目录位置无效。');

  const decoder = new TextDecoder('utf-8', { fatal: true });
  const names = new Set<string>();
  let expandedBytes = 0;
  const actualExpandedBytes = { value: 0 };
  const entries: ZipEntryBoundary[] = [];
  let cursor = directoryOffset;
  const decodeName = (start: number, length: number) => {
    try {
      return decoder.decode(bytes.subarray(start, start + length));
    } catch {
      fail('XLSX 内部路径不是有效的 UTF-8 编码。');
    }
  };

  for (let index = 0; index < entryCount; index += 1) {
    if (cursor + 46 > directoryOffset + directoryBytes || view.getUint32(cursor, true) !== 0x02014b50) {
      fail('XLSX 中央目录条目损坏。');
    }
    const flags = view.getUint16(cursor + 8, true);
    const method = view.getUint16(cursor + 10, true);
    const crc32 = view.getUint32(cursor + 16, true);
    const compressedBytes = view.getUint32(cursor + 20, true);
    const itemExpandedBytes = view.getUint32(cursor + 24, true);
    const nameBytes = view.getUint16(cursor + 28, true);
    const extraBytes = view.getUint16(cursor + 30, true);
    const commentBytes = view.getUint16(cursor + 32, true);
    const localOffset = view.getUint32(cursor + 42, true);
    const next = cursor + 46 + nameBytes + extraBytes + commentBytes;
    if (next > directoryOffset + directoryBytes) fail('XLSX 中央目录条目长度无效。');
    const name = decodeName(cursor + 46, nameBytes);
    validateExtraFields(bytes, cursor + 46 + nameBytes, extraBytes);
    cursor = next;

    if (!name || name.includes('\u0000') || name.includes('\\') || name.startsWith('/') || name.split('/').includes('..')) {
      fail('XLSX 包含无效的内部路径。');
    }
    if (names.has(name)) fail('XLSX 包含重复的内部路径。');
    names.add(name);
    if ((flags & 0x2041) !== 0) fail('不支持已加密或修补过的 XLSX 文件。');
    if (method !== 0 && method !== 8) fail('XLSX 使用了不支持的压缩方式。');
    if (method === 8 && compressedBytes === 0) fail('XLSX 压缩数据缺失。');
    if (itemExpandedBytes > XLSX_LIMITS.entryBytes) fail('XLSX 内的单个文件超过 25 MiB 展开上限。');
    expandedBytes += itemExpandedBytes;
    if (expandedBytes > XLSX_LIMITS.expandedBytes) fail('XLSX 解压后的内容超过 50 MiB 安全上限。');
    if (itemExpandedBytes > 1024 * 1024 && itemExpandedBytes / Math.max(compressedBytes, 1) > XLSX_LIMITS.compressionRatio) {
      fail('XLSX 压缩比超过安全上限，已停止读取。');
    }

    if (localOffset + 30 > directoryOffset || view.getUint32(localOffset, true) !== 0x04034b50) {
      fail('XLSX 本地文件目录损坏。');
    }
    const localFlags = view.getUint16(localOffset + 6, true);
    const localMethod = view.getUint16(localOffset + 8, true);
    const localCrc32 = view.getUint32(localOffset + 14, true);
    const localCompressedBytes = view.getUint32(localOffset + 18, true);
    const localExpandedBytes = view.getUint32(localOffset + 22, true);
    const localNameBytes = view.getUint16(localOffset + 26, true);
    const localExtraBytes = view.getUint16(localOffset + 28, true);
    const dataOffset = localOffset + 30 + localNameBytes + localExtraBytes;
    if (dataOffset > directoryOffset) fail('XLSX 本地文件目录损坏。');
    validateExtraFields(bytes, localOffset + 30 + localNameBytes, localExtraBytes);
    const hasDescriptor = (flags & 0x0008) !== 0;
    if ((localFlags & 0x2041) !== 0 || localFlags !== flags || localMethod !== method || dataOffset + compressedBytes > directoryOffset) {
      fail('XLSX 本地文件目录与中央目录不一致。');
    }
    const localName = decodeName(localOffset + 30, localNameBytes);
    if (localName !== name) fail('XLSX 内部文件名不一致。');
    if (hasDescriptor) {
      if (
        (localCrc32 !== 0 && localCrc32 !== crc32)
        || (localCompressedBytes !== 0 && localCompressedBytes !== compressedBytes)
        || (localExpandedBytes !== 0 && localExpandedBytes !== itemExpandedBytes)
      ) fail('XLSX 本地文件目录与中央目录不一致。');
    } else if (localCrc32 !== crc32 || localCompressedBytes !== compressedBytes || localExpandedBytes !== itemExpandedBytes) {
      fail('XLSX 本地文件目录与中央目录不一致。');
    }
    entries.push({
      method,
      crc32,
      compressedBytes,
      expandedBytes: itemExpandedBytes,
      localOffset,
      dataOffset,
      dataEnd: dataOffset + compressedBytes,
      hasDescriptor,
    });
  }

  if (cursor !== directoryOffset + directoryBytes) fail('XLSX 中央目录长度不一致。');
  if (!names.has('[Content_Types].xml') || !names.has('xl/workbook.xml')) fail('文件缺少有效的 XLSX 工作簿结构。');
  if (![...names].some((name) => /^xl\/worksheets\/sheet\d+\.xml$/u.test(name))) {
    fail('XLSX 中没有可读取的工作表。');
  }
  if ([...names].some((name) => /^xl\/vbaProject\.bin$/iu.test(name))) {
    fail('工作簿包含宏项目；为避免宏文件伪装成 XLSX，暂不支持预览。');
  }
  if ([...names].some((name) => /^xl\/externalLinks\//iu.test(name) || name === 'xl/connections.xml')) {
    fail('工作簿包含外部数据连接；为避免误读外部内容，暂不支持预览。');
  }
  const sheetFiles = [...names].filter((name) => /^xl\/worksheets\/sheet\d+\.xml$/u.test(name));
  if (sheetFiles.length > XLSX_LIMITS.sheets) fail(`工作表超过 ${XLSX_LIMITS.sheets} 张的阅读上限。`);

  const localEntries = [...entries].sort((left, right) => left.localOffset - right.localOffset);
  for (let index = 0; index < localEntries.length; index += 1) {
    const entry = localEntries[index]!;
    const regionEnd = localEntries[index + 1]?.localOffset ?? directoryOffset;
    if (entry.localOffset >= regionEnd || entry.dataEnd > regionEnd) fail('XLSX 本地文件数据范围重叠或无效。');
    validateEntryOutput(bytes, view, entry, regionEnd, actualExpandedBytes);
  }
}

// 每次只送入 256 个压缩字节，DEFLATE 最坏瞬时输出约 258 KiB；超限即停止。
function validateEntryOutput(
  bytes: Uint8Array,
  view: DataView,
  entry: ZipEntryBoundary,
  regionEnd: number,
  totalExpanded: { value: number },
): void {
  let expandedBytes = 0;
  let crc32 = 0xffffffff;
  const accept = (chunk: Uint8Array) => {
    if (chunk.byteLength === 0) return;
    expandedBytes += chunk.byteLength;
    totalExpanded.value += chunk.byteLength;
    if (expandedBytes > XLSX_LIMITS.entryBytes) fail('XLSX 内的单个文件超过 25 MiB 展开上限。');
    if (totalExpanded.value > XLSX_LIMITS.expandedBytes) fail('XLSX 解压后的内容超过 50 MiB 安全上限。');
    if (expandedBytes > 1024 * 1024 && expandedBytes / Math.max(entry.compressedBytes, 1) > XLSX_LIMITS.compressionRatio) {
      fail('XLSX 压缩比超过安全上限，已停止读取。');
    }
    if (expandedBytes > entry.expandedBytes) fail('XLSX 实际展开长度与中央目录声明不一致。');
    crc32 = updateCrc32(crc32, chunk);
  };

  if (entry.method === 0) {
    if (entry.compressedBytes !== entry.expandedBytes) fail('XLSX 实际展开长度与中央目录声明不一致。');
    for (let offset = entry.dataOffset; offset < entry.dataEnd; offset += 64 * 1024) {
      accept(bytes.subarray(offset, Math.min(offset + 64 * 1024, entry.dataEnd)));
    }
  } else {
    let failure: string | undefined;
    const inflater = new Inflate((chunk) => {
      try {
        accept(chunk);
      } catch (error) {
        failure = error instanceof Error ? error.message : 'XLSX 解压数据超过安全上限。';
        throw error;
      }
    });
    try {
      for (let offset = entry.dataOffset; offset < entry.dataEnd; offset += 256) {
        const next = Math.min(offset + 256, entry.dataEnd);
        inflater.push(bytes.subarray(offset, next), next === entry.dataEnd);
      }
    } catch {
      if (failure) fail(failure);
      fail('XLSX 压缩数据损坏或无法解压。');
    }
  }

  if (expandedBytes !== entry.expandedBytes) fail('XLSX 实际展开长度与中央目录声明不一致。');
  const actualCrc32 = (crc32 ^ 0xffffffff) >>> 0;
  if (actualCrc32 !== entry.crc32) fail('XLSX 条目校验和与中央目录不一致。');
  if (entry.hasDescriptor) validateDataDescriptor(view, entry, regionEnd, actualCrc32, expandedBytes);
}

// 同时接受带签名和无签名的 32 位 ZIP 数据描述符。
function validateDataDescriptor(view: DataView, entry: ZipEntryBoundary, regionEnd: number, crc32: number, expandedBytes: number): void {
  const offset = entry.dataEnd;
  const matches = (hasSignature: boolean) => {
    const descriptorBytes = hasSignature ? 16 : 12;
    if (offset + descriptorBytes > regionEnd) return false;
    if (hasSignature && view.getUint32(offset, true) !== 0x08074b50) return false;
    const fields = offset + (hasSignature ? 4 : 0);
    return view.getUint32(fields, true) === crc32
      && view.getUint32(fields + 4, true) === entry.compressedBytes
      && view.getUint32(fields + 8, true) === expandedBytes;
  };
  if (!matches(true) && !matches(false)) fail('XLSX 数据描述符与实际内容不一致。');
}

// 根据原始单元格类型区分日期、数值、错误与空值。
function valueKind(type: string, value: unknown): XlsxValueKind {
  if (type === 'z' || value === undefined || value === null) return 'empty';
  if (value instanceof Date || type === 'd') return 'date';
  if (type === 'n' || typeof value === 'number') return 'number';
  if (type === 'b' || typeof value === 'boolean') return 'boolean';
  if (type === 'e') return 'error';
  return 'text';
}

// 优先保留工作簿格式化后的文字，不生成链接或 HTML。
function displayValue(cell: XLSX.CellObject, kind: XlsxValueKind): string {
  if (kind === 'empty') return '';
  if (typeof cell.w === 'string') return cell.w;
  if (cell.v instanceof Date) return cell.v.toISOString();
  if (typeof cell.v === 'string' || typeof cell.v === 'number' || typeof cell.v === 'boolean') return String(cell.v);
  return '';
}

// 先限制 ZIP 目录与工作表边界，再只导出纯文本单元格结果。
export function parseXlsxWorkbook(bytes: Uint8Array): XlsxWorkbookValue {
  readZipEntries(bytes);

  let namesWorkbook: XLSX.WorkBook;
  try {
    namesWorkbook = XLSX.read(bytes, {
      type: 'array',
      bookSheets: true,
      bookVBA: false,
      bookFiles: false,
      cellHTML: false,
      WTF: true,
    });
  } catch {
    fail('XLSX 工作簿损坏或无法读取。可下载原件后使用本机表格软件检查。');
  }
  const sheetNames = namesWorkbook.SheetNames ?? [];
  if (sheetNames.length === 0) fail('XLSX 中没有可读取的工作表。');
  if (sheetNames.length > XLSX_LIMITS.sheets) fail(`工作表超过 ${XLSX_LIMITS.sheets} 张的阅读上限。`);

  let workbook: XLSX.WorkBook;
  try {
    workbook = XLSX.read(bytes, {
      type: 'array',
      cellDates: true,
      cellFormula: true,
      cellHTML: false,
      cellNF: true,
      cellStyles: false,
      cellText: true,
      sheetStubs: true,
      bookDeps: false,
      bookFiles: false,
      bookVBA: false,
      dense: false,
      sheetRows: XLSX_LIMITS.rowsPerSheet + 1,
      WTF: true,
    });
  } catch {
    fail('XLSX 工作表损坏或无法读取。可下载原件后使用本机表格软件检查。');
  }

  const result: XlsxSheetValue[] = [];
  let totalCells = 0;
  let totalTextCharacters = 0;

  for (const name of sheetNames) {
    const worksheet = workbook.Sheets[name];
    if (!worksheet) continue;
    let range: XLSX.Range | null = null;
    const fullRange = worksheet['!fullref'] ?? worksheet['!ref'];
    if (typeof fullRange === 'string') {
      try {
        range = XLSX.utils.decode_range(fullRange);
      } catch {
        fail(`工作表“${name}”的行列范围无效。`);
      }
    }

    const startRow = range ? range.s.r + 1 : 1;
    const endRow = range ? range.e.r + 1 : 0;
    const startColumn = range ? range.s.c : 0;
    const endColumn = range ? range.e.c : -1;
    const rowCount = range ? range.e.r - range.s.r + 1 : 0;
    const columnCount = range ? range.e.c - range.s.c + 1 : 0;
    if (rowCount > XLSX_LIMITS.rowsPerSheet) fail(`工作表“${name}”超过 ${XLSX_LIMITS.rowsPerSheet} 行的阅读上限。`);
    if (columnCount > XLSX_LIMITS.columnsPerSheet) fail(`工作表“${name}”超过 ${XLSX_LIMITS.columnsPerSheet} 列的阅读上限。`);

    const cells: XlsxCellValue[] = [];
    for (const [address, rawCell] of Object.entries(worksheet)) {
      if (address.startsWith('!') || typeof rawCell !== 'object' || rawCell === null) continue;
      const cell = rawCell as XLSX.CellObject;
      let position: XLSX.CellAddress;
      try {
        position = XLSX.utils.decode_cell(address);
      } catch {
        fail(`工作表“${name}”包含无效的单元格地址。`);
      }
      if (position.r < 0 || position.c < 0 || position.r >= XLSX_LIMITS.rowsPerSheet || position.c >= XLSX_LIMITS.columnsPerSheet) {
        fail(`工作表“${name}”的单元格超出 ${XLSX_LIMITS.rowsPerSheet} 行、${XLSX_LIMITS.columnsPerSheet} 列的阅读上限。`);
      }
      totalCells += 1;
      if (totalCells > XLSX_LIMITS.totalCells) fail(`工作簿超过 ${XLSX_LIMITS.totalCells.toLocaleString('zh-CN')} 个单元格的阅读上限。`);

      const kind = valueKind(cell.t, cell.v);
      const hasFormula = typeof cell.f === 'string';
      const hasCache = Object.hasOwn(cell, 'v') && cell.v !== undefined && cell.t !== 'z';
      const value = hasFormula && !hasCache ? '无缓存结果' : displayValue(cell, kind);
      const formula = hasFormula ? cell.f : undefined;
      if (value.length > XLSX_LIMITS.cellTextCharacters || (formula?.length ?? 0) > XLSX_LIMITS.cellTextCharacters) {
        fail(`工作表“${name}”包含超过 10,000 字符的单元格内容。`);
      }
      totalTextCharacters += value.length + (formula?.length ?? 0);
      if (totalTextCharacters > XLSX_LIMITS.workbookTextCharacters) fail('工作簿文本超过 2,000,000 字符的阅读上限。');
      cells.push({
        address,
        row: position.r,
        column: position.c,
        kind: hasFormula && !hasCache ? 'empty' : kind,
        value,
        ...(formula ? { formula } : {}),
        ...(hasFormula ? { formulaCache: hasCache ? 'present' as const : 'missing' as const } : {}),
      });
    }

    result.push({ name, startRow, endRow, startColumn, endColumn, cells });
  }

  return { sheets: result };
}
