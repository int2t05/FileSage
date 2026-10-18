import type { BatchFileOperationItemResult } from '../../api/files';
import { isDirectoryTargetAllowed } from './directoryOperationRules';

export interface BatchOperationSource {
  path: string;
  kind: 'file' | 'directory';
}

function parentPath(path: string): string {
  const separator = path.lastIndexOf('/');
  return separator < 0 ? '' : path.slice(0, separator);
}

// 找出被最外层已选目录包含的条目，与批量执行的去重范围一致。
export function batchSelectionInclusions(items: BatchOperationSource[]): Array<{ sourcePath: string; includedIn: string }> {
  const selectedDirectories = new Set(items.filter((item) => item.kind === 'directory').map((item) => item.path));
  const relations: Array<{ sourcePath: string; includedIn: string }> = [];
  for (const item of items) {
    const parents = item.path.split('/');
    parents.pop();
    let outermostSelectedParent = '';
    while (parents.length > 0) {
      const parent = parents.join('/');
      if (selectedDirectories.has(parent)) outermostSelectedParent = parent;
      parents.pop();
    }
    if (outermostSelectedParent) relations.push({ sourcePath: item.path, includedIn: outermostSelectedParent });
  }
  return relations;
}

// 检查目标目录是否会形成目录环或同目录移动。
export function batchOperationTargetError(mode: 'copy' | 'move', items: BatchOperationSource[], destination: string): string {
  for (const item of items) {
    if (item.kind === 'directory' && !isDirectoryTargetAllowed(mode, item.path, destination)) {
      return mode === 'move' && parentPath(item.path) === destination
        ? '目标不能是所选文件夹当前的父目录。'
        : '目标不能是所选文件夹本身或其子目录。';
    }
    if (mode === 'move' && item.kind === 'file' && parentPath(item.path) === destination) {
      return '目标与某个所选文件当前所在目录相同，请选择其他目录。';
    }
  }
  return '';
}

// 保留需要重试或人工核查的条目。
export function incompleteBatchOperationPaths(items: BatchFileOperationItemResult[]): Set<string> {
  return new Set(items
    .filter((item) => item.status === 'partial' || item.status === 'failed' || item.status === 'not_started')
    .map((item) => item.sourcePath));
}
