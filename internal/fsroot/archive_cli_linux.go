//go:build linux

package fsroot

import "os"

// 通过子进程文件描述符读取归档，避免每次导航复制大文件。
func archiveProcFDPath() string { return "/proc/self/fd/3" }

func archiveProcFDExtraFiles(file *os.File) []*os.File { return []*os.File{file} }
