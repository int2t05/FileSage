export type ImagePreviewSize = 'thumb' | 'view';

const convertedImageExtensions = new Set(['heic', 'heif', 'dng', 'cr2', 'cr3', 'nef', 'arw']);

// 判断是否需要服务端转为浏览器可读的 JPEG。
export function requiresImageConversion(name: string): boolean {
  const extension = name.split('.').at(-1)?.toLowerCase() ?? '';
  return convertedImageExtensions.has(extension);
}

// 图片版本绑定预览地址，文件变化时不会继续使用旧缓存。
export function imagePreviewURL(path: string, version: string, size: ImagePreviewSize): string {
  return `/api/v1/files/image-preview?${new URLSearchParams({ path, version, size }).toString()}`;
}
