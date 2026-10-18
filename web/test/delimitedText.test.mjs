// 本文件验证 CSV/TSV 解析规则、错误提示和资源上限。
import assert from 'node:assert/strict';
import test from 'node:test';
import { parseDelimitedText } from '../src/features/reader/delimitedText.ts';

test('CSV 支持带逗号、双引号转义和 CRLF 内嵌换行的单元格', () => {
  assert.deepEqual(parseDelimitedText('姓名,备注\r\n张三,"含逗号, 和 ""引号""\r\n下一行"\r\n李四,完成', ','), [
    ['姓名', '备注'],
    ['张三', '含逗号, 和 "引号"\r\n下一行'],
    ['李四', '完成'],
  ]);
});

test('TSV 允许引号单元格包含制表符和 LF 内嵌换行', () => {
  assert.deepEqual(parseDelimitedText('名称\t说明\n文件\t"第一段\t第二段\n续行"', '\t'), [
    ['名称', '说明'],
    ['文件', '第一段\t第二段\n续行'],
  ]);
});

test('空文件、空单元格、尾随空列和空白记录保持可区分', () => {
  assert.deepEqual(parseDelimitedText('', ','), []);
  assert.deepEqual(parseDelimitedText('\r\n,保留,,\r\n', ','), [
    [''],
    ['', '保留', '', ''],
  ]);
});

test('CSV 拒绝未闭合引号、未引用字段内的引号和闭合引号后的杂字符', () => {
  for (const source of ['列\n"未完成', 'a,b\nx,中"间', '列\n"值"后']) {
    assert.throws(() => parseDelimitedText(source, ','), /CSV 第\d+行第\d+列/);
  }
});

test('公式样式文本和 HTML 标记保留为单元格文本', () => {
  assert.deepEqual(parseDelimitedText('类型,内容\n公式,"=HYPERLINK(""javascript:alert(1)"")"\n标记,"<img src=x onerror=alert(1)>"', ','), [
    ['类型', '内容'],
    ['公式', '=HYPERLINK("javascript:alert(1)")'],
    ['标记', '<img src=x onerror=alert(1)>'],
  ]);
});

test('限制超宽或超大表格展开规模', () => {
  assert.throws(() => parseDelimitedText(Array(257).fill('').join(','), ','), /超过 256 列/);
  const wideRow = Array(256).fill('').join(',');
  const manyCells = Array(196).fill(wideRow).join('\n');
  assert.throws(() => parseDelimitedText(manyCells, ','), /超过 50,000 个/);
});
