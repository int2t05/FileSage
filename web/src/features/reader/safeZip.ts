// 安全读取浏览器内预览所需的 ZIP 条目。
import { Unzip, UnzipInflate, UnzipPassThrough } from 'fflate';

export interface SafeZipEntry {
  name: string;
  method: number;
  crc32: number;
  compressedSize: number;
  expandedSize: number;
  localOffset: number;
  dataStart: number;
  dataEnd: number;
  directory: boolean;
}

export interface SafeZipLimits {
  label: string;
  archiveBytes: number;
  expandedBytes: number;
  entries: number;
  entryBytes: number;
  compressionRatio: number;
}

export const READER_ZIP_LIMITS = {
  archiveBytes: 32 * 1024 * 1024,
  expandedBytes: 64 * 1024 * 1024,
  entries: 512,
  entryBytes: 8 * 1024 * 1024,
  compressionRatio: 200,
} as const;

function fail(label: string, message: string): never {
  throw new Error(`${label} ${message}`);
}

const crcTable = Uint32Array.from({ length: 256 }, (_, index) => {
  let value = index;
  for (let bit = 0; bit < 8; bit += 1) value = value & 1 ? 0xedb88320 ^ (value >>> 1) : value >>> 1;
  return value >>> 0;
});

function zipCrc32(bytes: Uint8Array): number {
  let value = 0xffffffff;
  for (const byte of bytes) value = crcTable[(value ^ byte) & 0xff]! ^ (value >>> 8);
  return (value ^ 0xffffffff) >>> 0;
}

// 规范化包内路径并拒绝绝对路径和越界路径。
export function normalizeSafeZipPath(value: string, label: string): string {
  if (!value || value.includes('\0') || value.includes('\\') || value.startsWith('/') || /^[A-Za-z]:/.test(value)) {
    return fail(label, '包内包含越界路径。');
  }

  const segments: string[] = [];
  for (const segment of value.split('/')) {
    if (!segment || segment === '.') continue;
    if (segment === '..') {
      if (segments.length === 0) return fail(label, '包内包含越界路径。');
      segments.pop();
      continue;
    }
    segments.push(segment);
  }
  if (segments.length === 0) return fail(label, '包内包含无效路径。');
  const normalized = segments.join('/');
  if (normalized === '__proto__') return fail(label, '包内包含不支持的特殊路径。');
  return normalized;
}

function readU16(view: DataView, offset: number, label: string): number {
  if (offset < 0 || offset + 2 > view.byteLength) return fail(label, 'ZIP 结构不完整。');
  return view.getUint16(offset, true);
}

function readU32(view: DataView, offset: number, label: string): number {
  if (offset < 0 || offset + 4 > view.byteLength) return fail(label, 'ZIP 结构不完整。');
  return view.getUint32(offset, true);
}

// 仅接受明确声明 UTF-8 的非 ASCII 文件名。
function decodeName(bytes: Uint8Array, utf8: boolean, label: string): string {
  if (!utf8 && bytes.some((value) => value > 0x7f)) return fail(label, '包内文件名编码不受支持。');
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    return fail(label, '包内包含无效的文件名编码。');
  }
}

// 拒绝损坏扩展字段及 ZIP64 长度声明。
function scanExtraFields(bytes: Uint8Array, start: number, length: number, label: string): void {
  const end = start + length;
  if (end > bytes.byteLength) fail(label, 'ZIP 扩展字段损坏。');
  for (let offset = start; offset < end;) {
    if (offset + 4 > end) fail(label, 'ZIP 扩展字段损坏。');
    const fieldId = bytes[offset]! | (bytes[offset + 1]! << 8);
    const fieldLength = bytes[offset + 2]! | (bytes[offset + 3]! << 8);
    offset += 4;
    if (offset + fieldLength > end) fail(label, 'ZIP 扩展字段损坏。');
    if (fieldId === 0x0001) fail(label, '暂不支持 ZIP64。');
    offset += fieldLength;
  }
}

function findEndRecord(view: DataView, label: string): number {
  const minimum = Math.max(0, view.byteLength - 65_557);
  for (let offset = view.byteLength - 22; offset >= minimum; offset -= 1) {
    if (readU32(view, offset, label) === 0x06054b50) {
      const commentLength = readU16(view, offset + 20, label);
      if (offset + 22 + commentLength === view.byteLength) return offset;
    }
  }
  return fail(label, '不是有效的 ZIP 文件，或文件已损坏。');
}

// 先校验中央目录和本地头，再允许解析器解压条目。
export function inspectSafeZip(bytes: Uint8Array, limits: SafeZipLimits): SafeZipEntry[] {
  const { label } = limits;
  if (bytes.byteLength > limits.archiveBytes) fail(label, `超过 ${Math.floor(limits.archiveBytes / (1024 * 1024))} MiB 读取上限。`);
  if (bytes.byteLength < 22) fail(label, '文件过小或已损坏。');

  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const endOffset = findEndRecord(view, label);
  const diskNumber = readU16(view, endOffset + 4, label);
  const centralDisk = readU16(view, endOffset + 6, label);
  const diskEntries = readU16(view, endOffset + 8, label);
  const entryCount = readU16(view, endOffset + 10, label);
  const centralSize = readU32(view, endOffset + 12, label);
  const centralOffset = readU32(view, endOffset + 16, label);

  if (
    diskNumber !== 0 || centralDisk !== 0 || diskEntries !== entryCount
    || entryCount === 0xffff || centralSize === 0xffffffff || centralOffset === 0xffffffff
  ) fail(label, '暂不支持 ZIP64 或分卷文件。');
  if (entryCount === 0 || entryCount > limits.entries) fail(label, `文件数量超过 ${limits.entries} 项上限。`);
  if (centralOffset + centralSize !== endOffset || centralOffset < 0 || centralSize < 0) fail(label, '中央目录损坏。');
  if (endOffset >= 20 && readU32(view, endOffset - 20, label) === 0x07064b50) fail(label, '暂不支持 ZIP64。');

  const entries: SafeZipEntry[] = [];
  const paths = new Set<string>();
  let offset = centralOffset;
  let expandedTotal = 0;

  for (let index = 0; index < entryCount; index += 1) {
    if (readU32(view, offset, label) !== 0x02014b50) fail(label, '中央目录损坏。');
    const madeBy = readU16(view, offset + 4, label);
    const flags = readU16(view, offset + 8, label);
    const method = readU16(view, offset + 10, label);
    const crc32 = readU32(view, offset + 16, label);
    const compressedSize = readU32(view, offset + 20, label);
    const expandedSize = readU32(view, offset + 24, label);
    const nameLength = readU16(view, offset + 28, label);
    const extraLength = readU16(view, offset + 30, label);
    const commentLength = readU16(view, offset + 32, label);
    const startDisk = readU16(view, offset + 34, label);
    const externalAttributes = readU32(view, offset + 38, label);
    const localOffset = readU32(view, offset + 42, label);
    const recordEnd = offset + 46 + nameLength + extraLength + commentLength;
    if (recordEnd > centralOffset + centralSize) fail(label, '中央目录损坏。');

    const rawName = bytes.subarray(offset + 46, offset + 46 + nameLength);
    const decodedName = decodeName(rawName, (flags & 0x0800) !== 0, label);
    const directory = decodedName.endsWith('/');
    const name = normalizeSafeZipPath(decodedName, label);
    const nameKey = name.toLocaleLowerCase('en-US');
    if (paths.has(nameKey)) fail(label, '包内存在重复路径。');
    paths.add(nameKey);

    if ((flags & (0x0001 | 0x0040 | 0x2000)) !== 0) {
      if (label === 'EPUB') fail(label, '存在加密 EPUB 内容，不支持安全预览。');
      fail(label, '不支持加密内容。');
    }
    if (startDisk !== 0 || compressedSize === 0xffffffff || expandedSize === 0xffffffff || localOffset === 0xffffffff) {
      fail(label, '暂不支持 ZIP64 或分卷文件。');
    }
    if (method !== 0 && method !== 8) fail(label, '使用了不支持的压缩方式。');
    scanExtraFields(bytes, offset + 46 + nameLength, extraLength, label);
    if (expandedSize > limits.entryBytes) fail(label, `单个文件超过 ${Math.floor(limits.entryBytes / (1024 * 1024))} MiB 解压上限。`);
    expandedTotal += expandedSize;
    if (expandedTotal > limits.expandedBytes) fail(label, `解压后超过 ${Math.floor(limits.expandedBytes / (1024 * 1024))} MiB 总量上限。`);
    if (expandedSize > 0 && (compressedSize === 0 || expandedSize / compressedSize > limits.compressionRatio)) {
      fail(label, '压缩比例异常，已停止读取。');
    }

    if (readU32(view, localOffset, label) !== 0x04034b50) fail(label, '本地文件头损坏。');
    const localFlags = readU16(view, localOffset + 6, label);
    const localMethod = readU16(view, localOffset + 8, label);
    const localNameLength = readU16(view, localOffset + 26, label);
    const localExtraLength = readU16(view, localOffset + 28, label);
    if (localOffset + 30 + localNameLength + localExtraLength > centralOffset) fail(label, '本地文件头损坏。');
    const localName = decodeName(bytes.subarray(localOffset + 30, localOffset + 30 + localNameLength), (localFlags & 0x0800) !== 0, label);
    const normalizedLocalName = normalizeSafeZipPath(localName, label);
    if ((localFlags & (0x0001 | 0x0040 | 0x2000)) !== 0 || localMethod !== method || normalizedLocalName !== name) {
      fail(label, '本地文件头与中央目录不一致。');
    }
    scanExtraFields(bytes, localOffset + 30 + localNameLength, localExtraLength, label);
    if ((flags & 0x0008) === 0) {
      const localCompressed = readU32(view, localOffset + 18, label);
      const localExpanded = readU32(view, localOffset + 22, label);
      if (localCompressed !== compressedSize || localExpanded !== expandedSize) fail(label, '文件长度声明不一致。');
    }

    const dataStart = localOffset + 30 + localNameLength + localExtraLength;
    const dataEnd = dataStart + compressedSize;
    if (localOffset >= centralOffset || dataStart > centralOffset || dataEnd > centralOffset) fail(label, '文件内容越过中央目录边界。');
    const host = madeBy >>> 8;
    const unixMode = externalAttributes >>> 16;
    const fileType = unixMode & 0xf000;
    if (host === 3 && fileType !== 0 && fileType !== 0x8000 && fileType !== 0x4000) {
      if (label === 'EPUB') fail(label, '不允许软链接或特殊文件。');
      fail(label, '包内包含链接或特殊文件。');
    }

    entries.push({
      name, method, crc32, compressedSize, expandedSize, localOffset, dataStart, dataEnd,
      directory: directory || (host === 3 && fileType === 0x4000),
    });
    offset = recordEnd;
  }

  if (offset !== centralOffset + centralSize) fail(label, '中央目录长度不一致。');
  const ranges = [...entries].sort((left, right) => left.localOffset - right.localOffset);
  for (let index = 1; index < ranges.length; index += 1) {
    if (ranges[index - 1]!.dataEnd > ranges[index]!.localOffset) fail(label, '文件数据区域重叠。');
  }
  return entries;
}

// 仅展开指定条目，并再次按实际输出字节数执行资源上限。
export function extractSafeZipEntries(
  bytes: Uint8Array,
  entries: ReadonlyMap<string, SafeZipEntry>,
  paths: readonly string[],
  actualExpanded: { value: number },
  limits: SafeZipLimits,
): Record<string, Uint8Array> {
  const wanted = new Set(paths.filter((path) => entries.has(path)));
  if (wanted.size === 0) return {};
  const selected = Object.create(null) as Record<string, Uint8Array>;
  const seen = new Set<string>();
  let failure: string | null = null;
  const unzip = new Unzip();
  unzip.register(UnzipPassThrough);
  unzip.register(UnzipInflate);
  unzip.onfile = (file) => {
    if (failure) return;
    let path: string;
    try {
      path = normalizeSafeZipPath(file.name, limits.label);
    } catch {
      return;
    }
    const entry = entries.get(path);
    if (!entry || !wanted.has(path)) return;
    if (seen.has(path)) {
      failure = `${limits.label} 包内存在重复的阅读文件。`;
      throw new Error(failure);
    }
    seen.add(path);
    if (
      file.compression !== entry.method
      || (file.size !== undefined && file.size !== entry.compressedSize)
      || (file.originalSize !== undefined && file.originalSize !== entry.expandedSize)
    ) {
      failure = `${limits.label} 本地文件头与中央目录不一致。`;
      throw new Error(failure);
    }

    const chunks: Uint8Array[] = [];
    let size = 0;
    file.ondata = (error, chunk, final) => {
      if (failure) throw new Error(failure);
      if (error) {
        failure = `${limits.label} 压缩数据损坏或无法解压。`;
        throw new Error(failure);
      }
      if (chunk?.byteLength) {
        size += chunk.byteLength;
        actualExpanded.value += chunk.byteLength;
        if (size > limits.entryBytes) failure = `${limits.label} 单个文件超过 ${Math.floor(limits.entryBytes / (1024 * 1024))} MiB 解压上限。`;
        else if (actualExpanded.value > limits.expandedBytes) failure = `${limits.label} 解压后超过 ${Math.floor(limits.expandedBytes / (1024 * 1024))} MiB 总量上限。`;
        else if (size > entry.expandedSize) failure = `${limits.label} 文件长度声明不一致。`;
        if (failure) {
          file.terminate();
          throw new Error(failure);
        }
        chunks.push(chunk);
      }
      if (final) {
        if (size !== entry.expandedSize) {
          failure = `${limits.label} 文件长度声明不一致。`;
          throw new Error(failure);
        }
        const content = new Uint8Array(size);
        let offset = 0;
        for (const part of chunks) {
          content.set(part, offset);
          offset += part.byteLength;
        }
        if (zipCrc32(content) !== entry.crc32) {
          failure = `${limits.label} 文件校验失败，内容可能已损坏。`;
          throw new Error(failure);
        }
        selected[path] = content;
      }
    };
    file.start();
  };

  try {
    unzip.push(bytes, true);
  } catch {
    if (!failure) failure = `${limits.label} 压缩数据损坏或无法解压。`;
  }
  if (failure) throw new Error(failure);

  for (const path of wanted) {
    const entry = entries.get(path)!;
    const contents = selected[path];
    if (!contents || contents.byteLength !== entry.expandedSize) fail(limits.label, '文件内容不完整或大小声明不一致。');
  }
  return selected;
}
