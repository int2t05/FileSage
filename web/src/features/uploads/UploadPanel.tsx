import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react';
import type { ChangeEvent, DragEvent } from 'react';
import * as tus from 'tus-js-client';
import {
  AlertTriangle,
  CheckCircle2,
  FileUp,
  FolderOpen,
  Loader2,
  Pause,
  Play,
  RefreshCw,
  RotateCcw,
  Trash2,
  UploadCloud,
  X,
} from 'lucide-react';
import {
  getUploadPublication,
  listUploadRecoveries,
  retryUploadPublication,
  type PublicationStatus,
  type UploadRecoveryRecord,
  UploadApiError,
} from '../../api/uploads';
import { createUploadFingerprint } from './uploadFingerprint';
import { findMatchingPreviousUpload, recoveryPhase } from './uploadRecovery';
import { createUploadTaskStore, uploadCancellationRequests } from './uploadTaskStore';
import './UploadPanel.css';

type UploadPhase =
  | 'preparing'
  | 'resuming'
  | 'starting'
  | 'transferring'
  | 'paused'
  | 'transfer-error'
  | 'checking'
  | 'publishing'
  | 'published'
  | 'failed'
  | 'partial'
  | 'cancelled'
  | 'status-error'
  | 'cancelling'
  | 'resume-needed'
  | 'unavailable';

// uploadTransferErrorMessage 将服务端限额错误转成可操作提示。
function uploadTransferErrorMessage(error: Error): string {
  if (error instanceof tus.DetailedError) {
    const response = error.originalResponse;
    const status = response?.getStatus();
    if (status === 413 && response?.getBody().includes('UPLOAD_STAGING_LIMIT')) {
      return '上传暂存空间已满，请等待当前上传完成或取消任务后重试。';
    }
    if (status === 413) return '文件超过服务端单文件上传上限。';
    if (status === 429) return '待处理上传过多，请完成或取消部分任务后重试。';
    if (status === 507) return '暂存状态无法核验，现有上传数据已保留；请联系管理员检查服务日志。';
  }
  return error.message || '上传失败，请重试。';
}

interface UploadTask {
  id: string;
  upload?: tus.Upload;
  file?: File;
  name: string;
  relativePath: string;
  targetPath: string;
  size: number;
  bytesSent: number;
  totalBytes: number;
  rateBytesPerSecond: number;
  hashProgress: number;
  resumed: boolean;
  phase: UploadPhase;
  uploadId?: string;
  serverStatus?: PublicationStatus;
  publishedPath?: string;
  error?: string;
  statusError?: string;
  retrying?: boolean;
  cancelAttemptActive?: boolean;
  restored?: boolean;
}

interface DroppedFile {
  file: File;
  relativePath: string;
}

type RecoveryLoadState = 'idle' | 'loading' | 'ready' | 'error';

interface FileSystemEntryLike {
  isFile: boolean;
  isDirectory: boolean;
  name: string;
  file?: (success: (file: File) => void, failure?: (error: unknown) => void) => void;
  createReader?: () => {
    readEntries: (success: (entries: FileSystemEntryLike[]) => void, failure?: (error: unknown) => void) => void;
  };
}

interface FileSystemDirectoryReaderLike {
  readEntries: (success: (entries: FileSystemEntryLike[]) => void, failure?: (error: unknown) => void) => void;
}

type DataTransferItemWithEntry = DataTransferItem & {
  webkitGetAsEntry?: () => FileSystemEntryLike | null;
};

const UPLOAD_ENDPOINT = '/api/v1/uploads/';
const uploadTaskStore = createUploadTaskStore<UploadTask>();
const pollingTimers = new Map<string, ReturnType<typeof setTimeout>>();

function isTaskCancelled(id: string): boolean {
  return uploadCancellationRequests.has(id);
}

interface UploadPanelProps {
  open: boolean;
  targetPath: string;
  onClose: () => void;
  onPublished: () => void;
}

function makeId(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let index = -1;
  do {
    value /= 1024;
    index += 1;
  } while (value >= 1024 && index < units.length - 1);
  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

function publicationPhase(status: PublicationStatus): UploadPhase {
  switch (status) {
    case 'published': return 'published';
    case 'failed': return 'failed';
    case 'partial': return 'partial';
    case 'cancelling': return 'cancelling';
    case 'cancelled': return 'cancelled';
    default: return 'publishing';
  }
}

function recoveredUploadTask(record: UploadRecoveryRecord): UploadTask {
  const publication = record.publication;
  const target = publication?.targetPath || record.task.scopePath;
  const name = target.split('/').filter(Boolean).at(-1) || '上传文件';
  const phase = publication
    ? recoveryPhase(publication.status)
    : record.errorStatus === 404 ? 'unavailable' : 'status-error';
  return {
    id: record.task.id,
    uploadId: record.task.id,
    name,
    relativePath: target,
    targetPath: target,
    size: publication?.size ?? 0,
    bytesSent: publication?.offset ?? 0,
    totalBytes: publication?.size ?? 0,
    rateBytesPerSecond: 0,
    hashProgress: 100,
    resumed: false,
    phase,
    serverStatus: publication?.status,
    publishedPath: publication?.publishedPath,
    error: publication?.error,
    statusError: record.error,
    restored: true,
  };
}

function publicationLabel(status?: PublicationStatus): string {
  switch (status) {
    case 'receiving': return '服务端仍在接收';
    case 'queued': return '已暂存，等待服务端发布';
    case 'publishing': return '服务端正在发布';
    case 'published': return '服务端已确认发布';
    case 'failed': return '服务端发布失败';
    case 'partial': return '服务端报告部分完成，需核对';
    case 'cancelling': return '服务端正在确认取消';
    case 'cancelled': return '发布已取消';
    default: return '正在查询服务端状态';
  }
}

function phaseLabel(task: UploadTask): string {
  switch (task.phase) {
    case 'preparing': return `正在分块校验文件（${task.hashProgress.toFixed(0)}%），检查可续传记录`;
    case 'resuming': return '找到匹配的旧暂存，正在恢复上传';
    case 'starting': return task.resumed ? '正在连接旧暂存' : '未找到匹配暂存，正在创建上传';
    case 'transferring': return task.resumed ? '正在从旧暂存继续上传' : '正在上传';
    case 'paused': return '已暂停';
    case 'transfer-error': return '上传失败';
    case 'checking': return '暂存完成，正在查询发布状态';
    case 'publishing': return publicationLabel(task.serverStatus);
    case 'published': return publicationLabel('published');
    case 'failed': return publicationLabel('failed');
    case 'partial': return publicationLabel('partial');
    case 'cancelled': return '已取消';
    case 'status-error': return '无法确认发布状态';
    case 'cancelling': return '正在取消上传';
    case 'resume-needed': return '服务端已接收，等待选择原文件续传';
    case 'unavailable': return '暂存状态不可用';
  }
}

function extractUploadId(uploadURL: string | null | undefined): string | undefined {
  if (!uploadURL) return undefined;
  try {
    const id = new URL(uploadURL, window.location.origin).pathname.split('/').filter(Boolean).at(-1);
    return id && /^[a-f0-9]{32}$/.test(id) ? id : undefined;
  } catch {
    return undefined;
  }
}

function readDirectoryEntries(reader: FileSystemDirectoryReaderLike): Promise<FileSystemEntryLike[]> {
  return new Promise((resolve, reject) => {
    const collected: FileSystemEntryLike[] = [];
    const readBatch = () => {
      reader.readEntries((entries) => {
        if (entries.length === 0) {
          resolve(collected);
          return;
        }
        collected.push(...entries);
        readBatch();
      }, reject);
    };
    readBatch();
  });
}

async function collectDroppedEntry(entry: FileSystemEntryLike, parent: string[] = []): Promise<DroppedFile[]> {
  if (entry.isFile && entry.file) {
    const file = await new Promise<File>((resolve, reject) => entry.file?.(resolve, reject));
    return [{ file, relativePath: [...parent, file.name].join('/') }];
  }
  if (!entry.isDirectory || !entry.createReader) return [];
  const children = await readDirectoryEntries(entry.createReader());
  const nextParent = [...parent, entry.name];
  const nested = await Promise.all(children.map((child) => collectDroppedEntry(child, nextParent)));
  return nested.flat();
}

async function filesFromDrop(dataTransfer: DataTransfer): Promise<DroppedFile[]> {
  const items = Array.from(dataTransfer.items) as DataTransferItemWithEntry[];
  const entries = items
    .filter((item) => item.kind === 'file')
    .map((item) => item.webkitGetAsEntry?.() ?? null);
  if (entries.length > 0 && entries.every((entry) => entry !== null)) {
    const groups = await Promise.all(entries.map((entry) => collectDroppedEntry(entry as FileSystemEntryLike)));
    return groups.flat();
  }
  return Array.from(dataTransfer.files).map((file) => ({ file, relativePath: file.name }));
}

export function UploadPanel({ open, targetPath, onClose, onPublished }: UploadPanelProps) {
  const tasks = useSyncExternalStore(uploadTaskStore.subscribe, uploadTaskStore.getSnapshot, uploadTaskStore.getSnapshot);
  const setTasks = uploadTaskStore.set;
  const [recoveryState, setRecoveryState] = useState<RecoveryLoadState>('idle');
  const [recoveryError, setRecoveryError] = useState('');
  const [recoveryRetryToken, setRecoveryRetryToken] = useState(0);
  const [dropActive, setDropActive] = useState(false);
  const [selectionError, setSelectionError] = useState('');
  const fileInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);
  const setFolderInputRef = useCallback((input: HTMLInputElement | null) => {
    folderInputRef.current = input;
    // 面板关闭时输入框不在 DOM，因此在挂载时设置目录选择属性。
    input?.setAttribute('webkitdirectory', '');
    input?.setAttribute('directory', '');
  }, []);
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const previousFocusRef = useRef<HTMLElement | null>(null);
  const publishedNotifiedRef = useRef(new Set<string>());
  const mountedRef = useRef(false);

  const updateTask = useCallback((id: string, update: (task: UploadTask) => UploadTask) => {
    setTasks((current) => current.map((task) => task.id === id ? update(task) : task));
  }, []);

  const scheduleStatusCheck = useCallback((taskId: string, uploadId: string, attempt: number, delay: number) => {
    const previous = pollingTimers.get(taskId);
    if (previous) clearTimeout(previous);
    const timer = setTimeout(() => {
      pollingTimers.delete(taskId);
      void checkStatus(taskId, uploadId, attempt);
    }, delay);
    pollingTimers.set(taskId, timer);
  }, []);

  const checkStatus = useCallback(async (taskId: string, uploadId: string, attempt = 0) => {
    try {
      const publication = await getUploadPublication(uploadId);
      const terminal = ['published', 'failed', 'partial', 'cancelled'].includes(publication.status);
      updateTask(taskId, (task) => ({
        ...task,
        phase: isTaskCancelled(taskId) && !terminal
          ? 'cancelling'
          : publication.status === 'receiving'
            ? task.restored ? 'resume-needed' : task.phase
            : publicationPhase(publication.status),
        serverStatus: publication.status,
        bytesSent: publication.offset,
        totalBytes: publication.size,
        publishedPath: publication.publishedPath,
        error: publication.error,
        statusError: undefined,
        ...(terminal ? { upload: undefined, file: undefined } : {}),
      }));
      if (terminal) uploadCancellationRequests.delete(taskId);
      if (publication.status === 'published') {
        if (!publishedNotifiedRef.current.has(uploadId)) {
          publishedNotifiedRef.current.add(uploadId);
          if (mountedRef.current) onPublished();
        }
        return;
      }
      if (terminal) return;
      scheduleStatusCheck(taskId, uploadId, 0, 1400);
    } catch (error) {
      const message = error instanceof UploadApiError ? error.message : '无法读取服务端发布状态。';
      updateTask(taskId, (task) => ({
        ...task,
        phase: isTaskCancelled(taskId) ? 'cancelling' : task.phase === 'checking' ? 'status-error' : task.phase,
        statusError: message,
      }));
      scheduleStatusCheck(taskId, uploadId, attempt + 1, Math.min(1500 * (2 ** Math.min(attempt, 3)), 10000));
    }
  }, [onPublished, scheduleStatusCheck, updateTask]);

  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    let active = true;
    setRecoveryState('loading');
    setRecoveryError('');
    listUploadRecoveries(controller.signal)
      .then((records) => {
        if (!active) return;
        const restored = records.map(recoveredUploadTask);
        setTasks((current) => {
          const liveTasks = current.filter((task) => !task.restored);
          const liveUploadIds = new Set(liveTasks.flatMap((task) => task.uploadId ? [task.uploadId] : []));
          return [
            ...liveTasks,
            ...restored.filter((task) => task.uploadId && !liveUploadIds.has(task.uploadId)),
          ];
        });
        for (const record of records) {
          if (record.publication && ['queued', 'publishing', 'cancelling'].includes(record.publication.status)) {
            scheduleStatusCheck(record.task.id, record.task.id, 0, 1200);
          }
        }
        setRecoveryState('ready');
      })
      .catch((error: unknown) => {
        if (!active || controller.signal.aborted) return;
        setRecoveryState('error');
        setRecoveryError(error instanceof UploadApiError ? error.message : '无法读取服务端上传记录。');
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [open, recoveryRetryToken, scheduleStatusCheck]);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  useEffect(() => {
    if (!open) return;
    previousFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    closeButtonRef.current?.focus();

    function handleKeys(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        event.preventDefault();
        onClose();
        return;
      }
      if (event.key !== 'Tab') return;
      const dialog = document.querySelector<HTMLElement>('.upload-panel');
      if (!dialog) return;
      const focusable = dialog.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled):not(.upload-hidden-input)');
      if (focusable.length === 0) {
        event.preventDefault();
        dialog.focus();
        return;
      }
      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      if (event.shiftKey && (document.activeElement === first || !dialog.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !dialog.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    }

    window.addEventListener('keydown', handleKeys);
    return () => {
      window.removeEventListener('keydown', handleKeys);
      document.body.style.overflow = previousOverflow;
      if (previousFocusRef.current?.isConnected) previousFocusRef.current.focus();
    };
  }, [onClose, open]);

  async function startUploadTask(task: UploadTask) {
    const upload = task.upload;
    const file = task.file;
    if (!upload || !file) return;
    try {
      const previousUploads = await upload.findPreviousUploads();
      if (isTaskCancelled(task.id)) return;

      const expectedMetadata = {
        filename: file.name,
        target: task.targetPath || '.',
        relativepath: task.relativePath,
        filetype: file.type || 'application/octet-stream',
      };
      const previous = findMatchingPreviousUpload(previousUploads, file.size, expectedMetadata, window.location.origin);
      if (previous) upload.resumeFromPreviousUpload(previous);
      const previousUploadId = previous && extractUploadId(previous.uploadUrl);

      updateTask(task.id, (current) => ({
        ...current,
        phase: previous ? 'resuming' : 'starting',
        resumed: Boolean(previous),
        hashProgress: 100,
        uploadId: extractUploadId(upload.url),
      }));
      if (previousUploadId) {
        setTasks((current) => current.filter((existing) => !(existing.restored && existing.uploadId === previousUploadId)));
      }
      if (isTaskCancelled(task.id)) {
        if (upload.url) void upload.abort(true);
        return;
      }
      upload.start();
    } catch (error) {
      if (isTaskCancelled(task.id)) return;
      updateTask(task.id, (current) => ({
        ...current,
        phase: 'transfer-error',
        error: error instanceof Error ? error.message : '读取上传指纹或续传记录失败。',
      }));
    }
  }

  function enqueueFiles(files: DroppedFile[]) {
    setSelectionError('');
    if (files.length === 0) {
      setSelectionError('没有读取到可上传的文件。');
      return;
    }

    const newTasks = files.map(({ file, relativePath }) => {
      const id = makeId();
      let lastProgress: { sent: number; time: number } | undefined;
      let lastHashProgress = 0;
      let upload: tus.Upload;
      let fingerprintPromise: Promise<string> | undefined;
      const target = targetPath || '.';
      const filetype = file.type || 'application/octet-stream';
      const getFingerprint = () => {
        fingerprintPromise ??= createUploadFingerprint(
          file,
          target,
          relativePath,
          (bytesRead) => {
            const progress = file.size === 0 ? 100 : Math.min(100, (bytesRead / file.size) * 100);
            if (progress < 100 && progress - lastHashProgress < 2) return;
            lastHashProgress = progress;
            updateTask(id, (task) => ({ ...task, hashProgress: progress }));
          },
          () => isTaskCancelled(id),
        );
        return fingerprintPromise;
      };
      upload = new tus.Upload(file, {
        endpoint: UPLOAD_ENDPOINT,
        fingerprint: getFingerprint,
        retryDelays: [0, 1000, 3000, 5000],
        metadata: {
          filename: file.name,
          target,
          relativepath: relativePath,
          filetype,
        },
        onUploadUrlAvailable() {
          const uploadId = extractUploadId(upload.url);
          if (uploadId) updateTask(id, (task) => ({ ...task, uploadId, statusError: undefined }));
          if (uploadId && isTaskCancelled(id)) {
            // 给 tus 留一个微任务周期保存 URL，随后取消才能清理服务器暂存和本地续传记录。
            updateTask(id, (task) => ({ ...task, cancelAttemptActive: true }));
            window.setTimeout(() => {
              void upload.abort(true)
                .then(async () => { await checkStatus(id, uploadId); })
                .catch((error: unknown) => {
                  updateTask(id, (task) => ({
                    ...task,
                    cancelAttemptActive: false,
                    statusError: error instanceof Error ? error.message : '取消暂存上传失败。',
                  }));
                  void checkStatus(id, uploadId);
                })
                .finally(() => {
                  updateTask(id, (task) => ({ ...task, cancelAttemptActive: false }));
                });
            }, 0);
          }
        },
        onProgress(bytesSent, bytesTotal) {
          if (isTaskCancelled(id)) return;
          const now = performance.now();
          const rate = lastProgress && now > lastProgress.time
            ? Math.max(0, (bytesSent - lastProgress.sent) / ((now - lastProgress.time) / 1000))
            : 0;
          lastProgress = { sent: bytesSent, time: now };
          updateTask(id, (task) => ['starting', 'resuming', 'transferring'].includes(task.phase)
            ? {
              ...task,
              phase: 'transferring',
              bytesSent,
              totalBytes: bytesTotal ?? file.size,
              rateBytesPerSecond: rate,
              error: undefined,
            }
            : task);
        },
        onSuccess() {
          if (isTaskCancelled(id)) return;
          const uploadId = extractUploadId(upload.url);
          if (!uploadId) {
            updateTask(id, (task) => ({
              ...task,
              phase: 'status-error',
              statusError: '数据已完成暂存，但服务端没有返回可查询的上传编号。',
            }));
            return;
          }
          updateTask(id, (task) => ['published', 'failed', 'partial', 'cancelled', 'cancelling'].includes(task.phase)
            ? task
            : { ...task, phase: 'checking', uploadId, bytesSent: file.size, totalBytes: file.size });
          void checkStatus(id, uploadId);
        },
        onError(error) {
          if (isTaskCancelled(id)) {
            if (!extractUploadId(upload.url)) {
              updateTask(id, (task) => ({
                ...task,
                phase: 'status-error',
                cancelAttemptActive: false,
                statusError: '服务端未返回上传编号，无法核实暂存是否已取消；文件数据没有完成传输。',
              }));
            }
            return;
          }
          updateTask(id, (task) => ['published', 'failed', 'partial', 'cancelled', 'cancelling', 'checking', 'publishing'].includes(task.phase)
            ? task
            : { ...task, phase: 'transfer-error', error: uploadTransferErrorMessage(error) });
        },
      });
      const task: UploadTask = {
        id,
        upload,
        file,
        name: file.name,
        relativePath,
        targetPath,
        size: file.size,
        bytesSent: 0,
        totalBytes: file.size,
        rateBytesPerSecond: 0,
        hashProgress: 0,
        resumed: false,
        phase: 'preparing',
      };
      return task;
    });

    setTasks((current) => [...newTasks, ...current]);
    for (const task of newTasks) void startUploadTask(task);
  }

  function handleFileSelection(event: ChangeEvent<HTMLInputElement>) {
    const files = Array.from(event.currentTarget.files ?? []).map((file) => ({
      file,
      relativePath: file.webkitRelativePath || file.name,
    }));
    enqueueFiles(files);
    event.currentTarget.value = '';
  }

  async function handleDrop(event: DragEvent<HTMLDivElement>) {
    event.preventDefault();
    setDropActive(false);
    try {
      enqueueFiles(await filesFromDrop(event.dataTransfer));
    } catch {
      setSelectionError('读取拖入的文件夹失败；请改用文件夹选择按钮重试。');
    }
  }

  async function pauseTask(task: UploadTask) {
    const upload = task.upload;
    if (!upload || isTaskCancelled(task.id)) return;
    try {
      await upload.abort();
      updateTask(task.id, (current) => current.phase === 'transferring' ? { ...current, phase: 'paused' } : current);
    } catch {
      updateTask(task.id, (current) => ({ ...current, error: '暂停上传失败，请重试。' }));
    }
  }

  function resumeTask(task: UploadTask) {
    const upload = task.upload;
    if (!upload || isTaskCancelled(task.id)) return;
    updateTask(task.id, (current) => ({ ...current, phase: 'transferring', error: undefined }));
    upload.start();
  }

  async function cancelTask(task: UploadTask) {
    if (['published', 'failed', 'partial', 'cancelled'].includes(task.phase) || task.cancelAttemptActive) return;
    uploadCancellationRequests.add(task.id);
    const upload = task.upload;
    const uploadId = task.uploadId ?? extractUploadId(upload?.url);
    if (task.phase === 'preparing') {
      await upload?.abort(true);
      updateTask(task.id, (current) => ({ ...current, phase: 'cancelled', upload: undefined, file: undefined, statusError: undefined }));
      return;
    }
    const timer = pollingTimers.get(task.id);
    if (timer) clearTimeout(timer);
    pollingTimers.delete(task.id);
    updateTask(task.id, (current) => ({ ...current, phase: 'cancelling', cancelAttemptActive: true, statusError: undefined }));
    if (task.phase === 'starting' && !uploadId) {
      updateTask(task.id, (current) => ({
        ...current,
        cancelAttemptActive: false,
        statusError: '服务端正在创建暂存编号；收到编号后会立即发送取消请求。',
      }));
      return;
    }
    try {
      await upload?.abort(true);
      if (uploadId) {
        updateTask(task.id, (current) => ({ ...current, uploadId }));
        await checkStatus(task.id, uploadId);
      } else {
        updateTask(task.id, (current) => ({
          ...current,
          statusError: '正在等待服务端返回上传编号，以核实取消结果。',
        }));
      }
    } catch (error) {
      updateTask(task.id, (current) => ({
        ...current,
        statusError: error instanceof Error ? error.message : '取消请求失败；发布状态仍需核实。',
      }));
      if (uploadId) await checkStatus(task.id, uploadId);
    } finally {
      updateTask(task.id, (current) => ({ ...current, cancelAttemptActive: false }));
    }
  }

  function retryTransfer(task: UploadTask) {
    const upload = task.upload;
    if (!upload || isTaskCancelled(task.id)) return;
    updateTask(task.id, (current) => ({ ...current, phase: 'transferring', error: undefined }));
    upload.start();
  }

  async function retryPublication(task: UploadTask) {
    if (!task.uploadId) return;
    uploadCancellationRequests.delete(task.id);
    updateTask(task.id, (current) => ({ ...current, retrying: true, statusError: undefined }));
    try {
      const publication = await retryUploadPublication(task.uploadId);
      updateTask(task.id, (current) => ({
        ...current,
        phase: isTaskCancelled(task.id) ? 'cancelling' : publicationPhase(publication.status),
        serverStatus: publication.status,
        error: publication.error,
        statusError: undefined,
        retrying: false,
      }));
      scheduleStatusCheck(task.id, task.uploadId, 0, 900);
    } catch (error) {
      const message = error instanceof UploadApiError ? error.message : '提交发布重试失败。';
      updateTask(task.id, (current) => ({ ...current, statusError: message, retrying: false }));
    }
  }

  function refreshStatus(task: UploadTask) {
    if (!task.uploadId) return;
    updateTask(task.id, (current) => ({ ...current, statusError: undefined }));
    void checkStatus(task.id, task.uploadId);
  }

  function clearFinished() {
    setTasks((current) => {
      const finished = current.filter((task) => ['published', 'cancelled'].includes(task.phase));
      finished.forEach((task) => uploadCancellationRequests.delete(task.id));
      return current.filter((task) => !['published', 'cancelled'].includes(task.phase));
    });
  }

  if (!open) return null;

  const activeCount = tasks.filter((task) => ['preparing', 'resuming', 'starting', 'transferring', 'paused', 'checking', 'publishing', 'cancelling'].includes(task.phase)).length;

  return (
    <div className="upload-panel-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <section className="upload-panel" role="dialog" aria-modal="true" aria-labelledby="upload-title" tabIndex={-1}>
        <header className="upload-panel-header">
          <div>
            <span className="upload-panel-icon"><UploadCloud size={20} aria-hidden="true" /></span>
            <div><h2 id="upload-title">上传文件</h2><p>目标文件夹：{targetPath ? `/${targetPath}` : '全部文件'}</p></div>
          </div>
          <button ref={closeButtonRef} type="button" className="upload-panel-close" aria-label="关闭上传面板" onClick={onClose}><X size={19} aria-hidden="true" /></button>
        </header>

        <div className="upload-panel-content">
          <div
            className={`upload-drop-zone${dropActive ? ' is-active' : ''}`}
            onDragOver={(event) => { event.preventDefault(); setDropActive(true); }}
            onDragLeave={(event) => {
              if (!(event.relatedTarget instanceof Node) || !event.currentTarget.contains(event.relatedTarget)) setDropActive(false);
            }}
            onDrop={(event) => { void handleDrop(event); }}
          >
            <span><FileUp size={24} aria-hidden="true" /></span>
            <strong>选择文件或拖到这里</strong>
            <p>支持多选和文件夹；文件夹内相对路径会保留。当前目录：{targetPath ? `/${targetPath}` : '/'}</p>
            <div className="upload-picker-actions">
              <button type="button" className="upload-action-primary" onClick={() => fileInputRef.current?.click()}><FileUp size={16} aria-hidden="true" />选择文件</button>
              <button type="button" className="upload-action-secondary" onClick={() => folderInputRef.current?.click()}><FolderOpen size={16} aria-hidden="true" />选择文件夹</button>
            </div>
            <input ref={fileInputRef} className="upload-hidden-input" type="file" multiple tabIndex={-1} onChange={handleFileSelection} aria-label="选择要上传的文件" />
            <input ref={setFolderInputRef} className="upload-hidden-input" type="file" multiple tabIndex={-1} onChange={handleFileSelection} aria-label="选择要上传的文件夹" />
          </div>

          <p className="upload-scope-note">服务器确认发布后，才会显示上传成功。</p>
          {selectionError && <p className="upload-selection-error" role="alert">{selectionError}</p>}
          {recoveryState === 'loading' && <p className="upload-recovery-note" role="status">正在恢复最近的服务端上传记录…</p>}
          {recoveryState === 'error' && (
            <div className="upload-recovery-error" role="alert">
              <span>{recoveryError}</span>
              <button type="button" onClick={() => setRecoveryRetryToken((value) => value + 1)}>重新读取</button>
            </div>
          )}

          <div className="upload-queue-heading">
            <div><h3>上传队列</h3><span>{tasks.length} 个文件{activeCount ? ` · ${activeCount} 项进行中` : ''}</span></div>
            {tasks.some((task) => ['published', 'cancelled'].includes(task.phase)) && <button type="button" onClick={clearFinished}>清除已完成</button>}
          </div>

          {tasks.length === 0 ? (
            <div className="upload-empty-state"><FolderOpen size={20} aria-hidden="true" /><span>选择文件后会在这里显示真实进度与发布结果。</span></div>
          ) : (
            <ul className="upload-task-list" aria-label="上传队列">
              {tasks.map((task) => {
                const progress = task.totalBytes > 0 ? Math.min(100, (task.bytesSent / task.totalBytes) * 100) : undefined;
                const isPreparing = task.phase === 'preparing';
                const transferActive = task.phase === 'transferring';
                const publicationFailed = task.phase === 'failed';
                const publicationPartial = task.phase === 'partial';
                const canRetryPublication = publicationFailed || (publicationPartial && !task.publishedPath);
                const canCancel = Boolean(task.upload) && !['published', 'failed', 'partial', 'cancelled'].includes(task.phase)
                  && !(task.phase === 'status-error' && !task.uploadId && !extractUploadId(task.upload?.url));
                const transferDone = ['checking', 'publishing', 'published', 'failed', 'partial', 'status-error'].includes(task.phase);
                return (
                  <li className={`upload-task is-${task.phase}`} key={task.id}>
                    <div className="upload-task-topline">
                      <div className="upload-task-file"><span className="upload-task-file-icon"><FileUp size={16} aria-hidden="true" /></span><div><strong title={task.name}>{task.name}</strong><small title={task.relativePath}>{task.restored ? `服务端目标：/${task.targetPath}` : `相对路径：${task.relativePath}`}</small></div></div>
                      <span className="upload-task-size">{formatBytes(task.size)}</span>
                    </div>
                    {isPreparing && (
                      <div className="upload-progress-wrap">
                        <progress aria-label={`${task.name} 内容指纹校验进度`} max={100} value={task.hashProgress} />
                        <span>{task.hashProgress.toFixed(0)}%</span>
                      </div>
                    )}
                    {progress !== undefined && !transferDone && !isPreparing && ['transferring', 'paused', 'transfer-error', 'cancelling', 'resume-needed'].includes(task.phase) && (
                      <div className="upload-progress-wrap">
                        <progress aria-label={`${task.name} 上传进度`} max={100} value={progress} />
                        <span>{progress.toFixed(0)}%</span>
                      </div>
                    )}
                    <div className="upload-task-status-row">
                      <span className="upload-task-status" role={task.phase === 'failed' || task.phase === 'partial' || task.phase === 'transfer-error' ? 'alert' : 'status'}>
                        {task.phase === 'published' ? <CheckCircle2 size={15} aria-hidden="true" /> : (task.phase === 'failed' || task.phase === 'partial' || task.phase === 'transfer-error') ? <AlertTriangle size={15} aria-hidden="true" /> : ['preparing', 'resuming', 'starting', 'transferring', 'checking', 'publishing', 'cancelling'].includes(task.phase) ? <Loader2 className="upload-spinner" size={15} aria-hidden="true" /> : null}
                        <span>{phaseLabel(task)}</span>
                      </span>
                      {transferActive && <small>{formatBytes(task.bytesSent)} / {formatBytes(task.totalBytes)}{task.rateBytesPerSecond > 0 ? ` · ${formatBytes(task.rateBytesPerSecond)}/秒` : ''}</small>}
                    </div>
                    {task.error && <p className="upload-task-error" role="alert">{task.error}</p>}
                    {task.statusError && <p className="upload-task-error" role="alert">{task.statusError}</p>}
                    {task.publishedPath && <p className="upload-published-path">实际路径：/{task.publishedPath}</p>}
                    {!task.publishedPath && !task.restored && <p className="upload-target-note">目标文件夹：{task.targetPath ? `/${task.targetPath}` : '/'}</p>}
                    {task.phase === 'resume-needed' && <p className="upload-task-hint">重新选择同一文件或文件夹后，系统会用 tus 内容指纹核对再续传；未匹配时会作为新上传，原暂存不会自动删除。</p>}
                    {task.phase === 'unavailable' && <p className="upload-task-hint">服务端已没有可查询的暂存状态。可重新选择源文件开始新上传；原任务记录不会触发自动删除。</p>}
                    {task.phase === 'status-error' && task.restored && <p className="upload-task-hint">当前无法确认服务端状态。恢复连接后重新查询；状态确认前不会显示为成功。</p>}
                    {task.phase === 'transfer-error' && <p className="upload-task-hint">发布状态尚未确认；重试会继续当前 tus 暂存传输。</p>}
                    {publicationPartial && <p className="upload-task-hint">系统不会将部分结果显示为成功；请核对服务器返回信息与目标目录。</p>}
                    <div className="upload-task-actions">
                      {transferActive && <button type="button" onClick={() => { void pauseTask(task); }}><Pause size={14} aria-hidden="true" />暂停</button>}
                      {task.phase === 'paused' && <button type="button" onClick={() => resumeTask(task)}><Play size={14} aria-hidden="true" />继续</button>}
                      {task.phase === 'transfer-error' && <button type="button" onClick={() => retryTransfer(task)}><RotateCcw size={14} aria-hidden="true" />重试上传</button>}
                      {canCancel && <button type="button" className="is-danger" disabled={Boolean(task.cancelAttemptActive)} onClick={() => { void cancelTask(task); }}><Trash2 size={14} aria-hidden="true" />{task.phase === 'cancelling' ? task.cancelAttemptActive ? '正在确认取消' : '重试取消' : '取消上传'}</button>}
                      {canRetryPublication && <button type="button" onClick={() => { void retryPublication(task); }} disabled={task.retrying}><RotateCcw size={14} aria-hidden="true" />{task.retrying ? '提交中' : '重试发布'}</button>}
                      {task.uploadId && ['publishing', 'status-error', 'failed', 'partial', 'resume-needed', 'unavailable'].includes(task.phase) && <button type="button" onClick={() => refreshStatus(task)}><RefreshCw size={14} aria-hidden="true" />重新查询</button>}
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
        </div>

        <footer className="upload-panel-footer">
          <span><FolderOpen size={15} aria-hidden="true" />上传到 {targetPath ? `/${targetPath}` : '全部文件'}</span>
          <button type="button" onClick={onClose}>完成</button>
        </footer>
      </section>
    </div>
  );
}
