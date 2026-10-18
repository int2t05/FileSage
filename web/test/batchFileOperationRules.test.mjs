import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const directorySource = await readFile(new URL('../src/features/files/directoryOperationRules.ts', import.meta.url), 'utf8');
const directoryOutput = ts.transpileModule(directorySource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const directoryModule = `data:text/javascript;base64,${Buffer.from(directoryOutput).toString('base64')}`;
const rulesSource = (await readFile(new URL('../src/features/files/batchFileOperationRules.ts', import.meta.url), 'utf8'))
  .replace("from './directoryOperationRules'", `from '${directoryModule}'`);
const rulesOutput = ts.transpileModule(rulesSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const rules = await import(`data:text/javascript;base64,${Buffer.from(rulesOutput).toString('base64')}`);
const {
  batchOperationTargetError,
  batchSelectionInclusions,
  incompleteBatchOperationPaths,
} = rules;

test('父目录和子项同时选择时以最外层已选目录作为提示归属', () => {
  const items = [
    { path: '项目', kind: 'directory' },
    { path: '项目/子目录', kind: 'directory' },
    { path: '项目/子目录/a.txt', kind: 'file' },
    { path: '项目外/b.txt', kind: 'file' },
  ];
  assert.deepEqual(batchSelectionInclusions(items), [
    { sourcePath: '项目/子目录', includedIn: '项目' },
    { sourcePath: '项目/子目录/a.txt', includedIn: '项目' },
  ]);
});

test('阻止移动到当前父目录、同目录或所选目录内部', () => {
  assert.match(batchOperationTargetError('move', [{ path: '目录/a.txt', kind: 'file' }], '目录'), /当前所在目录相同/);
  assert.match(batchOperationTargetError('move', [{ path: '目录/子目录', kind: 'directory' }], '目录'), /当前的父目录/);
  assert.match(batchOperationTargetError('copy', [{ path: '目录', kind: 'directory' }], '目录/新目录'), /本身或其子目录/);
  assert.equal(batchOperationTargetError('move', [{ path: '目录/a.txt', kind: 'file' }], '目标'), '');
});

test('仅保留失败、部分完成和未开始项目供重试', () => {
  const results = ['copied', 'moved', 'included', 'partial', 'failed', 'not_started'].map((status, index) => ({
    sourcePath: `项目/${index}.txt`,
    targetPath: '',
    status,
  }));
  assert.deepEqual([...incompleteBatchOperationPaths(results)], ['项目/3.txt', '项目/4.txt', '项目/5.txt']);
});
