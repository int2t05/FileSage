// 本文件解析 Agent 对话、确认卡和模型状态接口。
export type AgentConversationState =
  | 'clarifying'
  | 'awaiting_confirmation'
  | 'unsupported'
  | 'failed'
  | 'processing'
  | 'analyzing'
  | 'awaiting_approval'
  | 'analysis_complete';

export type AgentDestination = 'local' | 'remote';

export interface AgentClassificationFile {
  path: string;
  sizeBytes: number;
}

export interface AgentClassificationCategory {
  id: number;
  path: string;
}

export interface AgentAnalysisOperation {
  action: 'rename' | 'edit_text' | 'create_file' | 'overwrite_file' | 'create_directory' | 'move_file' | 'copy_file' | 'copy_directory' | 'create_symlink' | 'create_archive' | 'extract_archive' | 'set_permissions' | 'set_owner' | 'archive_photos' | 'classify_documents' | 'trash' | 'permanent_delete' | 'run_shell';
  filePath: string;
  targetPath: string;
  shellCommand?: string;
  workingDir?: string;
  enableOCR?: boolean;
  requestedMode?: number;
  requestedOwnerUid?: number;
  requestedGroupGid?: number;
  files?: AgentClassificationFile[];
  categories?: AgentClassificationCategory[];
}

export interface AgentModelStatus {
  available: boolean;
  provider?: string;
  model?: string;
  destination?: AgentDestination;
  reason?: 'not_configured' | 'incomplete_configuration' | 'invalid_configuration';
}

export interface AgentConversationMessage {
  role: 'user' | 'assistant';
  content: string;
}

export interface AgentAnalysisCard {
  goal: string;
  operations?: AgentAnalysisOperation[];
  // 兼容旧版单项确认卡字段。
  action?: AgentAnalysisOperation['action'] | 'find_duplicates';
  filePath?: string;
  targetPath?: string;
  shellCommand?: string;
  workingDir?: string;
  requestedMode?: number;
  requestedOwnerUid?: number;
  requestedGroupGid?: number;
  dataTypes: string[];
  provider: string;
  model: string;
  destination: AgentDestination;
}

export interface AgentConversation {
  id: string;
  state: AgentConversationState;
  messages: AgentConversationMessage[];
  analysisCard: AgentAnalysisCard | null;
  taskId: string;
  planId: string;
  duplicateScanId: string;
  error: string;
}

export interface AgentConversationSummary {
  id: string;
  state: AgentConversationState;
  title: string;
  updatedAt: string;
  taskId?: string;
  taskStatus?: string;
  planId?: string;
}

export interface AgentConversationPage {
  conversations: AgentConversationSummary[];
  limit: number;
  offset: number;
  hasMore: boolean;
}

export class AgentConversationApiError extends Error {
  readonly status?: number;

  constructor(message: string, status?: number) {
    super(message);
    this.name = 'AgentConversationApiError';
    this.status = status;
  }
}

const conversationStates = new Set<AgentConversationState>([
  'clarifying', 'awaiting_confirmation', 'unsupported', 'failed', 'processing', 'analyzing', 'awaiting_approval', 'analysis_complete',
]);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isDestination(value: unknown): value is AgentDestination {
  return value === 'local' || value === 'remote';
}

function optionalInteger(value: unknown, max: number, field: string): number | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0 || value > max) {
    throw new AgentConversationApiError(`Agent 服务返回了无效的 ${field}。`);
  }
  return value;
}

function optionalString(value: unknown, field: string): string {
  if (value === undefined || value === null) return '';
  if (typeof value !== 'string') throw new AgentConversationApiError(`Agent 服务返回了无效的 ${field}。`);
  return value;
}

function isSafeRelativePath(value: string, allowRoot: boolean): boolean {
  if (value === '') return allowRoot;
  if (value.length > 4096 || value.startsWith('/') || value.includes('\\') || value.includes('\0')
    || /^[A-Za-z]:/.test(value)) return false;
  return value.split('/').every((part) => part !== '' && part !== '.' && part !== '..'
    && part.length <= 255 && !/[\u0000-\u001f\u007f]/.test(part));
}

function isShellWorkingDirectory(value: string): boolean {
  return value === '.' || isSafeRelativePath(value, true);
}

function isWithinPath(scopePath: string, value: string): boolean {
  return scopePath === '' || value.startsWith(`${scopePath}/`);
}

function isTextFilePath(value: string): boolean {
  return /\.(txt|md|markdown|mdown)$/i.test(value);
}

// 匹配服务端 Shell 命令字节上限并拒绝空命令。
function isShellCommand(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
    && new TextEncoder().encode(value).byteLength <= 64 * 1024 && !value.includes('\0');
}

function classificationFileLimit(filePath: string): number {
  const extension = filePath.slice(filePath.lastIndexOf('.')).toLowerCase();
  if (['.txt', '.md', '.markdown', '.mdown'].includes(extension)) return 64 * 1024;
  if (['.pdf', '.docx', '.pptx'].includes(extension)) return 16 * 1024 * 1024;
  return 0;
}

function parseClassificationFiles(value: unknown, scopePath: string): AgentClassificationFile[] {
  if (!Array.isArray(value) || value.length < 1 || value.length > 20) {
    throw new AgentConversationApiError('Agent 服务返回了无效的分类文件清单。');
  }
  const files = value.map((item): AgentClassificationFile => {
    if (!isRecord(item)
      || typeof item.path !== 'string'
      || !isSafeRelativePath(item.path, false)
      || !isWithinPath(scopePath, item.path)
      || typeof item.sizeBytes !== 'number'
      || !Number.isSafeInteger(item.sizeBytes)
      || item.sizeBytes < 0
      || classificationFileLimit(item.path) === 0
      || item.sizeBytes > classificationFileLimit(item.path)) {
      throw new AgentConversationApiError('Agent 服务返回了无效的分类文件路径或大小。');
    }
    return { path: item.path, sizeBytes: item.sizeBytes };
  });
  if (new Set(files.map((file) => file.path)).size !== files.length) {
    throw new AgentConversationApiError('Agent 服务返回了重复的分类文件路径。');
  }
  return files;
}

function parseClassificationCategories(value: unknown, targetPath: string): AgentClassificationCategory[] {
  if (!Array.isArray(value) || value.length < 1) {
    throw new AgentConversationApiError('Agent 服务未返回可用的分类目录。');
  }
  const categories = value.map((item): AgentClassificationCategory => {
    if (!isRecord(item)
      || typeof item.id !== 'number'
      || !Number.isInteger(item.id)
      || item.id < 0
      || item.id > 49
      || typeof item.path !== 'string'
      || !isSafeRelativePath(item.path, false)
      || !item.path.startsWith(`${targetPath}/`)) {
      throw new AgentConversationApiError('Agent 服务返回了无效的分类目录 ID 或路径。');
    }
    return { id: item.id, path: item.path };
  });
  if (new Set(categories.map((category) => category.id)).size !== categories.length
    || new Set(categories.map((category) => category.path)).size !== categories.length) {
    throw new AgentConversationApiError('Agent 服务返回了重复的分类目录。');
  }
  return categories;
}

export function parseAgentModelStatus(value: unknown): AgentModelStatus {
  if (!isRecord(value) || typeof value.available !== 'boolean') {
    throw new AgentConversationApiError('Agent 服务返回了无效的模型状态。');
  }

  if (!value.available) {
    const reasons = new Set(['not_configured', 'incomplete_configuration', 'invalid_configuration']);
    if (typeof value.reason !== 'string' || !reasons.has(value.reason)) {
      throw new AgentConversationApiError('Agent 服务未说明模型不可用原因。');
    }
    return { available: false, reason: value.reason as AgentModelStatus['reason'] };
  }

  if (typeof value.provider !== 'string' || !value.provider.trim()
    || typeof value.model !== 'string' || !value.model.trim()
    || !isDestination(value.destination)) {
    throw new AgentConversationApiError('Agent 服务返回了不完整的模型信息。');
  }

  return {
    available: true,
    provider: value.provider,
    model: value.model,
    destination: value.destination,
  };
}

function parseAnalysisOperation(value: unknown): AgentAnalysisOperation {
  if (!isRecord(value)) throw new AgentConversationApiError('Agent 服务返回了无效的分析操作。');
  const action = value.action;
  const targetPath = (action === 'edit_text' || action === 'create_file' || action === 'overwrite_file' || action === 'trash' || action === 'permanent_delete' || action === 'extract_archive' || action === 'run_shell') && value.targetPath === undefined ? '' : value.targetPath;
  if ((action !== 'rename' && action !== 'edit_text' && action !== 'create_directory' && action !== 'move_file' && action !== 'copy_file' && action !== 'copy_directory' && action !== 'create_symlink'
    && action !== 'create_file' && action !== 'overwrite_file'
    && action !== 'create_archive' && action !== 'extract_archive'
    && action !== 'archive_photos' && action !== 'classify_documents' && action !== 'trash' && action !== 'permanent_delete'
    && action !== 'set_permissions' && action !== 'set_owner' && action !== 'run_shell')
    || typeof value.filePath !== 'string'
    || typeof targetPath !== 'string') {
    throw new AgentConversationApiError('Agent 服务返回了无效的分析操作。');
  }

  const requestedMode = optionalInteger(value.requestedMode, 511, '目标权限');
  const requestedOwnerUid = optionalInteger(value.requestedOwnerUid, 0xffff_ffff, '目标用户 ID');
  const requestedGroupGid = optionalInteger(value.requestedGroupGid, 0xffff_ffff, '目标用户组 ID');
  const enableOCR = value.enableOCR === undefined ? false : value.enableOCR;
  if ((action === 'run_shell'
      && (!isSafeRelativePath(value.filePath, true)
        || typeof value.workingDir !== 'string' || !isShellWorkingDirectory(value.workingDir)
        || !isShellCommand(value.shellCommand)
        || targetPath !== ''
        || requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))
    || (action !== 'run_shell' && (value.shellCommand !== undefined || value.workingDir !== undefined))
    || (action === 'classify_documents'
      && (!isSafeRelativePath(value.filePath, true)
        || !isSafeRelativePath(targetPath, false)
        || requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined
        || typeof enableOCR !== 'boolean'
        || value.files === undefined || value.categories === undefined))
    || (action !== 'classify_documents' && (value.files !== undefined || value.categories !== undefined || value.enableOCR !== undefined))
    || (action === 'set_permissions' && (requestedMode === undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))
    || (action === 'set_owner' && (requestedMode !== undefined || (requestedOwnerUid === undefined && requestedGroupGid === undefined)))
    || ((action === 'trash' || action === 'permanent_delete') && (!isSafeRelativePath(value.filePath as string, false) || targetPath !== '' || requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))
    || (action === 'create_symlink' && (!isSafeRelativePath(value.filePath as string, false) || !isSafeRelativePath(targetPath, false)))
    || (action === 'create_archive' && (!isSafeRelativePath(value.filePath as string, false) || !isSafeRelativePath(targetPath, false) || value.filePath === targetPath
      || requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))
    || ((action === 'create_file' || action === 'overwrite_file')
      && (!isSafeRelativePath(value.filePath as string, false) || !isTextFilePath(value.filePath as string) || targetPath !== ''
        || requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))
    || (action === 'extract_archive' && (!isSafeRelativePath(value.filePath as string, false) || !isSafeRelativePath(targetPath, true) || value.filePath === targetPath
      || requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))
    || ((action === 'rename' || action === 'edit_text' || action === 'create_directory' || action === 'move_file' || action === 'copy_file' || action === 'copy_directory' || action === 'create_symlink' || action === 'create_archive' || action === 'extract_archive' || action === 'archive_photos' || action === 'classify_documents')
      && (requestedMode !== undefined || requestedOwnerUid !== undefined || requestedGroupGid !== undefined))) {
    throw new AgentConversationApiError('Agent 服务返回了不完整的元数据确认信息。');
  }

  if (action === 'classify_documents') {
    return {
      action,
      filePath: value.filePath as string,
      targetPath,
      files: parseClassificationFiles(value.files, value.filePath as string),
      categories: parseClassificationCategories(value.categories, targetPath),
      ...(enableOCR ? { enableOCR: true } : {}),
    };
  }

  if (action === 'run_shell') {
    return {
      action,
      filePath: value.filePath as string,
      targetPath,
      shellCommand: value.shellCommand as string,
      workingDir: value.workingDir as string,
    };
  }

  return {
    action,
    filePath: value.filePath,
    targetPath,
    ...(requestedMode === undefined ? {} : { requestedMode }),
    ...(requestedOwnerUid === undefined ? {} : { requestedOwnerUid }),
    ...(requestedGroupGid === undefined ? {} : { requestedGroupGid }),
  };
}

// 保留特殊单项卡字段，避免降级为通用操作清单。
function parseAnalysisCard(value: unknown): AgentAnalysisCard | null {
  if (value === undefined || value === null) return null;
  if (!isRecord(value)) throw new AgentConversationApiError('Agent 服务返回了无效的分析确认卡。');
  if (typeof value.goal !== 'string'
    || !Array.isArray(value.dataTypes)
    || !value.dataTypes.every((item) => typeof item === 'string')
    || typeof value.provider !== 'string'
    || typeof value.model !== 'string'
    || !isDestination(value.destination)) {
    throw new AgentConversationApiError('Agent 服务返回了无效的分析确认卡。');
  }
  if (value.action !== 'classify_documents' && (value.files !== undefined || value.categories !== undefined)) {
    throw new AgentConversationApiError('分类文件与类别数据只能用于正文分类确认卡。');
  }

  const common = {
    goal: value.goal,
    dataTypes: value.dataTypes,
    provider: value.provider,
    model: value.model,
    destination: value.destination,
  };
  const hasShellOperation = Array.isArray(value.operations)
    && value.operations.some((operation) => isRecord(operation) && operation.action === 'run_shell');
  const shellOperation = hasShellOperation && Array.isArray(value.operations) ? value.operations[0] : undefined;
  if (value.action === 'run_shell' || hasShellOperation) {
    if (value.action !== undefined && value.action !== 'run_shell') {
      throw new AgentConversationApiError('Shell 操作不能与其他分析动作混合确认。');
    }
    if (value.operations !== undefined && (!Array.isArray(value.operations) || value.operations.length !== 1)) {
      throw new AgentConversationApiError('Agent 服务返回了无效的 Shell 操作。');
    }
    const operation = shellOperation ?? value;
    const filePath = value.filePath ?? operation.filePath;
    const shellCommand = value.shellCommand ?? operation.shellCommand;
    const workingDir = value.workingDir ?? operation.workingDir;
    if (typeof filePath !== 'string' || !isSafeRelativePath(filePath, true)
      || !isShellCommand(shellCommand) || typeof workingDir !== 'string' || !isShellWorkingDirectory(workingDir)
      || value.targetPath !== undefined && value.targetPath !== ''
      || value.files !== undefined || value.categories !== undefined
      || value.requestedMode != null || value.requestedOwnerUid != null || value.requestedGroupGid != null) {
      throw new AgentConversationApiError('Agent 服务返回了无效的 Shell 命令或范围。');
    }
    if (value.operations !== undefined) {
      const parsed = parseAnalysisOperation(operation);
      if (parsed.action !== 'run_shell' || parsed.filePath !== filePath
        || parsed.shellCommand !== shellCommand || parsed.workingDir !== workingDir) {
        throw new AgentConversationApiError('Agent 服务返回的 Shell 操作与确认范围不一致。');
      }
    }
    return {
      ...common,
      action: 'run_shell',
      filePath,
      targetPath: '',
      shellCommand,
      workingDir,
    };
  }
  if (value.action === 'find_duplicates') {
    if (typeof value.filePath !== 'string' || typeof value.targetPath !== 'string' || value.targetPath !== '') {
      throw new AgentConversationApiError('Agent 服务返回了无效的重复文件扫描范围。');
    }
    if (value.operations !== undefined) {
      if (!Array.isArray(value.operations) || value.operations.length !== 1) {
        throw new AgentConversationApiError('Agent 服务返回了无效的重复文件扫描操作。');
      }
      const operation = value.operations[0];
      if (!isRecord(operation) || operation.action !== 'find_duplicates'
        || operation.filePath !== value.filePath || operation.targetPath !== '') {
        throw new AgentConversationApiError('Agent 服务返回了无效的重复文件扫描操作。');
      }
    }
    return {
      ...common,
      action: 'find_duplicates',
      filePath: value.filePath,
      targetPath: '',
    };
  }

  // 照片归档虽由单项操作返回，确认卡仍保留专用动作与范围字段。
  if (value.action === 'archive_photos') {
    if (typeof value.filePath !== 'string' || typeof value.targetPath !== 'string' || !value.targetPath) {
      throw new AgentConversationApiError('Agent 服务返回了无效的照片归档范围。');
    }
    if (value.operations !== undefined) {
      if (!Array.isArray(value.operations) || value.operations.length !== 1) {
        throw new AgentConversationApiError('Agent 服务返回了无效的照片归档操作。');
      }
      const operation = value.operations[0];
      if (!isRecord(operation) || operation.action !== 'archive_photos'
        || operation.filePath !== value.filePath || operation.targetPath !== value.targetPath) {
        throw new AgentConversationApiError('Agent 服务返回了无效的照片归档操作。');
      }
    }
    return {
      ...common,
      action: 'archive_photos',
      filePath: value.filePath,
      targetPath: value.targetPath,
    };
  }

  if (value.action === 'create_file' || value.action === 'overwrite_file') {
    if (typeof value.filePath !== 'string' || !isSafeRelativePath(value.filePath, false) || !isTextFilePath(value.filePath)
      || value.targetPath !== '' || value.requestedMode != null || value.requestedOwnerUid != null || value.requestedGroupGid != null
      || value.shellCommand !== undefined || value.workingDir !== undefined || value.files !== undefined || value.categories !== undefined) {
      throw new AgentConversationApiError('Agent 服务返回了无效的文本文件目标。');
    }
    let operations: AgentAnalysisOperation[] | undefined;
    if (value.operations !== undefined) {
      if (!Array.isArray(value.operations) || value.operations.length !== 1) {
        throw new AgentConversationApiError('新建或覆盖文本文件必须是单项操作。');
      }
      const operation = parseAnalysisOperation(value.operations[0]);
      if (operation.action !== value.action || operation.filePath !== value.filePath || operation.targetPath !== '') {
        throw new AgentConversationApiError('文本文件操作与确认路径不一致。');
      }
      operations = [operation];
    }
    return {
      ...common,
      action: value.action,
      filePath: value.filePath,
      targetPath: '',
      ...(operations ? { operations } : {}),
    };
  }

  if (value.action === 'create_symlink') {
    if (typeof value.filePath !== 'string'
      || !isSafeRelativePath(value.filePath, false)
      || typeof value.targetPath !== 'string'
      || !isSafeRelativePath(value.targetPath, false)
      || value.files !== undefined
      || value.categories !== undefined
      || value.requestedMode != null
      || value.requestedOwnerUid != null
      || value.requestedGroupGid != null) {
      throw new AgentConversationApiError('Agent 服务返回了无效的软链接路径。');
    }
    if (value.operations !== undefined) {
      if (!Array.isArray(value.operations) || value.operations.length !== 1) {
        throw new AgentConversationApiError('Agent 服务返回了无效的软链接操作。');
      }
      const operation = parseAnalysisOperation(value.operations[0]);
      if (operation.action !== 'create_symlink'
        || operation.filePath !== value.filePath
        || operation.targetPath !== value.targetPath) {
        throw new AgentConversationApiError('Agent 服务返回的软链接操作与确认路径不一致。');
      }
      return { ...common, action: 'create_symlink', filePath: value.filePath, targetPath: value.targetPath, operations: [operation] };
    }
    return {
      ...common,
      ...parseAnalysisOperation({ action: value.action, filePath: value.filePath, targetPath: value.targetPath }),
    };
  }

  if (value.action === 'classify_documents') {
    if (typeof value.filePath !== 'string'
      || !isSafeRelativePath(value.filePath, true)
      || typeof value.targetPath !== 'string'
      || !isSafeRelativePath(value.targetPath, false)
      || !Array.isArray(value.operations)
      || value.operations.length !== 1
      || value.files !== undefined
      || value.categories !== undefined
      || value.requestedMode != null
      || value.requestedOwnerUid != null
      || value.requestedGroupGid != null) {
      throw new AgentConversationApiError('Agent 服务返回了无效的正文分类范围。');
    }
    const operation = parseAnalysisOperation(value.operations[0]);
    if (operation.action !== 'classify_documents'
      || operation.filePath !== value.filePath
      || operation.targetPath !== value.targetPath) {
      throw new AgentConversationApiError('Agent 服务返回的分类操作与确认范围不一致。');
    }
    return {
      ...common,
      action: 'classify_documents',
      filePath: value.filePath,
      targetPath: value.targetPath,
      operations: [operation],
    };
  }

  if (value.operations !== undefined) {
    if (!Array.isArray(value.operations) || value.operations.length === 0) {
      throw new AgentConversationApiError('Agent 服务返回了无效的分析操作清单。');
    }
    if (value.operations.some((operation) => isRecord(operation) && operation.action === 'classify_documents')) {
      throw new AgentConversationApiError('正文分类不能与其他操作混合确认。');
    }
    return { ...common, operations: value.operations.map(parseAnalysisOperation) };
  }

  const targetPath = (value.action === 'edit_text' || value.action === 'trash' || value.action === 'permanent_delete') && value.targetPath === undefined ? '' : value.targetPath;
  if (typeof value.filePath !== 'string' || typeof targetPath !== 'string') {
    throw new AgentConversationApiError('Agent 服务返回了无效的分析确认卡。');
  }
  const operation = parseAnalysisOperation({
    action: value.action,
    filePath: value.filePath,
    targetPath,
    requestedMode: value.requestedMode,
    requestedOwnerUid: value.requestedOwnerUid,
    requestedGroupGid: value.requestedGroupGid,
  });
  return {
    ...common,
    ...operation,
  };
}

export function parseAgentConversation(value: unknown): AgentConversation {
  if (!isRecord(value)
    || typeof value.id !== 'string'
    || !value.id
    || typeof value.state !== 'string'
    || !conversationStates.has(value.state as AgentConversationState)
    || !Array.isArray(value.messages)) {
    throw new AgentConversationApiError('Agent 服务返回了无效的会话。');
  }

  const messages = value.messages.map((message): AgentConversationMessage => {
    if (!isRecord(message)
      || (message.role !== 'user' && message.role !== 'assistant')
      || typeof message.content !== 'string') {
      throw new AgentConversationApiError('Agent 服务返回了无效的对话消息。');
    }
    return { role: message.role, content: message.content };
  });

  return {
    id: value.id,
    state: value.state as AgentConversationState,
    messages,
    analysisCard: parseAnalysisCard(value.analysisCard),
    taskId: optionalString(value.taskId, '任务 ID'),
    planId: optionalString(value.planId, '方案 ID'),
    duplicateScanId: optionalString(value.duplicateScanId, '重复文件扫描 ID'),
    error: optionalString(value.error, '错误信息'),
  };
}

export function parseAgentConversationPage(value: unknown): AgentConversationPage {
  if (!isRecord(value)
    || !Array.isArray(value.conversations)
    || typeof value.limit !== 'number'
    || !Number.isInteger(value.limit)
    || value.limit < 1
    || value.limit > 100
    || typeof value.offset !== 'number'
    || !Number.isInteger(value.offset)
    || value.offset < 0
    || typeof value.hasMore !== 'boolean') {
    throw new AgentConversationApiError('Agent 服务返回了无效的会话历史。');
  }

  const conversations = value.conversations.map((item): AgentConversationSummary => {
    if (!isRecord(item)
      || typeof item.id !== 'string'
      || !item.id
      || typeof item.state !== 'string'
      || !conversationStates.has(item.state as AgentConversationState)
      || typeof item.title !== 'string'
      || typeof item.updatedAt !== 'string') {
      throw new AgentConversationApiError('Agent 服务返回了无效的会话摘要。');
    }
    return {
      id: item.id,
      state: item.state as AgentConversationState,
      title: item.title,
      updatedAt: item.updatedAt,
      ...(item.taskId === undefined ? {} : { taskId: optionalString(item.taskId, '任务 ID') }),
      ...(item.taskStatus === undefined ? {} : { taskStatus: optionalString(item.taskStatus, '任务状态') }),
      ...(item.planId === undefined ? {} : { planId: optionalString(item.planId, '方案 ID') }),
    };
  });

  return {
    conversations,
    limit: value.limit,
    offset: value.offset,
    hasMore: value.hasMore,
  };
}

export function canSendAgentMessage(state: AgentConversationState | null, planStatus?: string, messageCount = 0, hasLinkedPlan = false): boolean {
  const terminalPlan = ['completed', 'failed', 'invalidated', 'rejected', 'cancelled'].includes(planStatus ?? '');
  const canContinue = hasLinkedPlan
    ? terminalPlan && ['awaiting_approval', 'analysis_complete', 'failed'].includes(state ?? '')
    : state === null || state === 'clarifying' || state === 'awaiting_confirmation'
      || state === 'unsupported' || state === 'failed' || state === 'analysis_complete';
  if (!canContinue) return false;
  return messageCount + (hasLinkedPlan ? 4 : 3) <= 100;
}

export function canConfirmAgentAnalysis(conversation: AgentConversation | null): boolean {
  return conversation?.state === 'awaiting_confirmation' && conversation.analysisCard !== null;
}

async function readResponseError(response: Response, fallback: string): Promise<AgentConversationApiError> {
  const message = response.status === 400 ? '请求无效，请检查消息后重试。'
    : response.status === 404 ? '会话不存在，请刷新历史记录。'
      : response.status === 409 ? '会话状态已变化，请刷新后重新核对。'
      : response.status === 413 ? '消息、文件正文或本会话记录已达上限。'
          : response.status === 422 ? '当前任务或文件格式暂不支持。'
            : response.status === 502 ? '模型请求失败；对话草稿已保留，请稍后重试。'
              : response.status === 503 ? '模型尚未配置或当前不可用。'
                : response.status >= 500 ? 'Agent 服务暂不可用。' : `${fallback}（HTTP ${response.status}）`;
  return new AgentConversationApiError(message, response.status);
}

async function requestConversation(url: string, init?: RequestInit, expectedStatus = 200): Promise<AgentConversation> {
  let response: Response;
  try {
    response = await fetch(url, {
      ...init,
      headers: {
        Accept: 'application/json',
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...init?.headers,
      },
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new AgentConversationApiError('无法连接当前 FileSage 服务。');
  }

  if (!response.ok) throw await readResponseError(response, 'Agent 请求失败');
  if (response.status !== expectedStatus) {
    throw new AgentConversationApiError(`Agent 服务返回了意外状态（HTTP ${response.status}）。`, response.status);
  }
  const payload = await response.json().catch(() => undefined) as unknown;
  return parseAgentConversation(payload);
}

export async function listAgentConversations(
  limit = 20,
  offset = 0,
  signal?: AbortSignal,
): Promise<AgentConversationPage> {
  const safeLimit = Math.min(100, Math.max(1, Math.trunc(limit)));
  const safeOffset = Math.max(0, Math.trunc(offset));
  let response: Response;
  try {
    const query = new URLSearchParams({ limit: String(safeLimit), offset: String(safeOffset) });
    response = await fetch(`/api/v1/agent/conversations?${query}`, {
      headers: { Accept: 'application/json' },
      signal,
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new AgentConversationApiError('无法连接当前 FileSage 服务。');
  }
  if (!response.ok) throw await readResponseError(response, '读取会话历史失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  return parseAgentConversationPage(payload);
}

export async function getAgentModelStatus(signal?: AbortSignal): Promise<AgentModelStatus> {
  let response: Response;
  try {
    response = await fetch('/api/v1/agent/status', { headers: { Accept: 'application/json' }, signal });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new AgentConversationApiError('无法连接当前 FileSage 服务。');
  }
  if (!response.ok) throw await readResponseError(response, '读取模型状态失败');
  const payload = await response.json().catch(() => undefined) as unknown;
  return parseAgentModelStatus(payload);
}

export function createAgentConversation(message: string): Promise<AgentConversation> {
  return requestConversation('/api/v1/agent/conversations', {
    method: 'POST',
    body: JSON.stringify({ message }),
  }, 201);
}

export function getAgentConversation(id: string, signal?: AbortSignal): Promise<AgentConversation> {
  return requestConversation(`/api/v1/agent/conversations/${encodeURIComponent(id)}`, { signal });
}

export function sendAgentConversationMessage(id: string, message: string): Promise<AgentConversation> {
  return requestConversation(`/api/v1/agent/conversations/${encodeURIComponent(id)}/messages`, {
    method: 'POST',
    body: JSON.stringify({ message }),
  });
}

export function confirmAgentAnalysis(id: string, enableOCR = false): Promise<AgentConversation> {
  return requestConversation(`/api/v1/agent/conversations/${encodeURIComponent(id)}/confirm-analysis`, {
    method: 'POST',
    body: JSON.stringify({ enableOCR }),
  });
}
