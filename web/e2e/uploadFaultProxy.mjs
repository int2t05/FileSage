// 为隔离上传 E2E 在单个分块中途制造一次 TCP 断开。
import { createServer, request as httpRequest } from 'node:http';
import { once } from 'node:events';

const port = Number(process.env.FILESAGE_E2E_PORT || 4179);
const cutAfterBytes = 8 * 1024 * 1024;
const upstreamURL = new URL(process.env.FILESAGE_E2E_PROXY_UPSTREAM || '');

if (upstreamURL.protocol !== 'http:' || !['127.0.0.1', '::1'].includes(upstreamURL.hostname)) {
  throw new Error('上传故障代理只允许连接本机 HTTP 隧道');
}
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  throw new Error('FILESAGE_E2E_PORT 必须是有效端口');
}

const state = { interrupted: false, bytesForwarded: 0, patches: [], headOffsets: [] };

export default async function globalSetup() {
  const server = createServer((incoming, outgoing) => {
    if (incoming.method === 'GET' && incoming.url === '/__filesage_upload_proxy_state') {
      outgoing.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
      outgoing.end(JSON.stringify(state));
      return;
    }

    const target = new URL(incoming.url || '/', upstreamURL);
    const headers = { ...incoming.headers };
    // 保留浏览器页面的主机头，使写请求仍通过同源校验。
    const upstream = httpRequest(target, { method: incoming.method, headers }, (response) => {
      if (incoming.method === 'HEAD' && response.headers['upload-offset']) {
        state.headOffsets.push(response.headers['upload-offset']);
      }
      outgoing.writeHead(response.statusCode || 502, response.headers);
      response.pipe(outgoing);
    });

    const offset = incoming.headers['upload-offset'] || '';
    const bodyLength = Number(incoming.headers['content-length'] || 0);
    if (incoming.method === 'PATCH') state.patches.push({ offset, bodyLength });
    const shouldInterrupt = incoming.method === 'PATCH' && offset === '0'
      && !state.interrupted && bodyLength > cutAfterBytes;
    if (!shouldInterrupt) {
      incoming.pipe(upstream);
    } else {
      let forwarded = 0;
      let dropScheduled = false;
      incoming.on('data', (chunk) => {
        if (dropScheduled) return;
        const part = chunk.subarray(0, Math.max(0, cutAfterBytes - forwarded));
        if (part.length) {
          forwarded += part.length;
          if (forwarded >= cutAfterBytes) {
            dropScheduled = true;
            state.interrupted = true;
            state.bytesForwarded = forwarded;
            upstream.write(part, () => {
              setTimeout(() => {
                upstream.destroy();
                outgoing.destroy();
                incoming.destroy();
              }, 250);
            });
          } else if (!upstream.write(part)) {
            incoming.pause();
            upstream.once('drain', () => incoming.resume());
          }
        }
      });
      incoming.on('end', () => {
        if (!dropScheduled) upstream.end();
      });
    }

    incoming.on('error', () => upstream.destroy());
    upstream.on('error', () => {
      if (!outgoing.destroyed && !state.interrupted) outgoing.writeHead(502).end();
    });
  });

  server.listen(port, '127.0.0.1');
  await once(server, 'listening');
  console.log(`FileSage upload fault proxy listening on 127.0.0.1:${port}`);

  return async () => {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  };
}
