// 安全解析文档包中的 XML，限制实体、体积、标签数和嵌套深度。
import { XMLParser, XMLValidator } from 'fast-xml-parser';

export interface SafeXmlNode {
  name: string;
  attributes: Record<string, string>;
  children: SafeXmlNode[];
  text: string;
}

export interface SafeXmlLimits {
  label: string;
  bytes: number;
  tags: number;
  depth: number;
}

function fail(label: string, message: string): never {
  throw new Error(`${label} ${message}`);
}

function orderedToNodes(value: unknown, label: string): SafeXmlNode[] {
  if (!Array.isArray(value)) return [];
  const result: SafeXmlNode[] = [];
  for (const item of value) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) continue;
    const record = item as Record<string, unknown>;
    const elementName = Object.keys(record).find((key) => key !== ':@');
    if (!elementName) continue;
    const childValue = record[elementName];
    if (elementName === '#text' || elementName === '__cdata') {
      const text = Array.isArray(childValue)
        ? childValue.map((entry) => typeof entry === 'string' ? entry : '').join('')
        : typeof childValue === 'string' ? childValue : '';
      if (text) result.push({ name: '#text', attributes: {}, children: [], text });
      continue;
    }
    if (elementName === '__comment' || elementName.startsWith('?')) continue;
    const rawAttributes = record[':@'];
    const attributes: Record<string, string> = {};
    if (rawAttributes && typeof rawAttributes === 'object') {
      for (const [key, raw] of Object.entries(rawAttributes)) {
        if (typeof raw === 'string') attributes[key.slice(2)] = raw;
      }
    }
    try {
      result.push({ name: elementName, attributes, children: orderedToNodes(childValue, label), text: '' });
    } catch (error) {
      if (error instanceof Error) throw error;
      return fail(label, 'XML 结构无法安全解析。');
    }
  }
  return result;
}

// 预先限制 XML 标记数量和嵌套深度，拒绝 DTD 与实体声明。
function assertXmlComplexity(xml: string, limits: SafeXmlLimits): void {
  if (new TextEncoder().encode(xml).byteLength > limits.bytes) fail(limits.label, `XML 文件超过 ${Math.floor(limits.bytes / (1024 * 1024))} MiB 上限。`);
  if (/<!\s*(?:DOCTYPE|ENTITY)\b/i.test(xml)) fail(limits.label, '包含不支持的 XML 文档类型声明。');

  let offset = 0;
  let depth = 0;
  let tags = 0;
  while (offset < xml.length) {
    const opening = xml.indexOf('<', offset);
    if (opening < 0) break;
    if (xml.startsWith('<!--', opening)) {
      const close = xml.indexOf('-->', opening + 4);
      if (close < 0) fail(limits.label, 'XML 注释未闭合。');
      offset = close + 3;
      continue;
    }
    if (xml.startsWith('<![CDATA[', opening)) {
      const close = xml.indexOf(']]>', opening + 9);
      if (close < 0) fail(limits.label, 'XML 数据未闭合。');
      offset = close + 3;
      continue;
    }
    if (xml.startsWith('<?', opening)) {
      const close = xml.indexOf('?>', opening + 2);
      if (close < 0) fail(limits.label, 'XML 声明未闭合。');
      offset = close + 2;
      continue;
    }
    if (xml.startsWith('<!', opening)) fail(limits.label, '包含不支持的 XML 声明。');

    let quote = '';
    let end = opening + 1;
    for (; end < xml.length; end += 1) {
      const character = xml[end]!;
      if (quote) {
        if (character === quote) quote = '';
      } else if (character === '"' || character === "'") {
        quote = character;
      } else if (character === '>') {
        break;
      }
    }
    if (end >= xml.length) fail(limits.label, 'XML 标签未闭合。');

    const token = xml.slice(opening + 1, end).trim();
    tags += 1;
    if (tags > limits.tags) fail(limits.label, `XML 结构超过 ${limits.tags.toLocaleString('zh-CN')} 个标签上限。`);
    if (token.startsWith('/')) {
      depth -= 1;
      if (depth < 0) fail(limits.label, 'XML 标签不匹配。');
    } else if (!token.endsWith('/')) {
      depth += 1;
      if (depth > limits.depth) fail(limits.label, `XML 嵌套超过 ${limits.depth} 层上限。`);
    }
    offset = end + 1;
  }
  if (depth !== 0) fail(limits.label, 'XML 标签不匹配。');
}

// 只返回无活动内容的 XML 节点，不创建 DOM 或执行任何节点。
export function parseSafeXml(xml: string, limits: SafeXmlLimits): SafeXmlNode[] {
  assertXmlComplexity(xml, limits);
  const validation = XMLValidator.validate(xml);
  if (validation !== true) fail(limits.label, 'XML 格式无效。');
  const parser = new XMLParser({
    preserveOrder: true,
    ignoreAttributes: false,
    attributeNamePrefix: '@_',
    textNodeName: '#text',
    cdataPropName: '__cdata',
    trimValues: false,
    parseTagValue: false,
    processEntities: {
      maxEntitySize: 1024,
      maxTotalExpansions: limits.bytes,
      maxExpansionDepth: 8,
      maxExpandedLength: limits.bytes,
    },
    maxNestedTags: limits.depth,
  });
  try {
    return orderedToNodes(parser.parse(xml), limits.label);
  } catch (error) {
    if (error instanceof Error && error.message.startsWith(`${limits.label} `)) throw error;
    return fail(limits.label, 'XML 无法安全解析。');
  }
}

// 比较 XML 名称时忽略命名空间前缀。
export function safeXmlLocalName(name: string): string {
  return name.slice(name.lastIndexOf(':') + 1).toLocaleLowerCase('en-US');
}

// 按文档顺序查找指定元素。
export function findSafeXmlNodes(nodes: SafeXmlNode[], name: string): SafeXmlNode[] {
  const matches: SafeXmlNode[] = [];
  const stack = [...nodes].reverse();
  const target = name.toLocaleLowerCase('en-US');
  while (stack.length > 0) {
    const node = stack.pop()!;
    if (safeXmlLocalName(node.name) === target) matches.push(node);
    for (let index = node.children.length - 1; index >= 0; index -= 1) stack.push(node.children[index]!);
  }
  return matches;
}

// 按命名空间无关的名称读取 XML 属性。
export function safeXmlAttribute(node: SafeXmlNode, name: string): string | undefined {
  const target = name.toLocaleLowerCase('en-US');
  const key = Object.keys(node.attributes).find((candidate) => safeXmlLocalName(candidate) === target);
  return key ? node.attributes[key] : undefined;
}

// 汇总节点文字并限制读取结果大小。
export function safeXmlText(node: SafeXmlNode, maxCharacters: number, label: string): string {
  const parts: string[] = [];
  const stack = [node];
  let length = 0;
  while (stack.length > 0) {
    const current = stack.pop()!;
    if (current.name === '#text') {
      length += current.text.length;
      if (length > maxCharacters) fail(label, `XML 文本超过 ${maxCharacters.toLocaleString('zh-CN')} 字符上限。`);
      parts.push(current.text);
    } else {
      for (let index = current.children.length - 1; index >= 0; index -= 1) stack.push(current.children[index]!);
    }
  }
  return parts.join('');
}
