// 提供只读 EPUB 章节阅读、目录导航和会话内位置恢复。
import { useEffect, useRef, useState } from 'react';
import type { CSSProperties, ElementType, ReactNode } from 'react';
import { AlertTriangle, ArrowDownToLine, ChevronLeft, ChevronRight, LoaderCircle, Minus, Plus, RefreshCw } from 'lucide-react';
import type { EpubBook, EpubSafeNode, EpubSafeTag } from './epubParser';
import { adjacentEpubChapter, parseEpubPosition } from './epubPosition';

interface EpubReaderProps {
  path: string;
  name: string;
  url: string;
  downloadUrl: string;
}

interface ReaderPosition {
  index: number;
  scrollTop: number;
  fontSize: number;
}

type LoadState = { status: 'loading' } | { status: 'ready'; book: EpubBook } | { status: 'error'; message: string };

const MAX_EPUB_BYTES = 32 * 1024 * 1024;
const MAX_PARSE_TIME_MS = 15_000;
const FONT_MIN = 15;
const FONT_MAX = 27;
const FONT_STEP = 2;

// 区分不同文件在当前会话中的阅读位置。
function positionKey(path: string): string {
  return `filesage:epub-position:${path}`;
}

// 读取并校验浏览器会话中的阅读位置。
function readPosition(path: string, chapterCount: number): ReaderPosition | null {
  try {
    const value = sessionStorage.getItem(positionKey(path));
    return parseEpubPosition(value, chapterCount);
  } catch {
    return null;
  }
}

// 存储当前章节、滚动位置与字号。
function writePosition(path: string, value: ReaderPosition): void {
  try {
    sessionStorage.setItem(positionKey(path), JSON.stringify(value));
  } catch {
    // 浏览器禁用会话存储时仍可正常阅读。
  }
}

// 流式下载并在达到 32 MiB 时停止读取。
async function readEpubResponse(response: Response): Promise<ArrayBuffer> {
  const contentLength = Number(response.headers.get('content-length'));
  if (Number.isFinite(contentLength) && contentLength > MAX_EPUB_BYTES) {
    await response.body?.cancel();
    throw new Error('EPUB 超过 32 MiB 读取上限。可下载原件后使用其他阅读器。');
  }

  const reader = response.body?.getReader();
  if (!reader) {
    const buffer = await response.arrayBuffer();
    if (buffer.byteLength > MAX_EPUB_BYTES) throw new Error('EPUB 超过 32 MiB 读取上限。可下载原件后使用其他阅读器。');
    return buffer;
  }

  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > MAX_EPUB_BYTES) {
        await reader.cancel().catch(() => undefined);
        throw new Error('EPUB 超过 32 MiB 读取上限。可下载原件后使用其他阅读器。');
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes.buffer;
}

const safeElements: Record<EpubSafeTag, ElementType> = {
  p: 'p', h1: 'h1', h2: 'h2', h3: 'h3', h4: 'h4', h5: 'h5', h6: 'h6',
  blockquote: 'blockquote', pre: 'pre', code: 'code', ul: 'ul', ol: 'ol', li: 'li',
  table: 'table', thead: 'thead', tbody: 'tbody', tr: 'tr', th: 'th', td: 'td',
  em: 'em', strong: 'strong', b: 'b', i: 'i', u: 'u', s: 's', del: 'del',
  sub: 'sub', sup: 'sup', small: 'small', span: 'span', div: 'div', br: 'br', hr: 'hr',
};

// 只把解析后的白名单节点映射为 React 元素。
function renderNodes(nodes: EpubSafeNode[], prefix = 'node'): ReactNode[] {
  return nodes.map((node, index) => {
    const key = `${prefix}-${index}`;
    if (node.type === 'text') return node.value;
    const Element = safeElements[node.tag];
    if (node.tag === 'br' || node.tag === 'hr') return <Element key={key} />;
    return <Element key={key}>{renderNodes(node.children, key)}</Element>;
  });
}

// 在 Worker 中读取 EPUB，并提供只读章节浏览。
export function EpubReader({ path, name, url, downloadUrl }: EpubReaderProps) {
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<LoadState>({ status: 'loading' });
  const [chapterIndex, setChapterIndex] = useState(0);
  const [fontSize, setFontSize] = useState(18);
  const scrollRef = useRef<HTMLDivElement>(null);
  const workerRef = useRef<Worker | null>(null);
  const restoreRef = useRef<ReaderPosition | null>(null);
  const saveTimerRef = useRef<number | null>(null);
  const positionRef = useRef<ReaderPosition>({ index: 0, scrollTop: 0, fontSize: 18 });
  const readyBook = state.status === 'ready' ? state.book : null;

  useEffect(() => {
    const controller = new AbortController();
    let current = true;
    setState({ status: 'loading' });
    let worker: Worker | null = null;

    async function load(): Promise<void> {
      try {
        const response = await fetch(url, { signal: controller.signal, cache: 'no-store' });
        if (!response.ok) {
          throw new Error(`读取 EPUB 失败（HTTP ${response.status}）。`);
        }
        const buffer = await readEpubResponse(response);
        if (!current) return;
        worker = new Worker(new URL('./epub.worker.ts', import.meta.url), { type: 'module' });
        workerRef.current = worker;

        const book = await new Promise<EpubBook>((resolve, reject) => {
          let finished = false;
          let timeoutId: number | null = null;
          const finish = () => {
            if (finished) return;
            finished = true;
            if (timeoutId !== null) window.clearTimeout(timeoutId);
            controller.signal.removeEventListener('abort', rejectAborted);
            worker?.terminate();
            if (workerRef.current === worker) workerRef.current = null;
          };
          const rejectAborted = () => {
            finish();
            reject(new DOMException('读取已取消。', 'AbortError'));
          };
          controller.signal.addEventListener('abort', rejectAborted, { once: true });
          timeoutId = window.setTimeout(() => {
            finish();
            reject(new Error('EPUB 解析超过 15 秒，已停止读取。可重试或下载原件。'));
          }, MAX_PARSE_TIME_MS);
          worker!.onmessage = (event: MessageEvent<{ type?: string; book?: EpubBook; message?: string }>) => {
            if (finished) return;
            finish();
            if (event.data.type === 'success' && event.data.book) resolve(event.data.book);
            else reject(new Error(event.data.message || 'EPUB 无法安全解析。'));
          };
          worker!.onerror = () => {
            if (finished) return;
            finish();
            reject(new Error('EPUB 解析线程发生错误。可重试或下载原件。'));
          };
          worker!.onmessageerror = () => {
            if (finished) return;
            finish();
            reject(new Error('EPUB 解析结果无效。可重试或下载原件。'));
          };
          worker!.postMessage({ buffer }, [buffer]);
        });

        if (!current) return;
        const saved = readPosition(path, book.chapters.length);
        const restoredIndex = saved?.index ?? 0;
        restoreRef.current = saved ? { ...saved, index: restoredIndex } : null;
        positionRef.current = { index: restoredIndex, scrollTop: saved?.scrollTop ?? 0, fontSize: saved?.fontSize ?? 18 };
        setChapterIndex(restoredIndex);
        setFontSize(saved?.fontSize ?? 18);
        setState({ status: 'ready', book });
      } catch (error) {
        if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
        setState({ status: 'error', message: error instanceof Error ? error.message : 'EPUB 读取失败。可重试或下载原件。' });
      }
    }

    void load();
    return () => {
      current = false;
      controller.abort();
      worker?.terminate();
      if (workerRef.current === worker) workerRef.current = null;
    };
  }, [attempt, path, url]);

  useEffect(() => {
    if (!readyBook) return;
    const restore = restoreRef.current;
    if (restore && restore.index !== chapterIndex) return;
    const top = restore ? restore.scrollTop : 0;
    const frame = window.requestAnimationFrame(() => {
      if (scrollRef.current) scrollRef.current.scrollTop = top;
      restoreRef.current = null;
    });
    return () => window.cancelAnimationFrame(frame);
  }, [chapterIndex, readyBook]);

  useEffect(() => () => {
    if (saveTimerRef.current !== null) window.clearTimeout(saveTimerRef.current);
    writePosition(path, positionRef.current);
  }, [path]);

  // 合并短时间内的连续滚动写入。
  function saveSoon(): void {
    if (saveTimerRef.current !== null) window.clearTimeout(saveTimerRef.current);
    saveTimerRef.current = window.setTimeout(() => writePosition(path, positionRef.current), 250);
  }

  // 切章后从新章节顶部开始阅读。
  function selectChapter(index: number): void {
    if (!readyBook || index < 0 || index >= readyBook.chapters.length) return;
    restoreRef.current = null;
    positionRef.current = { ...positionRef.current, index, scrollTop: 0 };
    setChapterIndex(index);
    saveSoon();
  }

  // 将字号限制在可读范围内。
  function updateFontSize(value: number): void {
    const next = Math.min(FONT_MAX, Math.max(FONT_MIN, value));
    positionRef.current = { ...positionRef.current, fontSize: next };
    setFontSize(next);
    saveSoon();
  }

  const chapter = readyBook?.chapters[chapterIndex];

  if (state.status === 'loading') {
    return <div className="reader-state reader-epub-state" role="status"><LoaderCircle className="reader-spinner" size={24} aria-hidden="true" /><strong>正在读取 EPUB…</strong><span>在独立线程中检查目录并提取正文；不会执行书内脚本或加载外部资源。</span></div>;
  }

  if (state.status === 'error') {
    return (
      <div className="reader-state reader-state-error reader-epub-state" role="alert">
        <AlertTriangle size={24} aria-hidden="true" />
        <strong>EPUB 无法预览</strong>
        <p>{state.message}</p>
        <div className="reader-state-actions">
          <button type="button" className="reader-retry" onClick={() => setAttempt((value) => value + 1)}><RefreshCw size={16} aria-hidden="true" />重新加载</button>
          <a href={downloadUrl} download={name} className="reader-download reader-download-secondary"><ArrowDownToLine size={16} aria-hidden="true" />下载原件</a>
        </div>
      </div>
    );
  }

  if (!chapter) return null;

  return (
    <section className="reader-epub" aria-label={`${name} EPUB 阅读器`}>
      <div className="reader-epub-controls">
        <div className="reader-epub-navigation">
          <button className="reader-action-button" type="button" onClick={() => selectChapter(adjacentEpubChapter(chapterIndex, -1, readyBook!.chapters.length))} disabled={chapterIndex === 0} aria-label="上一章">
            <ChevronLeft size={17} aria-hidden="true" /><span>上一章</span>
          </button>
          <label className="reader-epub-toc-label" htmlFor="reader-epub-toc">目录</label>
          <select id="reader-epub-toc" className="reader-epub-toc" value={chapterIndex} onChange={(event) => selectChapter(Number(event.currentTarget.value))}>
            {readyBook!.chapters.map((item, index) => <option value={index} key={item.id}>{index + 1}. {item.title}</option>)}
          </select>
          <button className="reader-action-button" type="button" onClick={() => selectChapter(adjacentEpubChapter(chapterIndex, 1, readyBook!.chapters.length))} disabled={chapterIndex + 1 >= readyBook!.chapters.length} aria-label="下一章">
            <span>下一章</span><ChevronRight size={17} aria-hidden="true" />
          </button>
        </div>
        <div className="reader-epub-font-controls" role="group" aria-label="调整正文字号">
          <button className="reader-action-button" type="button" onClick={() => updateFontSize(fontSize - FONT_STEP)} disabled={fontSize <= FONT_MIN} aria-label="缩小字号"><Minus size={16} aria-hidden="true" /></button>
          <span aria-live="polite">{fontSize}px</span>
          <button className="reader-action-button" type="button" onClick={() => updateFontSize(fontSize + FONT_STEP)} disabled={fontSize >= FONT_MAX} aria-label="放大字号"><Plus size={16} aria-hidden="true" /></button>
        </div>
      </div>
      <div
        className="reader-epub-scroll"
        ref={scrollRef}
        onScroll={(event) => {
          positionRef.current = { index: chapterIndex, scrollTop: event.currentTarget.scrollTop, fontSize };
          saveSoon();
        }}
        tabIndex={0}
        aria-label={`${chapter.title} 正文`}
      >
        <article className="reader-epub-page" style={{ '--epub-font-size': `${fontSize}px` } as CSSProperties}>
          <h1>{chapter.title}</h1>
          <div className="reader-epub-body">{renderNodes(chapter.content)}</div>
        </article>
      </div>
      <div className="reader-epub-status" role="status">第 {chapterIndex + 1} / {readyBook!.chapters.length} 章 · 位置仅保存在当前会话</div>
    </section>
  );
}
