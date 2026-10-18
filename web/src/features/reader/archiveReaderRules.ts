import type { ArchiveEntry } from '../../api/archive';
import type { ReaderMediaKind } from './ReaderPanel';

const supportedArchiveSuffixes = ['.zip', '.7z', '.rar', '.tar', '.tar.gz', '.tgz'];

const previewExtensions: Record<ReaderMediaKind, readonly string[]> = {
  image: ['avif', 'bmp', 'gif', 'heic', 'heif', 'jpeg', 'jpg', 'png', 'webp'],
  audio: ['aac', 'flac', 'm4a', 'mp3', 'oga', 'ogg', 'opus', 'wav'],
  video: ['avi', 'm4v', 'mkv', 'mov', 'mp4', 'ogv', 'webm'],
  pdf: ['pdf'],
  text: ['txt', 'md', 'markdown', 'mdown', 'csv', 'tsv', 'json', 'yaml', 'yml', 'xml', 'html', 'css', 'js', 'ts', 'go', 'py', 'log'],
  xlsx: ['xlsx'],
  epub: ['epub'],
  office: ['docx'],
};

// 仅识别服务端支持浏览的压缩格式。
export function isSupportedArchiveName(name: string): boolean {
  const lowerName = name.toLowerCase();
  return supportedArchiveSuffixes.some((suffix) => lowerName.endsWith(suffix));
}

// 仅将既有安全阅读器支持的包内格式设为可预览。
export function archivePreviewKind(entry: ArchiveEntry): ReaderMediaKind | null {
  if (entry.kind !== 'file') return null;
  const extension = entry.name.split('.').at(-1)?.toLocaleLowerCase() ?? '';
  return (Object.entries(previewExtensions) as Array<[ReaderMediaKind, readonly string[]]>).find(([, values]) => values.includes(extension))?.[0] ?? null;
}

// 让压缩包目录先显示文件夹，再按名称稳定排序。
export function sortArchiveEntries(entries: ArchiveEntry[]): ArchiveEntry[] {
  return [...entries].sort((left, right) => {
    const leftDirectory = left.kind === 'directory' ? 0 : 1;
    const rightDirectory = right.kind === 'directory' ? 0 : 1;
    return leftDirectory - rightDirectory || left.name.localeCompare(right.name, 'zh-CN', { numeric: true, sensitivity: 'base' });
  });
}

export function formatArchiveSize(bytes: number): string {
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
