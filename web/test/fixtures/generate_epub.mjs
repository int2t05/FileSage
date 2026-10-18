// 生成无版权依赖、包含两章与安全测试文本的最小 EPUB 样本。
import { writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { strToU8, zipSync } from 'fflate';

const date = new Date('2025-01-01T00:00:00.000Z');
const text = (value) => [strToU8(value), { mtime: date }];

const files = {
  mimetype: [strToU8('application/epub+zip'), { level: 0, mtime: date }],
  'META-INF/container.xml': text('<?xml version="1.0" encoding="UTF-8"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>'),
  'OEBPS/content.opf': text('<?xml version="1.0" encoding="UTF-8"?><package version="3.0" xmlns="http://www.idpf.org/2007/opf" xmlns:dc="http://purl.org/dc/elements/1.1/"><metadata><dc:title>FileSage 阅读样本</dc:title></metadata><manifest><item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/><item id="chapter-one" href="chapter-one.xhtml" media-type="application/xhtml+xml"/><item id="chapter-two" href="chapter-two.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="chapter-one"/><itemref idref="chapter-two"/></spine></package>'),
  'OEBPS/nav.xhtml': text('<?xml version="1.0" encoding="UTF-8"?><html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><body><nav epub:type="toc"><ol><li><a href="chapter-one.xhtml">第一章：开始</a></li><li><a href="chapter-two.xhtml">第二章：继续</a></li><li><a href="https://example.invalid/remote.xhtml">外部目录项</a></li></ol></nav></body></html>'),
  'OEBPS/chapter-one.xhtml': text('<?xml version="1.0" encoding="UTF-8"?><html xmlns="http://www.w3.org/1999/xhtml"><head><title>第一章</title><style>body{display:none}</style></head><body><h1>第一章</h1><p>你好 <em>世界</em>。</p><script>globalThis.__EPUB_SCRIPT_RAN__ = true</script><img src="https://example.invalid/track.png"/><p><a href="https://example.invalid/">外部链接仍是普通文字</a></p></body></html>'),
  'OEBPS/chapter-two.xhtml': text('<?xml version="1.0" encoding="UTF-8"?><html xmlns="http://www.w3.org/1999/xhtml"><body><h2>第二章正文标题</h2><p>第二章可以独立导航和恢复阅读位置。</p></body></html>'),
};

const archive = zipSync(files, { level: 6, mtime: date });
await writeFile(fileURLToPath(new URL('./mini-book.epub', import.meta.url)), archive);
