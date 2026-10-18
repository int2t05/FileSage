// 提供不含任务正文的全局操作记录接口与展示映射。
import { ApiError } from './files';

export interface AuditEvent {
  id: number;
  taskId?: string;
  eventType: string;
  actor: string;
  outcome: string;
  sourcePath?: string;
  targetPath?: string;
  createdAt: string;
}

export interface AuditPage {
  events: AuditEvent[];
  limit: number;
  nextCursor: number;
  hasMore: boolean;
}

const eventLabels: Readonly<Record<string, string>> = {
  'upload.publication': '上传文件',
  manual_create_directory: '新建文件夹',
  manual_rename: '重命名文件',
  manual_batch_rename: '批量重命名文件',
  manual_copy_file: '复制文件',
  manual_copy_directory: '复制文件夹',
  manual_move_file: '移动文件',
  manual_move_directory: '移动文件夹',
  manual_batch_copy: '批量复制文件',
  manual_batch_move: '批量移动文件',
  manual_delete: '删除文件',
  manual_batch_trash: '批量移入回收站',
  manual_text_save: '保存文件',
  manual_archive_create: '创建压缩包',
  manual_archive_extract: '解压文件',
  plan_created: '创建整理方案',
  operation_approved: '批准整理操作',
  operation_rejected: '拒绝整理操作',
  operation_started: '开始执行整理操作',
  operation_finished: '整理操作执行完成',
  operation_invalidated: '整理方案失效',
  operation_recovered: '恢复整理操作状态',
  plan_stop_requested: '请求停止后续操作',
  operation_cancelled: '取消未执行操作',
  plan_execution_stopped: '停止后续操作',
  duplicate_scan_queued: '重复扫描已排队',
  duplicate_scan_started: '开始重复文件扫描',
  duplicate_scan_finished: '重复文件扫描结束',
  trash_moving: '移入回收站',
  trash_destination_ready: '准备回收路径',
  trash_trashed: '移入回收站',
  trash_restore_started: '恢复文件',
  trash_restore_failed: '恢复文件失败',
  trash_restore_conflict: '恢复文件冲突',
  trash_restored: '恢复文件',
  trash_attention: '回收操作需检查',
  trash_purge_started: '清理回收文件',
  trash_purge_failed: '清理回收文件失败',
  trash_purged: '永久删除回收文件',
  trash_recovered: '核对回收状态',
  trash_recovery_attention: '回收状态需检查',
  permanent_delete_started: '永久删除文件',
  permanent_delete_failed: '永久删除失败',
  permanent_delete_completed: '永久删除文件',
};

const outcomeLabels: Readonly<Record<string, string>> = {
  accepted: '已接收',
  approved: '已批准',
  awaiting_approval: '待审批',
  cancelled: '已取消',
  completed: '已完成',
  failed: '失败',
  invalidated: '需重新审批',
  manual_review: '需人工复核',
  moving: '进行中',
  partial: '部分完成',
  pending: '待处理',
  queued: '排队中',
  requested: '已请求停止',
  purged: '已永久删除',
  recovered_success: '已恢复完成',
  rejected: '已拒绝',
  restoring: '进行中',
  running: '进行中',
  skipped: '未执行',
  stale: '已失效',
  started: '进行中',
  stopped: '已停止',
  succeeded: '已完成',
  retry_ready: '待重新执行',
  restored: '已恢复',
  trashed: '已在回收站',
  needs_attention: '需检查',
};

const actorLabels: Readonly<Record<string, string>> = {
  agent: 'Agent',
  system: '系统',
  user: '用户',
};

// 判断 JSON 是否为普通对象。
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// 校验必填字符串字段。
function requiredString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0;
}

// 校验可选字符串字段。
function optionalString(value: unknown): value is string | undefined {
  return value === undefined || typeof value === 'string';
}

// 校验单条记录，并丢弃未约定的字段。
function parseEvent(value: unknown): AuditEvent {
  if (!isRecord(value)
    || typeof value.id !== 'number' || !Number.isSafeInteger(value.id) || value.id < 1
    || !requiredString(value.eventType)
    || !requiredString(value.actor)
    || !requiredString(value.outcome)
    || !optionalString(value.taskId)
    || !optionalString(value.sourcePath)
    || !optionalString(value.targetPath)
    || !requiredString(value.createdAt)) {
    throw new ApiError('操作记录服务返回了无效事件。');
  }

  return {
    id: value.id,
    ...(value.taskId === undefined ? {} : { taskId: value.taskId }),
    eventType: value.eventType,
    actor: value.actor,
    outcome: value.outcome,
    ...(value.sourcePath === undefined ? {} : { sourcePath: value.sourcePath }),
    ...(value.targetPath === undefined ? {} : { targetPath: value.targetPath }),
    createdAt: value.createdAt,
  };
}

// 解析服务端分页，并仅保留页面展示字段。
export function parseAuditPage(value: unknown): AuditPage {
  if (!isRecord(value)
    || !Array.isArray(value.events)
    || typeof value.limit !== 'number' || !Number.isSafeInteger(value.limit) || value.limit < 1
    || typeof value.nextCursor !== 'number' || !Number.isSafeInteger(value.nextCursor) || value.nextCursor < 0
    || typeof value.hasMore !== 'boolean') {
    throw new ApiError('操作记录服务返回了无效响应。');
  }

  return {
    events: value.events.map(parseEvent),
    limit: value.limit,
    nextCursor: value.nextCursor,
    hasMore: value.hasMore,
  };
}

// 使用固定白名单生成友好操作名。
export function auditEventLabel(eventType: string): string {
  if (!Object.prototype.hasOwnProperty.call(eventLabels, eventType)) return '文件操作';
  return eventLabels[eventType] ?? '文件操作';
}

// 将服务端结果映射为简短状态。
export function auditOutcomeLabel(outcome: string): string {
  if (!Object.prototype.hasOwnProperty.call(outcomeLabels, outcome)) return '已记录';
  return outcomeLabels[outcome] ?? '已记录';
}

// 将已知操作方映射为简短名称。
export function auditActorLabel(actor: string): string {
  if (!Object.prototype.hasOwnProperty.call(actorLabels, actor)) return '操作方';
  return actorLabels[actor] ?? '操作方';
}

// 按游标读取全局操作记录。
export async function getAuditHistory(limit = 50, before?: number, signal?: AbortSignal): Promise<AuditPage> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (before !== undefined) params.set('before', String(before));

  let response: Response;
  try {
    response = await fetch(`/api/v1/audit?${params.toString()}`, {
      headers: { Accept: 'application/json' },
      signal,
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new ApiError('无法连接 FileSage 服务。请确认后端已启动。');
  }

  const payload = await response.json().catch(() => undefined) as unknown;
  if (!response.ok) {
    const message = isRecord(payload) && typeof payload.error === 'string'
      ? payload.error
      : response.status >= 500
        ? '操作记录服务暂不可用。请确认后端已启动。'
        : `读取操作记录失败（HTTP ${response.status}）`;
    throw new ApiError(message, response.status);
  }

  return parseAuditPage(payload);
}
