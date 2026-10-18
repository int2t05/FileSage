/** 只读 PDF 阅读器：按需渲染当前页，提供查找、书签与原件下载。 */
import { useCallback, useEffect, useRef, useState, type FormEvent, type KeyboardEvent, type TouchEvent } from 'react';
import {
  ArrowDownToLine,
  BookOpen,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Maximize2,
  Minimize2,
  LoaderCircle,
  Search,
  X,
  ZoomIn,
  ZoomOut,
} from 'lucide-react';
import type {
  PDFDocumentLoadingTask,
  PDFDocumentProxy,
  PDFPageProxy,
  RenderTask,
  TextLayer,
} from 'pdfjs-dist';
import { presentationKeyAction, slideSwipeDirection } from './presentationControls';
import './PdfReader.css';

export interface PdfReaderProps {
  /** 同源、支持 Range 的 PDF 预览地址。 */
  url: string;
  /** 原件下载地址。 */
  downloadUrl: string;
  /** 用于下载提示和无障碍标签的文件名。 */
  fileName: string;
  /** 仅 PPTX 启用幻灯片控制。 */
  presentationMode?: boolean;
}

type ReaderState =
  | { status: 'loading' }
  | { status: 'password'; message: string }
  | { status: 'error'; message: string }
  | { status: 'ready'; document: PDFDocumentProxy };

type OutlineItem = NonNullable<Awaited<ReturnType<PDFDocumentProxy['getOutline']>>>[number];
type PasswordUpdater = (password: string) => void;
type TextIndex = { text: string; owners: number[] };
type ActiveMatch = { query: string; page: number; offset: number };

const MIN_ZOOM = 50;
const MAX_ZOOM = 300;
const MAX_CANVAS_PIXELS = 20_000_000;
const MAX_CACHED_TEXT_PAGES = 20;

// 将 PDF.js 错误转成可处理的中文提示。
function safeErrorMessage(error: unknown): string {
  if (getErrorName(error) === 'InvalidPDFException') {
    return 'PDF 文件损坏或格式不受支持。可下载原件后使用其他阅读器检查。';
  }
  if (getErrorName(error) === 'MissingPDFException') return '找不到此 PDF 文件，可能已被移动或删除。';
  if (getErrorName(error) === 'ResponseException' || getErrorName(error) === 'UnexpectedResponseException') {
    return 'PDF 读取请求失败。请检查文件是否仍存在，或稍后重试。';
  }
  if (getErrorName(error) === 'PasswordException') return '此 PDF 需要正确的打开密码。';
  return 'PDF 无法读取，可能已损坏或包含当前阅读器不支持的内容。可下载原件后使用其他阅读器打开。';
}

// 读取 PDF.js 错误类型。
function getErrorName(error: unknown): string {
  return error instanceof Error ? error.name : '';
}

// 统一大小写与空白，支持跨行查找。
function normalizeForSearch(text: string): string {
  return text.normalize('NFKC').toLocaleLowerCase().replace(/\s+/gu, ' ');
}

// 过滤 PDF.js 内容项中的文本条目。
function isTextItem(item: unknown): item is { str: string; hasEOL: boolean } {
  return typeof item === 'object' && item !== null && 'str' in item && typeof item.str === 'string';
}

// 建立文本到文字层节点的偏移映射。
function makeTextIndex(items: readonly unknown[]): TextIndex {
  let text = '';
  const owners: number[] = [];

  items.forEach((item, itemIndex) => {
    if (!isTextItem(item)) return;
    const normalized = normalizeForSearch(item.str);
    text += normalized;
    for (let index = 0; index < normalized.length; index += 1) owners.push(itemIndex);
    if (item.hasEOL) {
      text += '\n';
      owners.push(itemIndex);
    }
  });

  return { text, owners };
}

// 读取 PDF 书签的内部跳转目标。
function outlineDestination(item: OutlineItem): string | Array<unknown> | null {
  return item.dest as string | Array<unknown> | null;
}

/** 按同源地址读取 PDF，整个阅读流程不写入原件。 */
export function PdfReader({ url, downloadUrl, fileName, presentationMode = false }: PdfReaderProps) {
  const [readerState, setReaderState] = useState<ReaderState>({ status: 'loading' });
  const [pageNumber, setPageNumber] = useState(1);
  const [pageDraft, setPageDraft] = useState('1');
  const [zoom, setZoom] = useState(100);
  const [viewportWidth, setViewportWidth] = useState(0);
  const [outline, setOutline] = useState<OutlineItem[]>([]);
  const [password, setPassword] = useState('');
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState('');
  const [searchStatus, setSearchStatus] = useState('');
  const [searching, setSearching] = useState(false);
  const [activeMatch, setActiveMatch] = useState<ActiveMatch | null>(null);
  const [pageError, setPageError] = useState('');
  const [rendering, setRendering] = useState(false);
  const [isFullscreen, setIsFullscreen] = useState(false);
  const [fullscreenMessage, setFullscreenMessage] = useState('');

  const loadingTaskRef = useRef<PDFDocumentLoadingTask | null>(null);
  const pdfjsRef = useRef<typeof import('pdfjs-dist') | null>(null);
  const passwordUpdaterRef = useRef<PasswordUpdater | null>(null);
  const pageFrameRef = useRef<HTMLDivElement>(null);
  const pageCanvasRef = useRef<HTMLCanvasElement>(null);
  const textLayerRef = useRef<HTMLDivElement>(null);
  const renderTaskRef = useRef<RenderTask | null>(null);
  const textLayerInstanceRef = useRef<TextLayer | null>(null);
  const currentPageRef = useRef(1);
  const activeMatchRef = useRef<ActiveMatch | null>(null);
  const pageTextIndexRef = useRef<TextIndex | null>(null);
  const textDivsRef = useRef<HTMLElement[]>([]);
  const textIndexCacheRef = useRef(new Map<number, TextIndex>());
  const searchAbortRef = useRef(false);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);
  const readerRef = useRef<HTMLElement>(null);

  currentPageRef.current = pageNumber;
  activeMatchRef.current = activeMatch;

  useEffect(() => {
    let cancelled = false;
    setReaderState({ status: 'loading' });
    setPageNumber(1);
    setPageDraft('1');
    setOutline([]);
    setPageError('');
    setSearchQuery('');
    setSearchStatus('');
    setActiveMatch(null);
    textIndexCacheRef.current.clear();
    passwordUpdaterRef.current = null;

    let documentUrl: URL;
    try {
      documentUrl = new URL(url, window.location.href);
      if (documentUrl.origin !== window.location.origin) {
        throw new Error('PDF 预览仅支持 FileSage 同源地址。');
      }
    } catch (error) {
      setReaderState({ status: 'error', message: error instanceof Error ? error.message : 'PDF 预览地址无效。' });
      return () => { cancelled = true; };
    }

    let loadingTask: PDFDocumentLoadingTask;
    const prepareDocument = async () => {
      // PDF.js 仅在打开 PDF 时加载，worker 仍随应用本地打包。
      const [pdfjs, workerModule] = await Promise.all([
        import('pdfjs-dist'),
        // @ts-expect-error Vite 的 ?url 查询由打包器解析。
        import('pdfjs-dist/build/pdf.worker.min.mjs?url'),
      ]);
      if (cancelled) return;
      pdfjs.GlobalWorkerOptions.workerSrc = workerModule.default;
      pdfjsRef.current = pdfjs;
      loadingTask = pdfjs.getDocument({
        url: documentUrl.href,
        rangeChunkSize: 65_536,
        disableStream: true,
        disableAutoFetch: true,
      });
      loadingTaskRef.current = loadingTask;
      loadingTask.onPassword = (updatePassword: (value: string) => void, reason: number) => {
        passwordUpdaterRef.current = updatePassword as PasswordUpdater;
        setPassword('');
        setReaderState({
          status: 'password',
          message: reason === pdfjs.PasswordResponses.INCORRECT_PASSWORD ? '密码错误，请重新输入。' : '此 PDF 需要密码才能打开。',
        });
      };

      loadingTask.promise.then(async (document) => {
        if (cancelled) return;
        setReaderState({ status: 'ready', document });
        try {
          const items = await document.getOutline();
          if (!cancelled) setOutline(items ?? []);
        } catch {
          if (!cancelled) setOutline([]);
        }
      }).catch((error: unknown) => {
        if (!cancelled) setReaderState({ status: 'error', message: safeErrorMessage(error) });
      });
    };

    void prepareDocument().catch((error: unknown) => {
      if (!cancelled) setReaderState({ status: 'error', message: safeErrorMessage(error) });
    });

    return () => {
      cancelled = true;
      searchAbortRef.current = true;
      renderTaskRef.current?.cancel();
      textLayerInstanceRef.current?.cancel();
      const currentTask = loadingTaskRef.current;
      loadingTaskRef.current = null;
      if (currentTask) void currentTask.destroy().catch(() => undefined);
    };
  }, [url]);

  useEffect(() => {
    const viewport = pageFrameRef.current?.parentElement;
    if (!viewport) return;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setViewportWidth(entry.contentRect.width);
    });
    observer.observe(viewport);
    setViewportWidth(viewport.clientWidth);
    return () => observer.disconnect();
  }, [readerState.status]);

  useEffect(() => {
    if (readerState.status !== 'ready') return;
    const pdfjs = pdfjsRef.current;
    if (!pdfjs) return;
    const pdfjsModule = pdfjs;
    const document = readerState.document;
    let cancelled = false;
    let renderTask: RenderTask | null = null;
    let textLayer: TextLayer | null = null;
    const canvas = pageCanvasRef.current;
    const textContainer = textLayerRef.current;
    if (!canvas || !textContainer) return;
    const targetCanvas = canvas;
    const targetTextContainer = textContainer;

    setPageError('');
    setRendering(true);
    targetTextContainer.replaceChildren();
    targetCanvas.width = 1;
    targetCanvas.height = 1;
    pageTextIndexRef.current = null;
    textDivsRef.current = [];
    renderTaskRef.current?.cancel();
    textLayerInstanceRef.current?.cancel();

    async function renderCurrentPage() {
      try {
        const page = await document.getPage(pageNumber);
        if (cancelled) return;
        const baseViewport = page.getViewport({ scale: 1 });
        const availableWidth = viewportWidth || pageFrameRef.current?.parentElement?.clientWidth || baseViewport.width;
        const fitScale = Math.min(1, Math.max(0.1, (availableWidth - 32) / baseViewport.width));
        const scale = fitScale * (zoom / 100);
        const viewport = page.getViewport({ scale });
        const dpr = window.devicePixelRatio || 1;
        const outputScale = Math.min(dpr, Math.sqrt(MAX_CANVAS_PIXELS / Math.max(1, viewport.width * viewport.height)));
        targetCanvas.width = Math.max(1, Math.floor(viewport.width * outputScale));
        targetCanvas.height = Math.max(1, Math.floor(viewport.height * outputScale));
        targetCanvas.style.width = `${Math.floor(viewport.width)}px`;
        targetCanvas.style.height = `${Math.floor(viewport.height)}px`;
        targetTextContainer.style.width = `${Math.floor(viewport.width)}px`;
        targetTextContainer.style.height = `${Math.floor(viewport.height)}px`;
        const pageFrame = pageFrameRef.current?.querySelector<HTMLElement>('.pdf-reader-page-frame');
        // 文字层与画布共用页面缩放，避免窄屏选中文字错位。
        pageFrame?.style.setProperty('--scale-factor', String(scale));
        pageFrame?.style.setProperty('--total-scale-factor', String(scale * page.userUnit));
        pageFrame?.style.setProperty('--user-unit', String(page.userUnit));
        pageFrame?.style.setProperty('--scale-round-x', '1px');
        pageFrame?.style.setProperty('--scale-round-y', '1px');

        const nextRenderTask = page.render({
          canvas: targetCanvas,
          viewport,
          transform: outputScale === 1 ? undefined : [outputScale, 0, 0, outputScale, 0, 0],
          background: '#ffffff',
        });
        renderTask = nextRenderTask;
        renderTaskRef.current = nextRenderTask;
        await nextRenderTask.promise;
        if (cancelled) return;

        const textContent = await page.getTextContent();
        if (cancelled) return;
        textLayer = new pdfjsModule.TextLayer({ textContentSource: textContent, container: targetTextContainer, viewport });
        textLayerInstanceRef.current = textLayer;
        await textLayer.render();
        if (cancelled) return;

        const textIndex = makeTextIndex(textContent.items);
        pageTextIndexRef.current = textIndex;
        textDivsRef.current = textLayer.textDivs;
        cacheTextIndex(textIndexCacheRef.current, pageNumber, textIndex);
        setPageDraft(String(pageNumber));
        setRendering(false);
        applyHighlights(activeMatchRef.current, pageNumber, textIndex, textLayer.textDivs);
      } catch (error) {
        if (!cancelled && getErrorName(error) !== 'RenderingCancelledException') {
          setPageError(safeErrorMessage(error));
          setRendering(false);
        }
      }
    }

    void renderCurrentPage();
    return () => {
      cancelled = true;
      renderTask?.cancel();
      textLayer?.cancel();
      if (renderTaskRef.current === renderTask) renderTaskRef.current = null;
      if (textLayerInstanceRef.current === textLayer) textLayerInstanceRef.current = null;
    };
  }, [readerState, pageNumber, zoom, viewportWidth]);

  useEffect(() => {
    applyHighlights(activeMatch, pageNumber, pageTextIndexRef.current, textDivsRef.current);
  }, [activeMatch, pageNumber]);

  useEffect(() => () => {
    searchAbortRef.current = true;
    renderTaskRef.current?.cancel();
    textLayerInstanceRef.current?.cancel();
  }, []);

  useEffect(() => {
    if (!presentationMode) return;
    const updateFullscreen = () => setIsFullscreen(document.fullscreenElement === readerRef.current);
    document.addEventListener('fullscreenchange', updateFullscreen);
    return () => document.removeEventListener('fullscreenchange', updateFullscreen);
  }, [presentationMode]);

  // 将用户输入的密码交给 PDF.js 校验。
  const submitPassword = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const updatePassword = passwordUpdaterRef.current;
    if (!password || !updatePassword) return;
    setReaderState({ status: 'loading' });
    updatePassword(password);
  };

  // 将输入页码限制在文档的有效范围内。
  const jumpToPage = useCallback((value: string) => {
    if (readerState.status !== 'ready') return;
    const parsed = Number.parseInt(value, 10);
    if (!Number.isFinite(parsed)) {
      setPageDraft(String(pageNumber));
      return;
    }
    const nextPage = Math.min(readerState.document.numPages, Math.max(1, parsed));
    setPageNumber(nextPage);
    setPageDraft(String(nextPage));
  }, [pageNumber, readerState]);

  // 将书签目标解析为页面序号。
  const goToOutline = useCallback(async (item: OutlineItem) => {
    if (readerState.status !== 'ready') return;
    try {
      const destination = outlineDestination(item);
      const resolved = typeof destination === 'string'
        ? await readerState.document.getDestination(destination)
        : destination;
      const pageRef = resolved?.[0];
      if (pageRef === undefined || pageRef === null) return;
      const pageIndex = typeof pageRef === 'number'
        ? pageRef
        : await readerState.document.getPageIndex(pageRef as Parameters<PDFDocumentProxy['getPageIndex']>[0]);
      const nextPage = Math.min(readerState.document.numPages, Math.max(1, pageIndex + 1));
      setPageNumber(nextPage);
      setPageDraft(String(nextPage));
      setSearchStatus('');
    } catch {
      setSearchStatus('无法跳转到此书签。');
    }
  }, [readerState]);

  // 按页搜索文本并循环定位匹配项。
  const findText = useCallback(async (direction: 'next' | 'previous') => {
    if (readerState.status !== 'ready' || searching) return;
    const query = normalizeForSearch(searchQuery.trim());
    if (!query) {
      setSearchStatus('请输入要查找的文字。');
      return;
    }

    const document = readerState.document;
    const startPage = currentPageRef.current;
    const previousMatch = activeMatchRef.current;
    const sameQuery = previousMatch?.query === query && previousMatch.page === startPage;
    setSearching(true);
    setSearchStatus('正在查找…');
    searchAbortRef.current = false;

    try {
      const attempts = document.numPages + (sameQuery ? 1 : 0);
      for (let step = 0; step < attempts; step += 1) {
        if (searchAbortRef.current) return;
        const wrappedToStart = sameQuery && step === document.numPages;
        const page = wrappedToStart ? startPage : direction === 'next'
          ? ((startPage - 1 + step) % document.numPages) + 1
          : ((startPage - 1 - step + document.numPages * 2) % document.numPages) + 1;
        let textIndex = textIndexCacheRef.current.get(page);
        if (!textIndex) {
          textIndex = await getPageTextIndex(document, page);
          cacheTextIndex(textIndexCacheRef.current, page, textIndex);
        }
        if (searchAbortRef.current) return;

        const startOffset = wrappedToStart
          ? direction === 'next' ? 0 : textIndex.text.length
          : page === startPage && sameQuery
          ? direction === 'next' ? previousMatch.offset + query.length : previousMatch.offset - 1
          : direction === 'next' ? 0 : textIndex.text.length;
        let offset = -1;
        if (direction === 'next') {
          offset = textIndex.text.indexOf(query, Math.max(0, startOffset));
        } else if (startOffset >= 0) {
          offset = textIndex.text.lastIndexOf(query, startOffset);
        }
        if (offset < 0) continue;

        setActiveMatch({ query, page, offset });
        setSearchStatus(`第 ${page} 页找到匹配项。`);
        if (page !== startPage) {
          setPageNumber(page);
          setPageDraft(String(page));
        } else {
          const renderedIndex = pageTextIndexRef.current ?? textIndex;
          applyHighlights({ query, page, offset }, page, renderedIndex, textDivsRef.current);
        }
        return;
      }
      setActiveMatch(null);
      setSearchStatus('未找到匹配内容。');
      applyHighlights(null, startPage, pageTextIndexRef.current, textDivsRef.current);
    } catch {
      setSearchStatus('查找失败，请稍后重试。');
    } finally {
      setSearching(false);
    }
  }, [readerState, searchQuery, searching]);

  // Enter 前进查找，Shift+Enter 反向查找，Escape 收起查找栏。
  const onSearchKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      void findText(event.shiftKey ? 'previous' : 'next');
    }
    if (event.key === 'Escape') setSearchOpen(false);
  };

  // 在幻灯片阅读区提供键盘翻页，并保留表单自身的方向键行为。
  const onPresentationKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    const target = event.target instanceof HTMLElement ? event.target : null;
    if (!presentationMode || target?.closest('input, textarea, select, [contenteditable="true"]')) return;
    if (event.key === ' ' && target?.closest('button, a')) return;
    const action = presentationKeyAction(event.key);
    if (!action) return;
    if (action === 'exit') {
      if (document.fullscreenElement === readerRef.current) {
        event.preventDefault();
        void document.exitFullscreen().catch(() => setFullscreenMessage('无法退出全屏，请使用浏览器的返回全屏操作。'));
      }
      return;
    }
    if (readerState.status !== 'ready') return;
    event.preventDefault();
    if (action === 'previous') jumpToPage(String(pageNumber - 1));
    if (action === 'next') jumpToPage(String(pageNumber + 1));
    if (action === 'first') jumpToPage('1');
    if (action === 'last') jumpToPage(String(readerState.document.numPages));
  };

  // 请求全屏失败时保留原阅读状态并给出可理解的提示。
  const toggleFullscreen = async () => {
    const reader = readerRef.current;
    if (!reader) return;
    setFullscreenMessage('');
    try {
      if (document.fullscreenElement === reader) await document.exitFullscreen();
      else if (reader.requestFullscreen && document.fullscreenEnabled !== false) {
        await reader.requestFullscreen();
        reader.focus();
      }
      else setFullscreenMessage('当前浏览器不支持全屏播放。');
    } catch {
      setFullscreenMessage('无法切换全屏。请检查浏览器权限，或继续在页面内阅读。');
    }
  };

  // 只将横向单指滑动作为翻页，保留纵向滚动和放大后的横向平移。
  const onSlideTouchStart = (event: TouchEvent<HTMLDivElement>) => {
    if (!presentationMode || zoom > 100 || event.touches.length !== 1) {
      touchStartRef.current = null;
      return;
    }
    const touch = event.touches[0];
    if (touch) touchStartRef.current = { x: touch.clientX, y: touch.clientY };
  };

  const onSlideTouchEnd = (event: TouchEvent<HTMLDivElement>) => {
    const start = touchStartRef.current;
    const touch = event.changedTouches[0];
    touchStartRef.current = null;
    if (!start || !touch || readerState.status !== 'ready') return;
    const direction = slideSwipeDirection(start.x, start.y, touch.clientX, touch.clientY);
    if (direction === 'previous') jumpToPage(String(pageNumber - 1));
    if (direction === 'next') jumpToPage(String(pageNumber + 1));
  };

  const pageCount = readerState.status === 'ready' ? readerState.document.numPages : 0;
  const canGoPrevious = pageNumber > 1;
  const canGoNext = pageNumber < pageCount;

  return (
    <section
      ref={readerRef}
      className={`pdf-reader${presentationMode ? ' is-presentation' : ''}`}
      aria-label={`${presentationMode ? 'PPTX 幻灯片放映' : 'PDF 阅读器'}：${fileName}`}
      tabIndex={presentationMode ? 0 : undefined}
      onKeyDown={presentationMode ? onPresentationKeyDown : undefined}
    >
      <div className="pdf-reader-controls">
        <div className="pdf-reader-page-controls" aria-label="页码导航">
          <button type="button" className="pdf-reader-icon-button" onClick={() => jumpToPage(String(pageNumber - 1))} disabled={!canGoPrevious || readerState.status !== 'ready'} aria-label={presentationMode ? '上一张幻灯片' : '上一页'} title={presentationMode ? '上一张幻灯片' : '上一页'}>
            <ChevronLeft size={17} aria-hidden="true" />
          </button>
          <form className="pdf-reader-page-form" onSubmit={(event) => { event.preventDefault(); jumpToPage(pageDraft); }}>
            <input
              aria-label="当前页码"
              type="number"
              min={1}
              max={pageCount || undefined}
              value={pageDraft}
              disabled={readerState.status !== 'ready'}
              onChange={(event) => setPageDraft(event.target.value)}
              onBlur={() => jumpToPage(pageDraft)}
            />
            <span>／{pageCount || '—'}</span>
          </form>
          <button type="button" className="pdf-reader-icon-button" onClick={() => jumpToPage(String(pageNumber + 1))} disabled={!canGoNext || readerState.status !== 'ready'} aria-label={presentationMode ? '下一张幻灯片' : '下一页'} title={presentationMode ? '下一张幻灯片' : '下一页'}>
            <ChevronRight size={17} aria-hidden="true" />
          </button>
          {presentationMode && readerState.status === 'ready' && (
            <select
              className="pdf-reader-slide-list"
              aria-label="幻灯片列表"
              value={pageNumber}
              onChange={(event) => jumpToPage(event.target.value)}
            >
              {Array.from({ length: pageCount }, (_, index) => index + 1).map((slide) => (
                <option key={slide} value={slide}>第 {slide} 张</option>
              ))}
            </select>
          )}
        </div>

        <div className="pdf-reader-tool-controls">
          <button type="button" className="pdf-reader-icon-button" onClick={() => setZoom((value) => Math.max(MIN_ZOOM, value - 25))} disabled={readerState.status !== 'ready' || zoom <= MIN_ZOOM} aria-label="缩小" title="缩小">
            <ZoomOut size={16} aria-hidden="true" />
          </button>
          <span className="pdf-reader-zoom-label" aria-live="polite">{zoom === 100 ? '适宽' : `${zoom}%`}</span>
          <button type="button" className="pdf-reader-icon-button" onClick={() => setZoom((value) => Math.min(MAX_ZOOM, value + 25))} disabled={readerState.status !== 'ready' || zoom >= MAX_ZOOM} aria-label="放大" title="放大">
            <ZoomIn size={16} aria-hidden="true" />
          </button>
          <button type="button" className={`pdf-reader-icon-button${searchOpen ? ' is-active' : ''}`} onClick={() => setSearchOpen((value) => !value)} disabled={readerState.status !== 'ready'} aria-label="查找文内文字" title="查找文内文字">
            {searchOpen ? <X size={16} aria-hidden="true" /> : <Search size={16} aria-hidden="true" />}
          </button>
          <details className="pdf-reader-outline">
            <summary aria-label="PDF 目录" title="PDF 目录">
              <BookOpen size={16} aria-hidden="true" /><ChevronDown className="pdf-reader-outline-chevron" size={13} aria-hidden="true" />
            </summary>
            <div className="pdf-reader-outline-menu">
              <strong>目录</strong>
              {outline.length ? <OutlineList items={outline} onSelect={(item) => { void goToOutline(item); }} /> : <p>此 PDF 没有目录书签。</p>}
            </div>
          </details>
          <a className="pdf-reader-download" href={downloadUrl} download={fileName} aria-label={`下载 ${fileName} 原件`} title="下载原件">
            <ArrowDownToLine size={16} aria-hidden="true" /><span>下载原件</span>
          </a>
          {presentationMode && (
            <button type="button" className="pdf-reader-icon-button" onClick={() => void toggleFullscreen()} aria-label={isFullscreen ? '退出全屏' : '全屏播放'} title={isFullscreen ? '退出全屏' : '全屏播放'}>
              {isFullscreen ? <Minimize2 size={16} aria-hidden="true" /> : <Maximize2 size={16} aria-hidden="true" />}
            </button>
          )}
        </div>
      </div>

      {fullscreenMessage && <p className="pdf-reader-presentation-status" role="status">{fullscreenMessage}</p>}

      {searchOpen && (
        <form className="pdf-reader-search" onSubmit={(event) => { event.preventDefault(); void findText('next'); }}>
          <Search size={16} aria-hidden="true" />
          <input
            type="search"
            value={searchQuery}
            disabled={searching}
            onChange={(event) => { setSearchQuery(event.target.value); setSearchStatus(''); setActiveMatch(null); }}
            onKeyDown={onSearchKeyDown}
            placeholder="查找 PDF 文本"
            aria-label="查找 PDF 文本"
            autoFocus
          />
          <button type="button" className="pdf-reader-icon-button" onClick={() => void findText('previous')} disabled={searching || !searchQuery.trim()} aria-label="上一个匹配项" title="上一个匹配项">
            <ChevronLeft size={16} aria-hidden="true" />
          </button>
          <button type="submit" className="pdf-reader-icon-button" disabled={searching || !searchQuery.trim()} aria-label="下一个匹配项" title="下一个匹配项">
            <ChevronRight size={16} aria-hidden="true" />
          </button>
          <span className="pdf-reader-search-status" role="status">{searchStatus}</span>
        </form>
      )}

      {readerState.status === 'loading' && (
        <div className="pdf-reader-state" role="status">
          <LoaderCircle className="pdf-reader-spinner" size={22} aria-hidden="true" />
          <strong>正在读取 PDF…</strong>
        </div>
      )}

      {readerState.status === 'password' && (
        <div className="pdf-reader-state" role="group" aria-label="输入 PDF 密码">
          <BookOpen size={22} aria-hidden="true" />
          <strong>此 PDF 受密码保护</strong>
          <p>{readerState.message}</p>
          <form className="pdf-reader-password-form" onSubmit={submitPassword}>
            <input type="password" value={password} onChange={(event) => setPassword(event.target.value)} autoComplete="current-password" aria-label="PDF 密码" placeholder="输入密码" autoFocus />
            <button type="submit" disabled={!password}>打开文件</button>
          </form>
          <a className="pdf-reader-download" href={downloadUrl} download={fileName}><ArrowDownToLine size={16} aria-hidden="true" />下载原件</a>
        </div>
      )}

      {readerState.status === 'error' && (
        <div className="pdf-reader-state pdf-reader-state-error" role="alert">
          <strong>无法打开此 PDF</strong>
          <p>{readerState.message}</p>
          <a className="pdf-reader-download" href={downloadUrl} download={fileName}><ArrowDownToLine size={16} aria-hidden="true" />下载原件</a>
        </div>
      )}

      {readerState.status === 'ready' && (
        <div className="pdf-reader-viewport">
          {pageError ? (
            <div className="pdf-reader-state pdf-reader-state-error" role="alert">
              <strong>此页无法显示</strong>
              <p>{pageError}</p>
              <a className="pdf-reader-download" href={downloadUrl} download={fileName}><ArrowDownToLine size={16} aria-hidden="true" />下载原件</a>
            </div>
          ) : (
            <div
              className="pdf-reader-page-scroll"
              ref={pageFrameRef}
              onTouchStart={presentationMode ? onSlideTouchStart : undefined}
              onTouchEnd={presentationMode ? onSlideTouchEnd : undefined}
            >
              <div className={`pdf-reader-page-frame${rendering ? ' is-rendering' : ''}`}>
                <canvas ref={pageCanvasRef} aria-label={`${fileName}，第 ${pageNumber} 页`} />
                <div ref={textLayerRef} className="pdf-reader-text-layer" aria-label={`${fileName} 第 ${pageNumber} 页文字层`} />
              </div>
            </div>
          )}
          {rendering && !pageError && <div className="pdf-reader-page-loading" role="status"><LoaderCircle className="pdf-reader-spinner" size={20} aria-hidden="true" /><span>正在载入第 {pageNumber} 页…</span></div>}
        </div>
      )}
    </section>
  );
}

// 递归显示 PDF 内已有的目录书签。
function OutlineList({ items, onSelect }: { items: OutlineItem[]; onSelect: (item: OutlineItem) => void }) {
  return (
    <ul>
      {items.map((item, index) => (
        <li key={`${item.title}-${index}`}>
          <button type="button" onClick={() => onSelect(item)} disabled={!outlineDestination(item)} title={item.title}>{item.title || '未命名书签'}</button>
          {item.items.length > 0 && <OutlineList items={item.items} onSelect={onSelect} />}
        </li>
      ))}
    </ul>
  );
}

// 读取单页文本，供查找使用。
async function getPageTextIndex(document: PDFDocumentProxy, pageNumber: number): Promise<TextIndex> {
  const page: PDFPageProxy = await document.getPage(pageNumber);
  const textContent = await page.getTextContent();
  return makeTextIndex(textContent.items);
}

// 保留近期页文本索引并限制内存占用。
function cacheTextIndex(cache: Map<number, TextIndex>, pageNumber: number, textIndex: TextIndex) {
  cache.delete(pageNumber);
  cache.set(pageNumber, textIndex);
  while (cache.size > MAX_CACHED_TEXT_PAGES) {
    const oldestPage = cache.keys().next().value;
    if (oldestPage === undefined) break;
    cache.delete(oldestPage);
  }
}

// 在文字层标记全部匹配项，不改动 PDF 内容。
function applyHighlights(active: ActiveMatch | null, pageNumber: number, textIndex: TextIndex | null, textDivs: HTMLElement[]) {
  for (const textDiv of textDivs) {
    textDiv.classList.remove('pdf-reader-search-hit', 'pdf-reader-search-hit-active');
  }
  if (!active || active.page !== pageNumber || !textIndex || textDivs.length === 0) return;

  const query = active.query;
  let offset = 0;
  while (offset <= textIndex.text.length - query.length) {
    const matchOffset = textIndex.text.indexOf(query, offset);
    if (matchOffset < 0) break;
    const itemIndexes = new Set(textIndex.owners.slice(matchOffset, matchOffset + query.length));
    for (const itemIndex of itemIndexes) {
      const textDiv = textDivs[itemIndex];
      if (!textDiv) continue;
      textDiv.classList.add('pdf-reader-search-hit');
      if (matchOffset === active.offset) textDiv.classList.add('pdf-reader-search-hit-active');
    }
    offset = matchOffset + Math.max(1, query.length);
  }
}
