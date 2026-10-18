import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/agent/AgentPlanReview.tsx', import.meta.url), 'utf8');
const shellReviewSource = await readFile(new URL('../src/features/agent/ShellChangeReview.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('AgentPlanReview.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const declarations = sourceFile.statements.filter((statement) => {
  if (ts.isVariableStatement(statement)) {
    return statement.declarationList.declarations.some((declaration) => ['APPROVABLE_ACTIONS', 'EDIT_ACTION_LABELS', 'EXECUTION_LABELS'].includes(declaration.name.getText(sourceFile)));
  }
  return ts.isFunctionDeclaration(statement)
    && ['statusLabel', 'isApprovableOperation', 'canExecute', 'approvalCounts', 'executionProgress', 'canStopExecution', 'executionLabel', 'shouldShowSafetyNotice', 'operationApprovalLabel', 'requiresTextCandidatePreview', 'canRejectPlan', 'canDecideOperation', 'operationPresentation', 'operationPresentationForOperation', 'canEditPlan', 'createPlanEditDrafts', 'cancelPlanEdit'].includes(statement.name?.text ?? '');
});
const helperSource = `${declarations.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { isApprovableOperation, canExecute, approvalCounts, executionProgress, canStopExecution, executionLabel, shouldShowSafetyNotice, operationApprovalLabel, requiresTextCandidatePreview, canRejectPlan, canDecideOperation, operationPresentation, operationPresentationForOperation, canEditPlan, createPlanEditDrafts, cancelPlanEdit };`;
const { outputText } = ts.transpileModule(helperSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const helpers = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('Agent 方案审核', () => {
  it('新建和覆盖文本候选须先预览当前版本后才能审批', () => {
    assert.equal(helpers.requiresTextCandidatePreview('create_file'), true);
    assert.equal(helpers.requiresTextCandidatePreview('overwrite_file'), true);
    assert.equal(helpers.requiresTextCandidatePreview('edit_text'), false);
    assert.match(source, /previewedTextCandidateKey === candidatePreviewKey/);
    assert.match(source, /请先打开并核对文本候选，之后才能批准/);
    assert.match(source, /mustPreviewCandidate && !candidateWasPreviewed/);
    assert.match(source, /onPreviewReady=\{markTextCandidatePreviewed\}/);
    assert.match(source, /\$\{plan\.planId\}:\$\{plan\.planVersion\}:\$\{operation\.id\}/);
  });

  it('候选预览显示新建无原文、覆盖完整原文，并允许编辑候选', () => {
    assert.match(source, /operation\.action === 'create_file' \? '原文（目标不存在）'/);
    assert.match(source, /目标文件尚不存在，不读取原文/);
    assert.match(source, /operation\.action === 'overwrite_file' \? '将被替换的原文'/);
    assert.match(source, /<pre>\{operation\.action === 'create_file' \? '[^']*' : preview\.sourceContent\}<\/pre>/);
    assert.match(source, /<textarea[^>]*value=\{draft\}/);
    assert.match(source, /onSave\(operation\.id, draft\)/);
    assert.match(source, /确认分析时已读取完整原文并发送给当前模型/);
    assert.match(source, /执行后原内容不可恢复/);
  });

  it('执行期间展示逐项进度，并仅在尚未请求停止时允许停止后续项', () => {
    const operations = [
      { executionStatus: 'succeeded' },
      { executionStatus: 'running' },
      { executionStatus: 'pending' },
    ];
    const executing = { status: 'executing', stopRequested: false, operations };
    assert.deepEqual(helpers.executionProgress(executing), {
      completed: 1, total: 3, remaining: 1, failed: 0, runningOrdinal: 2,
    });
    assert.equal(helpers.canStopExecution(executing), true);
    assert.equal(helpers.canStopExecution({ ...executing, stopRequested: true }), false);
    assert.equal(helpers.executionLabel('cancelled', 'cancelled'), '已停止 · 未执行');
    assert.equal(helpers.executionLabel('failed', 'cancelled'), '未执行');
  });

  it('执行状态通过可访问的实时进度区呈现停止语义', () => {
    assert.match(source, /role="status" aria-live="polite"/);
    assert.match(source, /停止后续操作/);
    assert.match(source, /当前操作安全结束后将停止后续项/);
  });

  it('用户停止会撤销未执行审批，但不误报文件状态变化', () => {
    const stoppedOperation = { approval: 'invalidated', executionStatus: 'cancelled' };
    const stoppedPlan = { status: 'cancelled', operations: [stoppedOperation] };
    assert.equal(helpers.operationApprovalLabel(stoppedPlan.status, stoppedOperation), '已撤销');
    assert.equal(helpers.shouldShowSafetyNotice(stoppedPlan), false);
    assert.equal(helpers.shouldShowSafetyNotice({ status: 'invalidated', operations: [] }), true);
    assert.equal(helpers.shouldShowSafetyNotice({ status: 'executing', operations: [{ executionStatus: 'partial' }] }), true);
  });

  it('新建文件夹可逐项审批，并按父目录与完整目标路径展示', () => {
    const operation = { action: 'create_directory', approval: 'pending', executionStatus: 'pending' };
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.deepEqual(helpers.operationPresentation('create_directory', '', '归档/合同'), {
      title: '新建文件夹',
      sourceLabel: '所在目录',
      sourceDisplayPath: '根目录',
      targetLabel: '新文件夹',
      targetDisplayPath: '归档/合同',
    });
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...operation, approval: 'approved' }],
    }), true);
  });

  it('Shell 父操作只显示沙箱命令，变更由独立清单逐项批准', () => {
    const operation = {
      id: 'shell-op', action: 'run_shell', sourcePath: '', targetPath: '',
      command: 'printf "hello" > result.txt', workingDir: '',
      approval: 'approved', executionStatus: 'pending',
    };
    assert.equal(helpers.canDecideOperation('awaiting_approval', { ...operation, approval: 'pending' }), false);
    assert.equal(helpers.canExecute({ status: 'approved', operations: [operation] }), true);
    assert.deepEqual(helpers.operationPresentationForOperation(operation), {
      title: '隔离运行 Shell', sourceLabel: '管理范围', sourceDisplayPath: '管理根目录',
      targetLabel: '工作目录', targetDisplayPath: '管理根目录', shellCommand: operation.command,
    });
    assert.match(shellReviewSource, /文件变化/);
    assert.match(source, /onShellChangeDecision/);
    assert.match(source, /拒绝整份方案/);
  });

  it('仅显式的永久删除动作可逐项审批并要求二次执行确认', () => {
    const permanentDelete = { action: 'permanent_delete', approval: 'pending', executionStatus: 'pending' };
    assert.equal(helpers.canDecideOperation('awaiting_approval', permanentDelete), true);
    assert.deepEqual(helpers.operationPresentationForOperation({
      ...permanentDelete, sourcePath: '文档/过期.txt', targetPath: '',
    }), {
      title: '永久删除（不可恢复）',
      sourceLabel: '待永久删除项目',
      sourceDisplayPath: '文档/过期.txt',
      targetLabel: '删除后',
      targetDisplayPath: '无法恢复',
    });
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...permanentDelete, approval: 'approved' }],
    }), true);
    assert.match(source, /永久删除后无法恢复/);
  });

  it('解压方案展示审批路径清单，并在服务端清单缺失时禁止审批', () => {
    const operation = {
      action: 'extract_archive', approval: 'pending', executionStatus: 'pending',
      sourcePath: '备份.zip', targetPath: '解压目录', plannedPaths: ['解压目录/a.txt', '解压目录/子目录'],
    };
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.deepEqual(helpers.operationPresentationForOperation(operation), {
      title: '解压压缩包', sourceLabel: '压缩包', sourceDisplayPath: '备份.zip',
      targetLabel: '解压到', targetDisplayPath: '解压目录',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', { ...operation, plannedPaths: undefined }), false);
    assert.match(source, /解压清单（\{paths.length\} 项）/);
    assert.match(source, /paths\.slice\(0, visible\)/);
  });

  it('未知删除动作不可审批或执行', () => {
    for (const action of ['delete', 'future_action']) {
      const operation = { action, approval: 'pending', executionStatus: 'pending' };
      assert.equal(helpers.canDecideOperation('awaiting_approval', operation), false, action);
      assert.equal(helpers.canExecute({
        status: 'approved',
        operations: [{ ...operation, approval: 'approved' }],
      }), false, action);
    }
  });

  it('普通删除逐项审批并明确显示为可恢复回收', () => {
    const operation = { action: 'trash', approval: 'pending', executionStatus: 'pending' };
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.deepEqual(helpers.operationPresentation('trash', '文档/旧稿.md', ''), {
      title: '移入回收站',
      sourceLabel: '待回收项目',
      sourceDisplayPath: '文档/旧稿.md',
      targetLabel: '处理方式',
      targetDisplayPath: '移入可恢复回收站',
    });
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...operation, approval: 'approved' }],
    }), true);
  });

  it('移动文件显示源与目标路径，并支持逐项审批和执行确认', () => {
    const operation = { action: 'move_file', approval: 'pending', executionStatus: 'pending' };
    assert.deepEqual(helpers.operationPresentation('move_file', '待整理/报告.pdf', '归档/报告.pdf'), {
      title: '移动文件',
      sourceLabel: '从',
      sourceDisplayPath: '待整理/报告.pdf',
      targetLabel: '移动到',
      targetDisplayPath: '归档/报告.pdf',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...operation, approval: 'approved' }],
    }), true);
  });

  it('照片归档逐项显示拍摄日期操作，可审批执行且不可编辑方案', () => {
    const operation = {
      action: 'archive_photo', sourcePath: '相机/IMG_001.jpg', targetPath: '照片/2025/2025-03-04/IMG_001.jpg',
      reason: '按照片拍摄日期归档', evidence: 'EXIF 拍摄日期：2025-03-04',
      approval: 'pending', executionStatus: 'pending',
    };
    assert.deepEqual(helpers.operationPresentationForOperation(operation), {
      title: '按拍摄日期归档照片',
      sourceLabel: '原路径',
      sourceDisplayPath: '相机/IMG_001.jpg',
      targetLabel: '归档到',
      targetDisplayPath: '照片/2025/2025-03-04/IMG_001.jpg',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canExecute({
      status: 'approved', operations: [{ ...operation, approval: 'approved' }],
    }), true);
    assert.equal(helpers.canEditPlan({
      proposalSource: 'conversation', status: 'awaiting_approval', operations: [operation],
    }, true), false);
    assert.match(source, /operation\.reason &&/);
    assert.match(source, /operation\.evidence &&/);
    assert.match(source, /执行照片归档/);
  });

  it('文本修改显示目标文件，可逐项审批和执行，但不进入普通方案编辑器', () => {
    const operation = {
      action: 'edit_text', approval: 'pending', executionStatus: 'pending',
      sourcePath: '文档/说明.md', targetPath: '文档/说明.md',
    };
    assert.deepEqual(helpers.operationPresentationForOperation(operation), {
      title: '修改文件正文', textFilePath: '文档/说明.md',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canExecute({ status: 'approved', operations: [{ ...operation, approval: 'approved' }] }), true);
    assert.equal(helpers.canEditPlan({
      proposalSource: 'conversation', status: 'awaiting_approval', operations: [operation],
    }, true), false);
  });

  it('新建展示父目录与新文件路径，覆盖突出将被替换的完整正文', () => {
    const create = { action: 'create_file', sourcePath: '文档', targetPath: '文档/新建.md', approval: 'pending', executionStatus: 'pending' };
    assert.equal(helpers.canDecideOperation('awaiting_approval', create), true);
    assert.deepEqual(helpers.operationPresentationForOperation(create), {
      title: '新建文本文件',
      sourceLabel: '所在目录', sourceDisplayPath: '文档',
      targetLabel: '新建文件', targetDisplayPath: '文档/新建.md',
    });

    const overwrite = { action: 'overwrite_file', sourcePath: '文档/说明.md', targetPath: '文档/说明.md', approval: 'pending', executionStatus: 'pending' };
    assert.equal(helpers.canDecideOperation('awaiting_approval', overwrite), true);
    assert.deepEqual(helpers.operationPresentationForOperation(overwrite), {
      title: '覆盖文本文件', textFilePath: '文档/说明.md', textFileLabel: '将被替换的文件正文（全文）',
    });
  });

  it('复制文件显示来源与目标路径，并允许逐项审批', () => {
    const operation = { action: 'copy_file', approval: 'pending', executionStatus: 'pending' };
    assert.deepEqual(helpers.operationPresentationForOperation({
      ...operation,
      sourcePath: '待整理/报告.pdf',
      targetPath: '归档/报告.pdf',
    }), {
      title: '复制文件',
      sourceLabel: '复制自',
      sourceDisplayPath: '待整理/报告.pdf',
      targetLabel: '复制到',
      targetDisplayPath: '归档/报告.pdf',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...operation, approval: 'approved' }],
    }), true);
  });

  it('复制文件夹显示来源与目标路径，并允许逐项审批', () => {
    const operation = { action: 'copy_directory', approval: 'pending', executionStatus: 'pending' };
    assert.deepEqual(helpers.operationPresentationForOperation({
      ...operation,
      sourcePath: '待整理/项目',
      targetPath: '归档/项目',
    }), {
      title: '复制文件夹',
      sourceLabel: '复制自',
      sourceDisplayPath: '待整理/项目',
      targetLabel: '复制到',
      targetDisplayPath: '归档/项目',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...operation, approval: 'approved' }],
    }), true);
  });

  it('软链接只允许逐项审批，显示指向对象与新链接路径，再显式执行', () => {
    const operation = {
      id: 'symlink-1',
      action: 'create_symlink',
      sourcePath: '资料/合同.pdf',
      targetPath: '快捷入口/合同 (1).pdf',
      approval: 'pending',
      executionStatus: 'pending',
    };
    assert.deepEqual(helpers.operationPresentationForOperation(operation), {
      title: '创建软链接',
      sourceLabel: '指向',
      sourceDisplayPath: '资料/合同.pdf',
      targetLabel: '新链接路径',
      targetDisplayPath: '快捷入口/合同 (1).pdf',
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canExecute({
      status: 'approved', operations: [{ ...operation, approval: 'approved' }],
    }), true);
    assert.equal(helpers.canExecute({
      status: 'approved', operations: [operation],
    }), false);
    assert.equal(helpers.canEditPlan({
      proposalSource: 'conversation', status: 'awaiting_approval', operations: [operation],
    }, true), false);
  });

  it('重复文件清理显示待回收副本与明确保留项', () => {
    const operation = {
      action: 'trash_duplicate', approval: 'pending', executionStatus: 'pending',
      sourcePath: 'docs/copy.pdf', targetPath: 'docs/keep.pdf',
      reason: '已确认只回收勾选副本。',
      evidence: '内容复核一致。',
    };
    const presentation = helpers.operationPresentationForOperation(operation);
    assert.equal(presentation.title, '移入回收站');
    assert.equal(presentation.sourceDisplayPath, 'docs/copy.pdf');
    assert.equal(presentation.targetLabel, '保留文件');
    assert.equal(presentation.targetDisplayPath, 'docs/keep.pdf');
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
  });

  it('权限审核显示四位八进制 before → after，并要求完整数值后才可审批', () => {
    const operation = {
      action: 'set_permissions', beforeMode: 493, mode: 420,
      approval: 'pending', executionStatus: 'pending',
    };
    assert.deepEqual(helpers.operationPresentationForOperation({ ...operation, sourcePath: '文档/报告.pdf', targetPath: '' }), {
      title: '修改权限',
      metadataPath: '文档/报告.pdf',
      metadataChanges: [{ label: '权限', before: '0755', after: '0644', unchanged: false }],
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canDecideOperation('awaiting_approval', { ...operation, mode: undefined }), false);
    assert.equal(helpers.canExecute({
      status: 'approved',
      operations: [{ ...operation, approval: 'approved' }],
    }), true);
  });

  it('所有者审核显示变更值，并明确保留未变化的 UID/GID', () => {
    const operation = {
      action: 'set_owner',
      beforeOwnerUid: 1000, ownerUid: 0,
      beforeGroupGid: 1000, groupGid: 1000,
      approval: 'pending', executionStatus: 'pending',
    };
    assert.deepEqual(helpers.operationPresentationForOperation({ ...operation, sourcePath: '文档/报告.pdf', targetPath: '' }), {
      title: '修改所有者',
      metadataPath: '文档/报告.pdf',
      metadataChanges: [
        { label: '用户 ID', before: '1000', after: '0', unchanged: false },
        { label: '用户组 ID', before: '1000', after: '1000', unchanged: true },
      ],
    });
    assert.equal(helpers.canDecideOperation('awaiting_approval', operation), true);
    assert.equal(helpers.canDecideOperation('awaiting_approval', { ...operation, groupGid: undefined }), false);
  });

  it('多项方案逐项计数，所有项目批准前不能执行，拒绝会取消整版', () => {
    const plan = {
      status: 'awaiting_approval',
      operations: [
        { action: 'rename', approval: 'approved', executionStatus: 'pending' },
        { action: 'move_file', approval: 'pending', executionStatus: 'pending' },
      ],
    };
    assert.deepEqual(helpers.approvalCounts(plan), { pending: 1, approved: 1, rejected: 0, total: 2 });
    assert.equal(helpers.canExecute({ status: 'awaiting_approval', operations: plan.operations }), false);
    assert.equal(helpers.canRejectPlan(plan), true);
    assert.equal(helpers.canRejectPlan({ status: 'approved', operations: plan.operations }), false);
  });

  it('仅未审批的对话方案可编辑，保存后重新进入待审状态', () => {
    const plan = {
      taskId: 'task-1', planId: 'plan-1', planVersion: 1, proposalSource: 'conversation', status: 'awaiting_approval',
      operations: [{
        id: 'old-op', action: 'move_file', sourcePath: 'a.pdf', targetPath: 'b.pdf', reason: '原理由', evidence: '原依据',
        approval: 'pending', executionStatus: 'pending',
      }],
    };
    assert.equal(helpers.canEditPlan(plan, true), true);
    assert.equal(helpers.canEditPlan(plan, false), false);
    assert.equal(helpers.canEditPlan({ ...plan, proposalSource: 'caller' }, true), false);
    assert.equal(helpers.canEditPlan({ ...plan, status: 'approved' }, true), false);
    assert.equal(helpers.canEditPlan({ ...plan, operations: [{ ...plan.operations[0], approval: 'approved' }] }, true), false);

    const draft = helpers.createPlanEditDrafts(plan);
    draft[0].targetPath = '归档/新名.pdf';
    draft[0].reason = '新理由';
    const cancelled = helpers.cancelPlanEdit(plan);
    assert.equal(cancelled[0].targetPath, 'b.pdf');
    assert.equal(cancelled[0].reason, '原理由');

    const revised = {
      ...plan, planId: 'plan-2', planVersion: 2, proposalSource: 'user-edit',
      operations: [{ ...plan.operations[0], id: 'new-op', targetPath: draft[0].targetPath, reason: draft[0].reason }],
    };
    assert.equal(helpers.canEditPlan(revised, true), true);
    assert.equal(helpers.canDecideOperation(revised.status, revised.operations[0]), true);
    assert.notEqual(revised.operations[0].id, plan.operations[0].id);
  });

  it('编辑清单拒绝未知动作和空清单', () => {
    const plan = {
      proposalSource: 'conversation', status: 'awaiting_approval',
      operations: [{ action: 'delete', approval: 'pending', executionStatus: 'pending' }],
    };
    assert.equal(helpers.canEditPlan(plan, true), false);
    assert.equal(helpers.canEditPlan({ ...plan, operations: [] }, true), false);
  });
});
