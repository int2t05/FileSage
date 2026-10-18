// 本组件以安全的只读方式渲染 Markdown 并提供标题目录。
import { useMemo, useRef, type MouseEvent } from 'react';
import ReactMarkdown, { defaultUrlTransform, type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { createMarkdownHeadingPlugin, extractMarkdownHeadings, markdownURLTransform } from './markdown';
import './MarkdownPreview.css';

interface MarkdownPreviewProps {
  path: string;
  content: string;
  allowLocalImages?: boolean;
}

// 以惰性加载的 Markdown 管线生成目录和安全预览。
export function MarkdownPreview({ path, content, allowLocalImages = true }: MarkdownPreviewProps) {
  const contentRef = useRef<HTMLDivElement>(null);
  const headings = useMemo(() => extractMarkdownHeadings(content), [content]);
  const urlTransform = useMemo(() => (url: string, propertyName: string) => {
    if (!allowLocalImages && propertyName === 'src') return '';
    return allowLocalImages ? markdownURLTransform(path, url, propertyName) : defaultUrlTransform(url);
  }, [allowLocalImages, path]);
  const components = useMemo<Components>(() => ({
    img: ({ src, alt, title }) => src
      ? <img src={src} alt={alt ?? ''} title={title} loading="lazy" referrerPolicy="no-referrer" />
      : <span className="reader-markdown-image-blocked">图片未显示：{alt || '无安全的本地路径'}</span>,
  }), []);

  // 在阅读区内定位标题，并让键盘焦点跟随目录选择。
  function visitHeading(event: MouseEvent<HTMLAnchorElement>, id: string) {
    const heading = contentRef.current?.querySelector<HTMLElement>(`[id="${CSS.escape(id)}"]`);
    if (!heading) return;
    event.preventDefault();
    heading.focus({ preventScroll: true });
    heading.scrollIntoView({ behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start' });
    const toc = event.currentTarget.closest('details');
    if (window.matchMedia('(max-width: 760px)').matches && toc) toc.open = false;
  }

  return (
    <div className="reader-markdown-preview">
      {headings.length > 0 && (
        <details className="reader-markdown-toc">
          <summary>目录（{headings.length}）</summary>
          <nav aria-label="Markdown 标题目录">
            <ol>
              {headings.map((heading) => (
                <li className={`reader-markdown-toc-level-${heading.level}`} key={heading.id}>
                  <a href={`#${heading.id}`} onClick={(event) => visitHeading(event, heading.id)}>{heading.label}</a>
                </li>
              ))}
            </ol>
          </nav>
        </details>
      )}
      <article ref={contentRef} className="reader-markdown-body" aria-label="Markdown 正文">
        <ReactMarkdown
          remarkPlugins={[remarkGfm]}
          rehypePlugins={[createMarkdownHeadingPlugin(headings)]}
          urlTransform={urlTransform}
          components={components}
          skipHtml
        >
          {content}
        </ReactMarkdown>
      </article>
    </div>
  );
}
