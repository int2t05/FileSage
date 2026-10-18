package fsroot

import (
	"fmt"
	"io/fs"
)

// MetadataUpdate 描述一次权限或属主修改。
type MetadataUpdate struct {
	Mode     *fs.FileMode
	OwnerUID *uint32
	GroupGID *uint32
}

// UpdateMetadata 按文件状态快照安全更新普通文件或目录的元数据。
func (r *Root) UpdateMetadata(rel string, expected Fingerprint, update MetadataUpdate) (Entry, error) {
	if err := validateMutationPath(rel); err != nil {
		return Entry{}, err
	}
	if hasTrashComponent(rel) {
		return Entry{}, ErrInvalidPath
	}
	if !expected.Exists || !expected.IdentityAvailable || !expected.OwnershipAvailable || !expected.ChangeTimeAvailable {
		return Entry{}, ErrIdentityUnavailable
	}
	if update.Mode != nil {
		if update.OwnerUID != nil || update.GroupGID != nil || update.Mode.Perm() != *update.Mode || *update.Mode > 0o777 {
			return Entry{}, fmt.Errorf("invalid permission mode: %w", fs.ErrInvalid)
		}
	} else if update.OwnerUID == nil && update.GroupGID == nil {
		return Entry{}, fmt.Errorf("metadata update is empty: %w", fs.ErrInvalid)
	}
	if err := r.rejectSymlinkParents(rel); err != nil {
		return Entry{}, err
	}
	return platformUpdateMetadata(r, rel, expected, update)
}
