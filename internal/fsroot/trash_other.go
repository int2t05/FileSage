//go:build !linux

package fsroot

const trashAreaName = ".filesage-trash"

// TrashArea 在无法校验挂载边界的平台上拒绝回收操作。
func (r *Root) TrashArea(source string) (string, string, uint64, error) {
	return "", "", 0, ErrTrashUnsupported
}

// VerifyTrashArea 在无法校验挂载边界的平台上返回错误。
func (r *Root) VerifyTrashArea(area string, mountID uint64) error { return ErrTrashUnsupported }

// MountID 在无法校验挂载边界的平台上返回错误。
func (r *Root) MountID(relativePath string) (uint64, error) {
	return 0, ErrTrashUnsupported
}

// CheckTreeMounts 在无法校验挂载边界的平台上返回错误。
func (r *Root) CheckTreeMounts(relativePath string, expectedMountID uint64) error {
	return ErrTrashUnsupported
}

// RemoveTreeNoFollow 在无法校验挂载边界的平台上返回错误。
func (r *Root) RemoveTreeNoFollow(relativePath string, expectedMountID, expectedDevice, expectedInode uint64) error {
	return ErrTrashUnsupported
}
