import assert from 'node:assert/strict';
import test from 'node:test';
import { EditorState } from '@codemirror/state';
import { defaultHighlightStyle, syntaxTree } from '@codemirror/language';
import { getSearchQuery, SearchCursor } from '@codemirror/search';
import { highlightTree } from '@lezer/highlight';
import {
  languageSupportFor,
  textEditorExtensions,
  textEditorKeymap,
  textReaderExtensions,
  textReaderKeymap,
} from '../src/features/reader/textEditorConfig.ts';

// 本测试验证白名单语法解析、编辑快捷键与文内查找行为。
function syntaxNodeNames(state) {
  const names = new Set();
  syntaxTree(state).iterate({ enter: (node) => names.add(node.name) });
  return names;
}

test('编辑器仅为白名单内常见代码扩展选择对应语法解析器', async () => {
  for (const name of [
    'app.js', 'component.tsx', 'data.json', 'page.html', 'theme.scss',
    'guide.mdown', 'script.py', 'service.go', 'config.yaml', 'main.cpp',
    'App.java', 'query.sql', 'icon.svg', 'build.sh', 'profile.ps1', 'settings.toml', 'app.ini',
  ]) {
    assert.ok(await languageSupportFor(name), `${name} 应有语法解析器`);
  }
  assert.equal(await languageSupportFor('notes.sum'), null);
  assert.equal(await languageSupportFor('readme.txt'), null);
});

test('编辑器使用真实 TypeScript 解析器并注册查找与撤销快捷键', async () => {
  const state = EditorState.create({
    doc: 'const total: number = 1;\n',
    extensions: textEditorExtensions(await languageSupportFor('sample.ts')),
  });
  const nodes = syntaxNodeNames(state);

  assert.ok(nodes.has('VariableDeclaration'));
  assert.ok(nodes.has('TypeAnnotation'));
  assert.equal(getSearchQuery(state).search, '');
  assert.ok(textEditorKeymap.some((binding) => binding.key === 'Mod-f'));
  assert.ok(textEditorKeymap.some((binding) => binding.key === 'Mod-z'));
});

test('只读阅读扩展使用真实语法高亮和查找，但不注册编辑快捷键', async () => {
  const state = EditorState.create({
    doc: 'const total: number = 1;\n',
    extensions: [
      ...textReaderExtensions(await languageSupportFor('sample.ts')),
      EditorState.readOnly.of(true),
    ],
  });
  const highlights = [];
  highlightTree(syntaxTree(state), defaultHighlightStyle, (from, to, classes) => {
    highlights.push({ text: state.doc.sliceString(from, to), classes });
  });

  assert.equal(state.readOnly, true);
  const keyword = highlights.find(({ text }) => text === 'const');
  const number = highlights.find(({ text }) => text === '1');
  assert.ok(keyword?.classes, '关键字应有高亮样式');
  assert.ok(number?.classes, '数字应有高亮样式');
  assert.notEqual(keyword.classes, number.classes, '不同语法类别应使用不同样式');
  assert.ok(textReaderKeymap.some((binding) => binding.key === 'Mod-f'));
  assert.equal(textReaderKeymap.some((binding) => binding.key === 'Mod-z'), false);
});

test('编辑与只读查找面板使用简体中文短语', () => {
  const reader = EditorState.create({
    extensions: [...textReaderExtensions(), EditorState.readOnly.of(true)],
  });
  const editor = EditorState.create({ extensions: textEditorExtensions() });
  const phrases = {
    Find: '查找',
    Replace: '替换',
    next: '下一个',
    previous: '上一个',
    all: '全部',
    'match case': '区分大小写',
    regexp: '正则表达式',
    'by word': '全字匹配',
    replace: '替换',
    'replace all': '全部替换',
    close: '关闭',
  };

  for (const [key, value] of Object.entries(phrases)) {
    assert.equal(reader.phrase(key), value);
    assert.equal(editor.phrase(key), value);
  }
  assert.equal(reader.readOnly, true);
  assert.equal(editor.readOnly, false);
});

test('查找可在编辑文档中定位文本的匹配范围', async () => {
  const state = EditorState.create({
    doc: 'first match\nsecond match',
    extensions: textEditorExtensions(await languageSupportFor('sample.txt')),
  });
  const cursor = new SearchCursor(state.doc, 'match');

  assert.equal(cursor.next().value.from, 6);
  assert.equal(cursor.value.to, 11);
  assert.equal(cursor.next().value.from, 19);
  assert.equal(cursor.value.to, 24);
  assert.equal(cursor.next().done, true);
});
