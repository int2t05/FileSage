import assert from 'node:assert/strict';
import test from 'node:test';
import { fetchOfficePreview, officePreviewURL } from '../src/features/reader/officePreview.ts';

test('Office 预览请求编码相对路径与当前版本', () => {
  const url = officePreviewURL('文档/季度 & 预算.docx', 'etag/一');
  const query = new URLSearchParams(url.split('?')[1]);
  assert.equal(url.split('?')[0], '/api/v1/files/office/preview');
  assert.equal(query.get('path'), '文档/季度 & 预算.docx');
  assert.equal(query.get('version'), 'etag/一');
});

test('只接受 PDF 响应并传递取消信号', async () => {
  const controller = new AbortController();
  let request;
  const expectedBlob = new Blob(['pdf'], { type: 'application/pdf' });
  const blob = await fetchOfficePreview('报告.docx', 'v1', controller.signal, async (url, options) => {
    request = { url, options };
    return new Response(expectedBlob, { headers: { 'Content-Type': 'application/pdf; charset=binary' } });
  });

  assert.equal(await blob.text(), 'pdf');
  assert.equal(request.options.signal, controller.signal);
  assert.equal(request.options.cache, 'no-store');
});

test('返回服务端 JSON 错误且拒绝非 PDF 成功响应', async () => {
  const signal = new AbortController().signal;
  await assert.rejects(
    fetchOfficePreview('报告.docx', 'v1', signal, async () => new Response(
      JSON.stringify({ error: 'Office 转换服务不可用' }),
      { status: 503, headers: { 'Content-Type': 'application/json' } },
    )),
    /Office 转换服务不可用/,
  );
  await assert.rejects(
    fetchOfficePreview('报告.docx', 'v1', signal, async () => new Response('not a pdf', {
      headers: { 'Content-Type': 'text/plain' },
    })),
    /未返回有效的 Office PDF/,
  );
});

test('文件版本缺失时不发出预览请求', async () => {
  let called = false;
  await assert.rejects(
    fetchOfficePreview('报告.docx', '', new AbortController().signal, async () => {
      called = true;
      return new Response();
    }),
    /未提供文件版本信息/,
  );
  assert.equal(called, false);
});
