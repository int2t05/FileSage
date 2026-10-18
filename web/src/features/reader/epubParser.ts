// 在隔离 Worker 中把 EPUB 转成无活动内容的章节数据。
import { extractSafeZipEntries, inspectSafeZip, normalizeSafeZipPath } from './safeZip.ts';
import type { SafeZipEntry, SafeZipLimits } from './safeZip.ts';
import { findSafeXmlNodes, parseSafeXml, safeXmlAttribute, safeXmlLocalName, safeXmlText } from './safeXml.ts';
import type { SafeXmlNode } from './safeXml.ts';

const MAX_ARCHIVE_BYTES = 32 * 1024 * 1024;
const MAX_ENTRIES = 512;
const MAX_EXPANDED_BYTES = 64 * 1024 * 1024;
const MAX_ENTRY_BYTES = 8 * 1024 * 1024;
const MAX_COMPRESSION_RATIO = 200;
const MAX_CHAPTERS = 200;
const MAX_XML_BYTES = 2 * 1024 * 1024;
const MAX_XML_TAGS = 50_000;
const MAX_XML_DEPTH = 64;
const MAX_CHAPTER_TEXT = 250_000;
const MAX_BOOK_TEXT = 4_000_000;
const MAX_SAFE_NODES = 50_000;

const zipLimits: SafeZipLimits = {
  label: 'EPUB',
  archiveBytes: MAX_ARCHIVE_BYTES,
  expandedBytes: MAX_EXPANDED_BYTES,
  entries: MAX_ENTRIES,
  entryBytes: MAX_ENTRY_BYTES,
  compressionRatio: MAX_COMPRESSION_RATIO,
};

export type EpubSafeTag =
  | 'p' | 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6' | 'blockquote' | 'pre' | 'code'
  | 'ul' | 'ol' | 'li' | 'table' | 'thead' | 'tbody' | 'tr' | 'th' | 'td'
  | 'em' | 'strong' | 'b' | 'i' | 'u' | 's' | 'del' | 'sub' | 'sup' | 'small'
  | 'span' | 'div' | 'br' | 'hr';

export type EpubSafeNode =
  | { type: 'text'; value: string }
  | { type: 'element'; tag: EpubSafeTag; children: EpubSafeNode[] };

export interface EpubChapter {
  id: string;
  title: string;
  content: EpubSafeNode[];
}

export interface EpubBook {
  title: string;
  chapters: EpubChapter[];
}

type XmlNode = SafeXmlNode;
type ZipEntry = SafeZipEntry;

const safeTags = new Set<EpubSafeTag>([
  'p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'blockquote', 'pre', 'code',
  'ul', 'ol', 'li', 'table', 'thead', 'tbody', 'tr', 'th', 'td',
  'em', 'strong', 'b', 'i', 'u', 's', 'del', 'sub', 'sup', 'small', 'span', 'div', 'br', 'hr',
]);

const droppedTags = new Set([
  'script', 'style', 'iframe', 'object', 'embed', 'svg', 'math', 'form', 'input', 'button',
  'video', 'audio', 'source', 'img', 'picture', 'link', 'meta', 'base', 'head', 'canvas',
]);

// 统一返回面向用户的中文解析错误。
function fail(message: string): never {
  throw new Error(message);
}

// 规范化包内路径并阻止越界或特殊路径。
function normalizeArchivePath(path: string): string {
  return normalizeSafeZipPath(path, 'EPUB');
}

// EPUB 必须将未压缩的 mimetype 文件放在 ZIP 首项。
function inspectEpubZip(bytes: Uint8Array): ZipEntry[] {
  const entries = inspectSafeZip(bytes, zipLimits);
  const mimetype = entries[0];
  if (!mimetype || mimetype.name !== 'mimetype' || mimetype.localOffset !== 0 || mimetype.method !== 0 || mimetype.expandedSize !== 20) {
    fail('缺少有效的 EPUB mimetype 文件。');
  }
  const mimeStart = mimetype.dataStart;
  let mime: string;
  try {
    mime = new TextDecoder('utf-8', { fatal: true }).decode(bytes.subarray(mimeStart, mimeStart + mimetype.expandedSize));
  } catch {
    return fail('EPUB mimetype 编码无效。');
  }
  if (mime !== 'application/epub+zip') fail('EPUB mimetype 内容无效。');
  return entries;
}

const epubXmlLimits = { label: 'EPUB', bytes: MAX_XML_BYTES, tags: MAX_XML_TAGS, depth: MAX_XML_DEPTH } as const;

function localName(name: string): string { return safeXmlLocalName(name); }
function parseXml(xml: string): XmlNode[] { return parseSafeXml(xml, epubXmlLimits); }

// 只接受 UTF-8 且受单文件大小上限约束。
function decodeXml(bytes: Uint8Array): string {
  if (bytes.byteLength > MAX_XML_BYTES) fail('EPUB 内的 XML 文件超过 2 MiB 上限。');
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    return fail('EPUB 内的 XML 不是有效的 UTF-8。');
  }
}

// 解码并校验 OPF 声明的内部路径。
function decodePackagePath(path: string): string {
  try {
    return normalizeArchivePath(decodeURIComponent(path));
  } catch {
    return fail('EPUB 阅读目录包含无效路径编码。');
  }
}

// 按文档顺序查找指定 XML 元素。
function findNodes(nodes: XmlNode[], name: string): XmlNode[] {
  return findSafeXmlNodes(nodes, name);
}

// 按命名空间无关的名称读取 XML 属性。
function attribute(node: XmlNode, name: string): string | undefined {
  return safeXmlAttribute(node, name);
}

// 汇总目录标签文字并限制展开规模。
function nodeText(node: XmlNode): string {
  return safeXmlText(node, MAX_CHAPTER_TEXT, 'EPUB').replace(/\s+/g, ' ').trim();
}

// 将 EPUB 相对链接安全解析到包内路径。
function safeArchiveHref(basePath: string, href: string): string {
  const withoutFragment = href.split('#', 1)[0]!.split('?', 1)[0]!;
  if (!withoutFragment || withoutFragment.startsWith('/') || withoutFragment.startsWith('//') || /^[A-Za-z][A-Za-z\d+.-]*:/.test(withoutFragment)) {
    fail('EPUB 目录包含越界或外部路径。');
  }
  let decoded: string;
  try {
    decoded = decodeURIComponent(withoutFragment);
  } catch {
    fail('EPUB 目录包含无效路径编码。');
  }
  if (decoded.includes('\\') || decoded.includes('\0')) fail('EPUB 目录包含越界路径。');
  const base = basePath.split('/').slice(0, -1);
  return normalizeArchivePath([...base, decoded].join('/'));
}

// 提取目录标签，不跟随外链或循环引用。
function collectNavigationLabels(nodes: XmlNode[], basePath: string): Map<string, string> {
  const labels = new Map<string, string>();
  for (const node of findNodes(nodes, 'a')) {
    const href = attribute(node, 'href');
    if (!href) continue;
    try {
      const target = safeArchiveHref(basePath, href);
      if (!labels.has(target)) {
        const label = nodeText(node).slice(0, 160);
        if (label) labels.set(target, label);
      }
    } catch {
      // 损坏或外部目录链接会被忽略，阅读顺序仍以 spine 为准。
    }
  }
  return labels;
}

// 仅保留安全标签和文本节点，丢弃活动内容及资源属性。
function readText(nodes: XmlNode[], total: { value: number }): EpubSafeNode[] {
  const result: EpubSafeNode[] = [];
  const stack: Array<{ node: XmlNode; parent: EpubSafeNode[]; depth: number }> = [];
  for (let index = nodes.length - 1; index >= 0; index -= 1) {
    stack.push({ node: nodes[index]!, parent: result, depth: 0 });
  }
  let nodeCount = 0;
  let chapterText = 0;

  while (stack.length > 0) {
    const current = stack.pop()!;
    nodeCount += 1;
    if (nodeCount > MAX_SAFE_NODES || current.depth > MAX_XML_DEPTH) fail('EPUB 章节结构超过阅读上限。');
    const { node, parent } = current;
    if (node.name === '#text') {
      chapterText += node.text.length;
      total.value += node.text.length;
      if (chapterText > MAX_CHAPTER_TEXT || total.value > MAX_BOOK_TEXT) fail('EPUB 正文超过阅读文本上限。');
      if (node.text) parent.push({ type: 'text', value: node.text });
      continue;
    }

    const tag = localName(node.name);
    if (droppedTags.has(tag)) continue;
    const safeTag: EpubSafeTag = safeTags.has(tag as EpubSafeTag) ? tag as EpubSafeTag : 'span';
    const element: EpubSafeNode = { type: 'element', tag: safeTag, children: [] };
    parent.push(element);
    if (safeTag === 'br' || safeTag === 'hr') continue;
    for (let index = node.children.length - 1; index >= 0; index -= 1) {
      stack.push({ node: node.children[index]!, parent: element.children, depth: current.depth + 1 });
    }
  }
  return result;
}

// 解析单章 XHTML 并转换成安全展示节点。
function readChapter(bytes: Uint8Array, title: string, totalText: { value: number }): EpubChapter {
  const document = parseXml(decodeXml(bytes));
  const body = findNodes(document, 'body')[0];
  const content = readText(body ? body.children : document, totalText);
  return { id: '', title, content };
}

// 读取书名，缺失时使用明确的默认值。
function parseBookTitle(packageNodes: XmlNode[]): string {
  const title = findNodes(packageNodes, 'title').map(nodeText).find(Boolean);
  return title?.slice(0, 160) || '未命名 EPUB';
}

// 只解压阅读所需条目，并校验实际字节数。
function extractSelected(
  bytes: Uint8Array,
  entries: Map<string, ZipEntry>,
  paths: string[],
  actualExpanded: { value: number },
): Record<string, Uint8Array> {
  return extractSafeZipEntries(bytes, entries, paths, actualExpanded, zipLimits);
}

// 按容器描述、阅读目录、导航和阅读顺序分阶段解析。
function parseEpub(bytes: Uint8Array, zipEntries: ZipEntry[]): EpubBook {
  const entryByPath = new Map(zipEntries.filter((entry) => !entry.directory).map((entry) => [entry.name, entry]));
  const actualExpanded = { value: 0 };
  let unzipped = extractSelected(bytes, entryByPath, ['META-INF/container.xml'], actualExpanded);
  const containerBytes = unzipped['META-INF/container.xml'];
  if (!containerBytes) fail('EPUB 缺少容器描述文件。');
  const container = parseXml(decodeXml(containerBytes));
  const rootfile = findNodes(container, 'rootfile').find((node) => attribute(node, 'media-type') === 'application/oebps-package+xml');
  const packagePath = rootfile ? attribute(rootfile, 'full-path') : undefined;
  if (!packagePath) fail('EPUB 未声明有效的阅读目录。');
  const safePackagePath = decodePackagePath(packagePath);
  if (!entryByPath.has(safePackagePath)) fail('EPUB 阅读目录不存在。');
  unzipped = { ...unzipped, ...extractSelected(bytes, entryByPath, [safePackagePath], actualExpanded) };
  const packageBytes = unzipped[safePackagePath];
  if (!packageBytes) fail('EPUB 阅读目录不存在。');
  const packageNodes = parseXml(decodeXml(packageBytes));

  const manifest = new Map<string, { path: string; mediaType: string; properties: string }>();
  for (const item of findNodes(packageNodes, 'item')) {
    const id = attribute(item, 'id');
    const href = attribute(item, 'href');
    const mediaType = attribute(item, 'media-type');
    if (!id || !href || !mediaType || manifest.has(id)) continue;
    try {
      manifest.set(id, {
        path: safeArchiveHref(safePackagePath, href),
        mediaType,
        properties: attribute(item, 'properties') ?? '',
      });
    } catch {
      // Manifest 外链和越界项不参与展示。
    }
  }

  const spine = findNodes(packageNodes, 'spine')[0];
  if (!spine) fail('EPUB 缺少章节阅读顺序。');
  const spineItems = spine.children.filter((node) => localName(node.name) === 'itemref');
  if (spineItems.length === 0 || spineItems.length > MAX_CHAPTERS) fail('EPUB 章节数量必须在 1 至 200 章之间。');

  const navItem = [...manifest.values()].find((item) => item.properties.split(/\s+/).includes('nav'));
  const tocId = attribute(spine, 'toc');
  const tocPath = tocId ? manifest.get(tocId)?.path : undefined;
  const chapterItems = spineItems
    .map((itemRef) => {
      const idref = attribute(itemRef, 'idref');
      return idref ? manifest.get(idref) : undefined;
    })
    .filter((item): item is { path: string; mediaType: string; properties: string } =>
      Boolean(item && ['application/xhtml+xml', 'text/html'].includes(item.mediaType)));
  const neededPaths = [navItem?.path, tocPath, ...chapterItems.map((item) => item.path)].filter((path): path is string => Boolean(path));
  unzipped = { ...unzipped, ...extractSelected(bytes, entryByPath, neededPaths, actualExpanded) };

  const titleLabels = new Map<string, string>();
  const navBytes = navItem ? unzipped[navItem.path] : undefined;
  if (navItem && navBytes) {
    try {
      titleLabels.clear();
      for (const [path, label] of collectNavigationLabels(parseXml(decodeXml(navBytes)), navItem.path)) {
        titleLabels.set(path, label);
      }
    } catch {
      titleLabels.clear();
    }
  }

  if (titleLabels.size === 0) {
    if (tocPath && unzipped[tocPath]) {
      try {
        const ncx = parseXml(decodeXml(unzipped[tocPath]));
        for (const point of findNodes(ncx, 'navpoint')) {
          const source = findNodes(point.children, 'content')[0];
          const href = source ? attribute(source, 'src') : undefined;
          const labelNode = findNodes(point.children, 'text')[0];
          if (!href || !labelNode) continue;
          try {
            const path = safeArchiveHref(tocPath, href);
            if (!titleLabels.has(path)) titleLabels.set(path, nodeText(labelNode).slice(0, 160));
          } catch {
            // 损坏的 NCX 条目不影响按 spine 阅读。
          }
        }
      } catch {
        titleLabels.clear();
      }
    }
  }

  const chapters: EpubChapter[] = [];
  const seenChapterPaths = new Set<string>();
  const totalText = { value: 0 };
  for (const itemRef of spineItems) {
    const idref = attribute(itemRef, 'idref');
    const item = idref ? manifest.get(idref) : undefined;
    if (!item || !['application/xhtml+xml', 'text/html'].includes(item.mediaType)) continue;
    if (seenChapterPaths.has(item.path)) continue;
    const chapterBytes = unzipped[item.path];
    if (!chapterBytes || !entryByPath.has(item.path)) fail('EPUB 章节文件不存在。');
    const entry = entryByPath.get(item.path)!;
    if (entry.expandedSize > MAX_XML_BYTES) fail('EPUB 单章正文文件超过 2 MiB 上限。');
    seenChapterPaths.add(item.path);

    const title = titleLabels.get(item.path) || item.path.split('/').at(-1)?.replace(/\.[^.]+$/, '') || `第 ${chapters.length + 1} 章`;
    const chapter = readChapter(chapterBytes, title.slice(0, 160), totalText);
    chapter.id = item.path;
    chapters.push(chapter);
  }
  if (chapters.length === 0) fail('EPUB 没有可阅读的 XHTML 章节。');
  if (chapters.length > MAX_CHAPTERS) fail('EPUB 章节数量超过 200 章上限。');

  return { title: parseBookTitle(packageNodes), chapters };
}

/** 校验 ZIP 声明上限后，只解析阅读顺序中的 XHTML 正文。 */
export function parseEpubArchive(buffer: ArrayBuffer): EpubBook {
  const bytes = new Uint8Array(buffer);
  const entries = inspectEpubZip(bytes);
  return parseEpub(bytes, entries);
}

/** 提供可复用的纯文本，便于测试危险节点是否被丢弃。 */
export function epubSafeText(nodes: EpubSafeNode[]): string {
  const output: string[] = [];
  const stack = [...nodes].reverse();
  while (stack.length > 0) {
    const node = stack.pop()!;
    if (node.type === 'text') output.push(node.value);
    else for (let index = node.children.length - 1; index >= 0; index -= 1) stack.push(node.children[index]!);
  }
  return output.join('');
}

export const EPUB_READER_LIMITS = {
  archiveBytes: MAX_ARCHIVE_BYTES,
  expandedBytes: MAX_EXPANDED_BYTES,
  entries: MAX_ENTRIES,
  entryBytes: MAX_ENTRY_BYTES,
  chapters: MAX_CHAPTERS,
  chapterText: MAX_CHAPTER_TEXT,
} as const;
