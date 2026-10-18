// 批量重命名对话框先收集每项的新名称，再由服务端校验并执行。
import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Pencil, X } from 'lucide-react';
import { ApiError, batchRenameEntries, renameEntry, type BatchRenameItemInput, type BatchRenameResult } from '../../api/files';

interface BatchRenameDialogProps {
  items: BatchRenameItemInput[];
  onClose: () => void;
  onConflict: () => void;
  onComplete: (result: BatchRenameResult) => void;
}

function parentPath(path: string): string {
  const separator = path.lastIndexOf('/');
  return separator < 0 ? '' : path.slice(0, separator);
}

function pathWithName(path: string, name: string): string {
  const parent = parentPath(path);
  return parent ? `${parent}/${name}` : name;
}

function validateNames(items: BatchRenameItemInput[]): string {
  const targets = new Set<string>();
  const sources = new Set(items.map((item) => item.path));

  for (const [index, item] of items.entries()) {
    if (!item.version) return `「/${item.path}」缺少版本信息，请刷新后重试。`;
    if (item.name.length === 0 || item.name === '.' || item.name === '..'
      || item.name.includes('/') || item.name.includes('\\') || /^[a-zA-Z]:/.test(item.name)
      || /[\u0000-\u001f\u007f]/.test(item.name)) {
      return `第 ${index + 1} 项名称无效，请填写单个文件或文件夹名称。`;
    }
    if (new TextEncoder().encode(item.name).byteLength > 255) {
      return `第 ${index + 1} 项名称超过文件系统限制（255 字节）。`;
    }

    const targetPath = pathWithName(item.path, item.name);
    if (targets.has(targetPath)) return `多个项目不能重命名为同一路径：/${targetPath}`;
    targets.add(targetPath);
    if (targetPath !== item.path && sources.has(targetPath)) {
      return `目标名称已被本次选择中的其他项目占用：/${targetPath}`;
    }
  }

  return '';
}

function changedItems(items: BatchRenameItemInput[]): BatchRenameItemInput[] {
  return items.filter((item) => item.name !== item.path.slice(item.path.lastIndexOf('/') + 1));
}

export function BatchRenameDialog({ items, onClose, onConflict, onComplete }: BatchRenameDialogProps) {
  const [drafts, setDrafts] = useState(items);
  const [error, setError] = useState('');
  const [result, setResult] = useState<BatchRenameResult | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const dialogRef = useRef<HTMLElement>(null);
  const firstInputRef = useRef<HTMLInputElement>(null);
  const onCloseRef = useRef(onClose);
  const submittingRef = useRef(submitting);
  onCloseRef.current = onClose;
  submittingRef.current = submitting;

  useEffect(() => {
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    firstInputRef.current?.focus();

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape' && !submittingRef.current) {
        event.preventDefault();
        onCloseRef.current();
        return;
      }
      if (event.key !== 'Tab') return;

      const dialog = dialogRef.current;
      const focusable = dialog?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled)');
      if (!focusable?.length) return;
      const first = focusable.item(0);
      const last = focusable.item(focusable.length - 1);
      if (event.shiftKey && (document.activeElement === first || !dialog?.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !dialog?.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    }

    document.addEventListener('keydown', handleKeyDown);
    return () => {
      document.removeEventListener('keydown', handleKeyDown);
      previousFocus?.focus();
    };
  }, []);

  function updateName(path: string, name: string) {
    setDrafts((current) => current.map((item) => item.path === path ? { ...item, name } : item));
    setError('');
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (submitting || result) return;
    const validationError = validateNames(drafts);
    if (validationError) {
      setError(validationError);
      return;
    }

    const changes = changedItems(drafts);
    if (changes.length === 0) {
      setError('请至少修改一个名称。');
      return;
    }

    setSubmitting(true);
    setError('');
    try {
      let completed: BatchRenameResult;
      if (changes.length === 1) {
        const item = changes[0];
        if (!item) throw new ApiError('没有可重命名的项目。');
        const renamed = await renameEntry(item.path, item.version, item.name);
        completed = {
          status: 'completed',
          items: [{ path: item.path, targetPath: renamed.path, name: renamed.name, status: 'renamed' }],
        };
      } else {
        completed = await batchRenameEntries(changes);
      }
      setResult(completed);
      onComplete(completed);
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.status === 409) {
        onConflict();
      } else {
        setError(requestError instanceof ApiError ? requestError.message : '批量重命名时发生错误，请重试。');
      }
    } finally {
      setSubmitting(false);
    }
  }

  const validationError = validateNames(drafts);
  const changesCount = changedItems(drafts).length;

  return (
    <div className="files-name-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !submitting) onClose(); }}>
      <section ref={dialogRef} className="files-name-dialog files-batch-rename-dialog" role="dialog" aria-modal="true" aria-labelledby="files-batch-rename-title" aria-describedby="files-batch-rename-description" tabIndex={-1} aria-busy={submitting}>
        <header className="files-name-dialog-header">
          <div>
            <span className="files-name-dialog-icon"><Pencil size={17} aria-hidden="true" /></span>
            <h2 id="files-batch-rename-title">批量重命名</h2>
          </div>
          {!submitting && <button type="button" className="files-name-dialog-close" aria-label="关闭批量重命名" onClick={onClose}><X size={18} aria-hidden="true" /></button>}
        </header>
        <p className="files-name-dialog-description" id="files-batch-rename-description">
          逐项填写新名称。目标已存在时不会覆盖；名称交换需分两次操作。
        </p>

        {result ? (
          <div className="files-batch-rename-result" role="status" aria-live="polite">
            <strong>{result.status === 'completed' ? '重命名已完成' : '重命名部分完成，请核对'}</strong>
            <ul>
              {result.items.map((item) => (
                <li key={item.path}>
                  <span>/{item.path}</span>
                  <small>{item.status === 'renamed' ? `已改为 /${item.targetPath}` : item.status === 'failed' ? `失败：${item.message ?? '未能重命名'}` : `未执行：${item.message ?? '前项失败后停止'}`}</small>
                </li>
              ))}
            </ul>
            <div className="files-name-dialog-actions">
              <button type="button" className="files-name-dialog-submit" onClick={onClose}>完成</button>
            </div>
          </div>
        ) : (
          <form className="files-batch-rename-form" onSubmit={(event) => { void submit(event); }}>
            <div className="files-batch-rename-list" aria-label={`${drafts.length} 个已选项目`}>
              {drafts.map((item, index) => (
                <label className="files-batch-rename-row" key={item.path}>
                  <span className="files-batch-rename-source">/{item.path}</span>
                  <input
                    ref={index === 0 ? firstInputRef : undefined}
                    aria-label={`新名称：/${item.path}`}
                    type="text"
                    autoComplete="off"
                    maxLength={255}
                    value={item.name}
                    disabled={submitting}
                    onChange={(event) => updateName(item.path, event.currentTarget.value)}
                  />
                </label>
              ))}
            </div>
            {error && <p className="files-name-dialog-error" role="alert">{error}</p>}
            {!error && validationError && <p className="files-name-dialog-error" role="status">{validationError}</p>}
            <div className="files-name-dialog-actions">
              <button type="button" className="files-name-dialog-cancel" onClick={onClose} disabled={submitting}>取消</button>
              <button type="submit" className="files-name-dialog-submit" disabled={submitting || changesCount === 0 || Boolean(validationError)}>
                {submitting ? '正在重命名…' : `重命名 ${changesCount} 项`}
              </button>
            </div>
          </form>
        )}
      </section>
    </div>
  );
}
