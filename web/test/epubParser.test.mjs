// 本文件验证 EPUB 目录、资源上限与安全正文转换。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { strToU8, zipSync } from 'fflate';
import { EPUB_READER_LIMITS, epubSafeText, parseEpubArchive } from '../src/features/reader/epubParser.ts';
import { adjacentEpubChapter, parseEpubPosition } from '../src/features/reader/epubPosition.ts';

const fixture = new Uint8Array(await readFile(new URL('./fixtures/mini-book.epub', import.meta.url)));
const fixedDate = new Date('2025-01-01T00:00:00.000Z');

function toArrayBuffer(bytes) {
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
}

function archiveOf(entries) {
  return zipSync(entries, { level: 6, mtime: fixedDate });
}

function baseArchive(extra = {}) {
  return archiveOf({
    mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: fixedDate }],
    'META-INF/container.xml': strToU8('<container><rootfiles><rootfile full-path="OEBPS/book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>'),
    'OEBPS/book.opf': strToU8('<package><metadata><title>测试</title></metadata><manifest><item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c1"/></spine></package>'),
    'OEBPS/c1.xhtml': strToU8('<html><body><p>测试正文</p></body></html>'),
    ...extra,
  });
}

function findEndRecord(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  for (let offset = bytes.byteLength - 22; offset >= 0; offset -= 1) {
    if (view.getUint32(offset, true) === 0x06054b50) return offset;
  }
  throw new Error('test ZIP end record missing');
}

function setExpandedSize(bytes, name, size) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const end = findEndRecord(bytes);
  let offset = view.getUint32(end + 16, true);
  const count = view.getUint16(end + 10, true);
  for (let index = 0; index < count; index += 1) {
    const nameLength = view.getUint16(offset + 28, true);
    const extraLength = view.getUint16(offset + 30, true);
    const commentLength = view.getUint16(offset + 32, true);
    const entryName = new TextDecoder().decode(bytes.subarray(offset + 46, offset + 46 + nameLength));
    if (entryName === name) {
      const localOffset = view.getUint32(offset + 42, true);
      view.setUint32(offset + 24, size, true);
      view.setUint32(localOffset + 22, size, true);
      return bytes;
    }
    offset += 46 + nameLength + extraLength + commentLength;
  }
  throw new Error(`test ZIP entry missing: ${name}`);
}

test('真实 EPUB 样本提供按 spine 排序的多章目录与安全文本', () => {
  const book = parseEpubArchive(toArrayBuffer(fixture));
  assert.equal(book.title, 'FileSage 阅读样本');
  assert.deepEqual(book.chapters.map(({ title }) => title), ['第一章：开始', '第二章：继续']);
  assert.equal(epubSafeText(book.chapters[0].content), '第一章你好 世界。外部链接仍是普通文字');
  assert.equal(epubSafeText(book.chapters[1].content), '第二章正文标题第二章可以独立导航和恢复阅读位置。');
  assert.equal(JSON.stringify(book).includes('example.invalid'), false);
  assert.equal(JSON.stringify(book).includes('__EPUB_SCRIPT_RAN__'), false);
});

test('正文只包含白名单元素；脚本、样式、图片和链接属性均不输出', () => {
  const book = parseEpubArchive(toArrayBuffer(fixture));
  const found = [];
  const stack = [...book.chapters[0].content];
  while (stack.length) {
    const node = stack.pop();
    if (node.type === 'element') {
      found.push(node.tag);
      stack.push(...node.children);
    }
  }
  assert.deepEqual(found.sort(), ['em', 'h1', 'p', 'p', 'span']);
  assert.equal(found.includes('a'), false);
  assert.equal(found.includes('script'), false);
  assert.equal(found.includes('img'), false);
});

test('错误 ZIP 或 XML 拒绝预览，不抛出原始解析器内容', () => {
  assert.throws(() => parseEpubArchive(toArrayBuffer(strToU8('not a zip'))), /ZIP 文件|文件过小/);
  const malformed = baseArchive({ 'OEBPS/c1.xhtml': strToU8('<html><body><p>未闭合</body></html>') });
  assert.throws(() => parseEpubArchive(toArrayBuffer(malformed)), /XML/);
  const dtd = baseArchive({ 'OEBPS/c1.xhtml': strToU8('<!DOCTYPE html [<!ENTITY x "boom">]><html><body>&x;</body></html>') });
  assert.throws(() => parseEpubArchive(toArrayBuffer(dtd)), /文档类型声明/);
});

test('拒绝越界包内路径、加密标记与 ZIP64', () => {
  const traversal = archiveOf({
    mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: fixedDate }],
    '../escape.txt': strToU8('越界'),
  });
  assert.throws(() => parseEpubArchive(toArrayBuffer(traversal)), /越界路径/);

  const encrypted = baseArchive();
  const encryptedView = new DataView(encrypted.buffer, encrypted.byteOffset, encrypted.byteLength);
  const encryptedEnd = findEndRecord(encrypted);
  const encryptedCentral = encryptedView.getUint32(encryptedEnd + 16, true);
  encryptedView.setUint16(encryptedCentral + 8, 1, true);
  assert.throws(() => parseEpubArchive(toArrayBuffer(encrypted)), /加密 EPUB/);

  const zip64 = baseArchive();
  const zip64View = new DataView(zip64.buffer, zip64.byteOffset, zip64.byteLength);
  zip64View.setUint16(findEndRecord(zip64) + 10, 0xffff, true);
  assert.throws(() => parseEpubArchive(toArrayBuffer(zip64)), /ZIP64/);

  const symlink = archiveOf({
    mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: fixedDate }],
    'link.txt': [strToU8('target'), { os: 3, attrs: 0o120777 << 16, mtime: fixedDate }],
  });
  assert.throws(() => parseEpubArchive(toArrayBuffer(symlink)), /不允许软链接/);
});

test('在解压前限制 ZIP 条目数、单文件膨胀量与输入字节', () => {
  const tooMany = { mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: fixedDate }] };
  for (let index = 0; index < EPUB_READER_LIMITS.entries; index += 1) {
    tooMany[`entry-${index}.txt`] = strToU8('x');
  }
  assert.throws(() => parseEpubArchive(toArrayBuffer(archiveOf(tooMany))), /512 项上限/);

  const tooLarge = baseArchive({ 'OEBPS/huge.txt': strToU8('x'.repeat(EPUB_READER_LIMITS.entryBytes + 1)) });
  assert.throws(() => parseEpubArchive(toArrayBuffer(tooLarge)), /单个文件超过 8 MiB/);

  const highRatio = baseArchive({ 'OEBPS/repetitive.txt': strToU8('重复内容'.repeat(10_000)) });
  assert.throws(() => parseEpubArchive(toArrayBuffer(highRatio)), /压缩比例异常/);

  const oversizedBuffer = new ArrayBuffer(EPUB_READER_LIMITS.archiveBytes + 1);
  assert.throws(() => parseEpubArchive(oversizedBuffer), /32 MiB/);
});

test('按实际流式输出限制解压量并拒绝伪造的文件长度声明', () => {
  const path = 'OEBPS/c1.xhtml';
  const content = new Uint8Array(EPUB_READER_LIMITS.entryBytes + 1024).fill(0x61);
  const tooLarge = archiveOf({
    mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: fixedDate }],
    'META-INF/container.xml': strToU8('<container><rootfiles><rootfile full-path="OEBPS/book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>'),
    'OEBPS/book.opf': strToU8('<package><manifest><item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c1"/></spine></package>'),
    [path]: [content, { level: 0, mtime: fixedDate }],
  });
  setExpandedSize(tooLarge, path, EPUB_READER_LIMITS.entryBytes);
  assert.throws(() => parseEpubArchive(toArrayBuffer(tooLarge)), /单个文件超过 8 MiB/);

  const underreported = baseArchive({ [path]: strToU8('<html><body>这段内容大于声明长度</body></html>') });
  setExpandedSize(underreported, path, 1);
  assert.throws(() => parseEpubArchive(toArrayBuffer(underreported)), /文件长度声明不一致/);
});

test('超过 200 个 spine 项时在解压章节前停止', () => {
  const count = EPUB_READER_LIMITS.chapters + 1;
  const manifest = Array.from({ length: count }, (_, index) => `<item id="c${index}" href="chapter-${index}.xhtml" media-type="application/xhtml+xml"/>`).join('');
  const spine = Array.from({ length: count }, (_, index) => `<itemref idref="c${index}"/>`).join('');
  const files = {
    mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: fixedDate }],
    'META-INF/container.xml': strToU8('<container><rootfiles><rootfile full-path="OEBPS/book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>'),
    'OEBPS/book.opf': strToU8(`<package><manifest>${manifest}</manifest><spine>${spine}</spine></package>`),
  };
  for (let index = 0; index < count; index += 1) files[`OEBPS/chapter-${index}.xhtml`] = strToU8('<html><body>正文</body></html>');
  assert.throws(() => parseEpubArchive(toArrayBuffer(archiveOf(files))), /1 至 200 章/);
});

test('目录切章限制在有效范围，并从当前会话恢复章节、滚动位置和字号', () => {
  const chapters = parseEpubArchive(toArrayBuffer(fixture)).chapters;
  assert.equal(adjacentEpubChapter(0, 1, chapters.length), 1);
  assert.equal(adjacentEpubChapter(1, 1, chapters.length), 1);
  assert.equal(adjacentEpubChapter(0, -1, chapters.length), 0);

  assert.deepEqual(parseEpubPosition(JSON.stringify({ index: 9, scrollTop: 320, fontSize: 32 }), chapters.length), {
    index: 1,
    scrollTop: 320,
    fontSize: 27,
  });
  assert.equal(parseEpubPosition('{malformed', chapters.length), null);
  assert.equal(parseEpubPosition(JSON.stringify({ index: -1, scrollTop: 0, fontSize: 18 }), chapters.length), null);
});
