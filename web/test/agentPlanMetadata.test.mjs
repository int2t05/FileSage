import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';
import { MAX_AGENT_PLAN_OPERATIONS } from '../src/api/agentPlanLimits.ts';

const source = await readFile(new URL('../src/api/agent.ts', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('agent.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const declarations = sourceFile.statements.filter((statement) => {
  if (ts.isFunctionDeclaration(statement)) {
    return ['isRecord', 'optionalInteger', 'isSafeRootRelativePath', 'isShellCommand', 'isShellWorkingDirectory', 'parsePlannedPaths', 'parseOperation', 'parsePlan'].includes(statement.name?.text ?? '');
  }
  return ts.isClassDeclaration(statement) && statement.name?.text === 'AgentApiError';
});
const helperSource = `const MAX_AGENT_PLAN_OPERATIONS = ${MAX_AGENT_PLAN_OPERATIONS};\n${declarations.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { parsePlan };`;
const { outputText } = ts.transpileModule(helperSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const helpers = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

function plan(operation) {
  return {
    taskId: 'task-1',
    planId: 'plan-1',
    planVersion: 1,
    status: 'awaiting_approval',
    stopRequested: false,
    proposalSource: 'conversation',
    operations: [{
      id: 'operation-1',
      action: 'rename',
      sourcePath: '文档/旧名.pdf',
      targetPath: '文档/新名.pdf',
      reason: '理由',
      evidence: '',
      approval: 'pending',
      executionStatus: 'pending',
      ...operation,
    }],
  };
}

function readPlan(payload) {
  return helpers.parsePlan(payload);
}

describe('Agent 方案元数据解析', () => {
  it('兼容不带元数据字段的历史文件操作', async () => {
    const parsed = await readPlan(plan({}));
    assert.equal(parsed.operations[0].action, 'rename');
    assert.equal(parsed.operations[0].mode, undefined);
  });

  it('Shell 方案解析限长输出并保留旧记录省略状态', async () => {
    const shell = await readPlan(plan({
      action: 'run_shell', sourcePath: '', targetPath: '',
      command: 'printf done > result.txt', workingDir: '', approval: 'approved',
    }));
    assert.equal(shell.operations[0].command, 'printf done > result.txt');
    assert.equal(shell.operations[0].workingDir, '');
    const shellWithOutput = await readPlan(plan({
      action: 'run_shell', sourcePath: '', targetPath: '', command: 'printf done', workingDir: '.',
      exitCode: 0, stdout: 'done\n', stderr: '',
    }));
    assert.equal(shellWithOutput.operations[0].exitCode, 0);
    assert.equal(shellWithOutput.operations[0].stdout, 'done\n');
    assert.equal(shellWithOutput.operations[0].stderr, '');
    const shellWithTruncation = await readPlan(plan({
      action: 'run_shell', sourcePath: '', targetPath: '', command: 'printf done', workingDir: '.',
      exitCode: 0, stdout: 'done', outputTruncated: true,
    }));
    assert.equal(shellWithTruncation.operations[0].outputTruncated, true);
    const shellWithoutOutput = await readPlan(plan({
      action: 'run_shell', sourcePath: '', targetPath: '', command: 'printf done', workingDir: '.',
      exitCode: 0, outputDiscarded: true,
    }));
    assert.equal(shellWithoutOutput.operations[0].outputDiscarded, true);
    assert.throws(() => readPlan(plan({
      action: 'run_shell', sourcePath: '', targetPath: '', command: 'printf done', workingDir: '.', outputDiscarded: 'true',
    })), /无效/);
    assert.throws(() => readPlan(plan({
      action: 'run_shell', sourcePath: '', targetPath: '', command: 'printf done', workingDir: '.', stdout: 'x'.repeat(16 * 1024 + 1),
    })), /无效/);
    assert.throws(() => readPlan(plan({ stdout: '不适用于文件操作' })), /无效/);
    const multi = { ...plan({ ...shell.operations[0] }), operations: [shell.operations[0], { ...shell.operations[0], id: 'shell-2' }] };
    assert.throws(() => readPlan(multi), /Shell 方案必须只包含一项父操作/);
    for (const invalid of [
      { command: '', workingDir: '' },
      { command: 'echo ok', workingDir: '../outside' },
      { command: 'echo ok', workingDir: '', targetPath: 'unexpected' },
    ]) {
      assert.throws(() => readPlan(plan({
        action: 'run_shell', sourcePath: '', targetPath: '', approval: 'pending', ...invalid,
      })), /无效/);
    }
  });

  it('解析权限与所有者操作的完整 before/after 数值', async () => {
    const permissions = await readPlan(plan({
      action: 'set_permissions',
      sourcePath: '文档/报告.pdf',
      targetPath: '',
      beforeMode: 493,
      mode: 420,
      requestedMode: 420,
    }));
    assert.equal(permissions.operations[0].beforeMode, 493);
    assert.equal(permissions.operations[0].mode, 420);
    assert.equal(permissions.operations[0].requestedMode, 420);

    const owner = await readPlan(plan({
      action: 'set_owner',
      sourcePath: '文档/报告.pdf',
      targetPath: '',
      beforeOwnerUid: 1000,
      ownerUid: 0,
      requestedOwnerUid: 0,
      beforeGroupGid: 1000,
      groupGid: 1000,
    }));
    assert.equal(owner.operations[0].ownerUid, 0);
    assert.equal(owner.operations[0].groupGid, 1000);
    assert.equal(owner.operations[0].requestedOwnerUid, 0);
    assert.equal(owner.operations[0].requestedGroupGid, undefined);
  });

  it('拒绝越界或不完整元数据，防止用户审批不明操作', async () => {
    assert.throws(() => readPlan(plan({ action: 'set_permissions', beforeMode: 493, mode: 512 })));
    assert.throws(() => readPlan(plan({ action: 'set_owner', beforeOwnerUid: 1000, ownerUid: 0 })));
    assert.throws(() => readPlan(plan({ action: 'rename', beforeMode: 493, mode: 420 })));
  });

  it('校验软链接方案的目标对象和新链接路径都是管理根目录内的相对路径', async () => {
    const valid = await readPlan(plan({
      action: 'create_symlink',
      sourcePath: '资料/合同.pdf',
      targetPath: '快捷入口/合同.pdf',
    }));
    assert.equal(valid.operations[0].action, 'create_symlink');

    for (const paths of [
      { sourcePath: '', targetPath: '快捷入口/合同.pdf' },
      { sourcePath: '../外部/合同.pdf', targetPath: '快捷入口/合同.pdf' },
      { sourcePath: '资料/合同.pdf', targetPath: '/快捷入口/合同.pdf' },
    ]) {
      assert.throws(() => readPlan(plan({ action: 'create_symlink', ...paths })));
    }
  });

  it('解压审批方案必须提供无重复、根相对的完整输出清单', async () => {
    const valid = await readPlan(plan({
      action: 'extract_archive',
      sourcePath: '包.zip',
      targetPath: '',
      plannedPaths: ['docs', 'docs/readme.txt'],
    }));
    assert.deepEqual(valid.operations[0].plannedPaths, ['docs', 'docs/readme.txt']);
    assert.throws(() => readPlan(plan({ action: 'extract_archive', sourcePath: '包.zip', targetPath: '', plannedPaths: ['../逃逸'] })));
    assert.throws(() => readPlan(plan({ action: 'extract_archive', sourcePath: '包.zip', targetPath: '' })));
    assert.throws(() => readPlan(plan({ action: 'extract_archive', sourcePath: '包.zip', targetPath: '', plannedPaths: ['docs', 'docs'] })));
  });
});
