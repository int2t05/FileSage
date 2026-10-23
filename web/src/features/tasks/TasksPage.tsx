// 展示任务记录与统一文件操作历史。
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  Clock3,
  FileText,
  Files,
  Info,
  RefreshCw,
  Sparkles,
  Upload,
  type LucideIcon,
} from 'lucide-react';
import {
  auditActorLabel,
  auditEventLabel,
  auditOutcomeLabel,
  getAuditHistory,
  type AuditEvent,
} from '../../api/audit';
import { getAgentTask, type AgentOperation, type AgentPlan } from '../../api/agent';
import { ShellChangeReview } from '../agent/ShellChangeReview';
import { ShellToolCall } from '../agent/ShellToolCall';
import { isSameDirectoryPath } from '../agent/pathSemantics';
import { getDuplicateScan, type DuplicateScan } from '../../api/duplicateScans';
import {
  getTaskAudit,
  getUploadStatus,
  listTasks,
  type TaskAuditEvent,
  type TaskStatus,
  type TaskSummary,
  type UploadStatus,
} from '../../api/tasks';
import './TasksPage.css';

const pageSize = 50;

const taskKindIcons: Record<string, LucideIcon> = {
  upload: Upload,
  agent: Sparkles,
  processing: FileText,
  duplicate_scan: Files,
};

const taskStatusOptions: readonly { value: TaskStatus; label: string }[] = [
  { value: 'queued', label: '排队中' },
  { value: 'analyzing', label: '分析中' },
  { value: 'awaiting_approval', label: '待审阅' },
  { value: 'running', label: '进行中' },
  { value: 'paused', label: '已暂停' },
  { value: 'succeeded', label: '已完成' },
  { value: 'partial', label: '部分完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
];

const taskStatusLabels: Record<TaskStatus, string> = Object.fromEntries(
  taskStatusOptions.map(({ value, label }) => [value, label]),
) as Record<TaskStatus, string>;

const uploadStatusLabels: Record<string, string> = {
  receiving: '接收中',
  queued: '等待发布',
  publishing: '发布中',
  published: '已发布',
  partial: '部分完成',
  failed: '失败',
  cancelled: '已取消',
};

interface TaskDetailsState {
  loading: boolean;
  auditEvents: TaskAuditEvent[];
  auditOffset: number;
  auditHasMore: boolean;
  auditLoadingMore: boolean;
  auditError: string;
  agentPlan?: AgentPlan;
  agentError: string;
  uploadStatus?: UploadStatus;
  uploadError: string;
}

function taskKindLabel(kind: string): string {
  switch (kind) {
    case 'upload':
      return '文件上传';
    case 'agent':
      return 'Agent 任务';
    case 'processing':
      return '文件处理';
    case 'duplicate_scan':
      return '重复文件扫描';
    default:
      return kind ? `${kind} 任务` : '未知类型';
  }
}

function taskStatusTone(status: TaskStatus): string {
  switch (status) {
    case 'queued':
    case 'analyzing':
    case 'running':
      return 'active';
    case 'awaiting_approval':
      return 'waiting';
    case 'succeeded':
      return 'completed';
    case 'partial':
    case 'failed':
      return 'attention';
    case 'paused':
    case 'cancelled':
      return 'neutral';
  }
}

function describeError(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN');
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '未知';
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let amount = value;
  let unit = -1;
  do {
    amount /= 1024;
    unit += 1;
  } while (amount >= 1024 && unit < units.length - 1);
  return `${amount.toFixed(amount >= 10 ? 0 : 1)} ${units[unit]}`;
}

function emptyDetails(): TaskDetailsState {
  return {
    loading: false,
    auditEvents: [],
    auditOffset: 0,
    auditHasMore: false,
    auditLoadingMore: false,
    auditError: '',
    agentError: '',
    uploadError: '',
  };
}

export function TasksPage() {
  const [view, setView] = useState<'tasks' | 'history'>('tasks');
  const [historyRefreshToken, setHistoryRefreshToken] = useState(0);
  const [kindFilter, setKindFilter] = useState('');
  const [statusFilter, setStatusFilter] = useState<TaskStatus | ''>('');
  const [reloadToken, setReloadToken] = useState(0);
  const [tasks, setTasks] = useState<TaskSummary[]>([]);
  const [offset, setOffset] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState('');
  const [loadMoreError, setLoadMoreError] = useState('');
  const [expandedTaskId, setExpandedTaskId] = useState<string | null>(null);
  const [details, setDetails] = useState<Record<string, TaskDetailsState>>({});
  const listRequestVersion = useRef(0);
  const loadMoreController = useRef<AbortController | null>(null);

  const invalidateTaskListRequests = () => {
    listRequestVersion.current += 1;
    loadMoreController.current?.abort();
    loadMoreController.current = null;
    setLoadingMore(false);
  };

  useEffect(() => {
    const requestVersion = ++listRequestVersion.current;
    const controller = new AbortController();
    loadMoreController.current?.abort();
    loadMoreController.current = null;
    setLoading(true);
    setLoadingMore(false);
    setError('');
    setLoadMoreError('');
    setTasks([]);
    setOffset(0);
    setHasMore(false);
    setExpandedTaskId(null);
    setDetails({});

    listTasks({ kind: kindFilter, status: statusFilter }, pageSize, 0, controller.signal)
      .then((page) => {
        if (requestVersion !== listRequestVersion.current) return;
        setTasks(page.tasks);
        setOffset(page.offset + page.tasks.length);
        setHasMore(page.hasMore);
      })
      .catch((requestError: unknown) => {
        if (requestVersion === listRequestVersion.current && !(requestError instanceof DOMException && requestError.name === 'AbortError')) {
          setError(describeError(requestError, '无法读取任务记录。'));
        }
      })
      .finally(() => {
        if (requestVersion === listRequestVersion.current && !controller.signal.aborted) setLoading(false);
      });

    return () => {
      controller.abort();
      if (requestVersion === listRequestVersion.current) {
        listRequestVersion.current += 1;
        loadMoreController.current?.abort();
        loadMoreController.current = null;
      }
    };
  }, [kindFilter, statusFilter, reloadToken]);

  const kindOptions = useMemo(() => {
    const values = new Set(['upload', 'agent', 'processing', 'duplicate_scan']);
    for (const task of tasks) values.add(task.kind);
    if (kindFilter) values.add(kindFilter);
    return [...values].filter(Boolean).sort((left, right) => left.localeCompare(right));
  }, [kindFilter, tasks]);

  const refresh = () => {
    if (view === 'history') {
      setHistoryRefreshToken((value) => value + 1);
      return;
    }
    invalidateTaskListRequests();
    setReloadToken((value) => value + 1);
  };

  // 支持用方向键在两个记录视图间切换。
  const handleViewTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    let nextView: 'tasks' | 'history' | undefined;
    if (event.key === 'Home') nextView = 'tasks';
    else if (event.key === 'End') nextView = 'history';
    else if (event.key === 'ArrowRight' || event.key === 'ArrowDown') {
      nextView = event.currentTarget.id === 'tasks-view-tab' ? 'history' : 'tasks';
    } else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') {
      nextView = event.currentTarget.id === 'tasks-view-tab' ? 'history' : 'tasks';
    }
    if (!nextView) return;
    event.preventDefault();
    setView(nextView);
    document.getElementById(nextView === 'tasks' ? 'tasks-view-tab' : 'history-view-tab')?.focus();
  };

  const loadMore = async () => {
    if (loading || loadingMore || !hasMore || loadMoreController.current) return;
    const requestVersion = listRequestVersion.current;
    const controller = new AbortController();
    loadMoreController.current = controller;
    setLoadingMore(true);
    setLoadMoreError('');
    try {
      const page = await listTasks({ kind: kindFilter, status: statusFilter }, pageSize, offset, controller.signal);
      if (requestVersion !== listRequestVersion.current || controller.signal.aborted) return;
      setTasks((current) => requestVersion === listRequestVersion.current ? [...current, ...page.tasks] : current);
      setOffset(page.offset + page.tasks.length);
      setHasMore(page.hasMore);
    } catch (requestError) {
      if (requestVersion === listRequestVersion.current && !(requestError instanceof DOMException && requestError.name === 'AbortError')) {
        setLoadMoreError(describeError(requestError, '无法读取更多任务。'));
      }
    } finally {
      if (loadMoreController.current === controller) loadMoreController.current = null;
      if (requestVersion === listRequestVersion.current) setLoadingMore(false);
    }
  };

  const openTaskDetails = async (task: TaskSummary) => {
    if (expandedTaskId === task.id) {
      setExpandedTaskId(null);
      return;
    }
    setExpandedTaskId(task.id);
    if (details[task.id]) return;

    setDetails((current) => ({ ...current, [task.id]: { ...emptyDetails(), loading: true } }));
    const auditRequest = getTaskAudit(task.id, pageSize, 0);
    const uploadRequest = task.kind === 'upload' ? getUploadStatus(task.id) : Promise.resolve(undefined);
    const agentRequest = task.kind === 'agent' ? getAgentTask(task.id) : Promise.resolve(undefined);
    const [auditResult, uploadResult, agentResult] = await Promise.allSettled([auditRequest, uploadRequest, agentRequest]);
    const next = emptyDetails();

    if (auditResult.status === 'fulfilled') {
      next.auditEvents = auditResult.value.events;
      next.auditOffset = auditResult.value.offset + auditResult.value.events.length;
      next.auditHasMore = auditResult.value.hasMore;
    } else {
      next.auditError = describeError(auditResult.reason, '无法读取审计记录。');
    }

    if (task.kind === 'upload') {
      if (uploadResult.status === 'fulfilled' && uploadResult.value) {
        next.uploadStatus = uploadResult.value;
      } else if (uploadResult.status === 'rejected') {
        next.uploadError = describeError(uploadResult.reason, '无法读取上传状态。');
      }
    }
    if (task.kind === 'agent') {
      if (agentResult.status === 'fulfilled' && agentResult.value) {
        next.agentPlan = agentResult.value;
      } else if (agentResult.status === 'rejected') {
        next.agentError = describeError(agentResult.reason, '无法读取 Agent 操作清单。');
      }
    }
    setDetails((current) => ({ ...current, [task.id]: next }));
  };

  const loadMoreAudit = async (taskId: string) => {
    const current = details[taskId];
    if (!current || current.auditLoadingMore) return;
    setDetails((values) => ({
      ...values,
      [taskId]: { ...current, auditLoadingMore: true, auditError: '' },
    }));
    try {
      const page = await getTaskAudit(taskId, pageSize, current.auditOffset);
      setDetails((values) => {
        const latest = values[taskId] ?? emptyDetails();
        return {
          ...values,
          [taskId]: {
            ...latest,
            auditEvents: [...latest.auditEvents, ...page.events],
            auditOffset: page.offset + page.events.length,
            auditHasMore: page.hasMore,
            auditLoadingMore: false,
          },
        };
      });
    } catch (requestError) {
      setDetails((values) => ({
        ...values,
        [taskId]: {
          ...(values[taskId] ?? emptyDetails()),
          auditLoadingMore: false,
          auditError: describeError(requestError, '无法读取更多审计记录。'),
        },
      }));
    }
  };

  return (
    <section className="tasks-page" aria-labelledby="tasks-page-title">
      <header className="tasks-heading">
        <div>
          <h1 id="tasks-page-title">任务</h1>
        </div>
        <button className="tasks-refresh" type="button" onClick={refresh} disabled={view === 'tasks' && loading}>
          <RefreshCw size={15} aria-hidden="true" />
          <span>刷新</span>
        </button>
      </header>

      <div className="tasks-view-tabs" role="tablist" aria-label="工作记录类型">
        <button
          id="tasks-view-tab"
          className="tasks-view-tab"
          type="button"
          role="tab"
          aria-selected={view === 'tasks'}
          aria-controls="tasks-view-panel"
          tabIndex={view === 'tasks' ? 0 : -1}
          onKeyDown={handleViewTabKeyDown}
          onClick={() => setView('tasks')}
        >任务</button>
        <button
          id="history-view-tab"
          className="tasks-view-tab"
          type="button"
          role="tab"
          aria-selected={view === 'history'}
          aria-controls="tasks-view-panel"
          tabIndex={view === 'history' ? 0 : -1}
          onKeyDown={handleViewTabKeyDown}
          onClick={() => setView('history')}
        >操作记录</button>
      </div>

      <div
        id="tasks-view-panel"
        className="tasks-view-panel"
        role="tabpanel"
        aria-labelledby={view === 'tasks' ? 'tasks-view-tab' : 'history-view-tab'}
        tabIndex={0}
      >
      {view === 'tasks' ? <>
      <div className="tasks-filters" aria-label="筛选任务">
        <label>
          <span>任务类型</span>
          <select value={kindFilter} onChange={(event) => {
            invalidateTaskListRequests();
            setKindFilter(event.target.value);
          }}>
            <option value="">全部类型</option>
            {kindOptions.map((kind) => <option value={kind} key={kind}>{taskKindLabel(kind)}</option>)}
          </select>
        </label>
        <label>
          <span>任务状态</span>
          <select value={statusFilter} onChange={(event) => {
            invalidateTaskListRequests();
            setStatusFilter(event.target.value as TaskStatus | '');
          }}>
            <option value="">全部状态</option>
            {taskStatusOptions.map(({ value, label }) => <option value={value} key={value}>{label}</option>)}
          </select>
        </label>
      </div>

      {error && (
        <div className="tasks-error" role="alert">
          <AlertTriangle size={17} aria-hidden="true" />
          <p>{error}</p>
          <button type="button" onClick={refresh}>重试</button>
        </div>
      )}

      {loading ? (
        <div className="tasks-loading" role="status">正在读取已保存的任务…</div>
      ) : error ? null : tasks.length > 0 ? (
        <>
          <ul className="tasks-list" aria-label="任务记录">
            {tasks.map((task) => {
              const Icon = taskKindIcons[task.kind] ?? Clock3;
              const isExpanded = expandedTaskId === task.id;
              const detailId = `task-details-${encodeURIComponent(task.id)}`;
              const taskDetails = details[task.id];
              const tone = taskStatusTone(task.status);

              return (
                <li key={task.id} className={`tasks-item tasks-item--${tone}`}>
                  <div className="tasks-item-summary">
                    <span className={`tasks-kind-icon tasks-kind-icon--${task.kind}`} aria-hidden="true">
                      <Icon size={18} strokeWidth={1.9} />
                    </span>
                    <div className="tasks-item-main">
                      <div className="tasks-title-row">
                        <h2>{taskKindLabel(task.kind)}</h2>
                        <span className={`tasks-status tasks-status--${tone}`}>
                          {task.status === 'succeeded' && <CheckCircle2 size={13} aria-hidden="true" />}
                          {(task.status === 'failed' || task.status === 'partial') && <AlertTriangle size={13} aria-hidden="true" />}
                          {taskStatusLabels[task.status] ?? task.status}
                        </span>
                      </div>
                      {task.scopePath && (
                        <div className="tasks-path-row">
                          <span className="tasks-field-label">范围</span>
                          <code>{task.scopePath}</code>
                        </div>
                      )}
                      <p className="tasks-updated">最近更新 · {formatDate(task.updatedAt)}</p>
                    </div>
                    <button
                      className="tasks-details-button"
                      type="button"
                      aria-expanded={isExpanded}
                      aria-controls={detailId}
                      aria-label={`${isExpanded ? '收起' : '查看'}“${taskKindLabel(task.kind)}”任务详情`}
                      onClick={() => void openTaskDetails(task)}
                    >
                      <span>{isExpanded ? '收起详情' : '查看详情'}</span>
                      <ChevronDown size={17} aria-hidden="true" />
                    </button>
                  </div>

                  {isExpanded && (
                    <div className="tasks-details" id={detailId}>
                      {task.kind === 'upload' ? (
                        <UploadDetails status={taskDetails?.uploadStatus} error={taskDetails?.uploadError ?? ''} loading={taskDetails?.loading ?? true} />
                      ) : task.kind === 'agent' ? (
                        <AgentExecutionDetails plan={taskDetails?.agentPlan} error={taskDetails?.agentError ?? ''} loading={taskDetails?.loading ?? true} />
                      ) : task.kind === 'duplicate_scan' ? (
                        <DuplicateScanDetails scanId={task.id} />
                      ) : (
                        <p className="tasks-unavailable-note">此类任务暂无详细进度。</p>
                      )}

                      <details className="tasks-technical-info">
                        <summary>任务信息</summary>
                        <dl className="tasks-detail-grid">
                          <div className="tasks-detail-row"><dt>任务 ID</dt><dd><code>{task.id}</code></dd></div>
                          <div className="tasks-detail-row"><dt>创建时间</dt><dd>{formatDate(task.createdAt)}</dd></div>
                        </dl>
                      </details>

                      <details className="tasks-audit" aria-label="历史审计">
                        <summary>
                          <span>历史审计</span>
                          <span className="tasks-audit-count">
                            {taskDetails?.loading ? '读取中' : taskDetails?.auditError ? '读取失败' : `${taskDetails?.auditEvents.length ?? 0} 条`}
                          </span>
                        </summary>
                        {taskDetails?.loading && <p className="tasks-detail-loading">正在读取审计记录…</p>}
                        {taskDetails?.auditError && <p className="tasks-detail-error" role="alert">{taskDetails.auditError}</p>}
                        {!taskDetails?.loading && !taskDetails?.auditError && taskDetails?.auditEvents.length === 0 && (
                          <p className="tasks-empty-audit">没有已保存的审计事件。</p>
                        )}
                        {taskDetails?.auditEvents.length ? (
                          <ol className="tasks-audit-list">
                            {taskDetails.auditEvents.map((event) => (
                              <li key={event.id}>
                                <div className="tasks-audit-heading">
                                  <strong>{auditEventLabel(event.eventType)}</strong>
                                  <time dateTime={event.createdAt}>{formatDate(event.createdAt)}</time>
                                </div>
                                <p>{auditActorLabel(event.actor)} · {auditOutcomeLabel(event.outcome)}</p>
                                {event.message && <p className="tasks-audit-message">{event.message}</p>}
                                {(event.sourcePath || event.targetPath) && (
                                  <dl className="tasks-audit-paths">
                                    {event.sourcePath && <div><dt>来源</dt><dd>{event.sourcePath}</dd></div>}
                                    {event.targetPath && <div><dt>目标</dt><dd>{event.targetPath}</dd></div>}
                                  </dl>
                                )}
                              </li>
                            ))}
                          </ol>
                        ) : null}
                        {taskDetails?.auditHasMore && (
                          <button className="tasks-more-audit" type="button" onClick={() => void loadMoreAudit(task.id)} disabled={taskDetails.auditLoadingMore}>
                            {taskDetails.auditLoadingMore ? '正在读取…' : '查看更多审计'}
                          </button>
                        )}
                      </details>
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
          {loadMoreError && <p className="tasks-detail-error" role="alert">{loadMoreError}</p>}
          {hasMore && (
            <button className="tasks-load-more" type="button" onClick={() => void loadMore()} disabled={loadingMore}>
              {loadingMore ? '正在读取…' : '加载更多任务'}
            </button>
          )}
        </>
      ) : (
        <div className="tasks-empty-state">
          <Info size={20} aria-hidden="true" />
          <p>{kindFilter || statusFilter ? '没有匹配当前筛选条件的任务。' : '还没有已保存的任务记录。'}</p>
        </div>
      )}
      </> : <OperationHistory refreshToken={historyRefreshToken} />}
      </div>
    </section>
  );
}

// 将操作状态复用任务状态色调。
function auditOutcomeTone(outcome: string): string {
  if (outcome === 'succeeded' || outcome === 'completed' || outcome === 'recovered_success' || outcome === 'trashed' || outcome === 'restored' || outcome === 'purged') return 'completed';
  if (outcome === 'failed' || outcome === 'partial' || outcome === 'manual_review' || outcome === 'needs_attention' || outcome === 'invalidated') return 'attention';
  if (outcome === 'awaiting_approval' || outcome === 'pending' || outcome === 'approved' || outcome === 'rejected' || outcome === 'retry_ready') return 'waiting';
  if (outcome === 'started' || outcome === 'running' || outcome === 'moving' || outcome === 'restoring' || outcome === 'purging') return 'active';
  return 'neutral';
}

// 展示游标分页的文件操作记录，并在刷新或卸载时取消请求。
function OperationHistory({ refreshToken }: { refreshToken: number }) {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [cursor, setCursor] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState('');
  const [loadMoreError, setLoadMoreError] = useState('');
  const [retryToken, setRetryToken] = useState(0);
  const requestVersion = useRef(0);
  const loadMoreController = useRef<AbortController | null>(null);

  useEffect(() => {
    const version = ++requestVersion.current;
    const controller = new AbortController();
    loadMoreController.current?.abort();
    loadMoreController.current = null;
    setEvents([]);
    setCursor(0);
    setHasMore(false);
    setLoading(true);
    setLoadingMore(false);
    setError('');
    setLoadMoreError('');

    getAuditHistory(pageSize, undefined, controller.signal)
      .then((page) => {
        if (version !== requestVersion.current) return;
        setEvents(page.events);
        setCursor(page.nextCursor);
        setHasMore(page.hasMore);
      })
      .catch((requestError: unknown) => {
        if (version === requestVersion.current && !(requestError instanceof DOMException && requestError.name === 'AbortError')) {
          setError(describeError(requestError, '无法读取操作记录。'));
        }
      })
      .finally(() => {
        if (version === requestVersion.current && !controller.signal.aborted) setLoading(false);
      });

    return () => {
      controller.abort();
      if (version === requestVersion.current) {
        requestVersion.current += 1;
        loadMoreController.current?.abort();
        loadMoreController.current = null;
      }
    };
  }, [refreshToken, retryToken]);

  const loadMore = async () => {
    if (loading || loadingMore || !hasMore || loadMoreController.current) return;
    const version = requestVersion.current;
    const controller = new AbortController();
    loadMoreController.current = controller;
    setLoadingMore(true);
    setLoadMoreError('');
    try {
      const page = await getAuditHistory(pageSize, cursor, controller.signal);
      if (version !== requestVersion.current || controller.signal.aborted) return;
      setEvents((current) => {
        const knownIds = new Set(current.map((event) => event.id));
        return [...current, ...page.events.filter((event) => !knownIds.has(event.id))];
      });
      setCursor(page.nextCursor);
      setHasMore(page.hasMore);
    } catch (requestError) {
      if (version === requestVersion.current && !(requestError instanceof DOMException && requestError.name === 'AbortError')) {
        setLoadMoreError(describeError(requestError, '无法读取更多操作记录。'));
      }
    } finally {
      if (loadMoreController.current === controller) loadMoreController.current = null;
      if (version === requestVersion.current) setLoadingMore(false);
    }
  };

  if (loading) return <div className="tasks-loading" role="status">正在读取操作记录…</div>;
  if (error) {
    return (
      <div className="tasks-history-error" role="alert">
        <AlertTriangle size={18} aria-hidden="true" />
        <p>{error}</p>
        <button className="tasks-history-button" type="button" onClick={() => setRetryToken((value) => value + 1)}>重试</button>
      </div>
    );
  }
  return (
    <section className="tasks-history" aria-label="文件操作记录">
      {events.length > 0 ? (
        <ol className="tasks-history-list">
          {events.map((event) => (
            <li className="tasks-history-item" key={event.id}>
              <div className="tasks-history-heading">
                <div className="tasks-history-title">
                  <strong>{auditEventLabel(event.eventType)}</strong>
                  <span className={`tasks-status tasks-status--${auditOutcomeTone(event.outcome)}`}>{auditOutcomeLabel(event.outcome)}</span>
                </div>
                <time dateTime={event.createdAt}>{formatDate(event.createdAt)}</time>
              </div>
              <p className="tasks-history-actor">{auditActorLabel(event.actor)}</p>
              {(event.sourcePath || event.targetPath) && (
                <dl className="tasks-history-paths">
                  {event.sourcePath && <div><dt>来源</dt><dd>{event.sourcePath}</dd></div>}
                  {event.targetPath && <div><dt>目标</dt><dd>{event.targetPath}</dd></div>}
                </dl>
              )}
            </li>
          ))}
        </ol>
      ) : (
        <div className="tasks-empty-state">
          <Info size={20} aria-hidden="true" />
          <p>暂无操作记录。</p>
        </div>
      )}
      {loadMoreError && (
        <div className="tasks-history-load-error" role="alert">
          <p>{loadMoreError}</p>
          <button className="tasks-history-button" type="button" onClick={() => void loadMore()}>重试</button>
        </div>
      )}
      {!loadMoreError && hasMore && (
        <button className="tasks-history-button tasks-history-load-more" type="button" onClick={() => void loadMore()} disabled={loadingMore}>
          {loadingMore ? '正在读取…' : '加载更多记录'}
        </button>
      )}
    </section>
  );
}

function UploadDetails({ status, error, loading }: { status?: UploadStatus; error: string; loading: boolean }) {
  return (
    <section className="tasks-upload-panel" aria-label="上传状态">
      <h3>上传状态</h3>
      {loading && <p className="tasks-detail-loading">正在读取已保存的上传状态…</p>}
      {error && <p className="tasks-detail-error" role="alert">{error}</p>}
      {status && (
        <dl className="tasks-detail-grid">
          <div className="tasks-detail-row"><dt>上传阶段</dt><dd>{uploadStatusLabels[status.status] ?? status.status}</dd></div>
          <div className="tasks-detail-row"><dt>已接收 / 总大小</dt><dd>{formatBytes(status.offset)} / {formatBytes(status.size)}</dd></div>
          <div className="tasks-detail-row"><dt>目标位置</dt><dd>{status.targetPath || '未记录'}</dd></div>
          <div className="tasks-detail-row"><dt>最终路径</dt><dd>{status.publishedPath || '尚无最终路径记录'}</dd></div>
          {status.candidatePath && <div className="tasks-detail-row"><dt>候选路径</dt><dd>{status.candidatePath}</dd></div>}
          {status.error && <div className="tasks-detail-row"><dt>错误说明</dt><dd className="tasks-upload-error">{status.error}</dd></div>}
          <div className="tasks-detail-row"><dt>状态更新时间</dt><dd>{formatDate(status.updatedAt)}</dd></div>
        </dl>
      )}
    </section>
  );
}

const agentActionLabels: Readonly<Record<string, string>> = {
  archive_photo: '归档照片',
  classify_documents: '分类文件',
  copy_directory: '复制文件夹',
  copy_file: '复制文件',
  create_archive: '创建压缩包',
  create_directory: '新建文件夹',
  create_file: '新建文件',
  create_symlink: '创建软链接',
  edit_text: '编辑内容',
  extract_archive: '解压文件',
  move_file: '移动文件',
  overwrite_file: '覆盖文件',
  permanent_delete: '永久删除',
  rename: '重命名',
  run_shell: 'Bash',
  set_owner: '修改属主',
  set_permissions: '修改权限',
  trash: '移入回收站',
  trash_duplicate: '清理重复文件',
};

function agentActionLabel(operation: AgentOperation): string {
  if (operation.action === 'move_file' && isSameDirectoryPath(operation.sourcePath, operation.targetPath)) {
    return '重命名';
  }
  return agentActionLabels[operation.action] ?? '文件操作';
}

// 渲染 Agent 计划步骤与执行结果。
function AgentExecutionDetails({ plan, error, loading }: { plan?: AgentPlan; error: string; loading: boolean }) {
  const shellOnly = plan !== undefined && plan.operations.length === 1 && plan.operations[0]?.action === 'run_shell';

  return (
    <section className={`tasks-agent-panel${shellOnly ? ' is-shell-only' : ''}`} aria-label="Agent 操作结果">
      {!shellOnly && <h3>操作清单</h3>}
      {loading && <p className="tasks-detail-loading">正在读取操作结果…</p>}
      {error && <p className="tasks-detail-error" role="alert">{error}</p>}
      {plan && plan.operations.length === 0 && <p className="tasks-unavailable-note">此任务没有文件操作项。</p>}
      {plan && plan.operations.length > 0 && (
        <ol className="tasks-agent-list">
          {plan.operations.map((operation) => (
            <AgentOperationDetails
              key={operation.id}
              operation={operation}
              planId={plan.planId}
              planVersion={plan.planVersion}
              showShellStatus={plan.operations.length > 1}
            />
          ))}
        </ol>
      )}
    </section>
  );
}

// 渲染单项 Agent 操作结果，不提供写入入口。
function AgentOperationDetails({ operation, planId, planVersion, showShellStatus }: { operation: AgentOperation; planId: string; planVersion: number; showShellStatus: boolean }) {
  const isShell = operation.action === 'run_shell';
  const shellScope = [operation.sourcePath, operation.workingDir && operation.workingDir !== '.' ? operation.workingDir : '']
    .filter(Boolean)
    .join('/') || '管理根目录';

  return (
    <li className="tasks-agent-item">
      {!isShell && <div className="tasks-agent-heading">
        <strong>{agentActionLabel(operation)}</strong>
        <span className={`tasks-status tasks-status--${auditOutcomeTone(operation.executionStatus)}`}>{auditOutcomeLabel(operation.executionStatus)}</span>
      </div>}
      {!isShell && (operation.sourcePath || operation.targetPath) && (
        <p className="tasks-agent-paths">
          {operation.sourcePath && <code>{operation.sourcePath}</code>}
          {operation.sourcePath && operation.targetPath && operation.sourcePath !== operation.targetPath && <span aria-hidden="true">→</span>}
          {operation.targetPath && operation.targetPath !== operation.sourcePath && <code>{operation.targetPath}</code>}
        </p>
      )}
      {!isShell && <p className="tasks-agent-approval">审批：{auditOutcomeLabel(operation.approval)}</p>}
      {!isShell && operation.reason && <p className="tasks-agent-reason">{operation.reason}</p>}
      {operation.resultMessage && (!isShell || operation.executionStatus !== 'succeeded') && (
        <p className="tasks-agent-result" role={operation.executionStatus === 'failed' || operation.executionStatus === 'partial' ? 'alert' : 'status'}>{operation.resultMessage}</p>
      )}
      {isShell && (
        <ShellToolCall
          command={operation.command ?? ''}
          resultMessage={operation.resultMessage ?? ''}
          stdout={operation.stdout ?? ''}
          stderr={operation.stderr ?? ''}
          outputDiscarded={operation.outputDiscarded ?? false}
          outputTruncated={operation.outputTruncated ?? false}
          exitCode={operation.exitCode}
          executionStatus={operation.executionStatus}
          scope={shellScope}
          showStatus={showShellStatus}
        >
          <ShellChangeReview
            planId={planId}
            planVersion={planVersion}
            executionStatus={operation.executionStatus}
          />
        </ShellToolCall>
      )}
    </li>
  );
}

// 展示重复扫描进度；临时读取失败时有限重试。
function DuplicateScanDetails({ scanId }: { scanId: string }) {
  const [scan, setScan] = useState<DuplicateScan | null>(null);
  const [error, setError] = useState('');
  const [retryToken, setRetryToken] = useState(0);

  useEffect(() => {
    let active = true;
    let timer: number | undefined;
    let failures = 0;
    const controller = new AbortController();

    const schedule = (delay: number) => {
      timer = window.setTimeout(() => { void readStatus(); }, delay);
    };
    const readStatus = async () => {
      try {
        const result = await getDuplicateScan(scanId, controller.signal);
        if (!active) return;
        setScan(result);
        setError('');
        failures = 0;
        if (result.status === 'queued' || result.status === 'running') schedule(1500);
      } catch (requestError) {
        if (!active || controller.signal.aborted) return;
        setError(describeError(requestError, '无法读取扫描进度。'));
        failures += 1;
        if (failures <= 3) schedule(1500 * 2 ** (failures - 1));
      }
    };

    void readStatus();
    return () => {
      active = false;
      controller.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [retryToken, scanId]);

  return (
    <section className="tasks-agent-panel" aria-label="重复文件扫描结果">
      <h3>扫描结果</h3>
      {!scan && !error && <p className="tasks-detail-loading" role="status">正在读取扫描状态…</p>}
      {error && (
        <div className="tasks-scan-error" role="alert">
          <p>{error}</p>
          <button className="tasks-history-button tasks-scan-retry" type="button" onClick={() => { setScan(null); setError(''); setRetryToken((value) => value + 1); }}>重试</button>
        </div>
      )}
      {scan && (
        <dl className="tasks-detail-grid">
          <div className="tasks-detail-row"><dt>扫描状态</dt><dd>{duplicateScanStatusLabels[scan.status]}</dd></div>
          <div className="tasks-detail-row"><dt>扫描范围</dt><dd>{scan.scopePath || '管理根目录'}</dd></div>
          <div className="tasks-detail-row"><dt>处理进度</dt><dd>{duplicateScanProgress(scan)}</dd></div>
          <div className="tasks-detail-row"><dt>重复文件</dt><dd>{scan.groupCount.toLocaleString('zh-CN')} 组 · {scan.duplicateFileCount.toLocaleString('zh-CN')} 个副本</dd></div>
          <div className="tasks-detail-row"><dt>可释放空间估算</dt><dd>{formatBytes(scan.reclaimableBytes)}</dd></div>
          <div className="tasks-detail-row"><dt>状态更新时间</dt><dd>{formatDate(scan.updatedAt)}</dd></div>
        </dl>
      )}
    </section>
  );
}

const duplicateScanStatusLabels: Readonly<Record<DuplicateScan['status'], string>> = {
  cancelled: '已取消',
  failed: '未完成',
  queued: '排队中',
  running: '扫描中',
  succeeded: '已完成',
};

// 按当前阶段生成扫描进度说明。
function duplicateScanProgress(scan: DuplicateScan): string {
  if (scan.phase === 'enumerating') {
    return `已检查 ${scan.entriesVisited.toLocaleString('zh-CN')} 项，发现 ${scan.candidateFiles.toLocaleString('zh-CN')} 个候选文件`;
  }
  if (scan.status === 'running' && scan.filesHashed >= scan.candidateFiles) return '正在复核文件状态';
  return `已核对 ${scan.filesHashed.toLocaleString('zh-CN')} / ${scan.candidateFiles.toLocaleString('zh-CN')} 个文件 · ${formatBytes(scan.bytesHashed)} / ${formatBytes(scan.totalBytes)}`;
}
