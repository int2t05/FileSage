//go:build !linux

package fsroot

// platformRemoveEntryIfUnchanged 在无法安全比较并删除条目的平台上失败关闭。
func platformRemoveEntryIfUnchanged(*Root, Entry) error {
	return ErrNoReplaceUnsupported
}
