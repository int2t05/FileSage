// 生成 Office 转 PDF 预览请求。
export function officePreviewURL(path: string, version: string): string {
  const query = new URLSearchParams({ path, version });
  return `/api/v1/files/office/preview?${query.toString()}`;
}

// 校验版本并仅接收服务端生成的 PDF。
export async function fetchOfficePreview(
  path: string,
  version: string,
  signal: AbortSignal,
  fetchImpl: typeof fetch = fetch,
): Promise<Blob> {
  if (!version.trim()) throw new Error('服务未提供文件版本信息，无法安全生成 Office 预览。');

  const response = await fetchImpl(officePreviewURL(path, version), {
    signal,
    cache: 'no-store',
  });
  if (!response.ok) {
    let message = `生成 Office 预览失败（HTTP ${response.status}）`;
    if (response.headers.get('content-type')?.includes('application/json')) {
      const payload = await response.json().catch(() => undefined) as { error?: string } | undefined;
      if (payload?.error) message = payload.error;
    }
    throw new Error(message);
  }
  const contentType = response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase();
  if (contentType !== 'application/pdf') throw new Error('服务未返回有效的 Office PDF 预览。');
  return response.blob();
}
