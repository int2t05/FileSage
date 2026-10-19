// 比较管理根目录内两个文件路径的父目录。
export function isSameDirectoryPath(sourcePath: string, targetPath: string): boolean {
  if (!sourcePath || !targetPath || sourcePath === targetPath) return false;
  const parent = (path: string) => {
    const separator = path.lastIndexOf('/');
    return separator < 0 ? '' : path.slice(0, separator);
  };
  return parent(sourcePath) === parent(targetPath);
}
