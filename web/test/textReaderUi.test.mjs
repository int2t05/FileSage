import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import ts from 'typescript';

const readerPanelSource = await readFile(new URL('../src/features/reader/ReaderPanel.tsx', import.meta.url), 'utf8');
const codeEditorSource = await readFile(new URL('../src/features/reader/TextCodeEditor.tsx', import.meta.url), 'utf8');
const configSource = await readFile(new URL('../src/features/reader/textEditorConfig.ts', import.meta.url), 'utf8');

function jsxElements(source, fileName, tagName) {
  const sourceFile = ts.createSourceFile(fileName, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const elements = [];
  function visit(node) {
    if (ts.isJsxSelfClosingElement(node) && node.tagName.getText(sourceFile) === tagName) elements.push(node);
    ts.forEachChild(node, visit);
  }
  visit(sourceFile);
  return elements.map((element) => ({
    attributes: element.attributes.properties,
    sourceFile,
  }));
}

function attributeText(element, name) {
  const attribute = element.attributes.find((item) => ts.isJsxAttribute(item) && item.name.text === name);
  return attribute?.initializer?.getText(element.sourceFile) ?? null;
}

function hasAttribute(element, name) {
  return element.attributes.some((item) => ts.isJsxAttribute(item) && item.name.text === name);
}

test('普通文本与 Markdown 原文走只读阅读器，编辑分支仍使用草稿编辑器', () => {
  const editors = jsxElements(readerPanelSource, 'ReaderPanel.tsx', 'TextCodeEditor');
  const reader = editors.find((element) => hasAttribute(element, 'readOnly') && attributeText(element, 'value') === '{visibleState.content}');
  const editor = editors.find((element) => !hasAttribute(element, 'readOnly') && attributeText(element, 'value') === '{draft}');

  assert.ok(reader, '阅读分支应向 CodeMirror 提供已加载正文');
  assert.ok(editor, '编辑分支应继续向 CodeMirror 提供草稿');
  assert.equal(hasAttribute(reader, 'readOnly'), true);
  assert.match(readerPanelSource, /value=\{draft\}[\s\S]*?onChange=\{\(value\) =>/);
  assert.match(codeEditorSource, /onChange\?: \(value: string\) => void/);
  assert.doesNotMatch(readerPanelSource, /reader-text-content/);
});

test('只读阅读器显示查找入口并使用行号与语法高亮扩展', () => {
  assert.match(codeEditorSource, /readOnly && \([\s\S]*?<button[\s\S]*?<Search[\s\S]*?>查找/);
  assert.match(codeEditorSource, /openSearchPanel\(view\)/);
  assert.match(codeEditorSource, /readOnly \? textReaderExtensions\(\) : textEditorExtensions\(\)/);
  assert.match(configSource, /function textReaderExtensions\([\s\S]*?textCodeViewExtensions\(\)/);
  assert.match(configSource, /function textCodeViewExtensions\([\s\S]*?lineNumbers\(\)/);
  assert.match(configSource, /syntaxHighlighting\(defaultHighlightStyle/);
});
