// 验证 DOCX 结构解析、资源边界与只读安全行为。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { strToU8, zipSync } from 'fflate';
import { DOCX_READER_LIMITS, parseDocxArchive } from '../src/features/reader/docxParser.ts';

const fixture = new Uint8Array(await readFile(new URL('./fixtures/structured.docx', import.meta.url)));
const date = new Date('2025-01-01T00:00:00.000Z');
const toBuffer = (bytes) => bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
const baseEntries = {
  '[Content_Types].xml': '<Types><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Default Extension="png" ContentType="image/png"/></Types>',
  '_rels/.rels': '<Relationships><Relationship Id="main" Type="x/officeDocument" Target="word/document.xml"/></Relationships>',
  'word/_rels/document.xml.rels': '<Relationships/>',
  'word/document.xml': '<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>正文</w:t></w:r></w:p></w:body></w:document>',
};
function archive(entries) {
  return zipSync(Object.fromEntries(Object.entries(entries).map(([name, data]) => [name, [typeof data === 'string' ? strToU8(data) : data, { level: 6, mtime: date }]])), { level: 6, mtime: date });
}
function corruptEntryCrc(bytes, name) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let end = -1;
  for (let offset = bytes.byteLength - 22; offset >= 0; offset -= 1) if (view.getUint32(offset, true) === 0x06054b50) { end = offset; break; }
  let offset = view.getUint32(end + 16, true);
  for (let index = 0; index < view.getUint16(end + 10, true); index += 1) {
    const nameLength = view.getUint16(offset + 28, true);
    const extraLength = view.getUint16(offset + 30, true);
    const commentLength = view.getUint16(offset + 32, true);
    if (new TextDecoder().decode(bytes.subarray(offset + 46, offset + 46 + nameLength)) === name) {
      view.setUint32(offset + 16, view.getUint32(offset + 16, true) ^ 0xffffffff, true);
      return bytes;
    }
    offset += 46 + nameLength + extraLength + commentLength;
  }
  throw new Error(`fixture entry not found: ${name}`);
}

test('固定 DOCX 样例含段落、标题目录、表格、图片与文档分页', () => {
  const before = fixture.slice();
  const document = parseDocxArchive(toBuffer(fixture));
  assert.equal(document.pages.length, 2);
  assert.deepEqual(document.headings.map(({ title, level, pageIndex }) => ({ title, level, pageIndex })), [
    { title: 'FileSage DOCX 示例', level: 1, pageIndex: 0 },
    { title: '正文与目录', level: 2, pageIndex: 0 },
    { title: '第二页', level: 2, pageIndex: 1 },
  ]);
  assert.equal(document.pages[0].blocks[2].type, 'paragraph');
  assert.equal(document.pages[0].blocks[3].type, 'paragraph');
  assert.ok(document.pages[0].blocks[2].runs.some((run) => run.type === 'text' && run.marks.includes('bold')));
  assert.equal(document.pages[0].blocks[3].runs.find((run) => run.type === 'image').alt, '小图');
  assert.equal(document.pages[1].blocks.at(-1).type, 'table');
  assert.deepEqual(document.images.map(({ mimeType }) => mimeType), ['image/png']);
  const bodyText = JSON.stringify(document);
  assert.match(bodyText, /这是一段加粗正文/);
  assert.match(bodyText, /外部链接只保留文字/);
  assert.doesNotMatch(bodyText, /example\.invalid/);
  assert.deepEqual(fixture, before, '原件字节不应被修改');
});

test('拒绝 DTD、损坏 XML 与不匹配的宏启用文档', () => {
  const dtd = archive({ ...baseEntries, 'word/document.xml': '<!DOCTYPE a [<!ENTITY x "boom">]><w:document><w:body><w:p><w:t>&x;</w:t></w:p></w:body></w:document>' });
  assert.throws(() => parseDocxArchive(toBuffer(dtd)), /文档类型声明/);
  const broken = archive({ ...baseEntries, 'word/document.xml': '<w:document><w:body>' });
  assert.throws(() => parseDocxArchive(toBuffer(broken)), /XML.*(?:无效|不匹配)/);
  const macro = archive({ ...baseEntries, '[Content_Types].xml': '<Types><Override PartName="/word/document.xml" ContentType="application/vnd.ms-word.document.macroEnabled.main+xml"/></Types>' });
  assert.throws(() => parseDocxArchive(toBuffer(macro)), /宏启用格式/);
});

test('拒绝越界图片关系、损坏 ZIP 和压缩比例异常', () => {
  const unsafe = archive({ ...baseEntries, 'word/_rels/document.xml.rels': '<Relationships><Relationship Id="bad" Type="x/image" Target="../../outside.png"/></Relationships>', 'word/document.xml': '<w:document xmlns:w="x"><w:body><w:p><w:r><w:drawing><w:blip r:embed="bad"/></w:drawing></w:r></w:p></w:body></w:document>' });
  assert.throws(() => parseDocxArchive(toBuffer(unsafe)), /越界|不安全/);
  assert.throws(() => parseDocxArchive(toBuffer(new Uint8Array([1, 2, 3]))), /ZIP|损坏/);
  const brokenCrc = corruptEntryCrc(fixture.slice(), 'word/document.xml');
  assert.throws(() => parseDocxArchive(toBuffer(brokenCrc)), /校验失败|压缩数据损坏/);
  const bomb = archive({ ...baseEntries, 'word/styles.xml': `<styles>${'a'.repeat(180_000)}</styles>` });
  assert.throws(() => parseDocxArchive(toBuffer(bomb)), /压缩比例异常/);
});

test('DOCX 输入大小存在硬上限', () => {
  const bytes = new Uint8Array(DOCX_READER_LIMITS.archiveBytes + 1);
  assert.throws(() => parseDocxArchive(toBuffer(bytes)), /超过 32 MiB 读取上限/);
});
