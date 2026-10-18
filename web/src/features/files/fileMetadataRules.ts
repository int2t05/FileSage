// 校验用户输入的权限与属主编号。
export function parseOctalMode(value: string): number | null {
  const normalized = value.trim();
  if (!/^0?[0-7]{3}$/.test(normalized)) return null;
  const mode = Number.parseInt(normalized, 8);
  return mode <= 0o777 ? mode : null;
}

function parseOptionalId(value: string): number | undefined | null {
  const normalized = value.trim();
  if (!normalized) return undefined;
  if (!/^\d+$/.test(normalized)) return null;
  const id = Number(normalized);
  return Number.isSafeInteger(id) ? id : null;
}

export type OwnerInputResult =
  | { ok: true; ownerUid?: number; groupGid?: number }
  | { ok: false; message: string };

export type OwnerChangeFields = Pick<Extract<OwnerInputResult, { ok: true }>, 'ownerUid' | 'groupGid'>;

export function parseOwnerInputs(uidInput: string, gidInput: string): OwnerInputResult {
  const ownerUid = parseOptionalId(uidInput);
  const groupGid = parseOptionalId(gidInput);
  if (ownerUid === null || groupGid === null) {
    return { ok: false, message: 'UID 和 GID 必须是非负整数。' };
  }
  if (ownerUid === undefined && groupGid === undefined) {
    return { ok: false, message: '至少填写 UID 或 GID。' };
  }
  return {
    ok: true,
    ...(ownerUid === undefined ? {} : { ownerUid }),
    ...(groupGid === undefined ? {} : { groupGid }),
  };
}

export function ownerChangeFields(change: Extract<OwnerInputResult, { ok: true }>): OwnerChangeFields {
  const { ownerUid, groupGid } = change;
  return {
    ...(ownerUid === undefined ? {} : { ownerUid }),
    ...(groupGid === undefined ? {} : { groupGid }),
  };
}

export function canEditFileMetadata(entry: { kind: string; symlink: boolean; version: string }): boolean {
  return !entry.symlink && (entry.kind === 'file' || entry.kind === 'directory') && Boolean(entry.version);
}

export function formatPermissionMode(mode: number): string {
  return mode.toString(8).padStart(4, '0');
}

export function formatOwnerId(id: number | undefined, ownershipAvailable: boolean | undefined): string {
  return ownershipAvailable === false || id === undefined ? '—' : String(id);
}
