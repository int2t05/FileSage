import { sha256 } from '@noble/hashes/sha2.js';

const uploadEndpoint = '/api/v1/uploads/';
const hashChunkBytes = 1024 * 1024;
const maxParallelHashes = 2;

let activeHashJobs = 0;
const waitingHashJobs: Array<() => void> = [];

async function acquireHashSlot(): Promise<() => void> {
  if (activeHashJobs < maxParallelHashes) activeHashJobs += 1;
  else await new Promise<void>((resolve) => waitingHashJobs.push(resolve));
  return () => {
    const next = waitingHashJobs.shift();
    if (next) next();
    else activeHashJobs -= 1;
  };
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
}

// 指纹覆盖文件正文和目标位置，避免把不同内容接到同一暂存上传。
export async function createUploadFingerprint(
  file: Pick<File, 'name' | 'size' | 'type' | 'slice'>,
  target: string,
  relativePath: string,
  onProgress: (bytesRead: number) => void,
  isCancelled: () => boolean,
): Promise<string> {
  const release = await acquireHashSlot();
  try {
    const hash = sha256.create();
    for (let offset = 0; offset < file.size; offset += hashChunkBytes) {
      if (isCancelled()) throw new DOMException('Upload cancelled', 'AbortError');
      const chunk = await file.slice(offset, Math.min(offset + hashChunkBytes, file.size)).arrayBuffer();
      if (isCancelled()) throw new DOMException('Upload cancelled', 'AbortError');
      hash.update(new Uint8Array(chunk));
      onProgress(Math.min(offset + chunk.byteLength, file.size));
    }

    const contentHash = hash.digest();
    const scope = JSON.stringify([uploadEndpoint, file.name, target, relativePath, file.type || 'application/octet-stream', file.size]);
    const fingerprint = sha256.create().update(contentHash).update(new TextEncoder().encode(scope)).digest();
    return `filesage-v1:${toHex(fingerprint)}`;
  } finally {
    release();
  }
}
