// 在版本校验下修改文件权限或属主。
import { useEffect, useRef, useState } from 'react';
import type { FormEvent, KeyboardEvent } from 'react';
import { X } from 'lucide-react';
import { ApiError, updateFileMetadata, type FileEntry } from '../../api/files';
import { formatPermissionMode, ownerChangeFields, parseOctalMode, parseOwnerInputs } from './fileMetadataRules';
import './FileMetadataDialog.css';

export type FileMetadataEditMode = 'mode' | 'owner';

interface FileMetadataDialogProps {
  entry: FileEntry;
  mode: FileMetadataEditMode;
  onClose: () => void;
  onComplete: (entry: FileEntry) => void;
  onConflict: () => void;
}

export function FileMetadataDialog({ entry, mode, onClose, onComplete, onConflict }: FileMetadataDialogProps) {
  const [modeInput, setModeInput] = useState(formatPermissionMode(entry.mode));
  const [uidInput, setUidInput] = useState('');
  const [gidInput, setGidInput] = useState('');
  const [error, setError] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [stale, setStale] = useState(false);
  const dialogRef = useRef<HTMLElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const submittingRef = useRef(false);
  const editingOwner = mode === 'owner';
  const title = editingOwner ? '修改属主' : '修改权限';

  useEffect(() => { inputRef.current?.focus(); }, []);

  function close() {
    if (!submittingRef.current) onClose();
  }

  function handleKeyDown(event: KeyboardEvent<HTMLElement>) {
    if (event.key === 'Escape') {
      event.preventDefault();
      close();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusable = dialogRef.current?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled)');
    if (!focusable?.length) return;
    const first = focusable.item(0);
    const last = focusable.item(focusable.length - 1);
    if (!first || !last) return;
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (stale) return;
    setError('');
    submittingRef.current = true;
    setSubmitting(true);
    try {
      let updated: FileEntry;
      if (editingOwner) {
        const change = parseOwnerInputs(uidInput, gidInput);
        if (!change.ok) {
          setError(change.message);
          return;
        }
        updated = await updateFileMetadata({ path: entry.path, version: entry.version, ...ownerChangeFields(change) });
      } else {
        const parsedMode = parseOctalMode(modeInput);
        if (parsedMode === null) {
          setError('请输入 0000 至 0777 的八进制权限。');
          return;
        }
        updated = await updateFileMetadata({ path: entry.path, version: entry.version, mode: parsedMode });
      }
      onComplete(updated);
    } catch (submitError) {
      if (submitError instanceof ApiError && submitError.status === 409) {
        setStale(true);
        onConflict();
      }
      setError(submitError instanceof ApiError ? submitError.message : '保存文件信息时发生错误，请重试。');
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  }

  return (
    <div className="file-metadata-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) close(); }}>
      <section
        ref={dialogRef}
        className="file-metadata-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="file-metadata-title"
        aria-describedby={stale ? 'file-metadata-description file-metadata-stale' : 'file-metadata-description'}
        tabIndex={-1}
        onKeyDown={handleKeyDown}
      >
        <header className="file-metadata-header">
          <div>
            <h2 id="file-metadata-title">{title}</h2>
            <p>{entry.name}</p>
          </div>
          <button type="button" aria-label={`关闭${title}`} onClick={close} disabled={submitting}><X size={18} aria-hidden="true" /></button>
        </header>
        <p className="file-metadata-description" id="file-metadata-description">
          {editingOwner
            ? '修改服务器文件系统中的属主。能否更改取决于 FileSage 服务账户的权限。'
            : '修改此文件系统条目的八进制权限；保存前会校验文件版本。'}
        </p>
        <form onSubmit={(event) => { void submit(event); }}>
          {editingOwner ? (
            <>
              <label htmlFor="file-owner-uid">属主 UID</label>
              <input
                ref={inputRef}
                id="file-owner-uid"
                type="text"
                inputMode="numeric"
                autoComplete="off"
                value={uidInput}
                onChange={(event) => { setUidInput(event.currentTarget.value); setError(''); }}
                placeholder={entry.ownerUid === undefined ? '留空则不修改' : `当前 ${entry.ownerUid}；留空则不修改`}
                readOnly={stale}
                disabled={submitting}
              />
              <label htmlFor="file-owner-gid">属组 GID</label>
              <input
                id="file-owner-gid"
                type="text"
                inputMode="numeric"
                autoComplete="off"
                value={gidInput}
                onChange={(event) => { setGidInput(event.currentTarget.value); setError(''); }}
                placeholder={entry.groupGid === undefined ? '留空则不修改' : `当前 ${entry.groupGid}；留空则不修改`}
                readOnly={stale}
                disabled={submitting}
              />
              <p className="file-metadata-hint">至少填写一项。服务器权限不足时，文件信息不会更改。</p>
            </>
          ) : (
            <>
              <label htmlFor="file-mode">八进制权限</label>
              <input
                ref={inputRef}
                id="file-mode"
                type="text"
                inputMode="numeric"
                autoComplete="off"
                maxLength={4}
                value={modeInput}
                onChange={(event) => { setModeInput(event.currentTarget.value); setError(''); }}
                aria-describedby="file-mode-range"
                readOnly={stale}
                disabled={submitting}
              />
              <p className="file-metadata-hint" id="file-mode-range">范围 0000–0777，例如 0644。</p>
            </>
          )}
          {stale && <p className="file-metadata-stale" id="file-metadata-stale" role="status">文件信息已被其他操作修改。请关闭此窗口，基于最新值重新打开后再修改。</p>}
          {error && <p className="file-metadata-error" role="alert">{error}</p>}
          <div className="file-metadata-actions">
            <button type="button" onClick={close} disabled={submitting}>取消</button>
            <button type="submit" disabled={submitting || stale}>{stale ? '需重新确认' : submitting ? '正在保存…' : `确认修改${editingOwner ? '属主' : '权限'}`}</button>
          </div>
        </form>
      </section>
    </div>
  );
}
