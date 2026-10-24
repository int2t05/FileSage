import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/files/fileViewPreferences.ts', import.meta.url), 'utf8');
const compiled = ts.transpileModule(source.replace("import type { FileSortMode } from '../../api/files';", "type FileSortMode = 'name-asc' | 'name-desc' | 'size-asc' | 'size-desc' | 'modified-asc' | 'modified-desc';"), {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const preferences = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`);

function memoryStorage(value = null) {
  let stored = value;
  return {
    getItem() { return stored; },
    setItem(_key, next) { stored = next; },
    value() { return stored; },
  };
}

describe('文件页显示偏好', () => {
  it('保存并恢复布局、排序和隐藏文件显示', () => {
    const storage = memoryStorage();
    const value = { viewMode: 'grid', sortMode: 'modified-desc', showHidden: true };
    assert.equal(preferences.saveFileViewPreferences(value, storage), true);
    assert.deepEqual(preferences.readFileViewPreferences(storage), value);
  });

  it('忽略损坏或不支持的存储值并采用默认值', () => {
    const storage = memoryStorage('{bad json');
    assert.deepEqual(preferences.readFileViewPreferences(storage), preferences.defaultFileViewPreferences);
    storage.setItem('', JSON.stringify({ viewMode: 'other', sortMode: 'capture-desc', showHidden: 'yes' }));
    assert.deepEqual(preferences.readFileViewPreferences(storage), preferences.defaultFileViewPreferences);
  });

  it('存储不可用时不阻断文件页', () => {
    const storage = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); } };
    assert.deepEqual(preferences.readFileViewPreferences(storage), preferences.defaultFileViewPreferences);
    assert.equal(preferences.saveFileViewPreferences(preferences.defaultFileViewPreferences, storage), false);
  });
});
