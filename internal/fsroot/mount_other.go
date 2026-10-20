//go:build !linux

// 本文件为非 Linux 平台保留现有挂载行为。

package fsroot

// platformRegisterMounts 保持非 Linux 平台的现有文件系统行为。
func platformRegisterMounts(string) ([]registeredMount, error) { return nil, nil }

// platformMountIDAt 在非 Linux 平台不参与卷离线检测。
func platformMountIDAt(string, string) (uint64, error) { return 0, ErrTrashUnsupported }

// platformMountIDForFD 在非 Linux 平台不参与卷离线检测。
func platformMountIDForFD(uintptr) (uint64, error) { return 0, ErrTrashUnsupported }
