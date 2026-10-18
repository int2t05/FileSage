import assert from 'node:assert/strict';
import test from 'node:test';
import { createElement } from 'react';
import ReactMarkdown from 'react-markdown';
import { renderToStaticMarkup } from 'react-dom/server';
import remarkGfm from 'remark-gfm';
import {
  createMarkdownHeadingPlugin,
  extractMarkdownHeadings,
  markdownURLTransform,
  resolveMarkdownImagePath,
} from '../src/features/reader/markdown.ts';

function renderMarkdown(source, path = 'notes/readme.md') {
  const headings = extractMarkdownHeadings(source);
  const components = {
    img: ({ src, alt }) => src
      ? createElement('img', { src, alt: alt ?? '' })
      : createElement('span', null, `图片未显示：${alt || '无安全的本地路径'}`),
  };
  const html = renderToStaticMarkup(createElement(ReactMarkdown, {
    remarkPlugins: [remarkGfm],
    rehypePlugins: [createMarkdownHeadingPlugin(headings)],
    urlTransform: (url, propertyName) => markdownURLTransform(path, url, propertyName),
    components,
    skipHtml: true,
  }, source));
  return { html, headings };
}

// 标题目录必须与阅读器中的锚点一致，并跳过代码块。
test('Markdown 标题目录支持中文、重复标题和 GFM 标题内容', () => {
  const source = '# 中文标题\n\n## 重复项\n\n## 重复项\n\n```md\n# 代码标题\n```';
  const { html, headings } = renderMarkdown(source);

  assert.deepEqual(headings.map(({ id, label, level }) => ({ id, label, level })), [
    { id: '中文标题', label: '中文标题', level: 1 },
    { id: '重复项', label: '重复项', level: 2 },
    { id: '重复项-1', label: '重复项', level: 2 },
  ]);
  assert.match(html, /<h1 id="中文标题" tabindex="-1">中文标题<\/h1>/);
  assert.match(html, /<h2 id="重复项-1" tabindex="-1">重复项<\/h2>/);
  assert.doesNotMatch(html, /id="代码标题"/);
});

// 表格、任务清单和删除线按常见 GFM 语法渲染。
test('Markdown 阅读器支持常用 GFM 内容', () => {
  const { html } = renderMarkdown('| 名称 | 状态 |\n| --- | --- |\n| 文件 | ~~待办~~ |\n\n- [x] 已完成');
  assert.match(html, /<table>/);
  assert.match(html, /<del>待办<\/del>/);
  assert.match(html, /type="checkbox" disabled="" checked=""/);
});

// 图片只映射到根目录内的预览 API，外部资源与越界路径不加载。
test('Markdown 图片限制在管理根目录内', () => {
  assert.equal(resolveMarkdownImagePath('文档/说明.md', '../图片/封面.png'), '图片/封面.png');
  assert.equal(resolveMarkdownImagePath('文档/说明.md', '/共享/封面.png'), '共享/封面.png');
  assert.equal(resolveMarkdownImagePath('说明.md', '../../outside.png'), null);
  assert.equal(resolveMarkdownImagePath('说明.md', 'https://example.com/image.png'), null);
  assert.equal(resolveMarkdownImagePath('说明.md', 'javascript:alert(1)'), null);
  assert.equal(resolveMarkdownImagePath('说明.md', '%2F%2Fevil.example/image.png'), null);
  assert.equal(markdownURLTransform('文档/说明.md', '../图片/封面 图.png', 'src'), '/api/v1/files/preview?path=%E5%9B%BE%E7%89%87%2F%E5%B0%81%E9%9D%A2+%E5%9B%BE.png');
});

// 原始 HTML 和危险链接不得进入可执行 DOM。
test('Markdown 渲染跳过原始 HTML、危险链接和远程图片', () => {
  const { html } = renderMarkdown([
    '<script>window.pwned = true</script>',
    '<img src="x" onerror="alert(1)">',
    '[危险链接](javascript:alert(1))',
    '![远程图片](https://example.com/image.png)',
    '![本地图片](../图片/封面.png)',
  ].join('\n\n'));

  assert.doesNotMatch(html, /<script|onerror=|javascript:/i);
  assert.doesNotMatch(html, /src="https:\/\/example\.com/);
  assert.match(html, /图片未显示：远程图片/);
  assert.match(html, /src="\/api\/v1\/files\/preview\?path=%E5%9B%BE%E7%89%87%2F%E5%B0%81%E9%9D%A2\.png"/);
});
