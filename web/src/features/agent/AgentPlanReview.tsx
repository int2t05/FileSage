// Agent 方案卡片：集中呈现逐项审核与执行确认。
import { useCallback, useEffect, useState } from 'react';
import {
  AlertTriangle,
  ArrowRight,
  Check,
  Clock3,
  Eye,
  EyeOff,
  Loader2,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
  X,
  XCircle,
} from 'lucide-react';
import { AgentApiError, getAgentOperationPreview, type AgentOperation, type AgentOperationPreview, type AgentPlan, type AgentPlanEditOperation, type AgentTextCandidateAction } from '../../api/agent';
import { ShellChangeReview } from './ShellChangeReview';

const PLAN_STATUS_LABELS: Record<string, string> = {
  awaiting_approval: '待审核',
  approved: '已批准',
  executing: '执行中',
  completed: '已完成',
  failed: '执行失败',
  invalidated: '方案已过期',
  rejected: '已拒绝',
  cancelled: '已停止',
  needs_attention: '需你核查',
};

const APPROVAL_LABELS: Record<string, string> = {
  pending: '待审核',
  approved: '已批准',
  rejected: '已拒绝',
  invalidated: '已过期',
};

const EXECUTION_LABELS: Record<string, string> = {
  pending: '等待执行',
  running: '执行中',
  succeeded: '已完成',
  failed: '执行失败',
  skipped: '已跳过',
  partial: '结果不确定 · 需核查',
  cancelled: '已取消',
};

type Decision = 'approve' | 'reject';

// 未明确支持的动作一律不提供审批和执行入口。
const APPROVABLE_ACTIONS = new Set(['rename', 'edit_text', 'create_file', 'overwrite_file', 'create_directory', 'move_file', 'copy_file', 'copy_directory', 'create_symlink', 'create_archive', 'extract_archive', 'set_permissions', 'set_owner', 'trash_duplicate', 'archive_photo', 'trash', 'permanent_delete', 'run_shell']);
const EDIT_ACTION_LABELS: Record<string, string> = {
  rename: '重命名',
  create_directory: '新建文件夹',
  move_file: '移动文件',
  copy_file: '复制文件',
  copy_directory: '复制文件夹',
  set_permissions: '修改权限',
  set_owner: '修改所有者',
};

interface AgentPlanReviewProps {
  plan: AgentPlan | null;
  loading: boolean;
  error: string;
  actionBusy: string;
  conversationBusy: string;
  actionError: string;
  notice: string;
  onRetry: () => void;
  onRefresh: () => void;
  onDecision: (operationId: string, decision: Decision) => void;
  onShellChangeDecision: (changeId: string) => void;
  onRejectPlan: () => void;
  onExecute: () => void;
  onStop: () => void;
  stopBusy: boolean;
  editable: boolean;
  onRevise: (operations: AgentPlanEditOperation[]) => Promise<boolean>;
  onEditTextContent: (operationId: string, proposedContent: string) => Promise<AgentPlan>;
}

function statusLabel(labels: Record<string, string>, status: string): string {
  return labels[status] ?? `未知状态：${status || '空'}`;
}

function planStatusTone(status: string): string {
  if (status === 'completed' || status === 'approved') return 'is-good';
  if (status === 'failed' || status === 'invalidated' || status === 'rejected' || status === 'cancelled' || status === 'needs_attention') return 'is-warning';
  if (status === 'executing') return 'is-active';
  return 'is-neutral';
}

export function canExecute(plan: AgentPlan): boolean {
  return plan.status === 'approved'
    && plan.operations.length > 0
    && plan.operations.every((operation) => isApprovableOperation(operation)
      && operation.approval === 'approved'
      && operation.executionStatus === 'pending');
}

export function approvalCounts(plan: AgentPlan) {
  return {
    pending: plan.operations.filter((operation) => operation.approval === 'pending').length,
    approved: plan.operations.filter((operation) => operation.approval === 'approved').length,
    rejected: plan.operations.filter((operation) => operation.approval === 'rejected').length,
    total: plan.operations.length,
  };
}

export function executionProgress(plan: AgentPlan) {
  const running = plan.operations.find((operation) => operation.executionStatus === 'running');
  return {
    completed: plan.operations.filter((operation) => operation.executionStatus === 'succeeded').length,
    total: plan.operations.length,
    remaining: plan.operations.filter((operation) => operation.executionStatus === 'pending').length,
    failed: plan.operations.filter((operation) => operation.executionStatus === 'failed' || operation.executionStatus === 'partial').length,
    runningOrdinal: running ? plan.operations.indexOf(running) + 1 : 0,
  };
}

export function canStopExecution(plan: AgentPlan): boolean {
  return plan.status === 'executing' && !plan.stopRequested;
}

export function executionLabel(planStatus: string, operationStatus: string): string {
  if (operationStatus === 'cancelled') return planStatus === 'cancelled' ? '已停止 · 未执行' : '未执行';
  return statusLabel(EXECUTION_LABELS, operationStatus);
}

// 仅状态失效或结果不确定时提示人工核查。
export function shouldShowSafetyNotice(plan: AgentPlan): boolean {
  return plan.status === 'invalidated' || plan.status === 'needs_attention'
    || plan.operations.some((operation) => operation.executionStatus === 'partial');
}

export function operationApprovalLabel(planStatus: string, operation: AgentOperation): string {
  if (planStatus === 'cancelled' && operation.approval === 'invalidated' && operation.executionStatus === 'cancelled') {
    return '已撤销';
  }
  return statusLabel(APPROVAL_LABELS, operation.approval);
}

export function requiresTextCandidatePreview(action: string): boolean {
  return action === 'create_file' || action === 'overwrite_file';
}

// 普通对话方案可在审批前编辑；照片归档路径由服务端生成。
export function canEditPlan(plan: AgentPlan, editable: boolean): boolean {
  return editable
    && ['conversation', 'model-analysis', 'user-edit'].includes(plan.proposalSource)
    && plan.status === 'awaiting_approval'
    && plan.operations.length > 0
    && plan.operations.length <= 20
    && plan.operations.every((operation) => operation.action !== 'archive_photo')
    && plan.operations.every((operation) => Object.hasOwn(EDIT_ACTION_LABELS, operation.action)
      && operation.approval === 'pending' && operation.executionStatus === 'pending');
}

export function createPlanEditDrafts(plan: AgentPlan): AgentPlanEditOperation[] {
  return plan.operations.map((operation) => {
    const ownerUid = operation.requestedOwnerUid
      ?? (operation.ownerUid !== operation.beforeOwnerUid ? operation.ownerUid : undefined);
    const groupGid = operation.requestedGroupGid
      ?? (operation.groupGid !== operation.beforeGroupGid ? operation.groupGid : undefined);
    return {
      action: operation.action,
      sourcePath: operation.sourcePath,
      targetPath: operation.action === 'set_permissions' || operation.action === 'set_owner' ? '' : operation.targetPath,
      reason: operation.reason,
      evidence: operation.evidence,
      ...((operation.requestedMode ?? operation.mode) === undefined ? {} : { mode: operation.requestedMode ?? operation.mode }),
      ...(ownerUid === undefined ? {} : { ownerUid }),
      ...(groupGid === undefined ? {} : { groupGid }),
    };
  });
}

// 取消编辑时恢复服务端当前版本，丢弃本地草稿。
export function cancelPlanEdit(plan: AgentPlan): AgentPlanEditOperation[] {
  return createPlanEditDrafts(plan);
}

export function canRejectPlan(plan: AgentPlan): boolean {
  return plan.status === 'awaiting_approval'
    && plan.operations.some((operation) => operation.approval === 'pending'
      && operation.executionStatus === 'pending');
}

function isApprovableOperation(operation: AgentPlan['operations'][number]): boolean {
  if (!APPROVABLE_ACTIONS.has(operation.action)) return false;
  if (operation.action === 'run_shell') {
    return typeof operation.command === 'string' && operation.command.trim().length > 0
      && operation.command.length <= 64 * 1024 && !operation.command.includes('\0')
      && typeof operation.workingDir === 'string';
  }
  if (operation.action === 'extract_archive' && (!operation.plannedPaths || operation.plannedPaths.length === 0)) return false;
  if (operation.action === 'set_permissions') {
    return Number.isInteger(operation.beforeMode) && operation.beforeMode! >= 0 && operation.beforeMode! <= 511
      && Number.isInteger(operation.mode) && operation.mode! >= 0 && operation.mode! <= 511;
  }
  if (operation.action === 'set_owner') {
    return [operation.beforeOwnerUid, operation.ownerUid, operation.beforeGroupGid, operation.groupGid]
      .every((value) => Number.isInteger(value) && value! >= 0 && value! <= 0xffff_ffff);
  }
  return true;
}

// 未知动作不能进入逐项审核。
export function canDecideOperation(planStatus: string, operation: AgentPlan['operations'][number]): boolean {
  return planStatus === 'awaiting_approval'
    && operation.action !== 'run_shell'
    && isApprovableOperation(operation)
    && operation.approval === 'pending'
    && operation.executionStatus === 'pending';
}

// 根据动作呈现对应的路径含义；新建目录的目标是完整路径。
export function operationPresentation(
  action: string,
  sourcePath: string,
  targetPath: string,
  metadata: Pick<AgentPlan['operations'][number], 'beforeMode' | 'mode' | 'beforeOwnerUid' | 'ownerUid' | 'beforeGroupGid' | 'groupGid'> = {},
) {
  if (action === 'set_permissions') {
    const before = metadata.beforeMode === undefined ? '—' : metadata.beforeMode.toString(8).padStart(4, '0');
    const after = metadata.mode === undefined ? '—' : metadata.mode.toString(8).padStart(4, '0');
    return {
      title: '修改权限',
      metadataPath: sourcePath || '—',
      metadataChanges: [{ label: '权限', before, after, unchanged: before === after }],
    };
  }

  if (action === 'set_owner') {
    const ownerBefore = metadata.beforeOwnerUid;
    const ownerAfter = metadata.ownerUid;
    const groupBefore = metadata.beforeGroupGid;
    const groupAfter = metadata.groupGid;
    return {
      title: '修改所有者',
      metadataPath: sourcePath || '—',
      metadataChanges: [
        ...(ownerBefore === undefined || ownerAfter === undefined ? [] : [{
          label: '用户 ID', before: String(ownerBefore), after: String(ownerAfter), unchanged: ownerBefore === ownerAfter,
        }]),
        ...(groupBefore === undefined || groupAfter === undefined ? [] : [{
          label: '用户组 ID', before: String(groupBefore), after: String(groupAfter), unchanged: groupBefore === groupAfter,
        }]),
      ],
    };
  }

  if (action === 'create_directory') {
    return {
      title: '新建文件夹',
      sourceLabel: '所在目录',
      sourceDisplayPath: sourcePath || '根目录',
      targetLabel: '新文件夹',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'move_file') {
    return {
      title: '移动文件',
      sourceLabel: '从',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '移动到',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'archive_photo') {
    return {
      title: '按拍摄日期归档照片',
      sourceLabel: '原路径',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '归档到',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'copy_file') {
    return {
      title: '复制文件',
      sourceLabel: '复制自',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '复制到',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'copy_directory') {
    return {
      title: '复制文件夹',
      sourceLabel: '复制自',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '复制到',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'create_archive' || action === 'extract_archive') {
    const extract = action === 'extract_archive';
    return {
      title: extract ? '解压压缩包' : '创建压缩包',
      sourceLabel: extract ? '压缩包' : '打包来源',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: extract ? '解压到' : '压缩包路径',
      targetDisplayPath: targetPath || (extract ? '根目录' : '—'),
    };
  }

  if (action === 'create_symlink') {
    return {
      title: '创建软链接',
      sourceLabel: '指向',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '新链接路径',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'trash_duplicate') {
    return {
      title: '移入回收站',
      sourceLabel: '待回收副本',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '保留文件',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'trash') {
    return {
      title: '移入回收站',
      sourceLabel: '待回收项目',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '处理方式',
      targetDisplayPath: '移入可恢复回收站',
    };
  }

  if (action === 'permanent_delete') {
    return {
      title: '永久删除（不可恢复）',
      sourceLabel: '待永久删除项目',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '删除后',
      targetDisplayPath: '无法恢复',
    };
  }

  if (action === 'rename') {
    return {
      title: '重命名',
      sourceLabel: '原路径',
      sourceDisplayPath: sourcePath || '—',
      targetLabel: '新路径',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'edit_text') {
    return { title: '修改文件正文', textFilePath: sourcePath || '—' };
  }

  if (action === 'create_file') {
    return {
      title: '新建文本文件',
      sourceLabel: '所在目录',
      sourceDisplayPath: sourcePath || '根目录',
      targetLabel: '新建文件',
      targetDisplayPath: targetPath || '—',
    };
  }

  if (action === 'overwrite_file') {
    return { title: '覆盖文本文件', textFilePath: sourcePath || '—', textFileLabel: '将被替换的文件正文（全文）' };
  }

  return {
    title: `未知操作：${action}`,
    sourceLabel: '原路径',
    sourceDisplayPath: sourcePath || '—',
    targetLabel: '新路径',
    targetDisplayPath: targetPath || '—',
  };
}

// 从方案操作提取展示所需的完整元数据。
export function operationPresentationForOperation(operation: AgentPlan['operations'][number]) {
  if (operation.action === 'run_shell') {
    return {
      title: '隔离运行 Shell',
      sourceLabel: '管理范围',
      sourceDisplayPath: operation.sourcePath || '管理根目录',
      targetLabel: '工作目录',
      targetDisplayPath: operation.workingDir === '.' || !operation.workingDir
        ? operation.sourcePath || '管理根目录'
        : [operation.sourcePath, operation.workingDir].filter(Boolean).join('/'),
      shellCommand: operation.command ?? '',
    };
  }
  return operationPresentation(operation.action, operation.sourcePath, operation.targetPath, {
    beforeMode: operation.beforeMode,
    mode: operation.mode,
    beforeOwnerUid: operation.beforeOwnerUid,
    ownerUid: operation.ownerUid,
    beforeGroupGid: operation.beforeGroupGid,
    groupGid: operation.groupGid,
  });
}

function ArchivePlannedPaths({ paths }: { paths: string[] }) {
  const [visible, setVisible] = useState(100);
  return (
    <details className="agent-operation-evidence">
      <summary>解压清单（{paths.length} 项）</summary>
      <ol>
        {paths.slice(0, visible).map((item) => <li key={item}><code>{item}</code></li>)}
      </ol>
      {visible < paths.length && (
        <button className="agent-text-button" type="button" onClick={() => setVisible((count) => Math.min(count + 100, paths.length))}>
          显示后续 {Math.min(100, paths.length - visible)} 项
        </button>
      )}
    </details>
  );
}

function textCandidatePath(operation: AgentOperation): string {
  return operation.action === 'create_file' ? operation.targetPath : operation.sourcePath;
}

// 预览只在用户点击后读取；正文仅保存在当前组件内存。
function OperationTextPreview({
  planId,
  planVersion,
  operation,
  editable,
  disabled,
  onSave,
  onEditingChange,
  onPreviewReady,
}: {
  planId: string;
  planVersion: number;
  operation: AgentOperation;
  editable: boolean;
  disabled: boolean;
  onSave: (operationId: string, proposedContent: string) => Promise<AgentPlan>;
  onEditingChange: (editing: boolean) => void;
  onPreviewReady: (key: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [editing, setEditing] = useState(false);
  const [preview, setPreview] = useState<AgentOperationPreview | null>(null);
  const [draft, setDraft] = useState('');
  const [error, setError] = useState('');
  const previewId = `agent-text-preview-${encodeURIComponent(operation.id)}`;

  useEffect(() => {
    if (!open) return undefined;
    const controller = new AbortController();
    setLoading(true);
    setPreview(null);
    setError('');
    void getAgentOperationPreview(planId, operation.id, planVersion, operation.action as AgentTextCandidateAction, textCandidatePath(operation), controller.signal)
      .then((value) => {
        setPreview(value);
        setDraft(value.proposedContent);
        onPreviewReady(`${planId}:${planVersion}:${operation.id}`);
      })
      .catch((reason: unknown) => {
        if (reason instanceof DOMException && reason.name === 'AbortError') return;
        setError(reason instanceof AgentApiError && reason.status === 409
          ? '文件状态已变化，预览已失效。请刷新方案后重试。'
          : reason instanceof Error ? reason.message : '读取修改预览失败。');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [open, operation.id, operation.sourcePath, operation.targetPath, operation.action, planId, planVersion, onPreviewReady]);

  function togglePreview() {
    if (open && editing) {
      setEditing(false);
      onEditingChange(false);
      setDraft(preview?.proposedContent ?? '');
    }
    setOpen((current) => !current);
    if (open) {
      setPreview(null);
      setError('');
    }
  }

  async function saveCandidate() {
    if (!preview || !editable || disabled || saving) return;
    setSaving(true);
    setError('');
    try {
      await onSave(operation.id, draft);
      setEditing(false);
      onEditingChange(false);
      setPreview(null);
    } catch (reason) {
      if (reason instanceof AgentApiError && reason.status === 409) {
        setEditing(false);
        onEditingChange(false);
        setPreview(null);
        setDraft('');
        setError('方案或文件已变化，候选未保存。请刷新后重新核对。');
      } else {
        setError(reason instanceof Error ? reason.message : '保存修改候选失败。');
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="agent-text-preview">
      <button className="agent-text-button" type="button" onClick={togglePreview} aria-expanded={open} aria-controls={previewId} disabled={saving}>
        {open ? <EyeOff size={14} aria-hidden="true" /> : <Eye size={14} aria-hidden="true" />}
        {open ? '收起修改预览' : '查看修改预览'}
      </button>
      {open && (
        <div id={previewId} className="agent-text-preview-panel">
          {loading && <p className="agent-text-preview-status" role="status"><Loader2 className="agent-spinner" size={14} aria-hidden="true" />正在读取预览…</p>}
          {error && <p className="agent-text-preview-error" role="alert">{error}</p>}
          {preview && (
            <>
              <div className="agent-text-preview-columns">
                <section aria-label={operation.action === 'create_file' ? '原文（目标不存在）' : operation.action === 'overwrite_file' ? '将被替换的原文' : '原文'}>
                  <h4>{operation.action === 'create_file' ? '原文（目标不存在）' : operation.action === 'overwrite_file' ? '将被替换的原文' : '原文'}</h4>
                  <pre>{operation.action === 'create_file' ? '目标文件尚不存在，不读取原文。' : preview.sourceContent}</pre>
                </section>
                <section aria-label="修改后内容">
                  <h4>修改后</h4>
                  <pre>{editing ? draft : preview.proposedContent}</pre>
                </section>
              </div>
              {editable && !editing && (
                <button className="agent-text-button agent-text-preview-edit" type="button" onClick={() => {
                  setDraft(preview.proposedContent);
                  setEditing(true);
                  onEditingChange(true);
                }} disabled={disabled || saving}>编辑候选</button>
              )}
              {editing && (
                <div className="agent-text-preview-editor">
                  <label htmlFor={`${previewId}-draft`}>修改后的文件正文
                    <textarea id={`${previewId}-draft`} rows={10} value={draft} onChange={(event) => setDraft(event.currentTarget.value)} disabled={saving || disabled || !editable} />
                  </label>
                  <div>
                    <button className="agent-secondary-button" type="button" onClick={() => {
                      setDraft(preview.proposedContent);
                      setEditing(false);
                      onEditingChange(false);
                      setError('');
                    }} disabled={saving}>取消</button>
                    <button className="agent-primary-button" type="button" onClick={() => { void saveCandidate(); }} disabled={saving || disabled || !editable}>
                      {saving ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                      {saving ? '正在保存…' : '保存候选'}
                    </button>
                  </div>
                </div>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );
}

export function AgentPlanReview({
  plan,
  loading,
  error,
  actionBusy,
  conversationBusy,
  actionError,
  notice,
  onRetry,
  onRefresh,
  onDecision,
  onShellChangeDecision,
  onRejectPlan,
  onExecute,
  onStop,
  stopBusy,
  editable,
  onRevise,
  onEditTextContent,
}: AgentPlanReviewProps) {
  const [editing, setEditing] = useState(false);
  const [editingTextOperationId, setEditingTextOperationId] = useState('');
  const [previewedTextCandidateKey, setPreviewedTextCandidateKey] = useState('');
  const [editOperations, setEditOperations] = useState<AgentPlanEditOperation[]>([]);
  const [editError, setEditError] = useState('');
  const editableNow = plan ? canEditPlan(plan, editable) : false;
  const markTextCandidatePreviewed = useCallback((key: string) => setPreviewedTextCandidateKey(key), []);
  const shellPlan = Boolean(plan?.operations.some((operation) => operation.action === 'run_shell'));
  const photoArchivePlan = Boolean(plan?.operations.some((operation) => operation.action === 'archive_photo'));
  const permanentDeleteCount = plan?.operations.filter((operation) => operation.action === 'permanent_delete').length ?? 0;
  const executeButtonLabel = permanentDeleteCount === plan?.operations.length
    ? `确认永久删除 ${permanentDeleteCount} 项`
    : permanentDeleteCount > 0
      ? `确认执行 ${plan?.operations.length ?? 0} 项（永久删除 ${permanentDeleteCount} 项）`
      : shellPlan ? '写入已批准的文件变化' : photoArchivePlan ? '执行照片归档' : `执行 ${plan?.operations.length ?? 0} 项`;

  function beginEditing() {
    if (!plan || !editableNow) return;
    setEditOperations(createPlanEditDrafts(plan));
    setEditError('');
    setEditing(true);
  }

  function cancelEditing() {
    if (plan) setEditOperations(cancelPlanEdit(plan));
    setEditError('');
    setEditing(false);
  }

  function updateOperation(index: number, changes: Partial<AgentPlanEditOperation>) {
    setEditOperations((current) => current.map((operation, itemIndex) => itemIndex === index
      ? { ...operation, ...changes }
      : operation));
  }

  async function saveRevision() {
    if (!plan || !canEditPlan(plan, editable) || actionBusy || editOperations.length < 1 || editOperations.length > 20) return;
    if (editOperations.some((operation) => !operation.reason.trim())) {
      setEditError('每项操作都需要填写理由。');
      return;
    }
    setEditError('');
    const saved = await onRevise(editOperations);
    if (saved) setEditing(false);
  }

  return (
    <section className="agent-proposal-section" aria-labelledby="agent-proposal-title">
      <div className="agent-section-heading agent-proposal-heading">
        <div><h2 id="agent-proposal-title">{photoArchivePlan ? '照片归档清单' : '操作清单'}</h2><p>{photoArchivePlan ? '按拍摄日期逐项核对原路径、归档位置和执行结果。' : '逐项查看路径、审批状态与执行结果。'}</p></div>
        {plan && (
          <span className={`agent-plan-status ${planStatusTone(plan.status)}`}>
            <Clock3 size={14} aria-hidden="true" />{statusLabel(PLAN_STATUS_LABELS, plan.status)}
          </span>
        )}
      </div>

      {actionError && <div className="agent-error" role="alert"><AlertTriangle size={16} aria-hidden="true" /><span>{actionError}</span></div>}
      {notice && <div className="agent-notice" role="status"><Check size={16} aria-hidden="true" /><span>{notice}</span></div>}
      {error && <div className="agent-error" role="alert"><AlertTriangle size={16} aria-hidden="true" /><span>{error}</span><button type="button" onClick={onRetry}>重试</button></div>}
      {loading ? (
        <div className="agent-state" role="status"><Loader2 className="agent-spinner" size={20} aria-hidden="true" /><strong>正在读取方案</strong></div>
      ) : !plan && !error ? (
        <div className="agent-state"><strong>方案尚未生成</strong></div>
      ) : plan ? (
        <>
          {(() => {
            const counts = approvalCounts(plan);
            return (
              <div className="agent-plan-meta">
                <span>{shellPlan ? '1 项隔离 Shell 命令' : `${counts.total} 项操作`}</span>
                {!shellPlan && <span className="agent-approval-count" role="status">待审核 {counts.pending} · 已批准 {counts.approved}</span>}
                {editableNow && !editing && (
                  <button className="agent-text-button" type="button" onClick={beginEditing} disabled={Boolean(actionBusy || conversationBusy)}>
                    <Pencil size={14} aria-hidden="true" />修改清单
                  </button>
                )}
                {!editing && (
                  <button className="agent-text-button" type="button" onClick={onRefresh} disabled={loading || Boolean(actionBusy || editingTextOperationId)}>
                    <RefreshCw size={14} aria-hidden="true" />刷新
                  </button>
                )}
              </div>
            );
          })()}

          {shouldShowSafetyNotice(plan) && (
            <div className="agent-safety-notice" role="status"><AlertTriangle size={16} aria-hidden="true" /><span>文件状态已变化或执行结果需要核查，请刷新并检查文件。</span></div>
          )}

          {editing && editableNow ? (
            <div className="agent-plan-editor" aria-label="编辑操作清单">
              <div className="agent-plan-editor-heading">
                <div><strong>修改操作清单</strong><p>保存后生成新版本，所有项目需要重新审核。</p></div>
                <button className="agent-text-button" type="button" onClick={() => setEditOperations((current) => current.length >= 20 ? current : [...current, { action: 'create_directory', sourcePath: '', targetPath: '', reason: '', evidence: '' }])} disabled={editOperations.length >= 20 || Boolean(actionBusy)}>
                  <Plus size={14} aria-hidden="true" />新增操作
                </button>
              </div>
              {editOperations.map((operation, index) => {
                const metadataAction = operation.action === 'set_permissions' || operation.action === 'set_owner';
                const pathLabel = operation.action === 'create_directory' ? '所在目录' : metadataAction ? '文件路径' : operation.action === 'rename' ? '原路径' : '源路径';
                const targetLabel = operation.action === 'create_directory' ? '新文件夹完整路径' : operation.action === 'rename' ? '新路径' : '目标路径';
                return (
                  <article className="agent-plan-edit-card" key={index}>
                    <div className="agent-plan-edit-heading">
                      <strong>第 {index + 1} 项</strong>
                      <button className="agent-icon-button" type="button" aria-label={`移除第 ${index + 1} 项`} onClick={() => setEditOperations((current) => current.filter((_, itemIndex) => itemIndex !== index))} disabled={editOperations.length <= 1 || Boolean(actionBusy)}>
                        <Trash2 size={15} aria-hidden="true" />
                      </button>
                    </div>
                    <label>操作类型
                      <select value={operation.action} onChange={(event) => updateOperation(index, {
                        action: event.currentTarget.value, sourcePath: '', targetPath: '', mode: undefined, ownerUid: undefined, groupGid: undefined,
                      })} disabled={Boolean(actionBusy)}>
                        {Object.entries(EDIT_ACTION_LABELS).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
                      </select>
                    </label>
                    <label>{pathLabel}
                      <input type="text" value={operation.sourcePath} maxLength={4096} onChange={(event) => updateOperation(index, { sourcePath: event.currentTarget.value })} disabled={Boolean(actionBusy)} />
                    </label>
                    {!metadataAction && (
                      <label>{targetLabel}
                        <input type="text" value={operation.targetPath} maxLength={4096} onChange={(event) => updateOperation(index, { targetPath: event.currentTarget.value })} disabled={Boolean(actionBusy)} />
                      </label>
                    )}
                    {operation.action === 'set_permissions' && (
                      <label>权限值（十进制）
                        <input type="number" min={0} max={511} step={1} value={operation.mode ?? ''} onChange={(event) => updateOperation(index, { mode: event.currentTarget.value === '' ? undefined : Number(event.currentTarget.value) })} disabled={Boolean(actionBusy)} />
                        <small>例如 384 表示 0600；有效范围 0–511。</small>
                      </label>
                    )}
                    {operation.action === 'set_owner' && (
                      <div className="agent-plan-edit-number-row">
                        <label>用户 ID（可选）
                          <input type="number" min={0} max={4294967295} step={1} value={operation.ownerUid ?? ''} onChange={(event) => updateOperation(index, { ownerUid: event.currentTarget.value === '' ? undefined : Number(event.currentTarget.value) })} disabled={Boolean(actionBusy)} />
                        </label>
                        <label>用户组 ID（可选）
                          <input type="number" min={0} max={4294967295} step={1} value={operation.groupGid ?? ''} onChange={(event) => updateOperation(index, { groupGid: event.currentTarget.value === '' ? undefined : Number(event.currentTarget.value) })} disabled={Boolean(actionBusy)} />
                        </label>
                      </div>
                    )}
                    <label>操作理由
                      <input type="text" value={operation.reason} maxLength={1024} onChange={(event) => updateOperation(index, { reason: event.currentTarget.value })} disabled={Boolean(actionBusy)} />
                    </label>
                    <label>依据（可选）
                      <textarea rows={2} value={operation.evidence} maxLength={2048} onChange={(event) => updateOperation(index, { evidence: event.currentTarget.value })} disabled={Boolean(actionBusy)} />
                    </label>
                  </article>
                );
              })}
              {editError && <p className="agent-composer-error" role="alert">{editError}</p>}
              <div className="agent-plan-editor-actions">
                <button className="agent-secondary-button" type="button" onClick={cancelEditing} disabled={Boolean(actionBusy || conversationBusy)}><X size={15} aria-hidden="true" />取消</button>
                <button className="agent-primary-button" type="button" onClick={() => { void saveRevision(); }} disabled={Boolean(actionBusy || conversationBusy || editOperations.length < 1 || editOperations.length > 20)}>
                  {actionBusy === 'revise' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}保存并重新审核
                </button>
              </div>
            </div>
          ) : plan.operations.length === 0 ? (
            <div className="agent-state"><strong>没有待处理操作</strong></div>
          ) : (
            <div className="agent-operation-list">
              {plan.operations.map((operation) => {
                const presentation = operationPresentationForOperation(operation);
                const canDecide = canDecideOperation(plan.status, operation);
                const candidatePreviewKey = `${plan.planId}:${plan.planVersion}:${operation.id}`;
                const mustPreviewCandidate = requiresTextCandidatePreview(operation.action);
                const candidateWasPreviewed = previewedTextCandidateKey === candidatePreviewKey;
                const unknownAction = !isApprovableOperation(operation);
                return (
                  <article className={`agent-operation-card${operation.action === 'permanent_delete' ? ' agent-operation-card-danger' : ''}`} key={operation.action === 'edit_text' ? `edit_text:${operation.sourcePath}` : operation.id}>
                    <div className="agent-operation-content">
                      <div className="agent-operation-heading">
                        <h3>{presentation.title}</h3>
                        <span className="agent-operation-state">{operationApprovalLabel(plan.status, operation)}</span>
                        {plan.status === 'executing' && <span className="agent-operation-execution">{executionLabel(plan.status, operation.executionStatus)}</span>}
                      </div>
                      <div className="agent-operation-paths">
                        {'metadataChanges' in presentation && Array.isArray(presentation.metadataChanges) ? (
                          <div className="agent-operation-metadata">
                            <div className="agent-operation-metadata-file"><span>文件</span><code>{presentation.metadataPath}</code></div>
                            {presentation.metadataChanges.map((change) => (
                              <div className="agent-operation-metadata-change" key={change.label}>
                                <span>{change.label}</span>
                                {change.unchanged
                                  ? <code>{change.before}（不变）</code>
                                  : <><code>{change.before}</code><ArrowRight size={14} aria-hidden="true" /><code>{change.after}</code></>}
                              </div>
                            ))}
                          </div>
                        ) : 'textFilePath' in presentation ? (
                          <div className="agent-operation-metadata-file"><span>{'textFileLabel' in presentation ? presentation.textFileLabel : '文件正文（全文）'}</span><code>{presentation.textFilePath}</code></div>
                        ) : (
                          <>
                            <div><span>{presentation.sourceLabel}</span><code>{presentation.sourceDisplayPath}</code></div>
                            <ArrowRight size={15} aria-hidden="true" />
                            <div><span>{presentation.targetLabel}</span><code>{presentation.targetDisplayPath}</code></div>
                          </>
                        )}
                      </div>
                      {operation.reason && <p className="agent-operation-reason">{operation.reason}</p>}
                      {operation.action === 'run_shell' && <pre className="agent-shell-command agent-shell-plan-command">{operation.command}</pre>}
                      {operation.action === 'run_shell' && (
                        <ShellChangeReview
                          key={`${plan.planId}:${operation.id}`}
                          planId={plan.planId}
                          planVersion={plan.planVersion}
                          planStatus={plan.status}
                          actionBusy={actionBusy}
                          conversationBusy={conversationBusy}
                          onApprove={onShellChangeDecision}
                          onRefreshPlan={onRefresh}
                        />
                      )}
                      {operation.action === 'extract_archive' && operation.plannedPaths && <ArchivePlannedPaths paths={operation.plannedPaths} />}
                      {operation.action === 'permanent_delete' && <p className="agent-safety-inline">永久删除后无法恢复。请核对路径，批准后还需再次确认执行。</p>}
                      {operation.action === 'create_file' && <p className="agent-safety-inline">仅按已确认的任务与路径生成候选，不读取原文；批准后仍需单独确认执行。</p>}
                      {operation.action === 'overwrite_file' && <p className="agent-safety-inline">确认分析时已读取完整原文并发送给当前模型。候选会完整替换原文件，执行后原内容不可恢复；批准后仍需单独确认执行。</p>}
                      {mustPreviewCandidate && !candidateWasPreviewed && <p className="agent-safety-inline">请先打开并核对文本候选，之后才能批准。</p>}
                      {operation.evidence && (
                        <details className="agent-operation-evidence">
                          <summary>查看依据</summary>
                          <p>{operation.evidence}</p>
                        </details>
                      )}
                      {(operation.action === 'edit_text' || operation.action === 'create_file' || operation.action === 'overwrite_file') && (
                        <OperationTextPreview
                          planId={plan.planId}
                          planVersion={plan.planVersion}
                          operation={operation}
                          editable={plan.status === 'awaiting_approval' && operation.approval === 'pending' && operation.executionStatus === 'pending'}
                          disabled={Boolean(actionBusy || conversationBusy)}
                          onSave={onEditTextContent}
                          onEditingChange={(isEditing) => setEditingTextOperationId((current) => isEditing ? operation.id : current === operation.id ? '' : current)}
                          onPreviewReady={markTextCandidatePreviewed}
                        />
                      )}
                      {(operation.executionStatus !== 'pending' || operation.resultMessage) && (
                        <div className="agent-operation-result">
                          <span>{executionLabel(plan.status, operation.executionStatus)}</span>
                          {operation.resultMessage && <p>{operation.resultMessage}</p>}
                        </div>
                      )}
                      {unknownAction && <p className="agent-safety-inline">此操作暂不支持。</p>}
                      <div className="agent-operation-actions">
                        {canDecide ? (
                          <button className="agent-primary-button" type="button" onClick={() => onDecision(operation.id, 'approve')} disabled={Boolean(actionBusy || conversationBusy || editingTextOperationId === operation.id || mustPreviewCandidate && !candidateWasPreviewed)}>
                            {actionBusy === operation.id ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}批准
                          </button>
                        ) : operation.action !== 'run_shell' && plan.status !== 'completed' && operation.approval === 'pending' ? (
                          <span className="agent-operation-lock">此项暂不可审批</span>
                        ) : null}
                      </div>
                    </div>
                  </article>
                );
              })}
            </div>
          )}

          {canRejectPlan(plan) && (
            <footer className="agent-reject-plan-panel">
              <p>拒绝会取消整份方案。</p>
              <button className="agent-reject-button" type="button" onClick={onRejectPlan} disabled={Boolean(actionBusy || conversationBusy)}>
                <XCircle size={15} aria-hidden="true" />拒绝整份方案
              </button>
            </footer>
          )}

          {canExecute(plan) && (
            <footer className="agent-execution-panel">
              <p>{permanentDeleteCount > 0 ? `${permanentDeleteCount} 项永久删除后无法恢复。` : ''}{shellPlan ? '已批准全部文件变化，确认后写入原目录。' : `已批准全部 ${plan.operations.length} 项${photoArchivePlan ? '照片归档' : '操作'}，确认后立即执行。`}</p>
              <button className="agent-primary-button" type="button" onClick={onExecute} disabled={Boolean(actionBusy)}>
                {actionBusy === 'execute' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                {executeButtonLabel}
              </button>
            </footer>
          )}

          {plan.status === 'executing' && (() => {
            const progress = executionProgress(plan);
            return (
              <footer className="agent-execution-progress" role="status" aria-live="polite">
                <div className="agent-execution-progress-heading">
                  <strong>已完成 {progress.completed} / {progress.total} 项</strong>
                  {progress.runningOrdinal > 0 && <span>正在执行第 {progress.runningOrdinal} 项</span>}
                  {progress.failed > 0 && <span>失败或需核查 {progress.failed} 项</span>}
                </div>
                <progress max={progress.total} value={progress.completed} aria-label={`已完成 ${progress.completed} / ${progress.total} 项`} />
                {!canStopExecution(plan) ? (
                  <p>已收到停止请求；当前操作安全结束后将停止后续项，已完成操作不会撤回。</p>
                ) : (
                  <>
                    <p>可停止后续项；当前操作会先安全结束，已完成操作不会撤回。</p>
                    <button className="agent-reject-button" type="button" onClick={onStop} disabled={stopBusy || Boolean(conversationBusy)}>
                      {stopBusy ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <XCircle size={15} aria-hidden="true" />}
                      {stopBusy ? '正在请求…' : '停止后续操作'}
                    </button>
                  </>
                )}
              </footer>
            );
          })()}
        </>
      ) : null}
    </section>
  );
}
