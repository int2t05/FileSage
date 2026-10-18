// 在 Worker 中读取 DOCX 的无活动内容结构。
import { extractSafeZipEntries, inspectSafeZip, normalizeSafeZipPath } from './safeZip.ts';
import type { SafeZipEntry, SafeZipLimits } from './safeZip.ts';
import { findSafeXmlNodes, parseSafeXml, safeXmlAttribute, safeXmlLocalName } from './safeXml.ts';
import type { SafeXmlNode } from './safeXml.ts';

const limits: SafeZipLimits = {
  label: 'DOCX', archiveBytes: 32 * 1024 * 1024, expandedBytes: 64 * 1024 * 1024,
  entries: 512, entryBytes: 8 * 1024 * 1024, compressionRatio: 200,
};
const XML_LIMITS = { label: 'DOCX', bytes: 2 * 1024 * 1024, tags: 60_000, depth: 80 } as const;
const MAX_TEXT = 2_000_000;
const MAX_BLOCKS = 50_000;
const MAX_PAGES = 500;
const MAX_IMAGES = 50;
const MAX_IMAGE_BYTES = 4 * 1024 * 1024;
const MAX_IMAGES_TOTAL = 16 * 1024 * 1024;
const MAIN_CONTENT_TYPE = 'application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml';

export type DocxMark = 'bold' | 'italic' | 'underline' | 'strike';
export type DocxInline = { type: 'text'; value: string; marks: DocxMark[] } | { type: 'break' } | { type: 'image'; id: string; alt: string } | { type: 'unsupported-image'; alt: string };
export interface DocxParagraph { type: 'paragraph'; id: string; headingLevel: number | null; runs: DocxInline[] }
export interface DocxTable { type: 'table'; rows: Array<{ cells: Array<{ blocks: DocxBlock[] }> }> }
export type DocxBlock = DocxParagraph | DocxTable;
export interface DocxPage { index: number; blocks: DocxBlock[] }
export interface DocxHeading { id: string; title: string; level: number; pageIndex: number }
export interface DocxImage { id: string; mimeType: string; bytes: Uint8Array }
export interface DocxDocument { pages: DocxPage[]; headings: DocxHeading[]; images: DocxImage[] }

function fail(message: string): never { throw new Error(`DOCX ${message}`); }
function local(node: SafeXmlNode): string { return safeXmlLocalName(node.name); }
function children(node: SafeXmlNode, name: string): SafeXmlNode[] { return node.children.filter((child) => local(child) === name); }
function first(node: SafeXmlNode, name: string): SafeXmlNode | undefined { return children(node, name)[0]; }
function attr(node: SafeXmlNode, name: string): string | undefined { return safeXmlAttribute(node, name); }
function xml(bytes: Uint8Array): SafeXmlNode[] {
  try { return parseSafeXml(new TextDecoder('utf-8', { fatal: true }).decode(bytes), XML_LIMITS); }
  catch (error) { if (error instanceof Error && error.message.startsWith('DOCX ')) throw error; return fail('XML 不是有效 UTF-8。'); }
}
function decodeTarget(target: string, source: string): string {
  if (!target || target.startsWith('/') || target.startsWith('//') || /^[A-Za-z][A-Za-z\d+.-]*:/.test(target) || target.includes('\\') || target.includes('\0') || target.includes('?') || target.includes('#')) {
    return fail('包含不安全的包内关系路径。');
  }
  let decoded: string;
  try { decoded = decodeURIComponent(target); } catch { return fail('包含无效的关系路径编码。'); }
  const base = source ? source.split('/').slice(0, -1) : [];
  return normalizeSafeZipPath([...base, decoded].join('/'), 'DOCX');
}
function requiredPart(entries: Map<string, SafeZipEntry>, path: string): SafeZipEntry {
  const entry = entries.get(path);
  if (!entry || entry.directory) return fail(`缺少必要文件：${path}。`);
  if (entry.expandedSize > XML_LIMITS.bytes) return fail('XML 文件超过 2 MiB 解析上限。');
  return entry;
}
function truthyProperty(node: SafeXmlNode | undefined): boolean {
  if (!node) return false;
  const value = attr(node, 'val');
  return value !== '0' && value?.toLowerCase() !== 'false' && value?.toLowerCase() !== 'off';
}

function imageMime(bytes: Uint8Array): string | null {
  if (bytes.length >= 8 && bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e && bytes[3] === 0x47) return 'image/png';
  if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return 'image/jpeg';
  return null;
}

function imageDimensionsSafe(bytes: Uint8Array, mime: string): boolean {
  let width = 0; let height = 0;
  if (mime === 'image/png' && bytes.length >= 24) {
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    width = view.getUint32(16); height = view.getUint32(20);
  } else if (mime === 'image/jpeg') {
    for (let offset = 2; offset + 4 < bytes.length;) {
      if (bytes[offset] !== 0xff) { offset += 1; continue; }
      const marker = bytes[offset + 1]!;
      if (marker === 0xd8 || marker === 0xd9 || marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) { offset += 2; continue; }
      const length = (bytes[offset + 2]! << 8) | bytes[offset + 3]!;
      if (length < 2 || offset + 2 + length > bytes.length) return false;
      if ([0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf].includes(marker)) {
        height = (bytes[offset + 5]! << 8) | bytes[offset + 6]!;
        width = (bytes[offset + 7]! << 8) | bytes[offset + 8]!;
        break;
      }
      offset += 2 + length;
    }
  }
  return width > 0 && height > 0 && width * height <= 16_000_000;
}

function relationshipMap(nodes: SafeXmlNode[], sourcePart: string, imagesOnly: boolean): Map<string, string> {
  const result = new Map<string, string>();
  for (const relation of findSafeXmlNodes(nodes, 'Relationship')) {
    const id = attr(relation, 'Id'); const target = attr(relation, 'Target'); const type = attr(relation, 'Type') ?? '';
    if (!id || !target || result.has(id)) continue;
    if (attr(relation, 'TargetMode')?.toLowerCase() === 'external') continue;
    if (imagesOnly && !type.endsWith('/image')) continue;
    result.set(id, decodeTarget(target, sourcePart));
  }
  return result;
}

function styleHeadingLevels(nodes: SafeXmlNode[]): Map<string, number> {
  const levels = new Map<string, number>();
  for (const style of findSafeXmlNodes(nodes, 'style')) {
    const id = attr(style, 'styleId'); const name = attr(first(style, 'name') ?? style, 'val') ?? '';
    const match = /^(?:heading|标题)\s*([1-9])$/i.exec(name.trim()) ?? /^(?:Heading|标题)([1-9])$/i.exec(id ?? '');
    if (id && match) levels.set(id, Number(match[1]));
  }
  return levels;
}

function paragraphRuns(node: SafeXmlNode, rels: Map<string, string>, imageByPath: Map<string, DocxImage>): DocxInline[] {
  const output: DocxInline[] = [];
  const textLength = { value: 0 };
  const walk = (current: SafeXmlNode, marks: DocxMark[]) => {
    const tag = local(current);
    if (tag === 't' || tag === 'deltext' || tag === 'instrtext') {
      const value = current.children.filter((item) => item.name === '#text').map((item) => item.text).join('');
      textLength.value += value.length;
      if (textLength.value > MAX_TEXT) fail('正文超过 2,000,000 字符上限。');
      if (value) output.push({ type: 'text', value, marks });
      return;
    }
    if (tag === 'tab') { output.push({ type: 'text', value: '\t', marks }); return; }
    if (tag === 'br') {
      if (attr(current, 'type') === 'page') output.push({ type: 'break' });
      else output.push({ type: 'text', value: '\n', marks });
      return;
    }
    if (tag === 'lastrenderedpagebreak') { output.push({ type: 'break' }); return; }
    if (tag === 'drawing' || tag === 'pict') {
      const blip = findSafeXmlNodes(current.children, 'blip')[0];
      const imageRef = blip ? attr(blip, 'embed') : undefined;
      const imagePath = imageRef ? rels.get(imageRef) : undefined;
      const image = imagePath ? imageByPath.get(imagePath) : undefined;
      if (blip) {
        const picture = findSafeXmlNodes(current.children, 'docPr')[0] ?? findSafeXmlNodes(current.children, 'cNvPr')[0];
        const alt = (picture ? attr(picture, 'descr') ?? attr(picture, 'title') : undefined) || '图片格式暂不支持或图片资源不可用';
        output.push(image ? { type: 'image', id: image.id, alt } : { type: 'unsupported-image', alt });
      }
      return;
    }
    if (tag === 'r') {
      const properties = first(current, 'rpr');
      const nextMarks: DocxMark[] = [];
      if (truthyProperty(first(properties ?? current, 'b'))) nextMarks.push('bold');
      if (truthyProperty(first(properties ?? current, 'i'))) nextMarks.push('italic');
      if (truthyProperty(first(properties ?? current, 'u'))) nextMarks.push('underline');
      if (truthyProperty(first(properties ?? current, 'strike'))) nextMarks.push('strike');
      for (const child of current.children) if (local(child) !== 'rpr') walk(child, nextMarks);
      return;
    }
    for (const child of current.children) walk(child, marks);
  };
  for (const child of node.children) walk(child, []);
  return output;
}

function parseParagraph(node: SafeXmlNode, rels: Map<string, string>, imageByPath: Map<string, DocxImage>, styles: Map<string, number>, id: string): { block: DocxParagraph; pageBefore: boolean } {
  const pPr = first(node, 'ppr');
  const styleId = attr(first(pPr ?? node, 'pstyle') ?? node, 'val');
  const outline = Number(attr(first(pPr ?? node, 'outlinelvl') ?? node, 'val'));
  const level = Number.isInteger(outline) && outline >= 0 && outline <= 8 ? outline + 1 : styles.get(styleId ?? '') ?? null;
  const runs = paragraphRuns(node, rels, imageByPath);
  return { block: { type: 'paragraph', id, headingLevel: level, runs }, pageBefore: Boolean(first(pPr ?? node, 'pagebreakbefore')) };
}

function parseTable(node: SafeXmlNode, rels: Map<string, string>, imageByPath: Map<string, DocxImage>, styles: Map<string, number>): DocxTable {
  const rows = children(node, 'tr').map((row) => ({ cells: children(row, 'tc').map((cell) => ({
    blocks: cell.children.flatMap((item, index): DocxBlock[] => {
      if (local(item) === 'p') return [parseParagraph(item, rels, imageByPath, styles, `cell-${index}`).block];
      if (local(item) === 'tbl') return [parseTable(item, rels, imageByPath, styles)];
      return [];
    }),
  })) }));
  return { type: 'table', rows };
}

// 校验包类型、关系和资源后，只解析正文、样式与安全位图。
export function parseDocxArchive(buffer: ArrayBuffer): DocxDocument {
  const bytes = new Uint8Array(buffer);
  const entries = inspectSafeZip(bytes, limits);
  const byPath = new Map(entries.filter((entry) => !entry.directory).map((entry) => [entry.name, entry]));
  requiredPart(byPath, '_rels/.rels');
  requiredPart(byPath, '[Content_Types].xml');
  const expanded = { value: 0 };
  const firstParts = extractSafeZipEntries(bytes, byPath, ['_rels/.rels', '[Content_Types].xml'], expanded, limits);
  const rootRels = xml(firstParts['_rels/.rels']!);
  const officeRel = findSafeXmlNodes(rootRels, 'Relationship').find((node) => (attr(node, 'Type') ?? '').endsWith('/officeDocument'));
  if (!officeRel || attr(officeRel, 'TargetMode')?.toLowerCase() === 'external') fail('缺少有效的内部主文档关系。');
  const documentPath = decodeTarget(attr(officeRel, 'Target') ?? '', '');
  const contentTypes = xml(firstParts['[Content_Types].xml']!);
  const mainType = findSafeXmlNodes(contentTypes, 'Override').find((node) => {
    const part = attr(node, 'PartName')?.replace(/^\//, '');
    return part === documentPath;
  });
  if (!mainType || attr(mainType, 'ContentType') !== MAIN_CONTENT_TYPE) fail('不是受支持的 DOCX 文档（宏启用格式不受支持）。');

  requiredPart(byPath, documentPath);
  const relsPath = `${documentPath.split('/').slice(0, -1).join('/')}/_rels/${documentPath.split('/').at(-1)}.rels`;
  const docRelsEntry = byPath.get(relsPath);
  const stylesPath = `${documentPath.split('/').slice(0, -1).join('/')}/styles.xml`;
  const stylesEntry = byPath.get(stylesPath);
  if (stylesEntry && stylesEntry.expandedSize > XML_LIMITS.bytes) fail('样式 XML 超过 2 MiB 上限。');
  const second = extractSafeZipEntries(bytes, byPath, [documentPath, ...(docRelsEntry ? [relsPath] : []), ...(stylesEntry ? [stylesPath] : [])], expanded, limits);
  const docRelsNodes = docRelsEntry ? xml(second[relsPath]!) : [];
  const rels = relationshipMap(docRelsNodes, documentPath, true);
  const styles = stylesEntry ? styleHeadingLevels(xml(second[stylesPath]!)) : new Map<string, number>();
  const blipRefs = findSafeXmlNodes(xml(second[documentPath]!), 'blip').map((node) => attr(node, 'embed')).filter((id): id is string => Boolean(id));
  const neededImagePaths = [...new Set(blipRefs.map((id) => rels.get(id)).filter((path): path is string => Boolean(path)))];
  if (neededImagePaths.length > MAX_IMAGES) fail('图片数量超过 50 张上限。');
  let imageTotal = 0;
  for (const path of neededImagePaths) {
    const entry = requiredPart(byPath, path);
    if (entry.expandedSize > MAX_IMAGE_BYTES) fail('单张图片超过 4 MiB 上限。');
    imageTotal += entry.expandedSize;
    if (imageTotal > MAX_IMAGES_TOTAL) fail('图片总量超过 16 MiB 上限。');
  }
  const imageParts = extractSafeZipEntries(bytes, byPath, neededImagePaths, expanded, limits);
  const images: DocxImage[] = [];
  const imageByPath = new Map<string, DocxImage>();
  for (const path of neededImagePaths) {
    const content = imageParts[path]!;
    const mimeType = imageMime(content);
    if (!mimeType) continue;
    if (!imageDimensionsSafe(content, mimeType)) fail('图片像素超过 40,000,000 上限。');
    const image = { id: `image-${images.length + 1}`, mimeType, bytes: content };
    images.push(image);
    imageByPath.set(path, image);
  }

  const docXml = xml(second[documentPath]!);
  const body = findSafeXmlNodes(docXml, 'body')[0];
  if (!body) fail('主文档缺少正文。');
  const pages: DocxPage[] = [{ index: 0, blocks: [] }];
  const headings: DocxHeading[] = [];
  let blockCount = 0;
  let headingIndex = 0;
  const newPage = () => {
    if (pages.length >= MAX_PAGES) fail('分页数量超过 500 页上限。');
    pages.push({ index: pages.length, blocks: [] });
  };
  const addParagraph = (paragraph: DocxParagraph, pageBefore = false) => {
    if (pageBefore && pages.at(-1)!.blocks.length > 0) newPage();
    let segment: DocxInline[] = [];
    let headingAdded = false;
    let segmentIndex = 0;
    const pushSegment = () => {
      const visible = segment.some((run) => run.type !== 'text' || run.value.trim());
      if (visible || paragraph.headingLevel !== null) {
        const block: DocxParagraph = { ...paragraph, id: segmentIndex ? `${paragraph.id}-${segmentIndex}` : paragraph.id, runs: segment };
        segmentIndex += 1;
        pages.at(-1)!.blocks.push(block);
        blockCount += 1;
        if (paragraph.headingLevel !== null && !headingAdded) {
          const title = segment.map((run) => run.type === 'text' ? run.value : '').join('').replace(/\s+/g, ' ').trim();
          if (title) headings.push({ id: paragraph.id, title: title.slice(0, 200), level: paragraph.headingLevel, pageIndex: pages.length - 1 });
          headingAdded = true;
        }
      }
      segment = [];
    };
    for (const run of paragraph.runs) {
      if (run.type === 'break') { pushSegment(); newPage(); }
      else {
        segment.push(run);
      }
    }
    pushSegment();
  };
  for (const item of body.children) {
    if (local(item) === 'p') {
      const result = parseParagraph(item, rels, imageByPath, styles, `heading-${++headingIndex}`);
      addParagraph(result.block, result.pageBefore);
    } else if (local(item) === 'tbl') {
      const table = parseTable(item, rels, imageByPath, styles);
      pages.at(-1)!.blocks.push(table);
      blockCount += 1;
    }
    if (blockCount > MAX_BLOCKS) fail('文档结构超过 50,000 个区块上限。');
  }
  if (pages.length > 1 && pages.at(-1)!.blocks.length === 0) pages.pop();
  let textTotal = 0;
  let totalBlocks = 0;
  const pendingBlocks = pages.flatMap((page) => page.blocks);
  while (pendingBlocks.length > 0) {
    const block = pendingBlocks.pop()!;
    totalBlocks += 1;
    if (totalBlocks > MAX_BLOCKS) fail('文档结构超过 50,000 个区块上限。');
    if (block.type === 'paragraph') {
      for (const run of block.runs) if (run.type === 'text') {
        textTotal += run.value.length;
        if (textTotal > MAX_TEXT) fail('正文超过 2,000,000 字符上限。');
      }
    } else {
      for (const row of block.rows) for (const cell of row.cells) pendingBlocks.push(...cell.blocks);
    }
  }
  return { pages, headings, images };
}

export const DOCX_READER_LIMITS = { archiveBytes: limits.archiveBytes, expandedBytes: limits.expandedBytes, pages: MAX_PAGES, images: MAX_IMAGES } as const;
