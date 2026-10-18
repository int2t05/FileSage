// 本文件只创建根目录内指向既有文件或目录的软链接。
package fsroot

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// SymlinkNoReplace 创建指向既有根内文件或目录的软链接，不覆盖已有项。
func (r *Root) SymlinkNoReplace(targetRel, linkRel string) error {
	if err := validateRelativePath(targetRel); err != nil || targetRel == "" {
		return ErrInvalidPath
	}
	if err := validateMutationPath(linkRel); err != nil {
		return err
	}
	if hasTrashComponent(targetRel) || hasTrashComponent(linkRel) {
		return ErrInvalidPath
	}
	if err := r.rejectSymlinkParents(linkRel); err != nil {
		return err
	}

	target, err := r.resolvePath(targetRel)
	if err != nil {
		return fmt.Errorf("resolve symbolic-link target %q: %w", targetRel, err)
	}
	if !target.info.Mode().IsRegular() && !target.info.IsDir() {
		return fmt.Errorf("symbolic-link target %q: %w", targetRel, ErrNotRegularFile)
	}

	linkTarget := relativeLinkTarget(path.Dir(linkRel), targetRel)
	// Root.Symlink 限制新链接路径，但不会校验 oldname；此处只传已校验路径的根内相对形式。
	if err := r.root.Symlink(linkTarget, linkRel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return conflictError("create symbolic link", linkRel, err)
		}
		return fmt.Errorf("create symbolic link %q: %w", linkRel, err)
	}
	return nil
}

// relativeLinkTarget 将根相对路径转换成从链接目录出发的路径。
func relativeLinkTarget(parentRel, targetRel string) string {
	parent := splitRelativeComponents(parentRel)
	target := splitRelativeComponents(targetRel)
	common := 0
	for common < len(parent) && common < len(target) && parent[common] == target[common] {
		common++
	}
	components := make([]string, 0, len(parent)-common+len(target)-common)
	for range len(parent) - common {
		components = append(components, "..")
	}
	components = append(components, target[common:]...)
	if len(components) == 0 {
		return "."
	}
	return strings.Join(components, "/")
}
