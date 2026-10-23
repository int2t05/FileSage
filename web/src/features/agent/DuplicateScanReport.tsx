// 展示只读重复扫描状态与分组结果。
import { useEffect, useState } from 'react';
import {
  AlertTriangle,
  ChevronDown,
  Loader2,
  RefreshCw,
  X,
} from 'lucide-react';
import {
  cancelDuplicateScan,
  createDuplicateCleanupPlan,
  getDuplicateScan,
  getDuplicateScanFiles,
  getDuplicateScanGroups,
  type DuplicateScan,
  type DuplicateScanFilePage,
  type DuplicateScanGroup,
} from '../../api/duplicateScans';

import './DuplicateScanReport.css';

const GROUP_PAGE_SIZE = 20;
const FILE_PAGE_SIZE = 100;

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let size = value;
  let unit = -1;
  do {
    size /= 1024;
    unit += 1;
  } while (size >= 1024 && unit < units.length - 1);
  return `${size.toLocaleString('zh-CN', { maximumFractionDigits: 1 })} ${units[unit]}`;
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return '修改时间未知';
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function scanTitle(scan: DuplicateScan): string {
  if (scan.status === 'queued') return '等待扫描';
  if (scan.status === 'running') {
    if (scan.phase === 'enumerating') return '正在读取目录';
    return scan.filesHashed >= scan.candidateFiles ? '正在复核扫描范围' : '正在核对文件内容';
  }
  if (scan.status === 'succeeded') return '扫描完成';
  if (scan.status === 'cancelled') return '扫描已取消';
  return '扫描未完成';
}

function failureText(errorCode: string): string {
  const messages: Record<string, string> = {
    file_changed: '扫描范围内的文件或目录发生变化，结果已丢弃。',
    scan_limit_exceeded: '扫描范围超过当前上限，请缩小范围后重试。',
    filesystem_identity_unavailable: '无法读取文件系统稳定标识，扫描已停止。',
    invalid_scope: '扫描范围不存在或不可访问，请检查范围后重试。',
    storage_failed: '无法保存扫描结果，部分结果已丢弃。',
    service_restarted: '服务重启时扫描已中断，请重新发起。',
    scan_failed: '扫描未能完成，请刷新状态或重新发起。',
  };
  return messages[errorCode] ?? '扫描未能完成，请刷新状态或重新发起。';
}

function progressLabel(scan: DuplicateScan): string {
  if (scan.phase === 'enumerating') return `已检查 ${scan.entriesVisited.toLocaleString('zh-CN')} 项 · 找到 ${scan.candidateFiles.toLocaleString('zh-CN')} 个候选文件`;
  if (scan.filesHashed >= scan.candidateFiles) return `已核对 ${scan.filesHashed.toLocaleString('zh-CN')} 个候选文件 · 正在复核目录状态`;
  return `已核对 ${scan.filesHashed.toLocaleString('zh-CN')} / ${scan.candidateFiles.toLocaleString('zh-CN')} 个文件`;
}

interface DuplicateScanReportProps {
  scanId: string;
  onTaskCreated(taskId: string): void;
}

export function DuplicateScanReport({ scanId, onTaskCreated }: DuplicateScanReportProps) {
  const [scan, setScan] = useState<DuplicateScan | null>(null);
  const [scanLoading, setScanLoading] = useState(true);
  const [scanError, setScanError] = useState('');
  const [scanRefresh, setScanRefresh] = useState(0);
  const [cancelBusy, setCancelBusy] = useState(false);
  const [cancelError, setCancelError] = useState('');
  const [groups, setGroups] = useState<DuplicateScanGroup[]>([]);
  const [groupsHasMore, setGroupsHasMore] = useState(false);
  const [groupsLoading, setGroupsLoading] = useState(false);
  const [groupsError, setGroupsError] = useState('');
  const [groupsRefresh, setGroupsRefresh] = useState(0);
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set());
  const [filePages, setFilePages] = useState<Record<string, DuplicateScanFilePage>>({});
  const [fileLoading, setFileLoading] = useState('');
  const [fileErrors, setFileErrors] = useState<Record<string, string>>({});
  const [keepers, setKeepers] = useState<Record<string, string>>({});
  const [trashSelections, setTrashSelections] = useState<Record<string, string[]>>({});
  const [planBusy, setPlanBusy] = useState('');
  const [planErrors, setPlanErrors] = useState<Record<string, string>>({});

  useEffect(() => {
    let active = true;
    let timer = 0;
    const controller = new AbortController();
    setScanLoading(true);
    setScanError('');

    const refresh = async () => {
      try {
        const latest = await getDuplicateScan(scanId, controller.signal);
        if (!active) return;
        setScan(latest);
        setScanError('');
        if (latest.status === 'queued' || latest.status === 'running') {
          timer = window.setTimeout(() => { void refresh(); }, 1200);
        }
      } catch (error) {
        if (active) setScanError(errorMessage(error, '读取扫描状态失败。'));
      } finally {
        if (active) setScanLoading(false);
      }
    };

    void refresh();
    return () => {
      active = false;
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [scanId, scanRefresh]);

  useEffect(() => {
    if (scan?.status !== 'succeeded') return;
    const controller = new AbortController();
    setGroupsLoading(true);
    setGroupsError('');
    setGroups([]);
    setExpandedGroups(new Set());
    setFilePages({});
    setFileErrors({});
    getDuplicateScanGroups(scanId, GROUP_PAGE_SIZE, 0, controller.signal)
      .then((page) => {
        if (controller.signal.aborted) return;
        setGroups(page.groups);
        setGroupsHasMore(page.hasMore);
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) setGroupsError(errorMessage(error, '读取重复组失败。'));
      })
      .finally(() => {
        if (!controller.signal.aborted) setGroupsLoading(false);
      });
    return () => controller.abort();
  }, [scan?.status, scanId, groupsRefresh]);

  async function loadMoreGroups() {
    if (groupsLoading) return;
    setGroupsLoading(true);
    setGroupsError('');
    try {
      const page = await getDuplicateScanGroups(scanId, GROUP_PAGE_SIZE, groups.length);
      setGroups((items) => [...items, ...page.groups]);
      setGroupsHasMore(page.hasMore);
    } catch (error) {
      setGroupsError(errorMessage(error, '读取更多重复组失败。'));
    } finally {
      setGroupsLoading(false);
    }
  }

  async function loadFiles(group: DuplicateScanGroup, offset = 0) {
    if (fileLoading) return;
    setFileLoading(group.id);
    setFileErrors((items) => ({ ...items, [group.id]: '' }));
    try {
      const page = await getDuplicateScanFiles(scanId, group.id, FILE_PAGE_SIZE, offset);
      setFilePages((items) => {
        const previous = items[group.id];
        return {
          ...items,
          [group.id]: offset === 0 || !previous
            ? page
            : { ...page, files: [...previous.files, ...page.files] },
        };
      });
    } catch (error) {
      setFileErrors((items) => ({ ...items, [group.id]: errorMessage(error, '读取文件列表失败。') }));
    } finally {
      setFileLoading('');
    }
  }

  async function toggleGroup(group: DuplicateScanGroup) {
    const willExpand = !expandedGroups.has(group.id);
    setExpandedGroups((items) => {
      const next = new Set(items);
      if (willExpand) next.add(group.id);
      else next.delete(group.id);
      return next;
    });
    if (willExpand && !filePages[group.id]) await loadFiles(group);
  }

  async function cancelScan() {
    setCancelBusy(true);
    setCancelError('');
    try {
      await cancelDuplicateScan(scanId);
      setScanRefresh((value) => value + 1);
    } catch (error) {
      setCancelError(errorMessage(error, '取消扫描失败。'));
    } finally {
      setCancelBusy(false);
    }
  }

  function chooseKeeper(groupId: string, path: string) {
    setKeepers((items) => ({ ...items, [groupId]: path }));
    setTrashSelections((items) => ({ ...items, [groupId]: (items[groupId] ?? []).filter((selected) => selected !== path) }));
  }

  function chooseTrashPath(groupId: string, path: string, checked: boolean) {
    setTrashSelections((items) => {
      const selected = new Set(items[groupId] ?? []);
      if (checked) selected.add(path);
      else selected.delete(path);
      return { ...items, [groupId]: [...selected] };
    });
  }

  async function createCleanupPlan(group: DuplicateScanGroup) {
    const keeperPath = keepers[group.id] ?? '';
    const trashPaths = trashSelections[group.id] ?? [];
    if (!keeperPath || trashPaths.length < 1 || planBusy) return;
    setPlanBusy(group.id);
    setPlanErrors((items) => ({ ...items, [group.id]: '' }));
    try {
      const created = await createDuplicateCleanupPlan(scanId, group.id, keeperPath, trashPaths);
      onTaskCreated(created.taskId);
    } catch (error) {
      setPlanErrors((items) => ({ ...items, [group.id]: errorMessage(error, '生成执行计划失败。') }));
    } finally {
      setPlanBusy('');
    }
  }

  const active = scan?.status === 'queued' || scan?.status === 'running';
  const progress = scan && scan.phase === 'hashing' && scan.totalBytes > 0
    ? Math.min(100, Math.round((scan.bytesHashed / scan.totalBytes) * 100))
    : undefined;

  return (
    <section className="agent-duplicate-report" aria-labelledby="agent-duplicate-report-title">
      <header className="agent-duplicate-report-heading">
        <div>
          <h2 id="agent-duplicate-report-title">重复文件</h2>
          {scan && <p>{scan.scopePath || '根目录'}</p>}
        </div>
        {active && (
          <button className="agent-duplicate-cancel" type="button" onClick={() => { void cancelScan(); }} disabled={cancelBusy}>
            {cancelBusy ? <Loader2 className="agent-spinner" size={15} aria-hidden="true" /> : <X size={15} aria-hidden="true" />}
            取消
          </button>
        )}
      </header>

      {!scan && scanLoading && (
        <div className="agent-duplicate-state" role="status"><Loader2 className="agent-spinner" size={16} aria-hidden="true" />正在读取扫描状态…</div>
      )}
      {scanError && (
        <div className="agent-duplicate-error" role="alert"><AlertTriangle size={15} aria-hidden="true" /><span>{scanError}</span>
          <button className="agent-text-button" type="button" onClick={() => setScanRefresh((value) => value + 1)}>重试</button>
        </div>
      )}
      {scan && (
        <>
          <div className={`agent-duplicate-status is-${scan.status}`} role={active ? 'status' : undefined}>
            {active ? <Loader2 className="agent-spinner" size={16} aria-hidden="true" /> : scan.status === 'failed' ? <AlertTriangle size={16} aria-hidden="true" /> : null}
            <strong>{scanTitle(scan)}</strong>
            {active && <span>{progressLabel(scan)}</span>}
            {progress !== undefined && <span>{progress}%</span>}
          </div>
          {active && progress !== undefined && (
            <progress className="agent-duplicate-progress" max={100} value={progress} aria-label="文件内容核对进度" />
          )}
          {active && scan.phase === 'hashing' && progress === undefined && (
            <progress className="agent-duplicate-progress" aria-label="文件内容核对进度" />
          )}
          {cancelError && <p className="agent-duplicate-inline-error" role="alert">{cancelError}</p>}
          {scan.status === 'failed' && (
            <div className="agent-duplicate-failure">
              <p>{failureText(scan.errorCode)}</p>
              <button className="agent-secondary-button" type="button" onClick={() => setScanRefresh((value) => value + 1)}>
                <RefreshCw size={14} aria-hidden="true" />刷新状态
              </button>
            </div>
          )}
          {scan.status === 'cancelled' && <p className="agent-duplicate-muted">扫描已停止，不完整结果不会展示。</p>}
          {scan.status === 'succeeded' && (
            <>
              <dl className="agent-duplicate-summary">
                <div><dt>重复组</dt><dd>{scan.groupCount.toLocaleString('zh-CN')}</dd></div>
                <div><dt>文件</dt><dd>{scan.duplicateFileCount.toLocaleString('zh-CN')}</dd></div>
                <div><dt>可释放空间估算</dt><dd>{formatBytes(scan.reclaimableBytes)}</dd></div>
              </dl>
              <p className="agent-duplicate-muted">先选择保留文件，再勾选要移入回收站的副本；执行后可恢复。</p>
              {scan.groupCount === 0 ? (
                <p className="agent-duplicate-empty">没有发现内容完全相同的文件。</p>
              ) : groupsLoading && groups.length === 0 ? (
                <div className="agent-duplicate-state" role="status"><Loader2 className="agent-spinner" size={15} aria-hidden="true" />正在读取重复组…</div>
              ) : groupsError ? (
                <div className="agent-duplicate-error" role="alert"><AlertTriangle size={15} aria-hidden="true" /><span>{groupsError}</span>
                  <button className="agent-text-button" type="button" onClick={() => setGroupsRefresh((value) => value + 1)}>重试</button>
                </div>
              ) : (
                <>
                  <div className="agent-duplicate-groups" aria-label="重复文件组">
                    {groups.map((group, index) => {
                      const expanded = expandedGroups.has(group.id);
                      const page = filePages[group.id];
                      const panelId = `agent-duplicate-files-${encodeURIComponent(group.id)}`;
                      return (
                        <article className="agent-duplicate-group" key={group.id}>
                          <button
                            className="agent-duplicate-group-toggle"
                            type="button"
                            aria-expanded={expanded}
                            aria-controls={panelId}
                            onClick={() => { void toggleGroup(group); }}
                          >
                            <span><strong>第 {index + 1} 组</strong><small>{group.duplicateCount} 个相同文件 · {formatBytes(group.size)} / 个</small></span>
                            <ChevronDown size={16} aria-hidden="true" />
                          </button>
                          {expanded && (
                            <div className="agent-duplicate-files" id={panelId}>
                              {fileLoading === group.id && !page && <div className="agent-duplicate-state" role="status"><Loader2 className="agent-spinner" size={14} aria-hidden="true" />正在读取文件…</div>}
                              {fileErrors[group.id] && (
                                <div className="agent-duplicate-error" role="alert"><AlertTriangle size={14} aria-hidden="true" /><span>{fileErrors[group.id]}</span>
                                  <button className="agent-text-button" type="button" onClick={() => { void loadFiles(group); }}>重试</button>
                                </div>
                              )}
                              {page?.files.map((file) => {
                                const keeperPath = keepers[group.id] ?? '';
                                const selected = trashSelections[group.id] ?? [];
                                const checked = selected.includes(file.path);
                                return (
                                  <div className="agent-duplicate-file" key={file.path}>
                                    <div className="agent-duplicate-file-choices">
                                      <label><input type="radio" name={`keeper-${scanId}-${group.id}`} checked={keeperPath === file.path} onChange={() => chooseKeeper(group.id, file.path)} />保留</label>
                                      <label><input type="checkbox" checked={checked} disabled={!keeperPath || keeperPath === file.path || !checked && selected.length >= 20} onChange={(event) => chooseTrashPath(group.id, file.path, event.currentTarget.checked)} />移入回收站</label>
                                    </div>
                                    <code>{file.path}</code>
                                    <small>{formatDate(file.modifiedAt)}</small>
                                  </div>
                                );
                              })}
                              {page?.hasMore && (
                                <button className="agent-text-button" type="button" onClick={() => { void loadFiles(group, page.files.length); }} disabled={fileLoading === group.id}>
                                  {fileLoading === group.id ? '正在读取…' : '加载更多文件'}
                                </button>
                              )}
                              <div className="agent-duplicate-cleanup">
                                <span>{(trashSelections[group.id] ?? []).length} 个副本已选择（最多 20 个）</span>
                                <button className="agent-secondary-button" type="button" onClick={() => { void createCleanupPlan(group); }} disabled={!keepers[group.id] || !(trashSelections[group.id]?.length) || Boolean(planBusy)}>
                                  {planBusy === group.id ? <Loader2 className="agent-spinner" size={14} aria-hidden="true" /> : null}
                                  {planBusy === group.id ? '正在核对…' : '生成执行计划'}
                                </button>
                              </div>
                              {planErrors[group.id] && <p className="agent-duplicate-inline-error" role="alert">{planErrors[group.id]}</p>}
                            </div>
                          )}
                        </article>
                      );
                    })}
                  </div>
                  {groupsError && <p className="agent-duplicate-inline-error" role="alert">{groupsError}</p>}
                  {groupsHasMore && (
                    <button className="agent-secondary-button agent-duplicate-more" type="button" onClick={() => { void loadMoreGroups(); }} disabled={groupsLoading}>
                      {groupsLoading ? <Loader2 className="agent-spinner" size={14} aria-hidden="true" /> : null}
                      {groupsLoading ? '正在读取…' : '加载更多重复组'}
                    </button>
                  )}
                </>
              )}
            </>
          )}
        </>
      )}
    </section>
  );
}
