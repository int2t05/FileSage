import { listTasks, type TaskSummary } from './tasks';

export interface AgentOperation {
  id: string;
  action: string;
  sourcePath: string;
  targetPath: string;
  reason: string;
  evidence: string;
  approval: string;
  executionStatus: string;
  beforeMode?: number;
  mode?: number;
  requestedMode?: number;
  beforeOwnerUid?: number;
  ownerUid?: number;
  requestedOwnerUid?: number;
  beforeGroupGid?: number;
  groupGid?: number;
  requestedGroupGid?: number;
  plannedPaths?: string[];
  resultMessage?: string;
  command?: string;
  workingDir?: string;
}

export interface AgentOperationPreview {
  planId: string;
  operationId: string;
  path: string;
  sourceContent: string;
  proposedContent: string;
}

export type AgentTextCandidateAction = 'edit_text' | 'create_file' | 'overwrite_file';

export interface AgentShellChangeState {
  sha256: string;
  mode: number;
  isDirectory: boolean;
  size: number;
}

export interface AgentShellChange {
  id: string;
  path: string;
  kind: 'added' | 'modified' | 'deleted';
  approval: string;
  status: string;
  binary: boolean;
  diffTruncated: boolean;
  before: AgentShellChangeState | null;
  after: AgentShellChangeState | null;
}

export interface AgentShellChangesPage {
  changes: AgentShellChange[];
  offset: number;
  limit: number;
  total: number;
  hasMore: boolean;
}

export interface AgentShellChangePreview {
  change: AgentShellChange;
  textDiff: string;
}

export interface AgentPlanEditOperation {
  action: string;
  sourcePath: string;
  targetPath: string;
  reason: string;
  evidence: string;
  mode?: number;
  ownerUid?: number;
  groupGid?: number;
}

export interface AgentPlan {
  taskId: string;
  planId: string;
  planVersion: number;
  status: string;
  stopRequested: boolean;
  proposalSource: string;
  operations: AgentOperation[];
}

export type AgentTaskHistoryItem = Pick<TaskSummary, 'id' | 'status' | 'scopePath' | 'createdAt' | 'updatedAt'>;

export class AgentApiError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
    this.name = 'AgentApiError';
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function optionalInteger(value: unknown, max: number): number | undefined {
  if (value === undefined || value === null) return undefined;
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= max ? value : Number.NaN;
}

function isSafeRootRelativePath(value: string): boolean {
  return value !== '' && value.length <= 4096 && !value.startsWith('/') && !value.includes('\\')
    && !value.includes('\0') && !/^[A-Za-z]:/.test(value)
    && value.split('/').every((part) => part !== '' && part !== '.' && part !== '..'
      && part.length <= 255 && !/[\u0000-\u001f\u007f]/.test(part));
}

function isSupportedTextPath(value: string): boolean {
  return /\.(txt|md|markdown|mdown)$/i.test(value);
}

function textOperationPath(action: string, sourcePath: string, targetPath: string): string {
  return action === 'create_file' ? targetPath : sourcePath;
}

function isShellWorkingDirectory(value: string): boolean {
  return value === '.' || value === '' || isSafeRootRelativePath(value);
}

// 匹配沙箱命令字节上限并拒绝空命令。
function isShellCommand(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
    && new TextEncoder().encode(value).byteLength <= 64 * 1024 && !value.includes('\0');
}

function parsePlannedPaths(value: unknown): string[] {
  if (!Array.isArray(value) || value.length < 1 || value.length > 50000) {
    throw new AgentApiError('Agent 服务返回了无效的解压清单。');
  }
  const paths: string[] = [];
  let size = 0;
  for (const item of value) {
    if (typeof item !== 'string' || !isSafeRootRelativePath(item)) {
      throw new AgentApiError('Agent 服务返回了不安全的解压路径。');
    }
    size += item.length;
    if (size > 4 * 1024 * 1024) throw new AgentApiError('Agent 解压清单超过展示上限。');
    paths.push(item);
  }
  if (new Set(paths).size !== paths.length) throw new AgentApiError('Agent 解压清单包含重复路径。');
  return paths;
}

function parseOperation(value: unknown): AgentOperation | null {
  if (!isRecord(value)
    || typeof value.id !== 'string'
    || typeof value.action !== 'string'
    || typeof value.sourcePath !== 'string'
    || typeof value.targetPath !== 'string'
    || typeof value.reason !== 'string'
    || typeof value.evidence !== 'string'
    || typeof value.approval !== 'string'
    || typeof value.executionStatus !== 'string'
    || (value.resultMessage !== undefined && value.resultMessage !== null && typeof value.resultMessage !== 'string')) {
    return null;
  }

  const beforeMode = optionalInteger(value.beforeMode, 511);
  const mode = optionalInteger(value.mode, 511);
  const requestedMode = optionalInteger(value.requestedMode, 511);
  const beforeOwnerUid = optionalInteger(value.beforeOwnerUid, 0xffff_ffff);
  const ownerUid = optionalInteger(value.ownerUid, 0xffff_ffff);
  const requestedOwnerUid = optionalInteger(value.requestedOwnerUid, 0xffff_ffff);
  const beforeGroupGid = optionalInteger(value.beforeGroupGid, 0xffff_ffff);
  const groupGid = optionalInteger(value.groupGid, 0xffff_ffff);
  const requestedGroupGid = optionalInteger(value.requestedGroupGid, 0xffff_ffff);
  let plannedPaths: string[] | undefined;
  if (value.action === 'extract_archive') plannedPaths = parsePlannedPaths(value.plannedPaths);
  else if (value.plannedPaths !== undefined) return null;
  const hasInvalidInteger = [beforeMode, mode, requestedMode, beforeOwnerUid, ownerUid, requestedOwnerUid, beforeGroupGid, groupGid, requestedGroupGid].some(Number.isNaN);
  const hasMode = beforeMode !== undefined || mode !== undefined || requestedMode !== undefined;
  const hasOwner = beforeOwnerUid !== undefined || ownerUid !== undefined || requestedOwnerUid !== undefined
    || beforeGroupGid !== undefined || groupGid !== undefined || requestedGroupGid !== undefined;
  const command = value.command;
  const workingDir = value.workingDir;
  if (hasInvalidInteger
    || (value.action === 'edit_text' && (!isSafeRootRelativePath(value.sourcePath) || value.sourcePath !== value.targetPath || !isSupportedTextPath(value.sourcePath)))
    || (value.action === 'create_file' && (!isSafeRootRelativePath(value.targetPath) || !isSupportedTextPath(value.targetPath)
      || value.sourcePath !== value.targetPath.split('/').slice(0, -1).join('/')))
    || (value.action === 'overwrite_file' && (!isSafeRootRelativePath(value.sourcePath) || value.sourcePath !== value.targetPath || !isSupportedTextPath(value.sourcePath)))
    || (value.action === 'run_shell' && (!isShellCommand(command)
      || !isSafeRootRelativePath(value.sourcePath) && value.sourcePath !== ''
      || typeof workingDir !== 'string' || !isShellWorkingDirectory(workingDir) || value.targetPath !== ''))
    || (value.action !== 'run_shell' && (command !== undefined || workingDir !== undefined))
    || (value.action === 'set_permissions' && (beforeMode === undefined || mode === undefined || hasOwner))
    || (value.action === 'set_owner' && (hasMode || beforeOwnerUid === undefined || ownerUid === undefined
      || beforeGroupGid === undefined || groupGid === undefined))
    || (value.action === 'create_symlink' && (!isSafeRootRelativePath(value.sourcePath) || !isSafeRootRelativePath(value.targetPath)))
    || (value.action === 'create_archive' && (!isSafeRootRelativePath(value.sourcePath) || !isSafeRootRelativePath(value.targetPath)))
    || (value.action === 'extract_archive' && (!isSafeRootRelativePath(value.sourcePath) || value.targetPath !== '' && !isSafeRootRelativePath(value.targetPath)))
    || (value.action !== 'set_permissions' && value.action !== 'set_owner' && (hasMode || hasOwner))) {
    return null;
  }

  return {
    id: value.id,
    action: value.action,
    sourcePath: value.sourcePath,
    targetPath: value.targetPath,
    reason: value.reason,
    evidence: value.evidence,
    approval: value.approval,
    executionStatus: value.executionStatus,
    ...(beforeMode === undefined ? {} : { beforeMode }),
    ...(mode === undefined ? {} : { mode }),
    ...(requestedMode === undefined ? {} : { requestedMode }),
    ...(beforeOwnerUid === undefined ? {} : { beforeOwnerUid }),
    ...(ownerUid === undefined ? {} : { ownerUid }),
    ...(requestedOwnerUid === undefined ? {} : { requestedOwnerUid }),
    ...(beforeGroupGid === undefined ? {} : { beforeGroupGid }),
    ...(groupGid === undefined ? {} : { groupGid }),
    ...(requestedGroupGid === undefined ? {} : { requestedGroupGid }),
    ...(plannedPaths === undefined ? {} : { plannedPaths }),
    ...(typeof value.resultMessage === 'string' ? { resultMessage: value.resultMessage } : {}),
    ...(value.action === 'run_shell' ? { command: command as string, workingDir: workingDir as string } : {}),
  };
}

function parsePlan(value: unknown): AgentPlan {
  if (!isRecord(value)
    || typeof value.taskId !== 'string'
    || typeof value.planId !== 'string'
    || !Number.isSafeInteger(value.planVersion)
    || typeof value.status !== 'string'
    || typeof value.stopRequested !== 'boolean'
    || typeof value.proposalSource !== 'string'
    || !Array.isArray(value.operations)) {
    throw new AgentApiError('Agent 服务返回了无效的方案。');
  }
  const operations = value.operations.map(parseOperation);
  if (operations.some((operation) => operation === null)) {
    throw new AgentApiError('Agent 服务返回了无效的操作清单。');
  }
  if (operations.some((operation) => operation?.action === 'run_shell') && operations.length !== 1) {
    throw new AgentApiError('Shell 方案必须只包含一项父操作。');
  }
  return {
    taskId: value.taskId,
    planId: value.planId,
    planVersion: value.planVersion as number,
    status: value.status,
    stopRequested: value.stopRequested,
    proposalSource: value.proposalSource,
    operations: operations as AgentOperation[],
  };
}

async function readResponseError(response: Response, fallback: string): Promise<AgentApiError> {
  const payload = await response.json().catch(() => undefined) as unknown;
  const message = isRecord(payload) && typeof payload.error === 'string'
    ? payload.error
    : response.status >= 500 ? 'Agent 服务暂不可用。' : `${fallback}（HTTP ${response.status}）`;
  return new AgentApiError(message, response.status);
}

async function request(url: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetch(url, {
      ...init,
      headers: {
        Accept: 'application/json',
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...init?.headers,
      },
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new AgentApiError('无法连接当前 FileSage 服务。');
  }
}

async function readPlanResponse(response: Response, fallback: string): Promise<AgentPlan> {
  if (!response.ok) throw await readResponseError(response, fallback);
  const payload = await response.json().catch(() => undefined) as unknown;
  return parsePlan(payload);
}

// 校验 Shell 清单中的文件状态摘要。
function parseShellChangeState(value: unknown): AgentShellChangeState | null {
  if (value === null) return null;
  if (!isRecord(value)
    || typeof value.sha256 !== 'string'
    || value.sha256 !== '' && !/^[a-f\d]{64}$/i.test(value.sha256)
    || typeof value.mode !== 'number' || !Number.isInteger(value.mode) || value.mode < 0 || value.mode > 0o7777
    || typeof value.isDirectory !== 'boolean'
    || typeof value.size !== 'number' || !Number.isSafeInteger(value.size) || value.size < 0) {
    throw new AgentApiError('Agent 服务返回了无效的 Shell 文件状态。');
  }
  if ((value.isDirectory && (value.sha256 !== '' || value.size !== 0))
    || (!value.isDirectory && !/^[a-f\d]{64}$/i.test(value.sha256))) {
    throw new AgentApiError('Agent 服务返回的 Shell 文件摘要与类型不符。');
  }
  return { sha256: value.sha256, mode: value.mode, isDirectory: value.isDirectory, size: value.size };
}

// 校验变更路径、类型、状态和前后快照。
function parseShellChange(value: unknown): AgentShellChange {
  if (!isRecord(value)
    || typeof value.id !== 'string' || !value.id || value.id.length > 128
    || typeof value.path !== 'string' || !isSafeRootRelativePath(value.path)
    || (value.kind !== 'added' && value.kind !== 'modified' && value.kind !== 'deleted')
    || typeof value.approval !== 'string' || !value.approval || value.approval.length > 64
    || typeof value.status !== 'string' || !value.status || value.status.length > 64
    || typeof value.binary !== 'boolean'
    || typeof value.diffTruncated !== 'boolean') {
    throw new AgentApiError('Agent 服务返回了无效的 Shell 变更项。');
  }
  const before = parseShellChangeState(value.before);
  const after = parseShellChangeState(value.after);
  if ((value.kind === 'added' && (before !== null || after === null))
    || (value.kind === 'deleted' && (before === null || after !== null))
    || (value.kind === 'modified' && (before === null || after === null))) {
    throw new AgentApiError('Agent 服务返回的 Shell 变更状态与类型不符。');
  }
  return {
    id: value.id,
    path: value.path,
    kind: value.kind,
    approval: value.approval,
    status: value.status,
    binary: value.binary,
    diffTruncated: value.diffTruncated,
    before,
    after,
  };
}

// 校验分页边界与条目唯一性。
function parseAgentShellChangesPage(value: unknown): AgentShellChangesPage {
  if (!isRecord(value)
    || !Array.isArray(value.changes)
    || typeof value.offset !== 'number' || !Number.isSafeInteger(value.offset) || value.offset < 0
    || typeof value.limit !== 'number' || !Number.isInteger(value.limit) || value.limit < 1 || value.limit > 100
    || typeof value.total !== 'number' || !Number.isSafeInteger(value.total) || value.total < 0
    || typeof value.hasMore !== 'boolean' || value.changes.length > value.limit) {
    throw new AgentApiError('Agent 服务返回了无效的 Shell 变更清单。');
  }
  const changes = value.changes.map(parseShellChange);
  if (new Set(changes.map((change) => change.id)).size !== changes.length
    || new Set(changes.map((change) => change.path)).size !== changes.length
    || value.offset + changes.length > value.total
    || value.hasMore !== (value.offset + changes.length < value.total)) {
    throw new AgentApiError('Agent 服务返回了不一致的 Shell 分页清单。');
  }
  return { changes, offset: value.offset, limit: value.limit, total: value.total, hasMore: value.hasMore };
}

// 预览必须属于用户当前选中的文本变更。
function parseAgentShellChangePreview(value: unknown, expectedChangeId: string, expectedPath: string): AgentShellChangePreview {
  if (!isRecord(value) || typeof value.textDiff !== 'string' || value.textDiff.length > 64 * 1024) {
    throw new AgentApiError('Agent 服务返回了无效的 Shell 文本差异。');
  }
  const change = parseShellChange(value.change);
  if (change.id !== expectedChangeId || change.path !== expectedPath || change.binary) {
    throw new AgentApiError('Agent 服务返回了不匹配或不可预览的 Shell 变更。');
  }
  return { change, textDiff: value.textDiff };
}

// 历史列表复用任务索引；方案详情仍从 Agent 任务端点恢复。
export async function listAgentTaskHistory(signal?: AbortSignal): Promise<AgentTaskHistoryItem[]> {
  const page = await listTasks({ kind: 'agent' }, 30, 0, signal);
  if (!Array.isArray(page.tasks) || !page.tasks.every((task) => typeof task.id === 'string'
    && typeof task.scopePath === 'string' && typeof task.status === 'string'
    && typeof task.createdAt === 'string' && typeof task.updatedAt === 'string')) {
    throw new AgentApiError('任务服务返回了无效的 Agent 历史列表。');
  }
  return page.tasks.map(({ id, status, scopePath, createdAt, updatedAt }) => ({ id, status, scopePath, createdAt, updatedAt }));
}

export async function getAgentTask(taskId: string, signal?: AbortSignal): Promise<AgentPlan> {
  const response = await request(`/api/v1/agent/tasks/${encodeURIComponent(taskId)}`, { signal });
  const plan = await readPlanResponse(response, '读取整理任务失败');
  if (plan.taskId !== taskId) throw new AgentApiError('Agent 服务返回了不匹配的任务方案。');
  return plan;
}

export async function getAgentPlan(planId: string, signal?: AbortSignal): Promise<AgentPlan> {
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}`, { signal });
  const plan = await readPlanResponse(response, '刷新方案失败');
  if (plan.planId !== planId) throw new AgentApiError('Agent 服务返回了不匹配的方案。');
  return plan;
}

// 仅在用户打开预览时读取正文，并绕过浏览器缓存。
export async function getAgentOperationPreview(
  planId: string,
  operationId: string,
  expectedPlanVersion: number,
  expectedAction: AgentTextCandidateAction,
  expectedPath: string,
  signal?: AbortSignal,
): Promise<AgentOperationPreview> {
  if (!Number.isSafeInteger(expectedPlanVersion) || expectedPlanVersion < 1 || !isSafeRootRelativePath(expectedPath)
    || (expectedAction !== 'edit_text' && expectedAction !== 'create_file' && expectedAction !== 'overwrite_file')
    || !isSupportedTextPath(expectedPath)) {
    throw new AgentApiError('文本候选预览的方案版本或文件路径无效。');
  }
  const response = await request(
    `/api/v1/agent/plans/${encodeURIComponent(planId)}/operations/${encodeURIComponent(operationId)}/preview?expectedPlanVersion=${encodeURIComponent(expectedPlanVersion)}`,
    { signal, cache: 'no-store' },
  );
  if (!response.ok) throw await readResponseError(response, '读取修改预览失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  if (!isRecord(payload)
    || payload.planId !== planId
    || payload.operationId !== operationId
    || payload.path !== expectedPath
    || typeof payload.sourceContent !== 'string'
    || typeof payload.proposedContent !== 'string'
    || expectedAction === 'create_file' && payload.sourceContent !== '') {
    throw new AgentApiError('Agent 服务返回了无效或不匹配的修改预览。');
  }
  return {
    planId,
    operationId,
    path: expectedPath,
    sourceContent: payload.sourceContent,
    proposedContent: payload.proposedContent,
  };
}

// Shell 清单与正文预览每次都从服务端读取，避免展示过期内容。
export async function getAgentShellChanges(
  planId: string,
  offset = 0,
  limit = 50,
  signal?: AbortSignal,
): Promise<AgentShellChangesPage> {
  if (!Number.isSafeInteger(offset) || offset < 0 || !Number.isInteger(limit) || limit < 1 || limit > 100) {
    throw new AgentApiError('Shell 变更清单分页参数无效。');
  }
  const query = new URLSearchParams({ offset: String(offset), limit: String(limit) });
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}/shell-changes?${query}`, {
    signal,
    cache: 'no-store',
  });
  if (!response.ok) throw await readResponseError(response, '读取 Shell 变更清单失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  return parseAgentShellChangesPage(payload);
}

// 只有用户打开某项预览时才读取文本差异。
export async function getAgentShellChangePreview(
  planId: string,
  changeId: string,
  expectedPath: string,
  signal?: AbortSignal,
): Promise<AgentShellChangePreview> {
  const response = await request(
    `/api/v1/agent/plans/${encodeURIComponent(planId)}/shell-changes/${encodeURIComponent(changeId)}/preview`,
    { signal, cache: 'no-store' },
  );
  if (!response.ok) throw await readResponseError(response, '读取 Shell 文本差异失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  return parseAgentShellChangePreview(payload, changeId, expectedPath);
}

export async function setAgentShellChangeApproval(
  planId: string,
  changeId: string,
  decision: 'approve' | 'reject',
  expectedPlanVersion: number,
): Promise<AgentPlan> {
  const response = await request(
    `/api/v1/agent/plans/${encodeURIComponent(planId)}/shell-changes/${encodeURIComponent(changeId)}/approval`,
    {
      method: 'POST',
      body: JSON.stringify({ decision, expectedPlanVersion }),
    },
  );
  const plan = await readPlanResponse(response, '提交 Shell 变更审批失败');
  if (plan.planId !== planId) throw new AgentApiError('Agent 服务返回了不匹配的 Shell 方案。');
  return plan;
}

// 保存候选正文后要求服务端返回新版本与新操作 ID。
export async function reviseAgentOperationContent(
  planId: string,
  taskId: string,
  operationId: string,
  expectedPlanVersion: number,
  expectedAction: AgentTextCandidateAction,
  expectedPath: string,
  proposedContent: string,
): Promise<AgentPlan> {
  if (!Number.isSafeInteger(expectedPlanVersion) || expectedPlanVersion < 1
    || (expectedAction !== 'edit_text' && expectedAction !== 'create_file' && expectedAction !== 'overwrite_file')
    || !isSafeRootRelativePath(expectedPath) || !isSupportedTextPath(expectedPath)
    || new TextEncoder().encode(proposedContent).byteLength > 32 * 1024 || proposedContent.includes('\0')) {
    throw new AgentApiError('文本候选的方案版本、文件路径或正文无效。');
  }
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}/operations/${encodeURIComponent(operationId)}/content`, {
    method: 'PUT',
    body: JSON.stringify({ expectedPlanVersion, proposedContent }),
  });
  const plan = await readPlanResponse(response, '保存修改候选失败');
  const revisedOperation = plan.operations.find((operation) => operation.action === expectedAction
    && operation.id !== operationId && textOperationPath(operation.action, operation.sourcePath, operation.targetPath) === expectedPath);
  if (plan.planId !== planId
    || plan.taskId !== taskId
    || plan.planVersion !== expectedPlanVersion + 1
    || plan.status !== 'awaiting_approval'
    || revisedOperation?.approval !== 'pending'
    || revisedOperation.executionStatus !== 'pending') {
    throw new AgentApiError('Agent 服务未返回更新后的修改方案。');
  }
  return plan;
}

// 保存用户修改的操作清单；服务端会重新校验全部文件状态。
export async function reviseAgentPlan(
  planId: string,
  expectedPlanVersion: number,
  operations: AgentPlanEditOperation[],
): Promise<AgentPlan> {
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}/revision`, {
    method: 'PUT',
    body: JSON.stringify({ expectedPlanVersion, operations }),
  });
  return readPlanResponse(response, '保存修改失败');
}

export async function setAgentOperationApproval(
  planId: string,
  operationId: string,
  decision: 'approve' | 'reject',
  expectedPlanVersion: number,
): Promise<AgentPlan> {
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}/operations/${encodeURIComponent(operationId)}/approval`, {
    method: 'POST',
    body: JSON.stringify({ decision, expectedPlanVersion }),
  });
  return readPlanResponse(response, '提交审批失败');
}

// 执行绑定当前方案版本，并直接返回最新结果。
export async function executeAgentPlan(planId: string, expectedPlanVersion: number): Promise<AgentPlan> {
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}/execute`, {
    method: 'POST',
    body: JSON.stringify({ expectedPlanVersion }),
  });
  return readPlanResponse(response, '执行方案失败');
}

// 停止只取消当前操作之后尚未开始的项目。
export async function requestAgentPlanStop(planId: string, expectedPlanVersion: number): Promise<AgentPlan> {
  const response = await request(`/api/v1/agent/plans/${encodeURIComponent(planId)}/stop`, {
    method: 'POST',
    body: JSON.stringify({ expectedPlanVersion }),
  });
  return readPlanResponse(response, '停止后续操作失败');
}
