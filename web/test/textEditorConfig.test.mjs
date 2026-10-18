import assert from 'node:assert/strict';
import test from 'node:test';
import { EditorState } from '@codemirror/state';
import { syntaxTree } from '@codemirror/language';
import { getSearchQuery, SearchCursor } from '@codemirror/search';
import {
  languageSupportFor,
  textEditorExtensions,
  textEditorKeymap,
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
