import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/files/fileMetadataRules.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const rules = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('文件权限与属主输入规则', () => {
  it('解析八进制权限并限制在 0000 至 0777', () => {
    assert.equal(rules.parseOctalMode('0000'), 0);
    assert.equal(rules.parseOctalMode('0644'), 0o644);
    assert.equal(rules.parseOctalMode('777'), 0o777);
    for (const value of ['1000', '0888', '8', '-644', '06 4', '']) {
      assert.equal(rules.parseOctalMode(value), null, value);
    }
  });

  it('允许只修改 UID 或 GID，要求非负安全整数且至少一项', () => {
    const uidOnly = rules.parseOwnerInputs('0', '');
    const gidOnly = rules.parseOwnerInputs('', '12');
    assert.deepEqual(uidOnly, { ok: true, ownerUid: 0 });
    assert.deepEqual(gidOnly, { ok: true, groupGid: 12 });
    assert.deepEqual(rules.ownerChangeFields(uidOnly), { ownerUid: 0 });
    assert.deepEqual(rules.ownerChangeFields(gidOnly), { groupGid: 12 });
    assert.deepEqual(rules.ownerChangeFields(rules.parseOwnerInputs('1000', '100')), { ownerUid: 1000, groupGid: 100 });
    assert.equal(rules.parseOwnerInputs('', '').ok, false);
    for (const value of ['-1', '+1', '1.5', '1e3', '9007199254740992']) {
      assert.equal(rules.parseOwnerInputs(value, '').ok, false, value);
    }
  });

  it('只允许有版本的普通文件和目录，不开放软链接或特殊文件', () => {
    assert.equal(rules.canEditFileMetadata({ kind: 'file', symlink: false, version: 'v1' }), true);
    assert.equal(rules.canEditFileMetadata({ kind: 'directory', symlink: false, version: 'v1' }), true);
    assert.equal(rules.canEditFileMetadata({ kind: 'file', symlink: true, version: 'v1' }), false);
    assert.equal(rules.canEditFileMetadata({ kind: 'other', symlink: false, version: 'v1' }), false);
    assert.equal(rules.canEditFileMetadata({ kind: 'file', symlink: false, version: '' }), false);
  });

  it('用四位八进制格式展示权限', () => {
    assert.equal(rules.formatPermissionMode(0), '0000');
    assert.equal(rules.formatPermissionMode(0o644), '0644');
    assert.equal(rules.formatPermissionMode(0o777), '0777');
  });

  it('属主信息不可用时隐藏后端占位 UID/GID', () => {
    assert.equal(rules.formatOwnerId(0, false), '—');
    assert.equal(rules.formatOwnerId(0, true), '0');
    assert.equal(rules.formatOwnerId(undefined, true), '—');
  });
});
