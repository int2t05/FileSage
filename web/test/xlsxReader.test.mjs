// 本文件验证 XLSX 只读解析、公式缓存与压缩包安全边界。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { deflateRawSync, inflateRawSync } from 'node:zlib';
import test from 'node:test';
import * as XLSX from 'xlsx';
import { XLSX_LIMITS } from '../src/features/reader/xlsxLimits.ts';
import { parseXlsxWorkbook } from '../src/features/reader/xlsxWorkbook.ts';

const fixtureUrl = new URL('./fixtures/xlsx-reader-sample.xlsx', import.meta.url);

// 读取固定的真实 XLSX 回归样本。
async function sampleBytes() {
  return new Uint8Array(await readFile(fixtureUrl));
}

// 从已解析工作表中按地址查找单元格。
function cell(sheet, address) {
  return sheet.cells.find((item) => item.address === address);
}

// 查找 ZIP 目录记录，用于构造损坏和越限样本。
function centralDirectoryOffset(bytes, signature) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  for (let offset = 0; offset < bytes.length - 4; offset += 1) {
    if (view.getUint32(offset, true) === signature) return offset;
  }
  throw new Error('fixture zip directory is missing');
}

// 按文件名查找中央目录项，便于断言攻击样本的实际头部字段。
function centralEntryOffset(bytes, name) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const endOffset = centralDirectoryOffset(bytes, 0x06054b50);
  const count = view.getUint16(endOffset + 10, true);
  let cursor = view.getUint32(endOffset + 16, true);
  for (let index = 0; index < count; index += 1) {
    const nameLength = view.getUint16(cursor + 28, true);
    const itemName = new TextDecoder().decode(bytes.subarray(cursor + 46, cursor + 46 + nameLength));
    if (itemName === name) return cursor;
    cursor += 46 + nameLength + view.getUint16(cursor + 30, true) + view.getUint16(cursor + 32, true);
  }
  throw new Error(`fixture zip entry is missing: ${name}`);
}

// 使用 SheetJS 生成具有边界条件的测试工作簿。
function singleSheetWorkbook(worksheet, compression = false) {
  const workbook = XLSX.utils.book_new();
  XLSX.utils.book_append_sheet(workbook, worksheet, '数据');
  return new Uint8Array(XLSX.write(workbook, { type: 'array', bookType: 'xlsx', compression }));
}

// 重建 ZIP 目录，制造中央目录与本地头声明不一致的真实攻击样本。
function workbookWithDescriptorEntry(source, withSignature, payload, declaredSize = payload.byteLength) {
  const view = new DataView(source.buffer, source.byteOffset, source.byteLength);
  const endOffset = centralDirectoryOffset(source, 0x06054b50);
  const entryCount = view.getUint16(endOffset + 10, true);
  let cursor = view.getUint32(endOffset + 16, true);
  const localParts = [];
  const centralParts = [];
  let localOffset = 0;

  for (let index = 0; index < entryCount; index += 1) {
    const nameLength = view.getUint16(cursor + 28, true);
    const extraLength = view.getUint16(cursor + 30, true);
    const commentLength = view.getUint16(cursor + 32, true);
    const name = source.subarray(cursor + 46, cursor + 46 + nameLength);
    const nameText = new TextDecoder().decode(name);
    const oldMethod = view.getUint16(cursor + 10, true);
    const oldCompressedSize = view.getUint32(cursor + 20, true);
    const oldExpandedSize = view.getUint32(cursor + 24, true);
    const oldLocalOffset = view.getUint32(cursor + 42, true);
    const localNameLength = view.getUint16(oldLocalOffset + 26, true);
    const localExtraLength = view.getUint16(oldLocalOffset + 28, true);
    const dataOffset = oldLocalOffset + 30 + localNameLength + localExtraLength;
    const isTarget = nameText === 'xl/worksheets/sheet1.xml';
    const method = isTarget ? 8 : oldMethod;
    const compressed = isTarget
      ? new Uint8Array(deflateRawSync(payload))
      : source.slice(dataOffset, dataOffset + oldCompressedSize);
    const crc = isTarget ? testCrc32(payload) : view.getUint32(cursor + 16, true);
    const compressedSize = compressed.byteLength;
    const expandedSize = isTarget ? declaredSize : oldExpandedSize;
    const flags = isTarget ? (view.getUint16(cursor + 8, true) | 0x0008) : (view.getUint16(cursor + 8, true) & ~0x0008);

    const local = new Uint8Array(30 + nameLength);
    const localView = new DataView(local.buffer);
    localView.setUint32(0, 0x04034b50, true);
    localView.setUint16(4, view.getUint16(cursor + 6, true), true);
    localView.setUint16(6, flags, true);
    localView.setUint16(8, method, true);
    localView.setUint16(10, view.getUint16(cursor + 12, true), true);
    localView.setUint16(12, view.getUint16(cursor + 14, true), true);
    localView.setUint32(14, isTarget ? 0 : crc, true);
    localView.setUint32(18, isTarget ? 0 : compressedSize, true);
    localView.setUint32(22, isTarget ? 0 : expandedSize, true);
    localView.setUint16(26, nameLength, true);
    local.set(name, 30);
    localParts.push(local, compressed);

    if (isTarget) {
      const descriptor = new Uint8Array(withSignature ? 16 : 12);
      const descriptorView = new DataView(descriptor.buffer);
      let offset = 0;
      if (withSignature) {
        descriptorView.setUint32(offset, 0x08074b50, true);
        offset += 4;
      }
      descriptorView.setUint32(offset, crc, true);
      descriptorView.setUint32(offset + 4, compressedSize, true);
      descriptorView.setUint32(offset + 8, payload.byteLength, true);
      localParts.push(descriptor);
    }

    const central = new Uint8Array(46 + nameLength);
    const centralView = new DataView(central.buffer);
    centralView.setUint32(0, 0x02014b50, true);
    centralView.setUint16(4, view.getUint16(cursor + 4, true), true);
    centralView.setUint16(6, view.getUint16(cursor + 6, true), true);
    centralView.setUint16(8, flags, true);
    centralView.setUint16(10, method, true);
    centralView.setUint16(12, view.getUint16(cursor + 12, true), true);
    centralView.setUint16(14, view.getUint16(cursor + 14, true), true);
    centralView.setUint32(16, crc, true);
    centralView.setUint32(20, compressedSize, true);
    centralView.setUint32(24, expandedSize, true);
    centralView.setUint16(28, nameLength, true);
    centralView.setUint32(42, localOffset, true);
    central.set(name, 46);
    centralParts.push(central);
    localOffset += local.byteLength + compressed.byteLength + (isTarget ? (withSignature ? 16 : 12) : 0);
    cursor += 46 + nameLength + extraLength + commentLength;
  }

  const centralOffset = localOffset;
  const centralSize = centralParts.reduce((total, part) => total + part.byteLength, 0);
  const end = new Uint8Array(22);
  const endView = new DataView(end.buffer);
  endView.setUint32(0, 0x06054b50, true);
  endView.setUint16(8, entryCount, true);
  endView.setUint16(10, entryCount, true);
  endView.setUint32(12, centralSize, true);
  endView.setUint32(16, centralOffset, true);
  return joinBytes([...localParts, ...centralParts, end]);
}

// 取出测试工作簿中指定 ZIP 条目的解压前内容。
function zipEntryContents(bytes, name) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const centralOffset = centralEntryOffset(bytes, name);
  const localOffset = view.getUint32(centralOffset + 42, true);
  const compressedSize = view.getUint32(centralOffset + 20, true);
  const nameLength = view.getUint16(localOffset + 26, true);
  const extraLength = view.getUint16(localOffset + 28, true);
  const dataOffset = localOffset + 30 + nameLength + extraLength;
  const compressed = bytes.subarray(dataOffset, dataOffset + compressedSize);
  return view.getUint16(centralOffset + 10, true) === 8 ? new Uint8Array(inflateRawSync(compressed)) : compressed;
}

// 使用 ZIP CRC-32 校验测试描述符。
function testCrc32(bytes) {
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

// 拼接 ZIP 记录而不额外改变样本内容。
function joinBytes(parts) {
  const length = parts.reduce((total, part) => total + part.byteLength, 0);
  const output = new Uint8Array(length);
  let offset = 0;
  for (const part of parts) {
    output.set(part, offset);
    offset += part.byteLength;
  }
  return output;
}

test('真实 XLSX fixture 支持多工作表、地址、日期、空值和公式缓存', async () => {
  const workbook = parseXlsxWorkbook(await sampleBytes());
  assert.deepEqual(workbook.sheets.map((sheet) => sheet.name), ['销售数据', '公式与空值']);

  const sales = workbook.sheets[0];
  assert.equal(cell(sales, 'A2')?.value, '咖啡');
  assert.equal(cell(sales, 'B2')?.kind, 'number');
  assert.equal(cell(sales, 'B2')?.value, '10');
  assert.equal(cell(sales, 'C2')?.kind, 'date');
  assert.match(cell(sales, 'C2')?.value ?? '', /2024-01-01/);
  assert.equal(cell(sales, 'B3')?.formula, 'SUM(B2:B2)');
  assert.equal(cell(sales, 'B3')?.formulaCache, 'present');
  assert.equal(cell(sales, 'B3')?.value, '10');
  assert.equal(cell(sales, 'B4')?.formulaCache, 'missing');
  assert.equal(cell(sales, 'B4')?.value, '无缓存结果');
  assert.equal(cell(sales, 'D3')?.kind, 'empty');
  assert.equal(cell(sales, 'A4')?.value, '=HYPERLINK("https://example.invalid")');
  assert.equal(cell(sales, 'A4')?.kind, 'text');

  const formulas = workbook.sheets[1];
  assert.equal(cell(formulas, 'B2')?.formula, 'NOW()');
  assert.equal(cell(formulas, 'B2')?.formulaCache, 'missing');
});

test('拒绝空文件、非 XLSX 内容和超过压缩字节上限的文件', () => {
  assert.throws(() => parseXlsxWorkbook(new Uint8Array()), /不是有效的 XLSX/);
  assert.throws(() => parseXlsxWorkbook(new TextEncoder().encode('not a workbook')), /不是有效的 XLSX/);
  assert.throws(() => parseXlsxWorkbook(new Uint8Array(XLSX_LIMITS.compressedBytes + 1)), /超过 10 MiB/);
});

test('拒绝加密 ZIP 条目和异常展开大小', async () => {
  const encrypted = await sampleBytes();
  const encryptedView = new DataView(encrypted.buffer, encrypted.byteOffset, encrypted.byteLength);
  const centralOffset = centralDirectoryOffset(encrypted, 0x02014b50);
  encryptedView.setUint16(centralOffset + 8, 1, true);
  assert.throws(() => parseXlsxWorkbook(encrypted), /已加密/);

  const expanded = await sampleBytes();
  const expandedView = new DataView(expanded.buffer, expanded.byteOffset, expanded.byteLength);
  const expandedCentralOffset = centralDirectoryOffset(expanded, 0x02014b50);
  expandedView.setUint32(expandedCentralOffset + 24, XLSX_LIMITS.entryBytes + 1, true);
  assert.throws(() => parseXlsxWorkbook(expanded), /单个文件超过 25 MiB/);

  const compressed = await sampleBytes();
  const compressedView = new DataView(compressed.buffer, compressed.byteOffset, compressed.byteLength);
  const compressedCentralOffset = centralDirectoryOffset(compressed, 0x02014b50);
  compressedView.setUint32(compressedCentralOffset + 20, 1_000, true);
  compressedView.setUint32(compressedCentralOffset + 24, 2 * 1024 * 1024, true);
  assert.throws(() => parseXlsxWorkbook(compressed), /压缩比超过安全上限/);
});

test('拒绝损坏的中央目录', async () => {
  const damaged = await sampleBytes();
  const endOffset = centralDirectoryOffset(damaged, 0x06054b50);
  damaged[endOffset] = 0;
  assert.throws(() => parseXlsxWorkbook(damaged), /目录缺失或损坏/);
});

test('拒绝中央目录与本地大小声明不一致', () => {
  const worksheet = XLSX.utils.aoa_to_sheet([['值']]);
  const bytes = singleSheetWorkbook(worksheet, true);
  const centralOffset = centralDirectoryOffset(bytes, 0x02014b50);
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  assert.equal(view.getUint16(centralOffset + 10, true), 8);
  assert.ok(view.getUint32(centralOffset + 20, true) > 0);
  view.setUint32(centralOffset + 24, 0, true);
  assert.throws(() => parseXlsxWorkbook(bytes), /本地文件目录与中央目录不一致/);
});

test('接受有签名和无签名的数据描述符', async () => {
  const original = await sampleBytes();
  const payload = zipEntryContents(original, 'xl/worksheets/sheet1.xml');
  for (const withSignature of [false, true]) {
    const workbook = parseXlsxWorkbook(workbookWithDescriptorEntry(original, withSignature, payload));
    assert.equal(workbook.sheets[0]?.name, '销售数据');
  }
});

test('校验中央 CRC 与数据描述符中的实际大小和校验和', async () => {
  const original = await sampleBytes();
  const payload = zipEntryContents(original, 'xl/worksheets/sheet1.xml');

  const badCrc = workbookWithDescriptorEntry(original, true, payload);
  const badCrcView = new DataView(badCrc.buffer, badCrc.byteOffset, badCrc.byteLength);
  const badCrcCentral = centralEntryOffset(badCrc, 'xl/worksheets/sheet1.xml');
  const badCrcLocal = badCrcView.getUint32(badCrcCentral + 42, true);
  const badCrcNameBytes = badCrcView.getUint16(badCrcLocal + 26, true);
  const badCrcExtraBytes = badCrcView.getUint16(badCrcLocal + 28, true);
  const badCrcDataEnd = badCrcLocal + 30 + badCrcNameBytes + badCrcExtraBytes + badCrcView.getUint32(badCrcCentral + 20, true);
  badCrcView.setUint32(badCrcDataEnd + 4, badCrcView.getUint32(badCrcDataEnd + 4, true) ^ 1, true);
  assert.throws(() => parseXlsxWorkbook(badCrc), /数据描述符与实际内容不一致/);

  const badCentralCrc = workbookWithDescriptorEntry(original, true, payload);
  const badCentralView = new DataView(badCentralCrc.buffer, badCentralCrc.byteOffset, badCentralCrc.byteLength);
  const badCentralEntry = centralEntryOffset(badCentralCrc, 'xl/worksheets/sheet1.xml');
  badCentralView.setUint32(badCentralEntry + 16, badCentralView.getUint32(badCentralEntry + 16, true) ^ 1, true);
  assert.throws(() => parseXlsxWorkbook(badCentralCrc), /条目校验和与中央目录不一致/);
});

test('拒绝本地大小为零、中央声明很小但实际超限的带描述符 ZIP 炸弹', async () => {
  const original = await sampleBytes();
  const noisy = new Uint8Array(500_000);
  let seed = 0x12345678;
  for (let index = 0; index < noisy.length; index += 1) {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    noisy[index] = 97 + (seed % 26);
  }
  const payload = new TextEncoder().encode(
    `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><!--${new TextDecoder().decode(noisy)}-->${' '.repeat(XLSX_LIMITS.entryBytes + 1)}<sheetData/></worksheet>`,
  );
  for (const withSignature of [false, true]) {
    const bytes = workbookWithDescriptorEntry(original, withSignature, payload, 1);
    const centralOffset = centralEntryOffset(bytes, 'xl/worksheets/sheet1.xml');
    const localOffset = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint32(centralOffset + 42, true);
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    assert.equal(view.getUint16(centralOffset + 8, true) & 0x0008, 0x0008);
    assert.equal(view.getUint32(centralOffset + 24, true), 1);
    assert.equal(view.getUint32(localOffset + 22, true), 0);
    assert.ok(bytes.byteLength < XLSX_LIMITS.compressedBytes);
    assert.throws(() => parseXlsxWorkbook(bytes), /实际展开长度与.*声明不一致|单个文件超过 25 MiB/u);
  }
});

test('实际展开超过单项上限时在解析前停止', async () => {
  const original = await sampleBytes();
  const noisy = new Uint8Array(500_000);
  let seed = 0x87654321;
  for (let index = 0; index < noisy.length; index += 1) {
    seed = (Math.imul(seed, 1103515245) + 12345) >>> 0;
    noisy[index] = 97 + (seed % 26);
  }
  const payload = new TextEncoder().encode(
    `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><!--${new TextDecoder().decode(noisy)}-->${' '.repeat(XLSX_LIMITS.entryBytes + 1)}<sheetData/></worksheet>`,
  );
  const bytes = workbookWithDescriptorEntry(original, true, payload, XLSX_LIMITS.entryBytes);
  assert.throws(() => parseXlsxWorkbook(bytes), /单个文件超过 25 MiB/u);
});

test('拒绝超出最大行列规模的工作表', () => {
  const tall = XLSX.utils.aoa_to_sheet([['值']]);
  tall.A2001 = { t: 's', v: '超出行上限' };
  tall['!ref'] = 'A1:A2001';
  assert.throws(() => parseXlsxWorkbook(singleSheetWorkbook(tall)), /超过 2000 行的阅读上限/);

  const wide = XLSX.utils.aoa_to_sheet([['值']]);
  wide.CW1 = { t: 's', v: '超出列上限' };
  wide['!ref'] = 'A1:CW1';
  assert.throws(() => parseXlsxWorkbook(singleSheetWorkbook(wide)), /超过 100 列的阅读上限/);
});

test('拒绝过多工作表与单元格', () => {
  const manySheets = XLSX.utils.book_new();
  for (let index = 0; index < XLSX_LIMITS.sheets + 1; index += 1) {
    XLSX.utils.book_append_sheet(manySheets, XLSX.utils.aoa_to_sheet([[index]]), `表${index + 1}`);
  }
  assert.throws(() => parseXlsxWorkbook(new Uint8Array(XLSX.write(manySheets, { type: 'array', bookType: 'xlsx' }))), /工作表超过 20 张/);

  const rows = Array.from({ length: 501 }, () => Array.from({ length: 100 }, () => '值'));
  const largeSheet = XLSX.utils.aoa_to_sheet(rows);
  assert.throws(() => parseXlsxWorkbook(singleSheetWorkbook(largeSheet)), /超过 50,000 个单元格/);
});
