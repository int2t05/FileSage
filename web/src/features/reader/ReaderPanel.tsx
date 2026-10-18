import { lazy, Suspense, useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import { AlertTriangle, ArrowDownToLine, LoaderCircle, Pencil, RefreshCw, Save, X } from 'lucide-react';
import { preserveDraftOnFileChange, watchFileVersion } from './fileVersionMonitor';
import { imagePreviewURL, requiresImageConversion } from './imagePreview';
import { fetchOfficePreview } from './officePreview';
import './ReaderPanel.css';

const MarkdownPreview = lazy(() => import('./MarkdownPreview').then((module) => ({ default: module.MarkdownPreview })));
const DelimitedTextTable = lazy(() => import('./DelimitedTextTable').then((module) => ({ default: module.DelimitedTextTable })));
const XlsxReader = lazy(() => import('./XlsxReader').then((module) => ({ default: module.XlsxReader })));
const EpubReader = lazy(() => import('./EpubReader').then((module) => ({ default: module.EpubReader })));
const DocxReader = lazy(() => import('./DocxReader').then((module) => ({ default: module.DocxReader })));
const TextCodeEditor = lazy(() => import('./TextCodeEditor').then((module) => ({ default: module.TextCodeEditor })));
const PdfReader = lazy(() => import('./PdfReader').then((module) => ({ default: module.PdfReader })));

export type ReaderMediaKind = 'image' | 'audio' | 'video' | 'pdf' | 'text' | 'xlsx' | 'epub' | 'office';

export interface ReaderPanelProps {
  path: string;
  name: string;
  kind: ReaderMediaKind;
  capturedAt?: string;
  onClose: () => void;
  onFileCreated?: (path: string) => void;
  // 提供只读字节地址，供压缩包条目复用现有阅读器。
  sourceURL?: string;
}

type ReaderState =
  | { status: 'loading' }
  | { status: 'text'; path: string; content: string; version: string | null }
  | { status: 'pdf'; path: string; url: string }
  | { status: 'xlsx'; path: string }
  | { status: 'epub'; path: string }
  | { status: 'docx'; path: string }
  | { status: 'media'; path: string; url: string; mimeType: string; previewLoading: boolean; previewFailed: boolean }
  | { status: 'unsupported'; path: string; message: string }
  | { status: 'error'; path: string; message: string };

type PendingAction = 'close' | 'stopEditing' | null;
type SaveStatus = 'idle' | 'saving' | 'saved' | 'error' | 'conflict';
type SaveAsStatus = 'idle' | 'saving' | 'saved' | 'exists' | 'error';
type MarkdownMode = 'preview' | 'source';
type MediaPlaybackStatus = 'loading' | 'ready' | 'playing' | 'paused' | 'buffering' | 'seeking' | 'ended';

interface MediaPlaybackState {
  status: MediaPlaybackStatus;
  currentTime: number;
  duration: number | null;
}

// EXIF 日期按服务端保存的 UTC 日期载体原样显示。
export function formatCaptureDateTime(value?: string): string {
  if (!value) return '未记录';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '未记录';
  const pad = (part: number) => String(part).padStart(2, '0');
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())} ${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}`;
}

// 按浏览器媒体事件更新用户可见的播放状态。
function playbackStatusForEvent(eventName: string, previous: MediaPlaybackStatus, paused: boolean, ended: boolean): MediaPlaybackStatus {
  if (eventName === 'ended' || ended) return 'ended';
  if (eventName === 'play' || eventName === 'playing') return 'playing';
  if (eventName === 'pause') return 'paused';
  if (eventName === 'waiting' || eventName === 'stalled') return 'buffering';
  if (eventName === 'seeking') return 'seeking';
  if (eventName === 'seeked') return paused ? 'paused' : 'playing';
  if (eventName === 'loadedmetadata') return 'ready';
  if (eventName === 'canplay' || eventName === 'canplaythrough') {
    return previous === 'loading' || previous === 'buffering' ? paused ? 'ready' : 'playing' : previous;
  }
  return previous;
}

// 以时分秒格式显示浏览器提供的媒体时间。
function formatMediaTime(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '--:--';
  const seconds = Math.floor(value);
  const minutes = Math.floor(seconds / 60);
  const remainder = String(seconds % 60).padStart(2, '0');
  if (minutes < 60) return `${minutes}:${remainder}`;
  return `${Math.floor(minutes / 60)}:${String(minutes % 60).padStart(2, '0')}:${remainder}`;
}

function mediaPlaybackLabel(status: MediaPlaybackStatus): string {
  const labels: Record<MediaPlaybackStatus, string> = {
    loading: '正在载入',
    ready: '就绪',
    playing: '正在播放',
    paused: '已暂停',
    buffering: '缓冲中',
    seeking: '正在定位',
    ended: '已结束',
  };
  return labels[status];
}

class ReaderFailure extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ReaderFailure';
  }
}

const MAX_TEXT_BYTES = 2 * 1024 * 1024;

const MEDIA_TYPES: Record<ReaderMediaKind, Record<string, string>> = {
  image: {
    avif: 'image/avif',
    bmp: 'image/bmp',
    gif: 'image/gif',
    heic: 'image/heic',
    heif: 'image/heif',
    dng: 'image/jpeg',
    cr2: 'image/jpeg',
    cr3: 'image/jpeg',
    nef: 'image/jpeg',
    arw: 'image/jpeg',
    jpeg: 'image/jpeg',
    jpg: 'image/jpeg',
    png: 'image/png',
    webp: 'image/webp',
  },
  audio: {
    aac: 'audio/aac',
    flac: 'audio/flac',
    m4a: 'audio/mp4',
    mp3: 'audio/mpeg',
    oga: 'audio/ogg',
    ogg: 'audio/ogg',
    opus: 'audio/ogg',
    wav: 'audio/wav',
  },
  video: {
    avi: 'video/x-msvideo',
    mkv: 'video/x-matroska',
    mov: 'video/quicktime',
    mp4: 'video/mp4',
    m4v: 'video/mp4',
    ogv: 'video/ogg',
    webm: 'video/webm',
  },
  pdf: {
    pdf: 'application/pdf',
  },
  office: {
    docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  },
  xlsx: {},
  epub: {},
  text: {},
};

function contentURL(path: string): string {
  const query = new URLSearchParams({ path });
  return `/api/v1/files/content?${query.toString()}`;
}

function previewURL(path: string): string {
  const query = new URLSearchParams({ path });
  return `/api/v1/files/preview?${query.toString()}`;
}

function extensionOf(name: string): string {
  return name.split('.').at(-1)?.toLocaleLowerCase() ?? '';
}

function isMarkdown(name: string): boolean {
  const extension = extensionOf(name);
  return extension === 'md' || extension === 'markdown' || extension === 'mdown';
}

function delimitedFormat(name: string): { label: 'CSV' | 'TSV'; delimiter: ',' | '\t' } | null {
  const extension = extensionOf(name);
  if (extension === 'csv') return { label: 'CSV', delimiter: ',' };
  if (extension === 'tsv') return { label: 'TSV', delimiter: '\t' };
  return null;
}

function suggestedCopyName(name: string): string {
  const extensionStart = name.lastIndexOf('.');
  if (extensionStart > 0) return `${name.slice(0, extensionStart)} - 副本${name.slice(extensionStart)}`;
  return `${name} - 副本`;
}

async function responseError(response: Response, action = '读取'): Promise<ReaderFailure> {
  let message = `${action}失败（HTTP ${response.status}）`;
  if (response.headers.get('content-type')?.includes('application/json')) {
    const payload = await response.json().catch(() => undefined) as { error?: string } | undefined;
    if (payload?.error) message = payload.error;
  }
  return new ReaderFailure(message);
}

async function readTextResponse(response: Response): Promise<string> {
  const contentLength = Number(response.headers.get('content-length'));
  if (Number.isFinite(contentLength) && contentLength > MAX_TEXT_BYTES) {
    await response.body?.cancel();
    throw new ReaderFailure('文本文件超过 2 MiB 预览上限。请下载原件后使用本机编辑器查看。');
  }

  const reader = response.body?.getReader();
  if (!reader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength > MAX_TEXT_BYTES) {
      throw new ReaderFailure('文本文件超过 2 MiB 预览上限。请下载原件后使用本机编辑器查看。');
    }
    return decodeUTF8(bytes);
  }

  const chunks: Uint8Array[] = [];
  let totalBytes = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      totalBytes += value.byteLength;
      if (totalBytes > MAX_TEXT_BYTES) {
        await reader.cancel().catch(() => undefined);
        throw new ReaderFailure('文本文件超过 2 MiB 预览上限。请下载原件后使用本机编辑器查看。');
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const bytes = new Uint8Array(totalBytes);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return decodeUTF8(bytes);
}

function decodeUTF8(bytes: Uint8Array): string {
  try {
    return new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    throw new ReaderFailure('文本不是有效的 UTF-8 编码，无法安全预览。可下载原件；此页面不会猜测编码。');
  }
}

function encodeUTF8(content: string): Uint8Array {
  const bytes = new TextEncoder().encode(content);
  const roundTrip = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes);
  if (roundTrip !== content) {
    throw new ReaderFailure('草稿包含无法编码为 UTF-8 的字符，请修正后再保存。');
  }
  return bytes;
}

export function ReaderPanel({ path, name, kind, capturedAt, onClose, onFileCreated, sourceURL }: ReaderPanelProps) {
  const [state, setState] = useState<ReaderState>({ status: 'loading' });
  const [mediaPlayback, setMediaPlayback] = useState<MediaPlaybackState>({ status: 'loading', currentTime: 0, duration: null });
  const [isOpen, setIsOpen] = useState(true);
  const [retryCount, setRetryCount] = useState(0);
  const [versionMonitorRevision, setVersionMonitorRevision] = useState(0);
  const [draft, setDraft] = useState('');
  const [isEditing, setIsEditing] = useState(false);
  const [markdownMode, setMarkdownMode] = useState<MarkdownMode>('preview');
  const [saveStatus, setSaveStatus] = useState<SaveStatus>('idle');
  const [saveMessage, setSaveMessage] = useState('');
  const [saveAsName, setSaveAsName] = useState(() => suggestedCopyName(name));
  const [saveAsStatus, setSaveAsStatus] = useState<SaveAsStatus>('idle');
  const [saveAsMessage, setSaveAsMessage] = useState('');
  const [pendingAction, setPendingAction] = useState<PendingAction>(null);
  const [reloading, setReloading] = useState(false);
  const [fileExpired, setFileExpired] = useState(false);
  const panelRef = useRef<HTMLElement>(null);
  const controllerRef = useRef<AbortController | null>(null);
  const fileVersionRef = useRef<string | null>(null);
  const versionMonitorStopRef = useRef<(() => void) | null>(null);
  const previewObjectURLRef = useRef<string | null>(null);
  const saveControllerRef = useRef<AbortController | null>(null);
  const onCloseRef = useRef(onClose);
  const closePanelRef = useRef<() => void>(() => {});
  const pendingActionRef = useRef<PendingAction>(null);
  const confirmButtonRef = useRef<HTMLButtonElement>(null);
  const confirmDialogRef = useRef<HTMLDivElement>(null);
  const currentPathRef = useRef(path);
  const readerId = useId();
  const isReadOnly = Boolean(sourceURL);
  const sourceHref = sourceURL ?? contentURL(path);
  const draftRef = useRef(draft);
  const editingRef = useRef(isEditing);
  draftRef.current = draft;
  editingRef.current = isEditing;

  const textState = state.status === 'text' && state.path === path ? state : null;
  const isDirty = Boolean(textState && draft !== textState.content && saveAsStatus !== 'saved');
  const isBusy = saveStatus === 'saving' || saveAsStatus === 'saving' || reloading;
  pendingActionRef.current = pendingAction;
  currentPathRef.current = path;
  const closeGuardRef = useRef({ dirty: isDirty, busy: isBusy });
  const tableFormat = delimitedFormat(name);
  closeGuardRef.current = { dirty: isDirty, busy: isBusy };

  function handleMediaEvent(event: SyntheticEvent<HTMLMediaElement>) {
    const media = event.currentTarget;
    setMediaPlayback((previous) => ({
      status: playbackStatusForEvent(event.type, previous.status, media.paused, media.ended),
      currentTime: Number.isFinite(media.currentTime) ? media.currentTime : 0,
      duration: Number.isFinite(media.duration) ? media.duration : null,
    }));
    if (event.type === 'loadedmetadata') {
      setState((current) => current.status === 'media' && current.path === path ? { ...current, previewLoading: false } : current);
    } else if (event.type === 'error') {
      setState((current) => current.status === 'media' && current.path === path ? { ...current, previewLoading: false, previewFailed: true } : current);
    }
  }

  onCloseRef.current = onClose;

  useEffect(() => {
    if (!isOpen || (!isDirty && !isBusy)) return;

    function warnBeforeUnload(event: BeforeUnloadEvent) {
      event.preventDefault();
      event.returnValue = '';
    }

    window.addEventListener('beforeunload', warnBeforeUnload);
    return () => window.removeEventListener('beforeunload', warnBeforeUnload);
  }, [isOpen, isDirty, isBusy]);

  useEffect(() => () => {
    saveControllerRef.current?.abort();
  }, []);

  useEffect(() => {
    if (!isOpen) return;

    let current = true;
    const controller = new AbortController();
    controllerRef.current = controller;
    setMediaPlayback({ status: 'loading', currentTime: 0, duration: null });
    fileVersionRef.current = null;
    if (previewObjectURLRef.current) URL.revokeObjectURL(previewObjectURLRef.current);
    previewObjectURLRef.current = null;
    setFileExpired(false);
    setState({ status: 'loading' });

    const loadText = async () => {
      if (kind !== 'text' && kind !== 'xlsx' && kind !== 'epub' && !MEDIA_TYPES[kind][extensionOf(name)]) {
        setState({ status: 'unsupported', path, message: '此文件格式暂不支持内嵌预览，可下载原件。' });
        return;
      }
      if (!sourceURL && kind !== 'text') {
        try {
          const response = await fetch(contentURL(path), { method: 'HEAD', signal: controller.signal, cache: 'no-store' });
          if (!response.ok) throw await responseError(response);
          fileVersionRef.current = response.headers.get('X-FileSage-Version')?.trim() || null;
        } catch (error) {
          if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
          const message = error instanceof ReaderFailure ? error.message : '无法读取文件版本。请检查服务连接后重试。';
          setState({ status: 'error', path, message });
          return;
        }
      }
      if (kind === 'epub') {
        setState({ status: 'epub', path });
        return;
      }
      if (kind === 'office' && extensionOf(name) === 'docx') {
        setState({ status: 'docx', path });
        return;
      }
      if (kind === 'xlsx') {
        setState({ status: 'xlsx', path });
        return;
      }
      if (kind !== 'text' && !MEDIA_TYPES[kind][extensionOf(name)]) {
        setState({ status: 'unsupported', path, message: '此文件格式暂不支持内嵌预览，可下载原件。' });
        return;
      }

      if (kind !== 'text') {
        const mimeType = MEDIA_TYPES[kind][extensionOf(name)];
        if (!mimeType) {
          setState({ status: 'unsupported', path, message: '此文件格式暂不支持内嵌预览，可下载原件。' });
          return;
        }
        if (kind === 'image' && requiresImageConversion(name)) {
          if (sourceURL) {
            setState({ status: 'unsupported', path, message: '压缩包内的 HEIC／RAW 图片暂不支持转换预览，可下载原件。' });
            return;
          }
          const version = fileVersionRef.current;
          if (!version) {
            setState({ status: 'error', path, message: '无法读取图片版本，请刷新后重试。' });
            return;
          }
          try {
            const response = await fetch(imagePreviewURL(path, version, 'view'), { signal: controller.signal, cache: 'no-store' });
            if (!response.ok) throw await responseError(response);
            const contentType = response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase();
            if (contentType !== 'image/jpeg') throw new ReaderFailure('服务未返回有效的图片预览。');
            const objectURL = URL.createObjectURL(await response.blob());
            if (current) {
              previewObjectURLRef.current = objectURL;
              setState({ status: 'media', path, url: objectURL, mimeType: 'image/jpeg', previewLoading: true, previewFailed: false });
            } else URL.revokeObjectURL(objectURL);
          } catch (error) {
            if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
            const message = error instanceof ReaderFailure
              ? error.message
              : error instanceof Error
                ? error.message
                : '无法生成图片预览，请检查系统解码工具或下载原件。';
            setState({ status: 'error', path, message });
          }
          return;
        }
        if (kind === 'pdf') {
          try {
            if (sourceURL) {
              const response = await fetch(sourceURL, { signal: controller.signal, cache: 'no-store' });
              if (!response.ok) throw await responseError(response);
              const contentType = response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase();
              if (contentType !== mimeType) throw new ReaderFailure('服务未返回有效的 PDF 预览。');
              const objectURL = URL.createObjectURL(await response.blob());
              if (current) {
                previewObjectURLRef.current = objectURL;
                setState({ status: 'pdf', path, url: objectURL });
              } else URL.revokeObjectURL(objectURL);
            } else {
              const response = await fetch(previewURL(path), { method: 'HEAD', signal: controller.signal });
              if (!response.ok) throw await responseError(response);
              const contentType = response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase();
              if (contentType !== mimeType) throw new ReaderFailure('服务未返回有效的 PDF 预览。');
              if (current) setState({ status: 'pdf', path, url: previewURL(path) });
            }
          } catch (error) {
            if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
            const message = error instanceof ReaderFailure
              ? error.message
              : '无法读取 PDF。请检查服务连接后重试，或下载原件。';
            setState({ status: 'error', path, message });
          }
          return;
        }
        if (kind === 'office') {
          if (sourceURL) {
            setState({ status: 'unsupported', path, message: '压缩包内的 Office 文件暂不支持预览，可下载原件。' });
            return;
          }
          try {
            const blob = await fetchOfficePreview(path, fileVersionRef.current ?? '', controller.signal);
            const objectURL = URL.createObjectURL(blob);
            if (current) {
              previewObjectURLRef.current = objectURL;
              setState({ status: 'pdf', path, url: objectURL });
            } else URL.revokeObjectURL(objectURL);
          } catch (error) {
            if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
            const message = error instanceof Error
              ? error.message
              : '无法生成 Office 预览。请检查服务连接后重试，或下载原件。';
            setState({ status: 'error', path, message });
          }
          return;
        }
        if (sourceURL) {
          try {
            const response = await fetch(sourceURL, { signal: controller.signal, cache: 'no-store' });
            if (!response.ok) throw await responseError(response);
            const objectURL = URL.createObjectURL(await response.blob());
            if (current) {
              previewObjectURLRef.current = objectURL;
              setState({ status: 'media', path, url: objectURL, mimeType, previewLoading: true, previewFailed: false });
            } else URL.revokeObjectURL(objectURL);
          } catch (error) {
            if (!current || error instanceof DOMException && error.name === 'AbortError') return;
            setState({ status: 'error', path, message: error instanceof Error ? error.message : '无法读取压缩包条目。' });
          }
          return;
        }
        // 由浏览器原生媒体元素直接请求流式预览，保留视频 Range/拖动能力。
        setState({ status: 'media', path, url: previewURL(path), mimeType, previewLoading: true, previewFailed: false });
        return;
      }

      try {
        const response = await fetch(sourceHref, { signal: controller.signal, cache: 'no-store' });
        if (!response.ok) throw await responseError(response);

        const content = await readTextResponse(response);
        if (current) {
          const version = response.headers.get('X-FileSage-Version')?.trim() || null;
          fileVersionRef.current = version;
          setState({ status: 'text', path, content, version });
          setDraft(content);
          setIsEditing(false);
          setMarkdownMode('preview');
          setSaveStatus('idle');
          setSaveMessage('');
        }
      } catch (error) {
        if (!current || (error instanceof DOMException && error.name === 'AbortError')) return;
        const message = error instanceof ReaderFailure
          ? error.message
          : '无法读取文件。请检查服务连接后重试，或下载原件。';
        setState({ status: 'error', path, message });
      }
    };

    void loadText();

    return () => {
      current = false;
      controller.abort();
      if (controllerRef.current === controller) controllerRef.current = null;
      if (previewObjectURLRef.current) URL.revokeObjectURL(previewObjectURLRef.current);
      previewObjectURLRef.current = null;
    };
  }, [isOpen, kind, name, path, retryCount, sourceHref, sourceURL]);

  useEffect(() => {
    if (!isOpen || sourceURL || ('path' in state && state.path !== path) || state.status === 'loading' || state.status === 'error' || state.status === 'unsupported') return;
    const initialVersion = fileVersionRef.current ?? (state.status === 'text' ? state.version : null);
    const stop = watchFileVersion({
      path,
      initialVersion,
      onExpired: () => {
        setFileExpired(true);
        if (state.status === 'text' && (editingRef.current || draftRef.current !== state.content)) {
          const preserved = preserveDraftOnFileChange(draftRef.current);
          setDraft(preserved.draft);
          setSaveStatus('conflict');
          setSaveMessage(preserved.message);
        }
      },
    });
    versionMonitorStopRef.current = stop;
    return () => {
      stop();
      if (versionMonitorStopRef.current === stop) versionMonitorStopRef.current = null;
    };
  }, [isOpen, path, sourceURL, state.status, state.status === 'text' ? state.version : null, versionMonitorRevision]);

  useEffect(() => {
    if (!isOpen) return;

    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    panelRef.current?.querySelector<HTMLButtonElement>('.reader-close')?.focus();

    function handleKeyDown(event: KeyboardEvent) {
      const panel = panelRef.current;
      if (!panel) return;

      if (event.key === 'Escape') {
        if (event.defaultPrevented) return;
        event.preventDefault();
        if (pendingActionRef.current) {
          setPendingAction(null);
        } else {
          closePanelRef.current();
        }
        return;
      }
      if (event.key !== 'Tab') return;

      const confirmDialog = confirmDialogRef.current;
      if (confirmDialog) {
        const confirmButtons = confirmDialog.querySelectorAll<HTMLElement>('button:not(:disabled)');
        if (confirmButtons.length === 0) return;
        const firstConfirmButton = confirmButtons.item(0);
        const lastConfirmButton = confirmButtons.item(confirmButtons.length - 1);
        const active = document.activeElement;
        if (event.shiftKey && (active === firstConfirmButton || !confirmDialog.contains(active))) {
          event.preventDefault();
          lastConfirmButton.focus();
        } else if (!event.shiftKey && (active === lastConfirmButton || !confirmDialog.contains(active))) {
          event.preventDefault();
          firstConfirmButton.focus();
        }
        return;
      }

      const focusable = panel.querySelectorAll<HTMLElement>(
        'a[href], button:not(:disabled), input:not(:disabled), textarea:not(:disabled), audio[controls], video[controls], [tabindex]:not([tabindex="-1"])',
      );
      if (focusable.length === 0) {
        event.preventDefault();
        panel.focus();
        return;
      }

      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      const active = document.activeElement;
      if (event.shiftKey && (active === first || !panel.contains(active))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (active === last || !panel.contains(active))) {
        event.preventDefault();
        first.focus();
      }
    }

    window.addEventListener('keydown', handleKeyDown);
    return () => {
      window.removeEventListener('keydown', handleKeyDown);
      document.body.style.overflow = previousOverflow;
      if (previousFocus && previousFocus !== document.body && previousFocus.isConnected) previousFocus.focus();
    };
  }, [isOpen]);

  useEffect(() => {
    if (pendingAction) confirmButtonRef.current?.focus();
  }, [pendingAction]);

  function releaseCurrentPreview() {
    controllerRef.current?.abort();
    controllerRef.current = null;
    if (previewObjectURLRef.current) URL.revokeObjectURL(previewObjectURLRef.current);
    previewObjectURLRef.current = null;
  }

  function finishClose() {
    releaseCurrentPreview();
    setIsOpen(false);
    onCloseRef.current();
  }

  function closePanel() {
    if (closeGuardRef.current.busy) {
      setSaveMessage('操作尚未完成，请稍后再关闭。');
      return;
    }
    if (closeGuardRef.current.dirty) {
      setPendingAction('close');
      return;
    }
    finishClose();
  }

  closePanelRef.current = closePanel;

  function confirmDiscard() {
    const action = pendingActionRef.current;
    setPendingAction(null);
    if (action === 'close') {
      finishClose();
      return;
    }
    if (action === 'stopEditing' && textState) {
      setDraft(textState.content);
      setIsEditing(false);
      setSaveStatus('idle');
      setSaveMessage('');
    }
  }

  function startEditing() {
    if (isReadOnly || !textState?.version) {
      setSaveStatus('error');
      setSaveMessage('服务未提供文件版本信息，无法安全编辑。');
      return;
    }
    setDraft(textState.content);
    setIsEditing(true);
    setSaveStatus('idle');
    setSaveMessage('');
  }

  function stopEditing() {
    if (isDirty) {
      setPendingAction('stopEditing');
      return;
    }
    setIsEditing(false);
    setSaveStatus('idle');
    setSaveMessage('');
  }

  async function saveText() {
    if (isReadOnly || !textState?.version || saveStatus === 'saving' || saveStatus === 'conflict') return;

    let bytes: Uint8Array;
    try {
      bytes = encodeUTF8(draft);
      if (bytes.byteLength > MAX_TEXT_BYTES) {
        throw new ReaderFailure('文本文件超过 2 MiB 保存上限。');
      }
    } catch (error) {
      setSaveStatus('error');
      setSaveMessage(error instanceof Error ? error.message : '草稿无法编码为 UTF-8。');
      return;
    }

    const targetPath = path;
    const controller = new AbortController();
    saveControllerRef.current?.abort();
    saveControllerRef.current = controller;
    setSaveStatus('saving');
    setSaveMessage('正在保存…');

    try {
      const response = await fetch(contentURL(targetPath), {
        method: 'PUT',
        headers: {
          'Content-Type': 'text/plain; charset=utf-8',
          'X-FileSage-Version': textState.version,
        },
        body: new Blob([bytes], { type: 'text/plain; charset=utf-8' }),
        signal: controller.signal,
        cache: 'no-store',
      });

      if (response.status === 409) {
        if (currentPathRef.current === targetPath) {
          setSaveStatus('conflict');
          setSaveMessage('文件已被其他操作修改，草稿已保留；可重载、同目录另存或下载草稿副本。');
        }
        return;
      }
      if (!response.ok) {
        throw await responseError(response, '保存');
      }

      const result = await response.json().catch(() => undefined) as { path?: unknown; version?: unknown } | undefined;
      if (!result || result.path !== targetPath || typeof result.version !== 'string' || result.version.length === 0) {
        throw new ReaderFailure('保存结果缺少有效版本信息。草稿仍保留，请重载确认文件状态。');
      }
      if (currentPathRef.current === targetPath) {
        versionMonitorStopRef.current?.();
        versionMonitorStopRef.current = null;
        setVersionMonitorRevision((revision) => revision + 1);
        fileVersionRef.current = result.version;
        setState({ status: 'text', path: targetPath, content: draft, version: result.version });
        setDraft(draft);
        setSaveStatus('saved');
        setSaveMessage('已保存。');
      }
    } catch (error) {
      if (error instanceof DOMException && error.name === 'AbortError') return;
      if (currentPathRef.current === targetPath) {
        setSaveStatus('error');
        setSaveMessage(error instanceof Error ? error.message : '保存失败，请检查服务连接后重试。');
      }
    } finally {
      if (saveControllerRef.current === controller) saveControllerRef.current = null;
      if (currentPathRef.current === targetPath) {
        setSaveStatus((status) => status === 'saving' ? 'error' : status);
        setSaveMessage((message) => message === '正在保存…' ? '保存未完成，请重试或重新载入确认。' : message);
      }
    }
  }

  async function saveDraftAs() {
    if (isReadOnly || saveAsStatus === 'saving') return;

    const fileName = saveAsName.trim();
    if (!fileName || fileName === '.' || fileName === '..' || /[\\/\u0000]/.test(fileName)) {
      setSaveAsStatus('error');
      setSaveAsMessage('请输入单个文件名，不能包含路径分隔符。');
      return;
    }

    let bytes: Uint8Array;
    try {
      bytes = encodeUTF8(draft);
      if (bytes.byteLength > MAX_TEXT_BYTES) {
        throw new ReaderFailure('文本文件超过 2 MiB 保存上限。');
      }
    } catch (error) {
      setSaveAsStatus('error');
      setSaveAsMessage(error instanceof Error ? error.message : '草稿无法编码为 UTF-8。');
      return;
    }

    const separator = path.lastIndexOf('/');
    const targetPath = `${separator >= 0 ? path.slice(0, separator + 1) : ''}${fileName}`;
    if (targetPath === path) {
      setSaveAsStatus('error');
      setSaveAsMessage('另存文件名必须与原文件不同。');
      return;
    }

    const controller = new AbortController();
    saveControllerRef.current?.abort();
    saveControllerRef.current = controller;
    setSaveAsStatus('saving');
    setSaveAsMessage('正在根目录内另存…');

    try {
      const response = await fetch(contentURL(targetPath), {
        method: 'PUT',
        headers: {
          'Content-Type': 'text/plain; charset=utf-8',
          'If-None-Match': '*',
        },
        body: new Blob([bytes], { type: 'text/plain; charset=utf-8' }),
        signal: controller.signal,
        cache: 'no-store',
      });
      if (response.status === 409) {
        setSaveAsStatus('exists');
        setSaveAsMessage('目标文件已存在，请修改文件名后重试；草稿仍保留。');
        return;
      }
      if (!response.ok) throw await responseError(response, '另存');

      const result = await response.json().catch(() => undefined) as { path?: unknown; version?: unknown } | undefined;
      if (!result || result.path !== targetPath || typeof result.version !== 'string' || result.version.length === 0) {
        throw new ReaderFailure('另存结果缺少有效版本信息；草稿仍保留，请检查文件列表。');
      }
      setSaveAsStatus('saved');
      setSaveAsMessage(`已另存为“${fileName}”；原文件冲突仍待处理。`);
      onFileCreated?.(targetPath);
    } catch (error) {
      if (error instanceof DOMException && error.name === 'AbortError') return;
      setSaveAsStatus('error');
      setSaveAsMessage(error instanceof Error ? error.message : '另存失败，请检查服务连接后重试。');
    } finally {
      if (saveControllerRef.current === controller) saveControllerRef.current = null;
      setSaveAsStatus((status) => status === 'saving' ? 'error' : status);
      setSaveAsMessage((message) => message === '正在根目录内另存…' ? '另存未完成；草稿仍保留，请重试。' : message);
    }
  }

  async function reloadLatest() {
    if (isReadOnly) return;
    const targetPath = path;
    const controller = new AbortController();
    controllerRef.current?.abort();
    controllerRef.current = controller;
    setReloading(true);
    setSaveMessage('正在重新载入…');
    try {
      const response = await fetch(contentURL(targetPath), { signal: controller.signal, cache: 'no-store' });
      if (!response.ok) throw await responseError(response);
      const content = await readTextResponse(response);
      const version = response.headers.get('X-FileSage-Version')?.trim() || null;
      if (currentPathRef.current === targetPath) {
        versionMonitorStopRef.current?.();
        versionMonitorStopRef.current = null;
        setVersionMonitorRevision((revision) => revision + 1);
        fileVersionRef.current = version;
        setFileExpired(false);
        setState({ status: 'text', path: targetPath, content, version });
        setDraft(content);
        setIsEditing(false);
        setMarkdownMode('preview');
        setSaveStatus(version ? 'idle' : 'error');
        setSaveMessage(version ? '已载入最新版本。原草稿已被放弃。' : '已载入，但服务未提供版本信息；当前只能阅读。');
      }
    } catch (error) {
      if (error instanceof DOMException && error.name === 'AbortError') return;
      if (currentPathRef.current === targetPath) {
        setSaveStatus('conflict');
        setSaveMessage(error instanceof Error ? `重载失败，草稿仍保留：${error.message}` : '重载失败，草稿仍保留。');
      }
    } finally {
      if (controllerRef.current === controller) controllerRef.current = null;
      if (currentPathRef.current === targetPath) setReloading(false);
    }
  }

  function downloadDraftCopy() {
    try {
      const bytes = encodeUTF8(draft);
      if (bytes.byteLength > MAX_TEXT_BYTES) {
        throw new ReaderFailure('文本文件超过 2 MiB 保存上限，无法导出草稿副本。');
      }
    } catch (error) {
      setSaveAsStatus('error');
      setSaveAsMessage(error instanceof Error ? error.message : '草稿无法编码为 UTF-8。');
      return;
    }
    const objectURL = URL.createObjectURL(new Blob([draft], { type: 'text/plain;charset=utf-8' }));
    const anchor = document.createElement('a');
    anchor.href = objectURL;
    anchor.download = `${name}.draft.txt`;
    anchor.click();
    window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
  }

  function retry() {
    releaseCurrentPreview();
    setFileExpired(false);
    setRetryCount((count) => count + 1);
  }

  if (!isOpen) return null;

  const downloadHref = sourceHref;
  const titleId = `${readerId}-title`;
  const pathId = `${readerId}-path`;
  const visibleState = 'path' in state && state.path !== path ? { status: 'loading' as const } : state;

  return (
    <div className="reader-backdrop">
      <section
        ref={panelRef}
        className="reader-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={pathId}
        tabIndex={-1}
      >
        <header className="reader-header">
          <div className="reader-file-heading">
            <span className="reader-eyebrow">{isReadOnly ? '压缩包条目 · 只读' : '文件阅读'}</span>
            <h2 id={titleId} title={name}>{name}</h2>
            <p id={pathId} title={path}>/{path}</p>
          </div>
          <div className="reader-header-actions">
            <a className="reader-download" href={downloadHref} download={name} aria-label={`下载 ${name} 原件`}>
              <ArrowDownToLine size={17} aria-hidden="true" /><span>下载原件</span>
            </a>
            <button className="reader-close" type="button" onClick={closePanel} aria-label={`关闭阅读面板：${name}`}>
              <X size={19} aria-hidden="true" />
            </button>
          </div>
        </header>

        <div className="reader-toolbar">
          <span>{isReadOnly ? '压缩包内容 · 只读' : kind === 'epub' ? 'EPUB · 只读阅读' : kind === 'xlsx' ? 'XLSX · 只读表格' : kind === 'text' ? (tableFormat ? `${tableFormat.label} · ${isEditing ? '编辑' : '表格'}` : isMarkdown(name) ? `Markdown · ${isEditing ? '编辑' : markdownMode === 'preview' ? '阅读' : '原文'}` : '纯文本') : kind === 'pdf' ? 'PDF · 阅读' : kind === 'office' && extensionOf(name) === 'docx' ? 'DOCX · 结构化只读阅读' : kind === 'office' ? 'Office · PDF 只读预览' : `${kind} · 浏览器原生预览`}</span>
          {kind === 'image' && <span className="reader-image-capture"><span>拍摄时间</span><time dateTime={capturedAt}>{formatCaptureDateTime(capturedAt)}</time></span>}
        </div>

        {fileExpired && visibleState.status !== 'loading' && (
          <div className="reader-expired" role="alert">
            <div>
              <strong>文件状态已变化，当前预览已过期。</strong>
              {visibleState.status === 'text' && (isEditing || isDirty)
                ? <p>当前草稿已保留，保存已停用。重载会放弃草稿；也可以先另存为。</p>
                : <p>重新载入后可查看最新内容。</p>}
            </div>
            {visibleState.status === 'text'
              ? <button className="reader-action-button" type="button" onClick={() => void reloadLatest()} disabled={isBusy}>{isEditing || isDirty ? '放弃草稿并重载' : '重载最新版本'}</button>
              : <button className="reader-action-button" type="button" onClick={retry}><RefreshCw size={16} aria-hidden="true" />重新加载预览</button>}
          </div>
        )}

        <main className={`reader-content${visibleState.status === 'epub' ? ' reader-content-epub' : ''}${visibleState.status === 'docx' ? ' reader-content-docx' : ''}`}>
          {visibleState.status === 'loading' && (
            <div className="reader-state" role="status">
              <LoaderCircle className="reader-spinner" size={24} aria-hidden="true" />
              <strong>正在读取原件…</strong>
              <span>内容以只读方式载入，不会执行文件内脚本或加载外部资源。</span>
            </div>
          )}

          {visibleState.status === 'error' && (
            <div className="reader-state reader-state-error" role="alert">
              <AlertTriangle size={24} aria-hidden="true" />
              <strong>预览失败</strong>
              <p>{visibleState.message}</p>
              <div className="reader-state-actions">
                <button type="button" className="reader-retry" onClick={retry}>
                  <RefreshCw size={16} aria-hidden="true" />重新加载
                </button>
                <a href={downloadHref} download={name} className="reader-download reader-download-secondary">
                  <ArrowDownToLine size={16} aria-hidden="true" />下载原件
                </a>
              </div>
            </div>
          )}

          {visibleState.status === 'unsupported' && (
            <div className="reader-state reader-state-error" role="status">
              <AlertTriangle size={24} aria-hidden="true" />
              <strong>此格式无法预览</strong>
              <p>{visibleState.message}</p>
              <a href={downloadHref} download={name} className="reader-download reader-download-secondary">
                <ArrowDownToLine size={16} aria-hidden="true" />下载原件
              </a>
            </div>
          )}

          {visibleState.status === 'text' && (
            <div className="reader-text-wrap">
              <div className="reader-text-actions">
                <span>{tableFormat ? `${tableFormat.label} · UTF-8 · 上限 2 MiB` : 'UTF-8 · 上限 2 MiB'}</span>
                <div className="reader-text-controls">
                  {isMarkdown(name) && !isEditing && (
                    <div className="reader-text-buttons" role="group" aria-label="Markdown 显示模式">
                      <button
                        className={`reader-action-button reader-mode-button${markdownMode === 'preview' ? ' reader-mode-button-active' : ''}`}
                        type="button"
                        aria-pressed={markdownMode === 'preview'}
                        onClick={() => setMarkdownMode('preview')}
                      >阅读</button>
                      <button
                        className={`reader-action-button reader-mode-button${markdownMode === 'source' ? ' reader-mode-button-active' : ''}`}
                        type="button"
                        aria-pressed={markdownMode === 'source'}
                        onClick={() => setMarkdownMode('source')}
                      >原文</button>
                    </div>
                  )}
                  {!tableFormat && !isReadOnly && (
                    <div className="reader-text-buttons">
                      {!isEditing ? (
                        <button className="reader-action-button" type="button" onClick={startEditing} disabled={!visibleState.version}>
                          <Pencil size={16} aria-hidden="true" />编辑
                        </button>
                      ) : (
                        <>
                          <button className="reader-action-button reader-save-button" type="button" onClick={() => void saveText()} disabled={isBusy || saveStatus === 'conflict' || !isDirty}>
                            {saveStatus === 'saving' ? <LoaderCircle className="reader-spinner" size={16} aria-hidden="true" /> : <Save size={16} aria-hidden="true" />}
                            {saveStatus === 'saving' ? '保存中' : '保存'}
                          </button>
                          <button className="reader-action-button" type="button" onClick={stopEditing} disabled={isBusy}>取消编辑</button>
                        </>
                      )}
                    </div>
                  )}
                </div>
              </div>
              {!isReadOnly && !tableFormat && !visibleState.version && <p className="reader-version-note" role="status">服务未提供文件版本信息，当前只能阅读，无法安全编辑。</p>}
              {isEditing ? (
                <Suspense fallback={<div className="reader-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在加载文本编辑器…</div>}>
                  <TextCodeEditor
                    name={name}
                    value={draft}
                    disabled={isBusy}
                    onChange={(value) => {
                      setDraft(value);
                      if (saveStatus !== 'conflict') {
                        setSaveStatus('idle');
                        setSaveMessage('');
                      }
                    }}
                  />
                </Suspense>
              ) : tableFormat ? (
                <Suspense fallback={<div className="reader-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在解析表格…</div>}>
                  <DelimitedTextTable
                    content={visibleState.content}
                    delimiter={tableFormat.delimiter}
                    formatName={tableFormat.label}
                    fileName={name}
                  />
                </Suspense>
              ) : isMarkdown(name) && markdownMode === 'preview' ? (
                <Suspense fallback={<div className="reader-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在渲染 Markdown…</div>}>
                  <MarkdownPreview path={path} content={visibleState.content} allowLocalImages={!isReadOnly} />
                </Suspense>
              ) : (
                <pre className="reader-text-content" tabIndex={0} aria-label={`${name} 的${isMarkdown(name) ? 'Markdown 原文' : '纯文本内容'}`}>{visibleState.content}</pre>
              )}
              {saveMessage && saveStatus !== 'conflict' && (
                <p className={`reader-save-message reader-save-${saveStatus}`} role={saveStatus === 'error' ? 'alert' : 'status'}>{saveMessage}</p>
              )}
              {saveStatus === 'conflict' && (
                <div className="reader-conflict" role="alert">
                  <p>{saveMessage || '文件已被其他操作修改，草稿已保留。'}</p>
                  <div className="reader-conflict-actions">
                    {!fileExpired && <button className="reader-action-button" type="button" onClick={() => void reloadLatest()} disabled={isBusy}>
                      <RefreshCw size={16} aria-hidden="true" />放弃草稿并重载
                    </button>}
                    <button className="reader-action-button" type="button" onClick={downloadDraftCopy}>
                      <ArrowDownToLine size={16} aria-hidden="true" />下载草稿副本
                    </button>
                  </div>
                  <div className="reader-save-as">
                    <label htmlFor="reader-save-as-name">在当前文件夹另存为</label>
                    <div className="reader-save-as-row">
                      <input
                        id="reader-save-as-name"
                        type="text"
                        value={saveAsName}
                        disabled={isBusy}
                        onKeyDown={(event) => {
                          if (event.key === 'Enter') {
                            event.preventDefault();
                            void saveDraftAs();
                          }
                        }}
                        onChange={(event) => {
                          setSaveAsName(event.target.value);
                          setSaveAsStatus('idle');
                          setSaveAsMessage('');
                        }}
                      />
                      <button className="reader-action-button" type="button" onClick={() => void saveDraftAs()} disabled={isBusy || !saveAsName.trim()}>
                        {saveAsStatus === 'saving' ? <LoaderCircle className="reader-spinner" size={16} aria-hidden="true" /> : <Save size={16} aria-hidden="true" />}
                        {saveAsStatus === 'saving' ? '另存中' : '另存为'}
                      </button>
                    </div>
                    {saveAsMessage && <p className={`reader-save-as-message reader-save-as-${saveAsStatus}`} role={saveAsStatus === 'error' || saveAsStatus === 'exists' ? 'alert' : 'status'}>{saveAsMessage}</p>}
                  </div>
                  <span>服务器会在原文件所在目录新建文件；若目标已存在，不会覆盖。</span>
                </div>
              )}
            </div>
          )}

          {visibleState.status === 'pdf' && (
            <Suspense fallback={<div className="reader-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在加载 PDF 阅读器…</div>}>
              <PdfReader
                key={`${path}-${retryCount}`}
                url={visibleState.url}
                downloadUrl={downloadHref}
                fileName={name}
                presentationMode={kind === 'office' && extensionOf(name) === 'pptx'}
              />
            </Suspense>
          )}

          {visibleState.status === 'xlsx' && (
            <Suspense fallback={<div className="reader-xlsx-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在载入 XLSX 阅读器…</div>}>
              <XlsxReader key={`${path}-${retryCount}`} url={sourceHref} fileName={name} onRetry={retry} />
            </Suspense>
          )}

          {visibleState.status === 'epub' && (
            <Suspense fallback={<div className="reader-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在载入 EPUB 阅读器…</div>}>
              <EpubReader key={`${path}-${retryCount}`} path={sourceURL ? sourceHref : path} name={name} url={sourceHref} downloadUrl={downloadHref} />
            </Suspense>
          )}

          {visibleState.status === 'docx' && (
            <Suspense fallback={<div className="reader-state" role="status"><LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />正在载入 DOCX 阅读器…</div>}>
              <DocxReader key={`${path}-${retryCount}`} path={sourceURL ? sourceHref : path} name={name} url={sourceHref} downloadUrl={downloadHref} />
            </Suspense>
          )}

          {visibleState.status === 'media' && visibleState.previewFailed && (
            <div className="reader-state reader-state-error" role="status">
              <AlertTriangle size={24} aria-hidden="true" />
              <strong>媒体预览不可用</strong>
              <p>预览请求失败，或当前浏览器无法解码 {visibleState.mimeType}。可重试，或下载原件后使用其他应用打开。</p>
              <div className="reader-state-actions">
                <button type="button" className="reader-retry" onClick={retry}>
                  <RefreshCw size={16} aria-hidden="true" />重新加载
                </button>
                <a href={downloadHref} download={name} className="reader-download reader-download-secondary">
                  <ArrowDownToLine size={16} aria-hidden="true" />下载原件
                </a>
              </div>
            </div>
          )}

          {visibleState.status === 'media' && !visibleState.previewFailed && (
            <div className={`reader-media reader-media-${kind}`}>
              {visibleState.previewLoading && (
                <div className="reader-media-status" role="status">
                  <LoaderCircle className="reader-spinner" size={22} aria-hidden="true" />
                  <span>正在载入媒体预览…</span>
                </div>
              )}
              {kind === 'image' && <img key={`${path}-${retryCount}`} src={visibleState.url} alt={name} onLoad={() => setState((current) => current.status === 'media' && current.path === path ? { ...current, previewLoading: false } : current)} onError={() => setState((current) => current.status === 'media' && current.path === path ? { ...current, previewLoading: false, previewFailed: true } : current)} />}
              {kind === 'audio' && (
                <div className="reader-media-player reader-media-player-audio">
                  <audio key={`${path}-${retryCount}`} src={visibleState.url} controls preload="metadata" aria-label={`音频预览：${name}`} onLoadedMetadata={handleMediaEvent} onDurationChange={handleMediaEvent} onCanPlay={handleMediaEvent} onCanPlayThrough={handleMediaEvent} onPlay={handleMediaEvent} onPlaying={handleMediaEvent} onPause={handleMediaEvent} onWaiting={handleMediaEvent} onStalled={handleMediaEvent} onSeeking={handleMediaEvent} onSeeked={handleMediaEvent} onEnded={handleMediaEvent} onTimeUpdate={handleMediaEvent} onError={handleMediaEvent} />
                  <div className="reader-media-playback">
                    <span role="status" aria-live="polite">{mediaPlaybackLabel(mediaPlayback.status)}</span>
                    <span aria-label="播放进度">{formatMediaTime(mediaPlayback.currentTime)} / {formatMediaTime(mediaPlayback.duration)}</span>
                  </div>
                </div>
              )}
              {kind === 'video' && (
                <div className="reader-media-player reader-media-player-video">
                  <video key={`${path}-${retryCount}`} src={visibleState.url} controls preload="metadata" aria-label={`视频预览：${name}`} onLoadedMetadata={handleMediaEvent} onDurationChange={handleMediaEvent} onCanPlay={handleMediaEvent} onCanPlayThrough={handleMediaEvent} onPlay={handleMediaEvent} onPlaying={handleMediaEvent} onPause={handleMediaEvent} onWaiting={handleMediaEvent} onStalled={handleMediaEvent} onSeeking={handleMediaEvent} onSeeked={handleMediaEvent} onEnded={handleMediaEvent} onTimeUpdate={handleMediaEvent} onError={handleMediaEvent} />
                  <div className="reader-media-playback">
                    <span role="status" aria-live="polite">{mediaPlaybackLabel(mediaPlayback.status)}</span>
                    <span aria-label="播放进度">{formatMediaTime(mediaPlayback.currentTime)} / {formatMediaTime(mediaPlayback.duration)}</span>
                  </div>
                </div>
              )}
            </div>
          )}
        </main>
        {pendingAction && (
          <div className="reader-unsaved-overlay">
            <div className="reader-unsaved-dialog" role="alertdialog" aria-modal="true" aria-labelledby="reader-unsaved-title" aria-describedby="reader-unsaved-copy" ref={confirmDialogRef}>
              <h3 id="reader-unsaved-title">尚有未保存的修改</h3>
              <p id="reader-unsaved-copy">继续操作会丢弃当前草稿。你可以返回编辑，或明确放弃草稿。</p>
              <div className="reader-unsaved-actions">
                <button className="reader-action-button" type="button" onClick={() => setPendingAction(null)}>返回编辑</button>
                <button className="reader-action-button reader-discard-button" type="button" onClick={confirmDiscard} ref={confirmButtonRef}>
                  {pendingAction === 'close' ? '放弃并关闭' : '放弃草稿'}
                </button>
              </div>
            </div>
          </div>
        )}
      </section>
    </div>
  );
}
