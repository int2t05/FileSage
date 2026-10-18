import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import * as pdfjs from 'pdfjs-dist/legacy/build/pdf.mjs';

// PDF.js 回归样本覆盖分页、可选文本与书签目录。
test('PDF 样本可解析页面文本与书签', async () => {
  const data = new Uint8Array(await readFile(new URL('../../test/fixtures/pdf-reader-sample.pdf', import.meta.url)));
  const task = pdfjs.getDocument({ data, useSystemFonts: true });

  try {
    const document = await task.promise;
    assert.equal(document.numPages, 3);

    const pageTexts = [];
    for (let pageNumber = 1; pageNumber <= document.numPages; pageNumber += 1) {
      const page = await document.getPage(pageNumber);
      const content = await page.getTextContent();
      pageTexts.push(content.items.map((item) => item.str).join(' '));
    }

    assert.match(pageTexts[0], /ORBIT/);
    assert.match(pageTexts[1], /selectable text/);
    assert.match(pageTexts[2], /Page three/);
    assert.deepEqual((await document.getOutline()).map((item) => item.title), ['Chapter 1', 'Chapter 2']);
  } finally {
    await task.destroy();
  }
});
