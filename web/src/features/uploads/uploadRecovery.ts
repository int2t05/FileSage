export type RecoveryPhase =
  | 'resume-needed'
  | 'publishing'
  | 'published'
  | 'failed'
  | 'partial'
  | 'cancelled'
  | 'cancelling'
  | 'unavailable';

export interface PreviousUploadCandidate {
  size: number | null;
  creationTime: string;
  urlStorageKey: string;
  uploadUrl: string | null;
  metadata: Record<string, string>;
  parallelUploadUrls: string[] | null;
}

// 续传状态只由服务端状态决定，不把接收完成当作发布成功。
export function recoveryPhase(status?: string): RecoveryPhase {
  switch (status) {
    case 'receiving': return 'resume-needed';
    case 'queued':
    case 'publishing': return 'publishing';
    case 'published': return 'published';
    case 'failed': return 'failed';
    case 'partial': return 'partial';
    case 'cancelled': return 'cancelled';
    case 'cancelling': return 'cancelling';
    default: return 'unavailable';
  }
}

// 只接受 tus 本机指纹索引返回的记录，并再次核对上传端点与元数据。
export function findMatchingPreviousUpload<T extends PreviousUploadCandidate>(
  candidates: T[],
  size: number,
  metadata: Record<string, string>,
  origin: string,
): T | undefined {
  return candidates.find((candidate) => candidate.size === size
    && isSafeUploadURL(candidate.uploadUrl, origin)
    && Object.entries(metadata).every(([key, value]) => candidate.metadata[key] === value));
}

function isSafeUploadURL(value: string | null, origin: string): boolean {
  if (!value) return false;
  try {
    const url = new URL(value, origin);
    return url.origin === origin
      && url.pathname.startsWith('/api/v1/uploads/')
      && /^[a-f0-9]{32}$/.test(url.pathname.split('/').filter(Boolean).at(-1) ?? '');
  } catch {
    return false;
  }
}
