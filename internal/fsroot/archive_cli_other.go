//go:build !linux

package fsroot

import "os"

// 其他平台没有稳定的 proc 文件描述符路径，改用私有副本。
func archiveProcFDPath() string { return "" }

func archiveProcFDExtraFiles(*os.File) []*os.File { return nil }
