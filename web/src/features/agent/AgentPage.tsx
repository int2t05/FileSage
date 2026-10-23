// Agent 页面：通过对话确认任务，再审核并执行文件方案。
import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import {
  AlertTriangle,
  ArrowRight,
  Check,
  Loader2,
  Plus,
  RefreshCw,
  FolderOpen,
  ShieldCheck,
} from 'lucide-react';
import {
  AgentApiError,
  executeAgentPlan,
  executeAgentStep,
  getAgentPlan,
  getAgentTask,
  listAgentTaskHistory,
  requestAgentPlanStop,
  rejectAgentPlan,
  reviseAgentOperationContent,
  reviseAgentPlan,
  startAgentPlan,
  type AgentPlan,
  type AgentPlanEditOperation,
  type AgentTextCandidateAction,
  type AgentTaskHistoryItem,
} from '../../api/agent';
import {
  AgentConversationApiError,
  canConfirmAgentAnalysis,
  canSendAgentMessage,
  confirmAgentAnalysis,
  createAgentConversation,
  getAgentConversation,
  getAgentModelStatus,
  listAgentConversations,
  sendAgentConversationMessage,
  type AgentConversation,
  type AgentAnalysisCard,
  type AgentConversationSummary,
  type AgentConversationState,
  type AgentModelStatus,
} from '../../api/agentConversation';
import { getAgentSettings } from '../../api/agentSettings';
import { DuplicateScanReport } from './DuplicateScanReport';
import { AgentPlanReview, canResumeBypassPlan, canStartPlanReview, executionProgress } from './AgentPlanReview';
import { isSameDirectoryPath } from './pathSemantics';
import './AgentPage.css';

const TASK_STATUS_LABELS: Record<string, string> = {
  queued: '排队中',
  analyzing: '处理中',
  awaiting_approval: '待审阅',
  running: '执行中',
  paused: '已暂停',
  succeeded: '已完成',
  partial: '部分完成 · 需核查',
  failed: '失败',
  cancelled: '已取消',
};

const CONVERSATION_STATUS_LABELS: Record<AgentConversationState, string> = {
  clarifying: '沟通中',
  awaiting_confirmation: '待你确认',
  unsupported: '暂不支持',
  failed: '未完成',
  processing: '处理中',
  analyzing: '分析中',
  awaiting_approval: '待你审阅',
  analysis_complete: '分析完成',
};

const SELECTED_ITEM_KEY = 'filesage.agent.selectedItem';
const MAX_CONVERSATIONS = 20;
const MAX_MESSAGE_BYTES = 4 * 1024;
const AGENT_STARTER_PROMPTS = ['按拍摄日期归档照片', '按内容整理 PDF', '查找重复文件'];

export type AgentLaunchContext =
  | { kind: 'selection'; paths: string[] }
  | { kind: 'directory'; path: string };

type SavedSelection = { kind: 'conversation' | 'task'; id: string } | { kind: 'draft' } | null;

function messageWithLaunchContext(message: string, context: AgentLaunchContext): string {
  const scope = context.kind === 'selection'
    ? { selectedPaths: context.paths }
    : { currentDirectory: context.path };
  return `${message}\n\n文件页上下文（路径均相对管理根目录；空目录路径表示管理根目录，请将其作为本次整理范围）：\n${JSON.stringify(scope)}`;
}

function apiMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '时间未知';
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function taskStatusLabel(status: string): string {
  return TASK_STATUS_LABELS[status] ?? `未知状态：${status || '空'}`;
}

function conversationStatusLabel(status: string): string {
  return CONVERSATION_STATUS_LABELS[status as AgentConversationState] ?? `未知状态：${status || '空'}`;
}

function readSavedSelection(): SavedSelection {
  try {
    const value = window.localStorage.getItem(SELECTED_ITEM_KEY) ?? '';
    if (value === 'draft') return { kind: 'draft' };
    const separator = value.indexOf(':');
    if (separator < 1) return null;
    const kind = value.slice(0, separator);
    const id = value.slice(separator + 1);
    if (!id || (kind !== 'conversation' && kind !== 'task')) return null;
    return { kind, id };
  } catch {
    return null;
  }
}

function conversationTitle(item: AgentConversationSummary): string {
  const title = item.title.trim();
  return title ? title.length > 54 ? `${title.slice(0, 54)}…` : title : '新对话';
}

// 确认卡和 Shell 步骤已包含结构化回执，不重复展示助手复述。
export function conversationMessagesForDisplay(conversation: AgentConversation | null, plan: AgentPlan | null) {
  if (!conversation) return [];
  let messages = conversation.messages;
  if (conversation.state === 'awaiting_confirmation' && conversation.analysisCard) {
    let confirmationReplyIndex = -1;
    messages.forEach((message, index) => {
      if (message.role === 'assistant') confirmationReplyIndex = index;
    });
    messages = messages.filter((_, index) => index !== confirmationReplyIndex);
  }
  if (!plan || conversation.planId !== plan.planId || !plan.operations.some((operation) => operation.action === 'run_shell')) return messages;
  let lastAssistantIndex = -1;
  messages.forEach((message, index) => {
    if (message.role === 'assistant') lastAssistantIndex = index;
  });
  return messages.filter((_, index) => index !== lastAssistantIndex);
}

// 长助手回复默认收起，保留完整原文供展开查看。
export function assistantMessagePreview(role: 'user' | 'assistant', content: string): string {
  if (role !== 'assistant') return '';
  const text = content.replace(/\s+/g, ' ').trim();
  if (text.length <= 160) return '';
  const sentenceEnd = text.indexOf('。', 64);
  const end = sentenceEnd >= 0 && sentenceEnd < 112 ? sentenceEnd + 1 : 96;
  return `${text.slice(0, end).trimEnd()}…`;
}

function destinationLabel(destination: string): string {
  return destination === 'local' ? '本地' : '远程';
}

function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB'];
  let size = value;
  let unit = -1;
  do {
    size /= 1024;
    unit += 1;
  } while (size >= 1024 && unit < units.length - 1);
  return `${size.toLocaleString('zh-CN', { maximumFractionDigits: 1 })} ${units[unit]}`;
}

// 用精简确认卡区分只生成方案与读取正文的任务。
export function analysisCardPresentation(
  actionOrCard: string | AgentAnalysisCard,
  filePath = '',
  targetPath = '',
  requestedMode?: number,
  requestedOwnerUid?: number,
  requestedGroupGid?: number,
) {
  if (typeof actionOrCard !== 'string') {
    const card = actionOrCard;
    if (card.action === 'run_shell') {
      const scopePath = card.filePath || '';
      const workingDir = card.workingDir || '.';
      return {
        heading: '确认隔离运行',
        scopePath: scopePath || '管理根目录',
        workingDir: workingDir === '.' ? scopePath || '管理根目录' : [scopePath, workingDir].filter(Boolean).join('/'),
        shellCommand: card.shellCommand ?? '',
        notice: '命令在隔离副本运行。续聊会向配置模型发送确认卡中的命令、范围、工作目录、执行结果及文件变化路径、类型和状态；stdout/stderr 各限 16 KiB。FileSage 不会另行读取文件正文，但命令输出可能包含文件内容或其他敏感信息。文件变化经审核后提交。',
        confirmLabel: '运行并查看变化',
      };
    }
    if (card.action === 'classify_documents') {
      const operation = card.operations?.[0];
      const files = operation?.files ?? [];
      const categories = operation?.categories ?? [];
      return {
        heading: '确认按正文分类',
        scopePath: card.filePath || '管理根目录',
        targetPath: card.targetPath || '—',
        files: files.map((file) => ({ ...file, sizeLabel: formatBytes(file.sizeBytes) })),
        categories: categories.map((category) => category.path),
        modelSummary: `${card.provider} · ${card.model} · ${destinationLabel(card.destination)}`,
        dataUse: `确认后会提取上列正文并发送到${destinationLabel(card.destination)}模型；PDF／Office 最多读取前 20 页。`,
        notice: '确认后只生成包含全部移动项的执行计划，不会立即移动文件。',
        confirmLabel: '确认并分析正文',
        createsPlanOnly: false,
      };
    }
    if (card.action === 'archive_photos') {
      return {
        heading: '确认照片归档',
        scopePath: card.filePath || '管理根目录',
        targetPath: card.targetPath || '管理根目录',
        dataUse: '只读取服务器本机的拍摄日期元数据；不读取或发送图片正文。',
        notice: '确认后生成完整归档计划，不会立即移动文件。',
        confirmLabel: '确认并生成方案',
        createsPlanOnly: true,
      };
    }
    if (card.action === 'create_symlink') {
      const operation = card.operations?.[0];
      return {
        heading: '确认创建软链接',
        operations: [{
          number: 1,
          title: '创建软链接',
          paths: [
            { label: '指向', path: operation?.filePath ?? card.filePath ?? '—' },
            { label: '新链接路径', path: operation?.targetPath ?? card.targetPath ?? '—' },
          ],
          metadata: [],
        }],
        dataTypes: card.dataTypes,
        provider: card.provider,
        model: card.model,
        destination: destinationLabel(card.destination),
        dataSummary: `将发送至${destinationLabel(card.destination)}模型：${card.dataTypes?.join('、') || '任务描述与操作路径'}`,
        notice: '确认后只生成执行计划，不会立即创建软链接。',
        confirmLabel: '确认并生成方案',
        createsPlanOnly: true,
      };
    }
    if (card.action === 'find_duplicates') {
      return {
        heading: '确认重复文件扫描',
        scopePath: card.filePath || '根目录',
        modelSummary: `${card.provider} · ${card.model} · ${destinationLabel(card.destination)}`,
        dataUse: `任务描述和范围会发送到${destinationLabel(card.destination)}模型；文件内容仅在服务器本地计算 SHA-256，不发送给模型。`,
        notice: '只读扫描，不会修改文件。',
        confirmLabel: '开始扫描',
        isDuplicateScan: true,
      };
    }
    const operations = card.operations ?? (card.action ? [{
      action: card.action,
      filePath: card.filePath ?? '',
      targetPath: card.targetPath ?? '',
      requestedMode: card.requestedMode,
      requestedOwnerUid: card.requestedOwnerUid,
      requestedGroupGid: card.requestedGroupGid,
    }] : []);
    const operationViews = operations.map((operation, index) => {
      const paths: Array<{ label: string; path: string }> = [];
      const metadata: Array<{ label: string; value: string }> = [];
      let title = '文件操作';
      switch (operation.action) {
        case 'rename':
          title = '重命名';
          paths.push({ label: '原路径', path: operation.filePath || '—' });
          paths.push({ label: '目标路径', path: operation.targetPath || '由 Agent 分析后提出' });
          break;
        case 'edit_text':
          title = '修改文件正文';
          paths.push({ label: '读取正文（全文）', path: operation.filePath || '—' });
          break;
        case 'create_file':
          title = '新建文本文件';
          paths.push({ label: '新建路径', path: operation.filePath || '—' });
          break;
        case 'overwrite_file':
          title = '覆盖文本文件';
          paths.push({ label: '目标文件', path: operation.filePath || '—' });
          break;
        case 'create_directory':
          title = '新建文件夹';
          paths.push({ label: '新文件夹', path: operation.targetPath || '—' });
          break;
        case 'trash':
          title = '移入回收站';
          paths.push({ label: '待回收项目', path: operation.filePath || '—' });
          paths.push({ label: '处理方式', path: '移入可恢复回收站' });
          break;
        case 'permanent_delete':
          title = '永久删除（不可恢复）';
          paths.push({ label: '待删除项目', path: operation.filePath || '—' });
          paths.push({ label: '删除后', path: '无法恢复' });
          break;
        case 'move_file':
          title = isSameDirectoryPath(operation.filePath, operation.targetPath) ? '重命名' : '移动文件';
          paths.push({ label: title === '重命名' ? '原路径' : '源路径', path: operation.filePath || '—' });
          paths.push({ label: title === '重命名' ? '新路径' : '目标路径', path: operation.targetPath || '—' });
          break;
        case 'copy_file':
          title = '复制文件';
          paths.push({ label: '复制自', path: operation.filePath || '—' });
          paths.push({ label: '复制到', path: operation.targetPath || '—' });
          break;
        case 'copy_directory':
          title = '复制文件夹';
          paths.push({ label: '复制自', path: operation.filePath || '—' });
          paths.push({ label: '复制到', path: operation.targetPath || '—' });
          break;
        case 'create_archive':
          title = '创建压缩包';
          paths.push({ label: '打包来源', path: operation.filePath || '—' });
          paths.push({ label: '压缩包路径', path: operation.targetPath || '—' });
          break;
        case 'extract_archive':
          title = '解压压缩包';
          paths.push({ label: '压缩包', path: operation.filePath || '—' });
          paths.push({ label: '解压到', path: operation.targetPath || '根目录' });
          break;
        case 'create_symlink':
          title = '创建软链接';
          paths.push({ label: '指向', path: operation.filePath || '—' });
          paths.push({ label: '新链接路径', path: operation.targetPath || '—' });
          break;
        case 'set_permissions':
          title = '修改权限';
          paths.push({ label: '文件', path: operation.filePath || '—' });
          metadata.push({ label: '目标权限', value: operation.requestedMode === undefined ? '—' : operation.requestedMode.toString(8).padStart(4, '0') });
          break;
        case 'set_owner':
          title = '修改所有者';
          paths.push({ label: '文件', path: operation.filePath || '—' });
          if (operation.requestedOwnerUid !== undefined) metadata.push({ label: '目标用户 ID', value: String(operation.requestedOwnerUid) });
          if (operation.requestedGroupGid !== undefined) metadata.push({ label: '目标用户组 ID', value: String(operation.requestedGroupGid) });
          break;
      }
      return { number: index + 1, title, paths, metadata };
    });
    const readsBody = operations.some((operation) => operation.action === 'edit_text' || operation.action === 'overwrite_file');
    const createsTextFile = operations.some((operation) => operation.action === 'create_file');
    const overwritesTextFile = operations.some((operation) => operation.action === 'overwrite_file');
    const permanentlyDeletes = operations.some((operation) => operation.action === 'permanent_delete');
    const createsPlanOnly = !operations.some((operation) => operation.action === 'rename' || operation.action === 'edit_text' || operation.action === 'overwrite_file');
    return {
      heading: '确认操作',
      operations: operationViews,
      dataTypes: card.dataTypes,
      provider: card.provider,
      model: card.model,
      destination: destinationLabel(card.destination),
      dataSummary: `将发送至${destinationLabel(card.destination)}模型：${card.dataTypes?.join('、') || '任务描述与操作路径'}`,
      notice: permanentlyDeletes
	        ? '永久删除后无法恢复。确认后生成计划；请审阅计划并明确确认执行。'
        : overwritesTextFile
	        ? '会读取并发送完整原文以生成替换候选；执行后无法恢复。现在只生成候选。'
        : createsTextFile
	        ? '只根据任务和路径生成候选，不读取文件内容；现在不会创建文件。'
        : readsBody
	        ? '会读取所列文件全文并发送给模型生成方案，不会修改文件。'
	        : createsPlanOnly ? '只生成执行计划，不会修改文件。' : '按所列范围分析并生成执行计划，不会修改文件。',
      confirmLabel: overwritesTextFile ? '确认并读取原文' : createsTextFile ? '确认并生成候选' : createsPlanOnly ? '确认并生成方案' : '确认并分析',
      createsPlanOnly,
    };
  }

  const action = actionOrCard;
  if (action === 'set_permissions') {
    return {
      heading: '确认权限变更',
      paths: [{ label: '文件', path: filePath }],
      metadata: [{ label: '目标权限', value: requestedMode === undefined ? '—' : requestedMode.toString(8).padStart(4, '0') }],
      basis: '仅使用任务描述、文件路径和目标权限，不读取文件内容。',
      notice: '确认后只生成待审核方案，不会修改文件。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  if (action === 'set_owner') {
    return {
      heading: '确认所有者变更',
      paths: [{ label: '文件', path: filePath }],
      metadata: [
        ...(requestedOwnerUid === undefined ? [] : [{ label: '目标用户 ID', value: String(requestedOwnerUid) }]),
        ...(requestedGroupGid === undefined ? [] : [{ label: '目标用户组 ID', value: String(requestedGroupGid) }]),
      ],
      basis: '仅使用任务描述、文件路径和目标所有者，不读取文件内容。',
      notice: '确认后只生成待审核方案，不会修改文件。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  if (action === 'move_file') {
    const isRename = isSameDirectoryPath(filePath, targetPath);
    return {
      heading: isRename ? '确认重命名' : '确认移动文件',
      paths: [{ label: isRename ? '原路径' : '源路径', path: filePath }, { label: isRename ? '新路径' : '目标路径', path: targetPath }],
      metadata: [],
      basis: '任务描述、源路径与目标路径',
      notice: '确认后生成待审核方案，不会修改文件。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  if (action === 'copy_file' || action === 'copy_directory') {
    const directory = action === 'copy_directory';
    return {
      heading: directory ? '确认复制文件夹' : '确认复制文件',
      paths: [{ label: '复制自', path: filePath }, { label: '复制到', path: targetPath }],
      metadata: [],
      basis: `任务描述、源路径与目标路径；${directory ? '目录内容' : '文件内容'}仅在本机复制，不发送给模型。`,
      notice: '确认后生成待审核方案，不会修改文件。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  if (action === 'create_archive' || action === 'extract_archive') {
    const extract = action === 'extract_archive';
    return {
      heading: extract ? '确认解压压缩包' : '确认创建压缩包',
      paths: extract
        ? [{ label: '压缩包', path: filePath }, { label: '解压到', path: targetPath || '根目录' }]
        : [{ label: '打包来源', path: filePath }, { label: '压缩包路径', path: targetPath }],
      metadata: [],
      basis: '任务描述、来源与目标路径；文件内容仅在本机处理，不发送给模型。',
      notice: '确认后生成执行计划，不会修改文件；解压清单会列在计划中。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  if (action === 'create_symlink') {
    return {
      heading: '确认创建软链接',
      paths: [{ label: '指向', path: filePath }, { label: '新链接路径', path: targetPath }],
      metadata: [],
      basis: '任务描述、目标对象与链接路径；仅在管理根目录内创建链接，不复制或修改目标。',
      notice: '确认后只生成执行计划，不会立即创建软链接。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  if (action === 'create_directory') {
    return {
      heading: '确认任务范围',
      paths: [{ label: '新建文件夹', path: targetPath }],
      metadata: [],
      basis: '任务描述与目标路径',
      notice: '确认后只生成待审核方案，不会新建文件夹。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    };
  }

  return {
    heading: '确认分析范围',
    paths: [{ label: '分析文件', path: filePath }],
    metadata: [],
    basis: '',
    notice: '确认后才会读取文件并生成建议，不会修改文件。',
    confirmLabel: '确认并分析',
    showModelDetails: true,
    createsPlanOnly: false,
  };
}

function conversationFailureMessage(conversation: AgentConversation): string {
  switch (conversation.error) {
    case 'service_restarted': return '服务重启时本轮尚未完成，请重新发送需求。';
    case 'provider_error': return '模型暂时无法处理，请稍后重试。';
    case 'invalid_model_response': return '模型返回内容无效，请调整描述后重试。';
    case 'classification_analysis_timeout': return '文档分析超时，请缩小范围后重试。';
    case 'classification_extractor_busy': return '文档处理较忙，请稍后重新发起。';
    case 'conversation_too_large': return '对话内容过长，请新建对话并简要说明。';
    default: return '本轮尚未完成，请继续说明或重试。';
  }
}

export function AgentPage({
  launchContext = null,
  onLaunchContextConsumed,
}: {
  launchContext?: AgentLaunchContext | null;
  onLaunchContextConsumed?: () => void;
}) {
  const savedSelection = readSavedSelection();
  const [activeLaunchContext, setActiveLaunchContext] = useState(launchContext);
  const [modelStatus, setModelStatus] = useState<AgentModelStatus | null>(null);
  const [modelStatusLoading, setModelStatusLoading] = useState(true);
  const [modelStatusError, setModelStatusError] = useState('');
  const [modelStatusRefreshKey, setModelStatusRefreshKey] = useState(0);
  const [bypassPermission, setBypassPermission] = useState(true);
  const [agentSettingsLoading, setAgentSettingsLoading] = useState(true);
  const [agentSettingsError, setAgentSettingsError] = useState('');
  const [selectedConversationId, setSelectedConversationId] = useState(() => launchContext ? '' : savedSelection?.kind === 'conversation' ? savedSelection.id : '');
  const [selectedTaskId, setSelectedTaskId] = useState(() => launchContext ? '' : savedSelection?.kind === 'task' ? savedSelection.id : '');
  const [conversationHistory, setConversationHistory] = useState<AgentConversationSummary[]>([]);
  const [conversationHistoryHasMore, setConversationHistoryHasMore] = useState(false);
  const [conversationHistoryOffset, setConversationHistoryOffset] = useState(0);
  const [conversationHistoryLoading, setConversationHistoryLoading] = useState(false);
  const [conversationHistoryError, setConversationHistoryError] = useState('');
  const [conversationHistoryRefreshKey, setConversationHistoryRefreshKey] = useState(0);
  const [conversationHistoryLoaded, setConversationHistoryLoaded] = useState(false);
  const [conversation, setConversation] = useState<AgentConversation | null>(null);
  const [conversationLoading, setConversationLoading] = useState(false);
  const [conversationError, setConversationError] = useState('');
  const [conversationRefreshKey, setConversationRefreshKey] = useState(0);
  const [draft, setDraft] = useState('');
  const [composerError, setComposerError] = useState('');
  const [conversationBusy, setConversationBusy] = useState('');
  const [enableDocumentOCR, setEnableDocumentOCR] = useState(false);
  const [history, setHistory] = useState<AgentTaskHistoryItem[]>([]);
  const [historyLoading, setHistoryLoading] = useState(true);
  const [historyError, setHistoryError] = useState('');
  const [historyRefreshKey, setHistoryRefreshKey] = useState(0);
  const [plan, setPlan] = useState<AgentPlan | null>(null);
  const [planLoading, setPlanLoading] = useState(false);
  const [planError, setPlanError] = useState('');
  const [planRefreshKey, setPlanRefreshKey] = useState(0);
  const [actionBusy, setActionBusy] = useState('');
  const [stopBusy, setStopBusy] = useState(false);
  const [actionError, setActionError] = useState('');
  const [notice, setNotice] = useState('');
  const autoExpandHistoryOffset = useRef<number | null>(null);
  const composerRef = useRef<HTMLTextAreaElement>(null);

  function selectStarterPrompt(prompt: string) {
    setDraft(prompt);
    setComposerError('');
    window.requestAnimationFrame(() => composerRef.current?.focus());
  }

  const currentConversation = conversation?.id === selectedConversationId ? conversation : null;
  const selectedConversationSummary = conversationHistory.find((item) => item.id === selectedConversationId) ?? null;
  const selectedConversationLabel = selectedConversationSummary ? conversationTitle(selectedConversationSummary) : '对话';
  const currentPlanStatus = currentConversation?.planId && plan?.planId === currentConversation.planId ? plan.status : undefined;
  const currentMessageCount = currentConversation?.messages.length ?? 0;
  const currentHasLinkedPlan = Boolean(currentConversation?.planId);
  const currentConversationCanSend = canSendAgentMessage(currentConversation?.state ?? null, currentPlanStatus, currentMessageCount, currentHasLinkedPlan);
  const conversationMessageLimitReached = Boolean(currentConversation
    && canSendAgentMessage(currentConversation.state, currentPlanStatus, 0, currentHasLinkedPlan)
    && !currentConversationCanSend);
  const needsAnalysisConfirmation = currentConversation?.state === 'awaiting_confirmation';
  const analysisCard = currentConversation?.state === 'awaiting_confirmation' ? currentConversation.analysisCard : null;
  const analysisCardView = analysisCard
    ? analysisCardPresentation(analysisCard)
    : null;
  const ocrDocumentCount = analysisCard?.action === 'classify_documents'
    ? (analysisCard.operations?.[0]?.files ?? []).filter((file) => /\.(pdf|docx|pptx)$/i.test(file.path)).length
    : 0;
  const ocrBatchLimitExceeded = enableDocumentOCR && ocrDocumentCount > 2;

  useEffect(() => {
    const controller = new AbortController();
    setAgentSettingsLoading(true);
    setAgentSettingsError('');
    getAgentSettings(controller.signal)
      .then((settings) => {
        if (!controller.signal.aborted) setBypassPermission(settings.bypassPermission);
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) setAgentSettingsError(apiMessage(error, '无法读取 Agent 执行设置。'));
      })
      .finally(() => {
        if (!controller.signal.aborted) setAgentSettingsLoading(false);
      });
    return () => controller.abort();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    setModelStatusLoading(true);
    setModelStatusError('');
    getAgentModelStatus(controller.signal)
      .then((status) => {
        if (!controller.signal.aborted) setModelStatus(status);
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setModelStatus(null);
          setModelStatusError(apiMessage(error, '无法读取模型状态。'));
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setModelStatusLoading(false);
      });
    return () => controller.abort();
  }, [modelStatusRefreshKey]);

  useEffect(() => {
    setEnableDocumentOCR(false);
  }, [selectedConversationId]);

  useEffect(() => {
    if (activeLaunchContext && !selectedConversationId && !selectedTaskId) return;
    try {
      const selection = selectedConversationId
        ? `conversation:${selectedConversationId}`
        : selectedTaskId ? `task:${selectedTaskId}` : 'draft';
      window.localStorage.setItem(SELECTED_ITEM_KEY, selection);
    } catch {
      // 服务端会话仍可用；本地存储仅用于恢复选择。
    }
  }, [activeLaunchContext, selectedConversationId, selectedTaskId]);

  useEffect(() => {
    const controller = new AbortController();
    if (conversationHistoryOffset === 0) {
      setConversationHistory([]);
    }

    setConversationHistoryLoading(true);
    setConversationHistoryError('');
    listAgentConversations(MAX_CONVERSATIONS, conversationHistoryOffset, controller.signal).then((page) => {
      if (controller.signal.aborted) return;
      setConversationHistory((items) => conversationHistoryOffset === 0
        ? page.conversations
        : [...items, ...page.conversations.filter((item) => !items.some((existing) => existing.id === item.id))]);
      setConversationHistoryHasMore(page.hasMore);
      setConversationHistoryLoaded(true);
    }).catch((error: unknown) => {
      if (!controller.signal.aborted) setConversationHistoryError(apiMessage(error, '读取对话历史失败。'));
    }).finally(() => {
      if (!controller.signal.aborted) setConversationHistoryLoading(false);
    });
    return () => controller.abort();
  }, [conversationHistoryOffset, conversationHistoryRefreshKey]);

  useEffect(() => {
    // 新建会话响应已含完整内容，等待历史列表同步时不要将其误判为不存在。
    if (currentConversation?.id === selectedConversationId) return;
    if (!conversationHistoryLoaded || conversationHistoryLoading || conversationHistoryError || !selectedConversationId
      || conversationHistory.some((item) => item.id === selectedConversationId)) return;
    if (conversationHistoryHasMore) {
      if (autoExpandHistoryOffset.current === conversationHistoryOffset) return;
      autoExpandHistoryOffset.current = conversationHistoryOffset;
      setConversationHistoryOffset((offset) => offset + MAX_CONVERSATIONS);
      return;
    }
    setSelectedConversationId('');
    setSelectedTaskId('');
    setConversation(null);
    setConversationError('上次打开的会话已不存在，已为你打开新对话。');
  }, [conversationHistory, conversationHistoryError, conversationHistoryHasMore, conversationHistoryLoaded, conversationHistoryLoading, conversationHistoryOffset, selectedConversationId, currentConversation]);

  useEffect(() => {
    if (!selectedConversationId) {
      setConversation(null);
      setConversationError('');
      setConversationLoading(false);
      return;
    }
    if (currentConversation) {
      setConversationError('');
      setConversationLoading(false);
      return;
    }

    const controller = new AbortController();
    setConversationLoading(true);
    setConversationError('');
    getAgentConversation(selectedConversationId, controller.signal)
      .then((loadedConversation) => {
        if (!controller.signal.aborted) setConversation(loadedConversation);
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) setConversationError(apiMessage(error, '无法恢复这条会话。'));
      })
      .finally(() => {
        if (!controller.signal.aborted) setConversationLoading(false);
      });
    return () => controller.abort();
  }, [selectedConversationId, conversationRefreshKey, currentConversation]);

  useEffect(() => {
    const controller = new AbortController();
    setHistoryLoading(true);
    setHistoryError('');
    listAgentTaskHistory(controller.signal)
      .then((items) => {
        if (!controller.signal.aborted) setHistory(items);
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) setHistoryError(apiMessage(error, '读取任务历史失败。'));
      })
      .finally(() => {
        if (!controller.signal.aborted) setHistoryLoading(false);
      });
    return () => controller.abort();
  }, [historyRefreshKey]);

  useEffect(() => {
    const conversationReady = !selectedConversationId || currentConversation !== null;
    const taskId = selectedConversationId ? currentConversation?.taskId ?? '' : selectedTaskId;
    const planId = selectedConversationId ? currentConversation?.planId ?? '' : '';
    if (!conversationReady) {
      setPlan(null);
      setPlanLoading(conversationLoading);
      setPlanError('');
      return;
    }
    if (!taskId && !planId) {
      setPlan(null);
      setPlanLoading(false);
      setPlanError('');
      return;
    }

    const controller = new AbortController();
    setPlanLoading(true);
    setPlanError('');
    const request = taskId
      ? getAgentTask(taskId, controller.signal)
      : getAgentPlan(planId, controller.signal);
    request.then((latestPlan) => {
      if (!controller.signal.aborted) setPlan(latestPlan);
    }).catch((error: unknown) => {
      if (!controller.signal.aborted) setPlanError(apiMessage(error, '读取方案失败。'));
    }).finally(() => {
      if (!controller.signal.aborted) setPlanLoading(false);
    });
    return () => controller.abort();
  }, [selectedConversationId, currentConversation, selectedTaskId, conversationLoading, planRefreshKey]);

  useEffect(() => {
    if (!selectedConversationId || !currentConversation
      || (currentConversation.state !== 'processing' && currentConversation.state !== 'analyzing')) return;

    const controller = new AbortController();
    const timer = window.setTimeout(() => {
      getAgentConversation(selectedConversationId, controller.signal)
        .then((latest) => {
          if (!controller.signal.aborted) {
            setConversation(latest);
            if (latest.state !== 'processing' && latest.state !== 'analyzing') {
              setConversationHistoryOffset(0);
              setConversationHistoryRefreshKey((value) => value + 1);
            }
          }
        })
        .catch((error: unknown) => {
          if (!controller.signal.aborted) setConversationError(apiMessage(error, '刷新会话状态失败。'));
        });
    }, 1200);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [selectedConversationId, currentConversation]);

  async function refreshCurrentPlan() {
    if (!plan) return;
    setPlanLoading(true);
    setPlanError('');
    setActionError('');
    try {
      const latestPlan = await getAgentPlan(plan.planId);
      setPlan(latestPlan);
      setNotice('方案状态已刷新。');
    } catch (error) {
      setPlanError(apiMessage(error, '刷新方案失败。'));
    } finally {
      setPlanLoading(false);
    }
  }

  async function reloadPlanAfterConflict(planId: string) {
    setPlanLoading(true);
    setPlanError('');
    try {
      setPlan(await getAgentPlan(planId));
    } catch (error) {
      setPlanError(apiMessage(error, '重新读取最新方案失败。'));
    } finally {
      setPlanLoading(false);
    }
  }

  async function submitMessage(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const message = draft.trim();
    if (!message || !modelStatus?.available || conversationBusy || actionBusy) return;
    if (!currentConversationCanSend) return;
    const initialMessage = !selectedConversationId && activeLaunchContext
      ? messageWithLaunchContext(message, activeLaunchContext)
      : message;
    if (new TextEncoder().encode(initialMessage).byteLength > MAX_MESSAGE_BYTES) {
      setComposerError('消息与文件范围合计最多 4 KiB；请缩短描述或减少已选项目。');
      return;
    }

    setConversationBusy('message');
    setComposerError('');
    setConversationError('');
    setActionError('');
    setNotice('');
    try {
      const updated = selectedConversationId
        ? await sendAgentConversationMessage(selectedConversationId, message)
        : await createAgentConversation(initialMessage);
      setConversation(updated);
      setSelectedTaskId('');
      setSelectedConversationId(updated.id);
      if (activeLaunchContext) {
        setActiveLaunchContext(null);
        onLaunchContextConsumed?.();
      }
      if (updated.state === 'failed') {
        setComposerError('本轮处理失败，输入已保留。');
      } else {
        setDraft('');
      }
      setConversationHistoryOffset(0);
      setConversationHistoryRefreshKey((value) => value + 1);
    } catch (error) {
      setComposerError(`${apiMessage(error, '消息发送失败。')} 草稿已保留。`);
      if (error instanceof AgentConversationApiError && error.status === 409 && selectedConversationId) {
        setConversationRefreshKey((value) => value + 1);
      }
    } finally {
      setConversationBusy('');
    }
  }

  async function startAnalysis() {
    if (!selectedConversationId || !canConfirmAgentAnalysis(currentConversation) || conversationBusy || actionBusy) return;
    setConversationBusy('analysis');
    setComposerError('');
    setConversationError('');
    try {
      const updated = await confirmAgentAnalysis(
        selectedConversationId,
        analysisCard?.action === 'classify_documents' && enableDocumentOCR,
      );
      setConversation(updated);
      setConversationHistoryOffset(0);
      setConversationHistoryRefreshKey((value) => value + 1);
    } catch (error) {
      setComposerError(`${apiMessage(error, '确认分析失败。')} 草稿已保留。`);
      if (error instanceof AgentConversationApiError && error.status === 409) {
        setConversationRefreshKey((value) => value + 1);
      }
    } finally {
      setConversationBusy('');
    }
  }

  function selectConversation(id: string) {
    autoExpandHistoryOffset.current = null;
    if (activeLaunchContext) {
      setActiveLaunchContext(null);
      onLaunchContextConsumed?.();
    }
    setSelectedTaskId('');
    setDraft('');
    setConversation(null);
    setConversationError('');
    setComposerError('');
    setNotice('');
    setActionError('');
    setSelectedConversationId(id);
  }

  function selectTask(taskId: string) {
    autoExpandHistoryOffset.current = null;
    if (activeLaunchContext) {
      setActiveLaunchContext(null);
      onLaunchContextConsumed?.();
    }
    setSelectedConversationId('');
    setDraft('');
    setConversation(null);
    setConversationError('');
    setComposerError('');
    setNotice('');
    setActionError('');
    setSelectedTaskId(taskId);
  }

  function startNewConversation() {
    autoExpandHistoryOffset.current = null;
    setSelectedConversationId('');
    setSelectedTaskId('');
    setDraft('');
    setConversation(null);
    setConversationError('');
    setComposerError('');
    setActionError('');
    setNotice('');
  }

  function refreshAgentHistory() {
    setHistoryRefreshKey((value) => value + 1);
    setConversationHistoryRefreshKey((value) => value + 1);
  }

  function refreshHistory() {
    autoExpandHistoryOffset.current = null;
    refreshAgentHistory();
    setConversationHistoryOffset(0);
    if (selectedConversationId) setConversationRefreshKey((value) => value + 1);
  }

  async function rejectPlan() {
    if (!plan || actionBusy || plan.status !== 'awaiting_approval' || plan.operations.length === 0) return;
    setActionBusy('reject-plan');
    setActionError('');
    setNotice('');
    try {
      const latestPlan = await rejectAgentPlan(plan.planId, plan.planVersion);
      setPlan(latestPlan);
      refreshAgentHistory();
      if (selectedConversationId) setConversationRefreshKey((current) => current + 1);
      setNotice('已拒绝整份方案。');
    } catch (error) {
      if (error instanceof AgentApiError && error.status === 409) {
        setActionError('方案状态已变化，拒绝未生效。最新状态已刷新，请重新核对。');
        await reloadPlanAfterConflict(plan.planId);
        refreshAgentHistory();
      } else {
        setActionError(apiMessage(error, '拒绝方案失败，请重新载入核对。'));
      }
    } finally {
      setActionBusy('');
    }
  }

  async function runPlanRequest(planId: string, busyLabel: string, request: () => Promise<AgentPlan>) {
    if (!plan || actionBusy) return;
    let polling = true;
    const pollController = new AbortController();
    const pollProgress = async () => {
      while (polling) {
        await new Promise<void>((resolve) => window.setTimeout(resolve, 500));
        if (!polling) return;
        try {
          const current = await getAgentPlan(planId, pollController.signal);
          if (!polling) return;
          setPlan((selected) => selected?.planId === planId
            ? { ...current, stopRequested: selected.stopRequested || current.stopRequested }
            : selected);
        } catch {
          // 最终执行响应仍返回权威状态。
        }
      }
    };
    void pollProgress();
    setActionBusy(busyLabel);
    setActionError('');
    setNotice('');
    try {
      const latestPlan = await request();
      polling = false;
      pollController.abort();
      setPlan(latestPlan);
      refreshAgentHistory();
      if (selectedConversationId) setConversationRefreshKey((current) => current + 1);
      setNotice(latestPlan.status === 'completed'
        ? '计划已完成，服务端已记录结果。'
        : latestPlan.status === 'approved' && latestPlan.executionMode === 'manual'
          ? '计划已确认；你可以逐步执行。'
          : latestPlan.status === 'cancelled'
            ? `已停止：完成 ${executionProgress(latestPlan).completed} 项，后续项未执行。`
            : latestPlan.status === 'failed' || latestPlan.status === 'invalidated' || latestPlan.status === 'needs_attention'
              ? '计划未完成；请核对服务端返回的操作结果。'
              : '执行状态已刷新；状态不确定时请按结果提示核查。');
    } catch (error) {
      polling = false;
      pollController.abort();
      if (error instanceof AgentApiError && error.status === 409) {
        setActionError('方案或文件状态已变化，操作未生效。最新状态已刷新，请重新核对。');
        await reloadPlanAfterConflict(planId);
        refreshAgentHistory();
      } else {
        try {
          const latestPlan = await getAgentPlan(planId);
          setPlan((selected) => selected?.planId === planId ? latestPlan : selected);
          refreshAgentHistory();
          setActionError(apiMessage(error, '执行失败，最新方案状态已刷新。'));
        } catch (refreshError) {
          setActionError(`${apiMessage(error, '执行失败。')} ${apiMessage(refreshError, '最新方案读取失败，请手动刷新。')}`);
        }
      }
    } finally {
      polling = false;
      pollController.abort();
      setActionBusy('');
    }
  }

  async function runPlan(confirmPermanentDelete: boolean) {
    if (!plan || !canStartPlanReview(plan) || actionBusy || agentSettingsLoading || agentSettingsError) return;
    const containsPermanentDelete = plan.operations.some((operation) => operation.action === 'permanent_delete');
    if (containsPermanentDelete !== confirmPermanentDelete) return;
    const planId = plan.planId;
    const planVersion = plan.planVersion;
    const expectedMode = bypassPermission ? 'bypass' : 'manual';
    await runPlanRequest(planId, 'start-plan', async () => {
      const settings = await getAgentSettings();
      const latestMode = settings.bypassPermission ? 'bypass' : 'manual';
      setBypassPermission(settings.bypassPermission);
      if (latestMode !== expectedMode) {
        throw new AgentApiError('Agent 执行模式刚刚发生变化，请重新核对后再确认计划。');
      }
      return startAgentPlan(planId, planVersion, expectedMode, confirmPermanentDelete);
    });
  }

  async function runStep(operationId: string) {
    if (!plan || actionBusy || plan.status !== 'approved' || plan.executionMode !== 'manual') return;
    await runPlanRequest(plan.planId, operationId, () => executeAgentStep(plan.planId, operationId, plan.planVersion));
  }

  async function resumePlan() {
    if (!plan || actionBusy || !canResumeBypassPlan(plan)) return;
    await runPlanRequest(plan.planId, 'execute', () => executeAgentPlan(plan.planId, plan.planVersion));
  }

  async function stopPlan() {
    if (!plan || plan.status !== 'executing' || plan.stopRequested || stopBusy) return;
    const planId = plan.planId;
    setStopBusy(true);
    setActionError('');
    setNotice('');
    try {
      const latestPlan = await requestAgentPlanStop(planId, plan.planVersion);
      setPlan((selected) => selected?.planId === planId
        ? { ...latestPlan, stopRequested: selected.stopRequested || latestPlan.stopRequested }
        : selected);
      refreshAgentHistory();
      if (selectedConversationId) setConversationRefreshKey((current) => current + 1);
      setNotice(latestPlan.status === 'cancelled'
        ? `已停止：完成 ${executionProgress(latestPlan).completed} 项，后续项未执行。`
        : '已收到停止请求，当前操作安全结束后将停止后续项。');
    } catch (error) {
      if (error instanceof AgentApiError && error.status === 409) {
        setActionError('任务状态已变化，停止请求未生效。最新状态已刷新，请核对。');
        await reloadPlanAfterConflict(planId);
        refreshAgentHistory();
      } else {
        setActionError(apiMessage(error, '停止请求失败；请刷新任务核对当前状态。'));
      }
    } finally {
      setStopBusy(false);
    }
  }

  async function revisePlan(operations: AgentPlanEditOperation[]): Promise<boolean> {
    if (!plan || actionBusy) return false;
    setActionBusy('revise');
    setActionError('');
    setNotice('');
    try {
      const revised = await reviseAgentPlan(plan.planId, plan.planVersion, operations);
      setPlan(revised);
      refreshAgentHistory();
      if (selectedConversationId) setConversationRefreshKey((current) => current + 1);
      setNotice('修改已保存；所有操作需要重新审核。');
      return true;
    } catch (error) {
      if (error instanceof AgentApiError && error.status === 409) {
        setActionError('方案或文件状态已变化，修改未生效。最新方案已刷新，请重新核对。');
        setPlanLoading(true);
        try {
          setPlan(await getAgentTask(plan.taskId));
          if (selectedConversationId) setConversationRefreshKey((current) => current + 1);
        } catch (refreshError) {
          setPlanError(apiMessage(refreshError, '重新读取最新方案失败。'));
        } finally {
          setPlanLoading(false);
        }
      } else {
        setActionError(apiMessage(error, '保存修改失败，请重新载入核对。'));
      }
      return false;
    } finally {
      setActionBusy('');
    }
  }

  async function reviseTextCandidate(operationId: string, proposedContent: string): Promise<AgentPlan> {
    if (!plan || actionBusy) throw new AgentApiError('当前方案暂不可修改。');
    const operation = plan.operations.find((item) => item.id === operationId
      && (item.action === 'edit_text' || item.action === 'create_file' || item.action === 'overwrite_file'));
    if (!operation) throw new AgentApiError('修改操作已变化，请刷新方案后重试。');
    const expectedPath = operation.action === 'create_file' ? operation.targetPath : operation.sourcePath;
    setActionBusy(operationId);
    setActionError('');
    setNotice('');
    try {
      const updated = await reviseAgentOperationContent(
        plan.planId,
        plan.taskId,
        operation.id,
        plan.planVersion,
        operation.action as AgentTextCandidateAction,
        expectedPath,
        proposedContent,
      );
      setPlan(updated);
      refreshAgentHistory();
      if (selectedConversationId) setConversationRefreshKey((current) => current + 1);
      setNotice('修改候选已保存，方案已重置为待审核。');
      return updated;
    } catch (error) {
      if (error instanceof AgentApiError && error.status === 409) {
        await reloadPlanAfterConflict(plan.planId);
        refreshAgentHistory();
      }
      throw error;
    } finally {
      setActionBusy('');
    }
  }

  const displayedPlan = selectedConversationId
    ? plan && ((currentConversation?.taskId && plan.taskId === currentConversation.taskId)
      || (currentConversation?.planId && plan.planId === currentConversation.planId)) ? plan : null
    : plan?.taskId === selectedTaskId ? plan : null;
  const conversationTaskIds = new Set(conversationHistory.flatMap((item) => item.taskId ? [item.taskId] : []));
  const taskHistory = history.filter((item) => !conversationTaskIds.has(item.id));
  const composerLocked = Boolean(conversationBusy || actionBusy
    || (selectedConversationId && currentConversation === null)
    || !currentConversationCanSend);
  const composerCanSend = modelStatus?.available === true
    && !modelStatusLoading
    && !modelStatusError
    && !composerLocked;
  const showPlanReview = Boolean(selectedTaskId || displayedPlan || planLoading || planError);
  const modelUnavailableMessage = modelStatusLoading
    ? '正在连接 AI…'
    : modelStatusError ? 'AI 服务暂不可用。'
      : modelStatus?.available === false ? 'AI 尚未配置。' : '';
  const showModelUnavailable = Boolean(modelUnavailableMessage && !conversationError);
  const isNewConversation = !selectedConversationId && !selectedTaskId;

  return (
    <section className="agent-page" aria-labelledby="agent-page-title">
      <header className="agent-page-heading">
        <div className="agent-page-title">
          <h1 id="agent-page-title">整理助手</h1>
        </div>
      </header>

      <div className="agent-workspace">
        <aside className="agent-session-sidebar" aria-label="Agent 会话">
          <div className="agent-session-heading">
            <strong>对话</strong>
            <button className="agent-session-refresh" type="button" onClick={refreshHistory} aria-label="刷新会话和任务记录" disabled={conversationHistoryLoading || historyLoading || planLoading || Boolean(actionBusy)}>
              <RefreshCw size={15} aria-hidden="true" />
            </button>
          </div>
          <button
            className={`agent-session-new${isNewConversation ? ' is-active' : ''}`}
            type="button"
            onClick={() => { if (!isNewConversation) startNewConversation(); }}
            disabled={Boolean(actionBusy || conversationBusy)}
            aria-pressed={isNewConversation}
          >
            <Plus size={16} aria-hidden="true" />新对话
          </button>
          <div className="agent-session-scroll">
            {conversationHistoryError && <p className="agent-inline-error" role="alert"><AlertTriangle size={15} aria-hidden="true" />{conversationHistoryError}</p>}
            {conversationHistoryLoading && <div className="agent-loading" role="status"><Loader2 className="agent-spinner" size={16} aria-hidden="true" />正在读取对话…</div>}
            {conversationHistory.length > 0 && (
              <div className="agent-session-group">
                <div className="agent-session-list" role="group" aria-label="已保存对话">
                  {conversationHistory.map((item) => (
                    <button
                      className={`agent-session-item${selectedConversationId === item.id ? ' is-active' : ''}`}
                      type="button"
                      key={item.id}
                      onClick={() => selectConversation(item.id)}
                      disabled={Boolean(actionBusy || conversationBusy)}
                      aria-current={selectedConversationId === item.id ? 'true' : undefined}
                    >
                      <span className="agent-session-item-main"><strong>{conversationTitle(item)}</strong><small>{formatDate(item.updatedAt)}</small></span>
                      <span className="agent-session-state">{item.taskStatus ? taskStatusLabel(item.taskStatus) : conversationStatusLabel(item.state)}</span>
                    </button>
                  ))}
                </div>
                {conversationHistoryHasMore && (
                  <button
                    className="agent-text-button"
                    type="button"
                    onClick={() => conversationHistoryError
                      ? setConversationHistoryRefreshKey((value) => value + 1)
                      : setConversationHistoryOffset((offset) => offset + MAX_CONVERSATIONS)}
                    disabled={conversationHistoryLoading}
                  >
                    {conversationHistoryLoading ? '正在读取…' : conversationHistoryError ? '重试加载' : '加载更多对话'}
                  </button>
                )}
              </div>
            )}
            {(taskHistory.length > 0 || historyLoading || historyError) && (
              <details className="agent-task-history" open={Boolean(selectedTaskId)}>
                <summary><span>任务记录</span>{taskHistory.length > 0 && <small>{taskHistory.length}</small>}</summary>
                {historyError && <p className="agent-inline-error" role="alert"><AlertTriangle size={15} aria-hidden="true" />{historyError}</p>}
                {historyLoading && taskHistory.length === 0 && <div className="agent-loading" role="status"><Loader2 className="agent-spinner" size={16} aria-hidden="true" />正在读取任务…</div>}
                <div className="agent-session-list" role="group" aria-label="历史 Agent 任务">
                  {taskHistory.map((item) => (
                    <button
                      className={`agent-session-item${selectedTaskId === item.id ? ' is-active' : ''}`}
                      type="button"
                      key={item.id}
                      onClick={() => selectTask(item.id)}
                      disabled={Boolean(actionBusy || conversationBusy)}
                      aria-current={selectedTaskId === item.id ? 'true' : undefined}
                    >
                      <span className="agent-session-item-main"><strong>{item.scopePath || '根目录'}</strong><small>{formatDate(item.createdAt)}</small></span>
                      <span className="agent-session-state">{taskStatusLabel(item.status)}</span>
                    </button>
                  ))}
                </div>
              </details>
            )}
          </div>
        </aside>
        <section className="agent-chat-panel" aria-label="与整理助手对话">
          <header className="agent-chat-header">
            <h2>{selectedTaskId ? '任务记录' : selectedConversationId ? selectedConversationLabel : '新对话'}</h2>
            {currentConversation && !currentConversation.planId && <span className="agent-conversation-status">{conversationStatusLabel(currentConversation.state)}</span>}
          </header>
          <div className={`agent-conversation-flow${isNewConversation ? ' is-empty' : ''}`}>
            <div className="agent-chat-thread" role="log" aria-label="Agent 对话" aria-live="polite">
              {isNewConversation && (
                <article className="agent-empty-state" aria-label="整理助手">
                  <span className="agent-empty-mark"><FolderOpen size={21} strokeWidth={1.8} aria-hidden="true" /></span>
                  <div>
                    <h2>想整理什么文件？</h2>
                    <p>说出目标，我先与你对齐范围，再给出待审计划。</p>
                  </div>
                  {!draft.trim() && (
                    <div className="agent-prompt-suggestions" aria-label="常见整理示例">
                      <span>常见整理</span>
                      {AGENT_STARTER_PROMPTS.map((prompt) => (
                        <button type="button" key={prompt} onClick={() => selectStarterPrompt(prompt)}>
                          {prompt}
                        </button>
                      ))}
                    </div>
                  )}
                </article>
              )}
              {selectedConversationId && conversationLoading && (
                <div className="agent-state agent-chat-state" role="status"><Loader2 className="agent-spinner" size={18} aria-hidden="true" />正在恢复会话…</div>
              )}
              {conversationError && (
                <div className="agent-error" role="alert"><AlertTriangle size={16} aria-hidden="true" /><span>{conversationError}</span>
                  <button type="button" onClick={() => setConversationRefreshKey((value) => value + 1)}>重试</button>
                </div>
              )}
              {conversationMessagesForDisplay(currentConversation, displayedPlan).map((message, index) => {
                const preview = assistantMessagePreview(message.role, message.content);
                return (
                  <article
                    className={`agent-chat-message is-${message.role}`}
                    aria-label={message.role === 'assistant' ? '整理助手回复' : '你的消息'}
                    key={`${currentConversation?.id ?? 'draft'}-${index}`}
                  >
                    {preview ? (
                      <details className="agent-chat-message-details">
                        <summary>
                          <span>{preview}</span>
                          <strong className="agent-chat-message-expand">展开</strong>
                          <strong className="agent-chat-message-collapse">收起</strong>
                        </summary>
                        <p>{message.content}</p>
                      </details>
                    ) : <p>{message.content}</p>}
                  </article>
                );
              })}
              {currentConversation?.state === 'failed'
                && currentConversation.error
                && currentConversation.messages.at(-1)?.role !== 'assistant' && (
                  <p className="agent-inline-error" role="status">
                    <AlertTriangle size={15} aria-hidden="true" />{conversationFailureMessage(currentConversation)}
                  </p>
                )}
              {currentConversation && ['processing', 'analyzing'].includes(currentConversation.state) && (
                <div className="agent-loading" role="status"><Loader2 className="agent-spinner" size={17} aria-hidden="true" />{currentConversation.state === 'analyzing' && currentConversation.analysisCard?.action === 'run_shell' ? '正在隔离运行并生成文件变化清单…' : currentConversation.state === 'analyzing' && currentConversation.analysisCard?.action === 'create_file' ? '正在生成新文件候选…' : currentConversation.state === 'analyzing' && currentConversation.analysisCard?.action === 'overwrite_file' ? '正在读取原文并生成替换候选…' : currentConversation.state === 'analyzing' && currentConversation.analysisCard && analysisCardPresentation(currentConversation.analysisCard).createsPlanOnly ? '正在生成待审核方案…' : currentConversation.state === 'analyzing' ? '正在分析已确认的文件…' : '正在处理…'}</div>
              )}
            </div>
            {needsAnalysisConfirmation && (
              analysisCard?.action === 'find_duplicates' && analysisCardView && 'scopePath' in analysisCardView ? (
                <section className="agent-duplicate-confirmation" aria-labelledby="agent-duplicate-confirmation-title">
                  <div className="agent-analysis-heading"><ShieldCheck size={17} aria-hidden="true" /><strong id="agent-duplicate-confirmation-title">{analysisCardView.heading}</strong></div>
                  <div><span>扫描范围</span><code>{analysisCardView.scopePath}</code></div>
                  <small className="agent-duplicate-data-use">{analysisCardView.dataUse}</small>
                  <details className="agent-duplicate-model-details">
                    <summary>模型信息</summary>
                    <small>{analysisCardView.modelSummary}</small>
                  </details>
                  <small>{analysisCardView.notice}</small>
                  <button className="agent-primary-button" type="button" onClick={() => { void startAnalysis(); }} disabled={Boolean(conversationBusy || actionBusy)}>
                    {conversationBusy === 'analysis' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                    {analysisCardView.confirmLabel}
                  </button>
                </section>
              ) : analysisCard ? (
                analysisCard.action === 'run_shell' && analysisCardView && 'shellCommand' in analysisCardView ? (
                  <section className="agent-analysis-card agent-shell-confirmation" aria-labelledby="agent-shell-confirmation-title">
                    <div className="agent-analysis-heading"><ShieldCheck size={17} aria-hidden="true" /><strong id="agent-shell-confirmation-title">{analysisCardView.heading}</strong></div>
                    <dl>
                      <div><dt>管理范围</dt><dd><code>{analysisCardView.scopePath}</code></dd></div>
                      <div><dt>工作目录</dt><dd><code>{analysisCardView.workingDir}</code></dd></div>
                    </dl>
                    <pre className="agent-shell-command">{analysisCardView.shellCommand}</pre>
                    <p>{analysisCardView.notice}</p>
                    <button className="agent-primary-button" type="button" onClick={() => { void startAnalysis(); }} disabled={Boolean(conversationBusy || actionBusy)}>
                      {conversationBusy === 'analysis' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                      {analysisCardView.confirmLabel}
                    </button>
                  </section>
                ) : analysisCard.action === 'archive_photos' && analysisCardView && 'scopePath' in analysisCardView ? (
                  <section className="agent-analysis-card" aria-labelledby="agent-analysis-title">
                    <div className="agent-analysis-heading"><ShieldCheck size={17} aria-hidden="true" /><strong id="agent-analysis-title">{analysisCardView.heading}</strong></div>
                    <dl>
                      <div><dt>照片范围</dt><dd><code>{analysisCardView.scopePath}</code></dd></div>
                      <div><dt>归档到</dt><dd><code>{analysisCardView.targetPath}</code></dd></div>
                    </dl>
                    <p>{analysisCardView.dataUse}</p>
                    <p>{analysisCardView.notice}</p>
                    <button className="agent-primary-button" type="button" onClick={() => { void startAnalysis(); }} disabled={Boolean(conversationBusy || actionBusy)}>
                      {conversationBusy === 'analysis' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                      {analysisCardView.confirmLabel}
                    </button>
                  </section>
              ) : analysisCard?.action === 'classify_documents' && analysisCardView && 'files' in analysisCardView ? (
                <section className="agent-analysis-card agent-classification-confirmation" aria-labelledby="agent-classification-title">
                  <div className="agent-analysis-heading"><ShieldCheck size={17} aria-hidden="true" /><strong id="agent-classification-title">{analysisCardView.heading}</strong></div>
                  <dl className="agent-classification-summary">
                    <div><dt>递归范围</dt><dd><code>{analysisCardView.scopePath}</code></dd></div>
                    <div><dt>类别根目录</dt><dd><code>{analysisCardView.targetPath}</code></dd></div>
                  </dl>
                  <div className="agent-classification-lists">
                    <section aria-label="确认后读取的候选文件">
                      <h3>确认后读取的文件（{analysisCardView.files?.length ?? 0}）</h3>
                      <ul>
                        {analysisCardView.files?.map((file) => (
                          <li key={file.path}><code>{file.path}</code><small>{file.sizeLabel}</small></li>
                        ))}
                      </ul>
                    </section>
                    <section aria-label="可选的已有分类目录">
                      <h3>已有分类目录（{analysisCardView.categories?.length ?? 0}）</h3>
                      <ul>
                        {analysisCardView.categories?.map((categoryPath) => (
                          <li key={categoryPath}><code>{categoryPath}</code></li>
                        ))}
                      </ul>
                    </section>
                  </div>
                  <label className="agent-ocr-option">
                    <input
                      type="checkbox"
                      checked={enableDocumentOCR}
                      onChange={(event) => setEnableDocumentOCR(event.target.checked)}
                      disabled={Boolean(conversationBusy || actionBusy)}
                      aria-describedby="agent-ocr-description"
                    />
                    <span>扫描件启用中英文 OCR</span>
                  </label>
                  <p id="agent-ocr-description" className="agent-ocr-description">每次最多 2 份 PDF／Office，仅识别整份没有可提取文字的扫描件；正文会发送给所选模型。</p>
                  {ocrBatchLimitExceeded && <p className="agent-inline-error" role="alert">启用 OCR 时最多分析 2 份 PDF／Office；请缩小范围或关闭 OCR。</p>}
                  <p>{analysisCardView.dataUse}</p>
                  <details className="agent-analysis-model-details">
                    <summary>模型信息</summary>
                    <p>{analysisCardView.modelSummary}</p>
                  </details>
                  <p>{analysisCardView.notice}</p>
                  <button className="agent-primary-button" type="button" onClick={() => { void startAnalysis(); }} disabled={Boolean(conversationBusy || actionBusy || ocrBatchLimitExceeded)}>
                    {conversationBusy === 'analysis' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                    {analysisCardView.confirmLabel}
                  </button>
                </section>
              ) : (
                  <section className="agent-analysis-card" aria-labelledby="agent-analysis-title">
                    <div className="agent-analysis-heading"><ShieldCheck size={17} aria-hidden="true" /><strong id="agent-analysis-title">{analysisCardView?.heading}</strong></div>
                    {analysisCardView && 'operations' in analysisCardView && (
                      <>
                        <div className="agent-analysis-operations" aria-label="待分析操作清单">
                          {analysisCardView.operations?.map((operation) => (
                            <article className="agent-analysis-operation" key={operation.number}>
                              <strong>第 {operation.number} 项 · {operation.title}</strong>
                              <dl>
                                {operation.paths.map(({ label, path }) => (
                                  <div key={`${label}:${path}`}><dt>{label}</dt><dd><code>{path}</code></dd></div>
                                ))}
                                {operation.metadata.map(({ label, value }) => (
                                  <div key={label}><dt>{label}</dt><dd><code>{value}</code></dd></div>
                                ))}
                              </dl>
                            </article>
                          ))}
                        </div>
                        <p className="agent-analysis-data-summary">{analysisCardView.dataSummary}</p>
                        <details className="agent-analysis-model-details">
                          <summary>模型信息</summary>
                          <p>{analysisCardView.provider} · {analysisCardView.model} · {analysisCardView.destination}</p>
                        </details>
                        <p>{analysisCardView.notice}</p>
                      </>
                    )}
                    <button className="agent-primary-button" type="button" onClick={() => { void startAnalysis(); }} disabled={Boolean(conversationBusy || actionBusy)}>
                      {conversationBusy === 'analysis' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
                      {analysisCardView?.confirmLabel}
                    </button>
                  </section>
                )
              ) : (
                <p className="agent-inline-error" role="alert"><AlertTriangle size={15} aria-hidden="true" />服务未返回分析范围，暂不能确认。</p>
              )
            )}

            {currentConversation?.duplicateScanId && (
              <DuplicateScanReport
                key={currentConversation.duplicateScanId}
                scanId={currentConversation.duplicateScanId}
                onTaskCreated={(taskId) => {
                  selectTask(taskId);
                  setHistoryRefreshKey((value) => value + 1);
                }}
              />
            )}

            {showPlanReview && (
              <AgentPlanReview
                plan={displayedPlan}
                loading={planLoading}
                error={planError}
                actionBusy={actionBusy}
                conversationBusy={conversationBusy}
                actionError={actionError}
                notice={notice}
                executionMode={bypassPermission ? 'bypass' : 'manual'}
                executionModeReady={!agentSettingsLoading && !agentSettingsError}
                executionModeError={agentSettingsLoading ? '正在读取 Agent 执行设置，暂不能开始计划。' : agentSettingsError}
                onRetry={() => setPlanRefreshKey((current) => current + 1)}
                onRefresh={() => { void refreshCurrentPlan(); }}
                onStartPlan={(confirmPermanentDelete) => { void runPlan(confirmPermanentDelete); }}
                onResumePlan={() => { void resumePlan(); }}
                onRunStep={(operationId) => { void runStep(operationId); }}
                editable={Boolean(selectedConversationId)}
                onRevise={revisePlan}
                onEditTextContent={reviseTextCandidate}
                onRejectPlan={() => { void rejectPlan(); }}
                onStop={() => { void stopPlan(); }}
                stopBusy={stopBusy}
              />
            )}
          </div>

          <form className="agent-chat-composer" onSubmit={(event) => { void submitMessage(event); }}>
            <label className="agent-visually-hidden" htmlFor="agent-chat-input">整理需求消息</label>
            {activeLaunchContext && !selectedConversationId && !selectedTaskId && (
              <p className="agent-composer-hint" style={{ textAlign: 'left', overflowWrap: 'anywhere' }}>
                整理范围：{activeLaunchContext.kind === 'directory'
                  ? activeLaunchContext.path || '管理根目录'
                  : `已选 ${activeLaunchContext.paths.length} 项（${activeLaunchContext.paths.slice(0, 2).join('、')}${activeLaunchContext.paths.length > 2 ? '…' : ''}）`}
              </p>
            )}
            <textarea
              ref={composerRef}
              id="agent-chat-input"
              rows={3}
              value={draft}
              onChange={(event) => { setDraft(event.currentTarget.value); setComposerError(''); }}
              onKeyDown={(event) => {
                if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && composerCanSend && draft.trim()) {
                  event.preventDefault();
                  event.currentTarget.form?.requestSubmit();
                }
              }}
              placeholder={conversationLoading || conversationBusy ? '请稍候…'
                  : conversationError ? '恢复对话后继续'
                  : conversationMessageLimitReached ? '本会话消息接近上限，请新建对话继续'
                  : displayedPlan && canSendAgentMessage('awaiting_approval', displayedPlan.status, 0, true) ? '任务已结束，继续描述下一步…'
                      : currentConversation?.state === 'awaiting_confirmation' ? '有补充要求可以继续说…'
                        : currentConversation?.state === 'awaiting_approval' ? '请先审核方案'
                          : selectedConversationId ? '回复或补充整理要求…' : '描述你想完成的文件整理…'}
              disabled={composerLocked}
              aria-describedby={[
                composerError ? 'agent-composer-error' : '',
                showModelUnavailable ? 'agent-model-message' : '',
                'agent-privacy-hint',
              ].filter(Boolean).join(' ') || undefined}
            />
            <div className="agent-chat-composer-footer">
              {showModelUnavailable ? (
                <span className="agent-connection-message" id="agent-model-message" role={modelStatusError ? 'alert' : 'status'}>
                  {modelStatusLoading ? <Loader2 className="agent-spinner" size={14} aria-hidden="true" /> : <AlertTriangle size={14} aria-hidden="true" />}
                  {modelUnavailableMessage}
                </span>
              ) : null}
              <div>
                {showModelUnavailable && modelStatusError && <button className="agent-text-button" type="button" onClick={() => setModelStatusRefreshKey((value) => value + 1)} disabled={modelStatusLoading}>重试</button>}
                <button className="agent-primary-button" type="submit" disabled={!composerCanSend || !draft.trim()}>
                  {conversationBusy === 'message' ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <ArrowRight size={16} aria-hidden="true" />}
                  发送
                </button>
              </div>
            </div>
            <p className="agent-composer-hint agent-composer-privacy-hint" id="agent-privacy-hint">对话消息会发送到当前模型；文件正文仅在你确认分析后发送。</p>
            <p className="agent-composer-hint agent-composer-keyboard-hint">{composerCanSend ? 'Enter 发送 · Shift + Enter 换行' : 'Shift + Enter 换行'}</p>
            {composerError && <p className="agent-composer-error" id="agent-composer-error" role="alert">{composerError}</p>}
          </form>
        </section>

      </div>

    </section>
  );
}
