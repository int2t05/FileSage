// 文件夹操作规则，避免覆盖源文件夹或形成目录环。

function parentPath(path: string): string {
  const separator = path.lastIndexOf('/');
  return separator < 0 ? '' : path.slice(0, separator);
}

function isSameOrDescendantPath(path: string, sourcePath: string): boolean {
  return path === sourcePath || path.startsWith(`${sourcePath}/`);
}

// 判断文件夹目标是否安全且不会产生无意义的同目录移动。
export function isDirectoryTargetAllowed(mode: 'copy' | 'move', sourcePath: string, targetPath: string): boolean {
  if (isSameOrDescendantPath(targetPath, sourcePath)) return false;
  return mode !== 'move' || parentPath(sourcePath) !== targetPath;
}
