// 本文件负责 Markdown 标题、图片路径和链接安全规则。
import type { ElementContent, Root as HastRoot, RootContent } from 'hast';
import type { Plugin } from 'unified';
import { defaultUrlTransform } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import remarkParse from 'remark-parse';
import { unified } from 'unified';

export interface MarkdownHeading {
  id: string;
  label: string;
  level: number;
}

interface MarkdownNode {
  type: string;
  value?: string;
  depth?: number;
  alt?: string | null;
  children?: MarkdownNode[];
}

const parser = unified().use(remarkParse).use(remarkGfm);

// 将标题文本转成稳定且唯一的页内锚点。
function uniqueHeadingID(label: string, used: Map<string, number>): string {
  const base = label.normalize('NFKC').toLocaleLowerCase()
    .replace(/[^\p{L}\p{N}_-]+/gu, '-')
    .replace(/-+/g, '-')
    .replace(/^[-_]+|[-_]+$/g, '') || 'section';
  const count = used.get(base) ?? 0;
  used.set(base, count + 1);
  return count === 0 ? base : `${base}-${count}`;
}

// 从 Markdown 语法树提取标题，代码块中的井号不会进入目录。
export function extractMarkdownHeadings(source: string): MarkdownHeading[] {
  const tree = parser.parse(source) as unknown as MarkdownNode;
  const headings: MarkdownHeading[] = [];
  const used = new Map<string, number>();

  function textOf(node: MarkdownNode): string {
    if (node.type === 'text' || node.type === 'inlineCode') return node.value ?? '';
    if (node.type === 'image' || node.type === 'imageReference') return node.alt ?? '';
    if (node.type === 'break') return ' ';
    return node.children?.map(textOf).join('') ?? '';
  }

  function visit(node: MarkdownNode): void {
    if (node.type === 'heading' && node.depth !== undefined) {
      const label = node.children?.map(textOf).join('').trim() ?? '';
      headings.push({ id: uniqueHeadingID(label, used), label: label || '标题', level: node.depth });
      return;
    }
    node.children?.forEach(visit);
  }

  visit(tree);
  return headings;
}

// 给阅读器生成的标题加目录锚点和键盘焦点。
export function createMarkdownHeadingPlugin(headings: readonly MarkdownHeading[]): Plugin<[], HastRoot> {
  return function addHeadingIDs() {
    return function transform(tree) {
      let index = 0;

      function visit(node: ElementContent | RootContent): void {
        if (node.type !== 'element') return;
        if (/^h[1-6]$/.test(node.tagName)) {
          const heading = headings[index];
          index += 1;
          if (heading) node.properties = { ...node.properties, id: heading.id, tabIndex: -1 };
        }
        node.children.forEach(visit);
      }

      tree.children.forEach(visit);
    };
  };
}

// 将图片相对路径限制在管理根目录内。
export function resolveMarkdownImagePath(markdownPath: string, imageURL: string): string | null {
  const rawPath = imageURL.split(/[?#]/, 1)[0] ?? '';
  if (!rawPath || rawPath.startsWith('//') || /^(?:[a-z][a-z\d+.-]*:)/i.test(rawPath) || rawPath.includes('\\')) return null;

  let decodedPath: string;
  try {
    decodedPath = decodeURIComponent(rawPath);
  } catch {
    return null;
  }
  if (!decodedPath || decodedPath.startsWith('//') || /^(?:[a-z][a-z\d+.-]*:)/i.test(decodedPath)
    || decodedPath.includes('\\') || /[\u0000-\u001f\u007f]/u.test(decodedPath)) return null;

  const segments = decodedPath.startsWith('/') ? [] : markdownPath.split('/').slice(0, -1);
  for (const segment of decodedPath.split('/')) {
    if (!segment || segment === '.') continue;
    if (segment === '..') {
      if (segments.length === 0) return null;
      segments.pop();
      continue;
    }
    segments.push(segment);
  }

  return segments.length > 0 ? segments.join('/') : null;
}

// 图片只从管理根目录读取，其他链接沿用安全协议白名单。
export function markdownURLTransform(markdownPath: string, url: string, propertyName: string): string {
  if (propertyName !== 'src') return defaultUrlTransform(url);
  const imagePath = resolveMarkdownImagePath(markdownPath, url);
  return imagePath ? `/api/v1/files/preview?${new URLSearchParams({ path: imagePath }).toString()}` : '';
}
