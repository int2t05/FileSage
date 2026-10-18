// 创建指向管理根目录内目标的软链接。
import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link2, X } from 'lucide-react';
import { ApiError, createSymlink, type PathNameResult } from '../../api/files';
import './CreateSymlinkDialog.css';

interface CreateSymlinkDialogProps {
  targetPath: string;
  targetName: string;
  parentPath: string;
  onClose: () => void;
  onComplete: (result: PathNameResult) => void;
  onConflict?: () => void;
}

function pathLabel(path: string): string {
  return path ? `/${path}` : '根目录 /';
}

function isRootRelativePath(path: string, allowRoot: boolean): boolean {
  if (path === '') return allowRoot;
  return !path.startsWith('/') && !path.includes('\\') && !/[\u0000-\u001f\u007f]/.test(path)
    && path.split('/').every((segment) => segment !== '' && segment !== '.' && segment !== '..');
}

function validateSymlinkName(name: string): string {
  if (!name) return '请输入链接名称。';
  if (name === '.' || name === '..' || name.includes('/') || name.includes('\\') || /^[a-zA-Z]:/.test(name)
    || /[\u0000-\u001f\u007f]/.test(name)) return '名称只能包含单个文件名，不能包含路径分隔符。';
  if (new TextEncoder().encode(name).byteLength > 255) return '名称超过文件系统限制（255 字节）。';
  return '';
}

export function CreateSymlinkDialog({
  targetPath,
  targetName,
  parentPath,
  onClose,
  onComplete,
  onConflict,
}: CreateSymlinkDialogProps) {
  const titleId = useId();
  const descriptionId = useId();
  const nameId = useId();
  const nameHelpId = useId();
  const [name, setName] = useState(targetName);
  const [error, setError] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const dialogRef = useRef<HTMLElement>(null);
  const nameInputRef = useRef<HTMLInputElement>(null);
  const submittingRef = useRef(false);
  const onCloseRef = useRef(onClose);
  const onCompleteRef = useRef(onComplete);
  const onConflictRef = useRef(onConflict);
  onCloseRef.current = onClose;
  onCompleteRef.current = onComplete;
  onConflictRef.current = onConflict;

  const normalizedName = name.trim();
  const nameIssue = validateSymlinkName(normalizedName);
  const sourceIssue = isRootRelativePath(targetPath, false)
    ? ''
    : '来源路径无效，请刷新目录后重试。';
  const parentIssue = isRootRelativePath(parentPath, true)
    ? ''
    : '当前目录路径无效，请刷新目录后重试。';
  const linkPath = normalizedName
    ? (parentPath ? `${parentPath}/${normalizedName}` : normalizedName)
    : `${parentPath ? `/${parentPath}` : '根目录 /'} · 未命名`;

  useEffect(() => {
    const previousFocus = document.activeElement;
    nameInputRef.current?.focus();

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        event.preventDefault();
        if (!submittingRef.current) onCloseRef.current();
        return;
      }
      if (event.key !== 'Tab') return;
      const dialog = dialogRef.current;
      const focusable = dialog?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled)');
      if (!focusable?.length) {
        event.preventDefault();
        dialog?.focus();
        return;
      }
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
      if (previousFocus instanceof HTMLElement && previousFocus.isConnected) previousFocus.focus();
    };
  }, []);

  // 冲突时保留名称草稿，并交由页面刷新目录后重试。
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (submittingRef.current || nameIssue || sourceIssue || parentIssue) return;

    submittingRef.current = true;
    setSubmitting(true);
    setError('');
    let result: PathNameResult;
    try {
      result = await createSymlink(parentPath, normalizedName, targetPath);
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.status === 409) {
        onConflictRef.current?.();
        setError('目录或来源已变化。页面已刷新，请核对后重试。');
      } else {
        setError(requestError instanceof ApiError ? requestError.message : '创建软链接时发生错误，请重试。');
      }
      return;
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }

    onCompleteRef.current(result);
  }

  return (
    <div
      className="files-name-dialog-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !submittingRef.current) onCloseRef.current();
      }}
    >
      <section
        ref={dialogRef}
        className="files-name-dialog files-symlink-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={descriptionId}
        aria-busy={submitting}
        tabIndex={-1}
      >
        <header className="files-name-dialog-header">
          <div>
            <span className="files-name-dialog-icon"><Link2 size={17} aria-hidden="true" /></span>
            <h2 id={titleId}>创建软链接</h2>
          </div>
          <button type="button" className="files-name-dialog-close" aria-label="关闭创建软链接" onClick={onClose} disabled={submitting}>
            <X size={18} aria-hidden="true" />
          </button>
        </header>

        <p className="files-name-dialog-description" id={descriptionId}>
          在当前目录创建指向所选项目的链接。仅支持管理根目录内的目标，不会复制或修改目标。
        </p>

        <div className="files-symlink-details">
          <div><span>新链接路径</span><strong>{pathLabel(linkPath)}</strong></div>
          <div><span>指向</span><strong>{pathLabel(targetPath)}</strong></div>
        </div>

        <form id="files-symlink-form" className="files-name-dialog-form files-symlink-form" onSubmit={(event) => { void submit(event); }}>
          <label htmlFor={nameId}>链接名称</label>
          <input
            ref={nameInputRef}
            id={nameId}
            name="name"
            type="text"
            autoComplete="off"
            maxLength={255}
            value={name}
            onChange={(event) => { setName(event.currentTarget.value); setError(''); }}
            placeholder="输入链接名称"
            disabled={submitting}
            aria-invalid={Boolean(nameIssue)}
            aria-describedby={nameIssue ? `${nameId}-error` : nameHelpId}
          />
          {nameIssue
            ? <p className="files-name-dialog-error" id={`${nameId}-error`} role="alert">{nameIssue}</p>
            : <p className="files-symlink-help" id={nameHelpId}>若名称已存在，系统会自动加数字，不会覆盖原项目。</p>}
        </form>

        {sourceIssue && <p className="files-name-dialog-error" role="alert">{sourceIssue}</p>}
        {parentIssue && <p className="files-name-dialog-error" role="alert">{parentIssue}</p>}
        {error && <p className="files-name-dialog-error" role="alert">{error}</p>}

        <div className="files-name-dialog-actions">
          <button type="button" className="files-name-dialog-cancel" onClick={onClose} disabled={submitting}>取消</button>
          <button type="submit" form="files-symlink-form" className="files-name-dialog-submit" disabled={submitting || Boolean(nameIssue) || Boolean(sourceIssue) || Boolean(parentIssue)}>
            {submitting ? '正在创建…' : '创建软链接'}
          </button>
        </div>
      </section>
    </div>
  );
}
