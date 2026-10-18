// 提供只读结构化 DOCX 阅读与显式分页导航。
import { useEffect, useMemo, useState } from 'react';
import { AlertTriangle, ArrowDownToLine, ChevronLeft, ChevronRight, LoaderCircle, RefreshCw } from 'lucide-react';
import type { DocxBlock, DocxDocument, DocxInline } from './docxParser';
import './DocxReader.css';

interface DocxReaderProps { path: string; name: string; url: string; downloadUrl: string; }
type LoadState = { status: 'loading' } | { status: 'ready'; document: DocxDocument } | { status: 'error'; message: string };
const MAX_DOCX_BYTES = 32 * 1024 * 1024;
const MAX_PARSE_TIME_MS = 15_000;

async function readResponse(response: Response): Promise<ArrayBuffer> {
  const declared = Number(response.headers.get('content-length'));
  if (Number.isFinite(declared) && declared > MAX_DOCX_BYTES) {
    await response.body?.cancel();
    throw new Error('DOCX 超过 32 MiB 阅读上限。可下载原件后使用其他阅读器。');
  }
  const reader = response.body?.getReader();
  if (!reader) {
    const buffer = await response.arrayBuffer();
    if (buffer.byteLength > MAX_DOCX_BYTES) throw new Error('DOCX 超过 32 MiB 阅读上限。');
    return buffer;
  }
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > MAX_DOCX_BYTES) {
        await reader.cancel().catch(() => undefined);
        throw new Error('DOCX 超过 32 MiB 阅读上限。可下载原件后使用其他阅读器。');
      }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  const buffer = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) { buffer.set(chunk, offset); offset += chunk.byteLength; }
  return buffer.buffer;
}

function renderInline(runs: DocxInline[], imageUrls: Map<string, string>): React.ReactNode[] {
  return runs.map((run, index) => {
    if (run.type === 'break') return <br key={index} />;
    if (run.type === 'image') {
      const src = imageUrls.get(run.id);
      return src ? <img key={index} src={src} alt={run.alt || '文档图片'} loading="lazy" /> : null;
    }
    if (run.type === 'unsupported-image') return <span className="reader-docx-image-placeholder" key={index} role="img" aria-label={run.alt}>[{run.alt}]</span>;
    let node: React.ReactNode = run.value;
    for (const mark of run.marks) {
      if (mark === 'bold') node = <strong>{node}</strong>;
      else if (mark === 'italic') node = <em>{node}</em>;
      else if (mark === 'underline') node = <u>{node}</u>;
      else node = <s>{node}</s>;
    }
    return <span key={index}>{node}</span>;
  });
}

function renderBlocks(blocks: DocxBlock[], imageUrls: Map<string, string>): React.ReactNode[] {
  return blocks.map((block, index) => {
    if (block.type === 'paragraph') {
      const heading = block.headingLevel;
      const content = renderInline(block.runs, imageUrls);
      const props = { key: block.id || index, id: block.id, className: 'reader-docx-paragraph' };
      if (heading) {
        const Tag = `h${Math.min(6, heading)}` as 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6';
        return <Tag {...props}>{content}</Tag>;
      }
      return <p {...props}>{content}</p>;
    }
    return (
      <div className="reader-docx-table-wrap" key={`table-${index}`} role="region" aria-label="文档表格，可横向滚动" tabIndex={0}>
        <table className="reader-docx-table"><tbody>
          {block.rows.map((row, rowIndex) => (
            <tr key={rowIndex}>{row.cells.map((cell, cellIndex) => (
              <td key={cellIndex}>{renderBlocks(cell.blocks, imageUrls)}</td>
            ))}</tr>
          ))}
        </tbody></table>
      </div>
    );
  });
}

// 流式读取原件，Worker 解析过程可取消且有时间上限。
export function DocxReader({ path, name, url, downloadUrl }: DocxReaderProps) {
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<LoadState>({ status: 'loading' });
  const [pageIndex, setPageIndex] = useState(0);
  const [selectedHeadingId, setSelectedHeadingId] = useState('');
  const [imageUrls, setImageUrls] = useState<Map<string, string>>(new Map());

  useEffect(() => {
    const controller = new AbortController();
    let current = true;
    let worker: Worker | null = null;
    setState({ status: 'loading' });
    async function load() {
      try {
        const response = await fetch(url, { signal: controller.signal, cache: 'no-store' });
        if (!response.ok) throw new Error(`读取 DOCX 失败（HTTP ${response.status}）。`);
        const buffer = await readResponse(response);
        if (!current) return;
        worker = new Worker(new URL('./docx.worker.ts', import.meta.url), { type: 'module' });
        const document = await new Promise<DocxDocument>((resolve, reject) => {
          let finished = false;
          const timeout = window.setTimeout(() => finish(new Error('DOCX 解析超时。可下载原件后使用其他阅读器。')), MAX_PARSE_TIME_MS);
          const finish = (error?: Error, result?: DocxDocument) => {
            if (finished) return;
            finished = true;
            window.clearTimeout(timeout);
            if (error) reject(error); else if (result) resolve(result);
            worker?.terminate();
            worker = null;
          };
          worker!.onmessage = (event: MessageEvent<{ type: string; document?: DocxDocument; message?: string }>) => {
            if (event.data.type === 'success' && event.data.document) finish(undefined, event.data.document);
            else finish(new Error(event.data.message || 'DOCX 无法安全解析。'));
          };
          worker!.onerror = () => finish(new Error('DOCX 解析线程异常退出。'));
          controller.signal.addEventListener('abort', () => finish(new DOMException('读取已取消。', 'AbortError')), { once: true });
          worker!.postMessage({ buffer }, [buffer]);
        });
        if (current) { setState({ status: 'ready', document }); setPageIndex(0); setSelectedHeadingId(''); }
      } catch (error) {
        if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
        setState({ status: 'error', message: error instanceof Error ? error.message : '无法安全读取 DOCX。' });
      }
    }
    void load();
    return () => { current = false; controller.abort(); worker?.terminate(); };
  }, [attempt, url]);

  const document = state.status === 'ready' ? state.document : null;
  useEffect(() => {
    if (!document) { setImageUrls(new Map()); return; }
    const urls = new Map<string, string>();
    for (const image of document.images) urls.set(image.id, URL.createObjectURL(new Blob([image.bytes], { type: image.mimeType })));
    setImageUrls(urls);
    return () => { for (const objectUrl of urls.values()) URL.revokeObjectURL(objectUrl); };
  }, [document]);

  const currentPage = document?.pages[pageIndex];
  const headingOptions = useMemo(() => document?.headings ?? [], [document]);
  useEffect(() => {
    if (!selectedHeadingId) return;
    window.requestAnimationFrame(() => window.document.getElementById(selectedHeadingId)?.scrollIntoView({ block: 'start' }));
  }, [pageIndex, selectedHeadingId]);

  return (
    <div className="reader-docx" data-document-path={path}>
      {state.status === 'loading' && <div className="reader-docx-state"><LoaderCircle className="reader-spinner" size={20} /><span>正在安全解析 DOCX…</span></div>}
      {state.status === 'error' && <div className="reader-docx-state reader-docx-error"><AlertTriangle size={20} /><strong>无法阅读此 DOCX</strong><p>{state.message}</p><div className="reader-docx-actions"><button type="button" className="reader-action-button" onClick={() => setAttempt((value) => value + 1)}><RefreshCw size={16} />重试</button><a className="reader-action-button" href={downloadUrl} download={name}><ArrowDownToLine size={16} />下载原件</a></div></div>}
      {state.status === 'ready' && document && currentPage && <>
        <div className="reader-docx-controls">
          <div className="reader-docx-nav">
            <button type="button" className="reader-action-button" aria-label="上一页" disabled={pageIndex === 0} onClick={() => { setSelectedHeadingId(''); setPageIndex((value) => Math.max(0, value - 1)); }}><ChevronLeft size={17} /></button>
            <span>第 {pageIndex + 1} / {document.pages.length} 页</span>
            <button type="button" className="reader-action-button" aria-label="下一页" disabled={pageIndex + 1 >= document.pages.length} onClick={() => { setSelectedHeadingId(''); setPageIndex((value) => Math.min(document.pages.length - 1, value + 1)); }}><ChevronRight size={17} /></button>
          </div>
          {headingOptions.length > 0 && <label className="reader-docx-toc"><span>目录</span><select aria-label="标题目录" value={selectedHeadingId} onChange={(event) => {
            const heading = headingOptions.find((item) => item.id === event.target.value);
            if (heading) { setSelectedHeadingId(heading.id); setPageIndex(heading.pageIndex); }
          }}>
            <option value="" disabled>跳转到标题</option>
            {headingOptions.map((heading) => <option key={heading.id} value={heading.id}>{'　'.repeat(heading.level - 1)}{heading.title}</option>)}
          </select></label>}
        </div>
        <p className="reader-docx-note">按文档中的分页符导航；页面自动换行与 Word 排版可能不同。</p>
        <div className="reader-docx-scroll" aria-label={`${name}，第 ${pageIndex + 1} 页`}>
          <article className="reader-docx-page" key={pageIndex}>
            {renderBlocks(currentPage.blocks, imageUrls)}
            {currentPage.blocks.length === 0 && <p className="reader-docx-empty">此页没有可显示的正文。</p>}
          </article>
        </div>
      </>}
    </div>
  );
}
