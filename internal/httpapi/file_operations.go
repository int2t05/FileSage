// 本文件处理经版本校验的单文件复制和移动。
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"filesage/internal/fsroot"
)

type singleFileOperator interface {
	CopyFileNoReplace(context.Context, string, string, fsroot.Fingerprint) (fsroot.FileCopyResult, error)
	MoveFileNoReplace(context.Context, string, string, fsroot.Fingerprint) (fsroot.FileMoveResult, error)
}

type directoryTreeOperator interface {
	SnapshotTree(context.Context, string) (fsroot.TreeSnapshot, error)
	VerifyTreeSnapshot(context.Context, fsroot.TreeSnapshot) error
	CopyTreeNoReplace(context.Context, string, string, fsroot.TreeSnapshot) (fsroot.TreeCopyResult, error)
}

type directoryTreeMoveOperator interface {
	SnapshotTree(context.Context, string) (fsroot.TreeSnapshot, error)
	VerifyTreeSnapshot(context.Context, fsroot.TreeSnapshot) error
	MoveTreeNoReplace(context.Context, string, string, fsroot.TreeSnapshot) (fsroot.TreeMoveResult, error)
}

type fileOperationRequest struct {
	Path               string `json:"path"`
	Version            string `json:"version"`
	Destination        string `json:"destination"`
	DestinationVersion string `json:"destinationVersion"`
}

type fileOperationResponse struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// copyFile 处理单个普通文件复制请求。
func (api *API) copyFile(w http.ResponseWriter, r *http.Request) {
	api.runFileOperation(w, r, false)
}

// copyDirectory 按服务端快照复制目录树，并为重名目标选择新名称。
func (api *API) copyDirectory(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request fileOperationRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if !validRelativePath(request.Path, false) || isReservedPath(request.Path) {
		writeError(w, http.StatusBadRequest, "path must identify a directory within the managed root")
		return
	}
	if !validRelativePath(request.Destination, true) || isReservedPath(request.Destination) {
		writeError(w, http.StatusBadRequest, "destination must identify a directory within the managed root")
		return
	}
	if request.Version == "" || (request.Destination == "" && request.DestinationVersion != "") || (request.Destination != "" && request.DestinationVersion == "") {
		writeError(w, http.StatusBadRequest, "source and destination directory versions are required")
		return
	}
	if request.Path == request.Destination || strings.HasPrefix(request.Destination, request.Path+"/") {
		writeError(w, http.StatusBadRequest, "destination cannot be inside the source directory")
		return
	}
	operator, ok := api.files.(directoryTreeOperator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "safe directory copy is not supported by this filesystem")
		return
	}

	release, ok := api.lockMutationPaths(w, r, request.Path, request.Destination)
	if !ok {
		return
	}
	defer release()

	source, err := api.verifyFileOperationVersion(request.Path, request.Version)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if source.Symlink || source.Kind != fsroot.KindDirectory {
		writeError(w, http.StatusBadRequest, "only regular directories can be copied")
		return
	}
	if !source.IdentityAvailable {
		writeFileError(w, fsroot.ErrIdentityUnavailable)
		return
	}
	if request.Destination != "" {
		directory, err := api.verifyFileOperationVersion(request.Destination, request.DestinationVersion)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if directory.Symlink {
			writeError(w, http.StatusBadRequest, "destination directory cannot be a symbolic link")
			return
		}
		if directory.Kind != fsroot.KindDirectory {
			writeError(w, http.StatusBadRequest, "destination must be an existing directory")
			return
		}
		if !directory.IdentityAvailable {
			writeFileError(w, fsroot.ErrIdentityUnavailable)
			return
		}
	} else if _, err := api.files.List(""); err != nil {
		writeFileError(w, err)
		return
	}

	snapshot, err := operator.SnapshotTree(r.Context(), request.Path)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if len(snapshot.Entries) == 0 || versionForEntry(snapshot.Entries[0].Entry) != request.Version {
		writeFileError(w, fsroot.ErrConflict)
		return
	}

	for suffix := 0; suffix <= 10000; suffix++ {
		name := numberedDirectoryName(source.Name, suffix)
		target := joinPath(request.Destination, name)
		if !validEntryName(name) || isReservedPath(target) {
			writeError(w, http.StatusConflict, "no available directory name could be reserved")
			return
		}
		exists, lookupErr := api.fileOperationTargetExists(request.Destination, target)
		if lookupErr != nil {
			writeFileError(w, lookupErr)
			return
		}
		if exists {
			continue
		}
		auditID, ok := beginFileAudit(api, w, r, "manual_copy_directory", source.Path, target)
		if !ok {
			return
		}
		result, err := operator.CopyTreeNoReplace(r.Context(), source.Path, target, snapshot)
		if err == nil {
			if !result.Published {
				if !finishFileAudit(api, w, r, auditID, "needs_review", "复制结果待核查。") {
					return
				}
				writeError(w, http.StatusInternalServerError, "file system did not confirm directory copy publication; refresh and verify the result")
				return
			}
			if !finishFileAudit(api, w, r, auditID, "succeeded", "目录复制完成。") {
				return
			}
			writeJSON(w, http.StatusCreated, fileOperationResponse{Path: target, Name: name})
			return
		}
		if result.Published {
			if !finishFileAudit(api, w, r, auditID, "needs_review", "复制结果待核查。") {
				return
			}
			writeError(w, http.StatusInternalServerError, "directory copy was published but durability could not be confirmed; refresh and verify the result")
			return
		}
		if !finishFileAudit(api, w, r, auditID, "failed", "目录复制未完成。") {
			return
		}
		if errors.Is(err, fsroot.ErrConflict) {
			if verifyErr := operator.VerifyTreeSnapshot(r.Context(), snapshot); verifyErr != nil {
				writeFileError(w, verifyErr)
				return
			}
			if request.Destination != "" {
				if _, verifyErr := api.verifyFileOperationVersion(request.Destination, request.DestinationVersion); verifyErr != nil {
					writeFileError(w, verifyErr)
					return
				}
			} else if _, verifyErr := api.files.List(""); verifyErr != nil {
				writeFileError(w, verifyErr)
				return
			}
			exists, lookupErr := api.fileOperationTargetExists(request.Destination, target)
			if lookupErr != nil {
				writeFileError(w, lookupErr)
				return
			}
			if exists {
				continue
			}
		}
		writeFileError(w, err)
		return
	}
	writeError(w, http.StatusConflict, "no available directory name could be reserved")
}

// moveDirectory 原子移动目录，跨挂载时先复制校验再移除源目录。
func (api *API) moveDirectory(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request fileOperationRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if !validRelativePath(request.Path, false) || isReservedPath(request.Path) {
		writeError(w, http.StatusBadRequest, "path must identify a directory within the managed root")
		return
	}
	if !validRelativePath(request.Destination, true) || isReservedPath(request.Destination) {
		writeError(w, http.StatusBadRequest, "destination must identify a directory within the managed root")
		return
	}
	if request.Version == "" || (request.Destination == "" && request.DestinationVersion != "") || (request.Destination != "" && request.DestinationVersion == "") {
		writeError(w, http.StatusBadRequest, "source and destination directory versions are required")
		return
	}
	if request.Path == request.Destination || strings.HasPrefix(request.Destination, request.Path+"/") {
		writeError(w, http.StatusBadRequest, "destination cannot be inside the source directory")
		return
	}
	if parentPath(request.Path) == request.Destination {
		writeError(w, http.StatusBadRequest, "directory is already in the destination directory")
		return
	}
	operator, ok := api.files.(directoryTreeMoveOperator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "safe directory move is not supported by this filesystem")
		return
	}

	release, ok := api.lockMutationPaths(w, r, request.Path, request.Destination)
	if !ok {
		return
	}
	defer release()

	source, err := api.verifyFileOperationVersion(request.Path, request.Version)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if source.Symlink || source.Kind != fsroot.KindDirectory {
		writeError(w, http.StatusBadRequest, "only regular directories can be moved")
		return
	}
	if !source.IdentityAvailable {
		writeFileError(w, fsroot.ErrIdentityUnavailable)
		return
	}
	if request.Destination != "" {
		directory, err := api.verifyFileOperationVersion(request.Destination, request.DestinationVersion)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if directory.Symlink {
			writeError(w, http.StatusBadRequest, "destination directory cannot be a symbolic link")
			return
		}
		if directory.Kind != fsroot.KindDirectory {
			writeError(w, http.StatusBadRequest, "destination must be an existing directory")
			return
		}
		if !directory.IdentityAvailable {
			writeFileError(w, fsroot.ErrIdentityUnavailable)
			return
		}
	} else if _, err := api.files.List(""); err != nil {
		writeFileError(w, err)
		return
	}

	snapshot, err := operator.SnapshotTree(r.Context(), request.Path)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if len(snapshot.Entries) == 0 || versionForEntry(snapshot.Entries[0].Entry) != request.Version {
		writeFileError(w, fsroot.ErrConflict)
		return
	}

	for suffix := 0; suffix <= 10000; suffix++ {
		name := numberedDirectoryName(source.Name, suffix)
		target := joinPath(request.Destination, name)
		if !validEntryName(name) || isReservedPath(target) {
			writeError(w, http.StatusConflict, "no available directory name could be reserved")
			return
		}
		exists, lookupErr := api.fileOperationTargetExists(request.Destination, target)
		if lookupErr != nil {
			writeFileError(w, lookupErr)
			return
		}
		if exists {
			continue
		}
		auditID, ok := beginFileAudit(api, w, r, "manual_move_directory", source.Path, target)
		if !ok {
			return
		}
		result, err := operator.MoveTreeNoReplace(r.Context(), source.Path, target, snapshot)
		if result.Published && !result.SourceRemoved {
			if !finishFileAudit(api, w, r, auditID, "partial", "目录已发布，来源未完全移除。") {
				return
			}
			message := "目录副本已完整发布到目标；源目录未能安全移除，请核对后处理。"
			if result.SourceRetained != "" {
				message = fmt.Sprintf("目录副本已完整发布到目标；源目录已隔离并保留在 %q，请核对后处理。", result.SourceRetained)
			}
			writeJSON(w, http.StatusMultiStatus, fileOperationResponse{Path: target, Name: name, Status: "partial", Message: message})
			return
		}
		if err == nil {
			if !result.Published || !result.SourceRemoved {
				outcome := "failed"
				if result.Published {
					outcome = "needs_review"
				}
				if !finishFileAudit(api, w, r, auditID, outcome, "目录移动结果待核查。") {
					return
				}
				writeError(w, http.StatusInternalServerError, "file system did not confirm directory move; refresh and verify the result")
				return
			}
			if !finishFileAudit(api, w, r, auditID, "succeeded", "目录移动完成。") {
				return
			}
			writeJSON(w, http.StatusOK, fileOperationResponse{Path: target, Name: name})
			return
		}
		if result.Published && result.SourceRemoved {
			if !finishFileAudit(api, w, r, auditID, "needs_review", "目录移动结果待核查。") {
				return
			}
			writeError(w, http.StatusInternalServerError, "directory was moved but the filesystem could not confirm directory durability; refresh and verify the result")
			return
		}
		if !finishFileAudit(api, w, r, auditID, "failed", "目录移动未完成。") {
			return
		}
		if errors.Is(err, fsroot.ErrConflict) {
			if verifyErr := operator.VerifyTreeSnapshot(r.Context(), snapshot); verifyErr != nil {
				writeFileError(w, verifyErr)
				return
			}
			if request.Destination != "" {
				if _, verifyErr := api.verifyFileOperationVersion(request.Destination, request.DestinationVersion); verifyErr != nil {
					writeFileError(w, verifyErr)
					return
				}
			} else if _, verifyErr := api.files.List(""); verifyErr != nil {
				writeFileError(w, verifyErr)
				return
			}
			exists, lookupErr := api.fileOperationTargetExists(request.Destination, target)
			if lookupErr != nil {
				writeFileError(w, lookupErr)
				return
			}
			if exists {
				continue
			}
		}
		writeFileError(w, err)
		return
	}
	writeError(w, http.StatusConflict, "no available directory name could be reserved")
}

// numberedDirectoryName 将序号追加到完整目录名后。
func numberedDirectoryName(name string, suffix int) string {
	if suffix == 0 {
		return name
	}
	return fmt.Sprintf("%s (%d)", name, suffix)
}

// fileOperationTargetExists 仅确认冲突路径已存在，不把源快照冲突误当重名。
func (api *API) fileOperationTargetExists(destination, target string) (bool, error) {
	entries, err := api.files.List(destination)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Path == target {
			return true, nil
		}
	}
	return false, nil
}

// moveFile 处理单个普通文件移动请求。
func (api *API) moveFile(w http.ResponseWriter, r *http.Request) {
	api.runFileOperation(w, r, true)
}

// runFileOperation 校验版本、选择无冲突名称并执行文件操作。
func (api *API) runFileOperation(w http.ResponseWriter, r *http.Request, move bool) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request fileOperationRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if !validRelativePath(request.Path, false) || isReservedPath(request.Path) {
		writeError(w, http.StatusBadRequest, "path must identify an entry within the managed root")
		return
	}
	if !validRelativePath(request.Destination, true) || isReservedPath(request.Destination) {
		writeError(w, http.StatusBadRequest, "destination must identify a directory within the managed root")
		return
	}
	if request.Version == "" || (request.Destination == "" && request.DestinationVersion != "") || (request.Destination != "" && request.DestinationVersion == "") {
		writeError(w, http.StatusBadRequest, "source and destination directory versions are required")
		return
	}
	if move && parentPath(request.Path) == request.Destination {
		writeError(w, http.StatusBadRequest, "file is already in the destination directory")
		return
	}
	operator, ok := api.files.(singleFileOperator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "safe file copy and move are not supported by this filesystem")
		return
	}

	release, ok := api.lockMutationPaths(w, r, request.Path, request.Destination)
	if !ok {
		return
	}
	defer release()

	source, err := api.verifyFileOperationVersion(request.Path, request.Version)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if source.Symlink || source.Kind != fsroot.KindFile {
		writeError(w, http.StatusBadRequest, "only regular files can be copied or moved")
		return
	}
	if !source.IdentityAvailable {
		writeFileError(w, fsroot.ErrIdentityUnavailable)
		return
	}
	if request.Destination != "" {
		directory, err := api.verifyFileOperationVersion(request.Destination, request.DestinationVersion)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if directory.Symlink {
			writeError(w, http.StatusBadRequest, "destination directory cannot be a symbolic link")
			return
		}
		if directory.Kind != fsroot.KindDirectory {
			writeError(w, http.StatusBadRequest, "destination must be an existing directory")
			return
		}
		if !directory.IdentityAvailable {
			writeFileError(w, fsroot.ErrIdentityUnavailable)
			return
		}
	} else if _, err := api.files.List(""); err != nil {
		writeFileError(w, err)
		return
	}

	for suffix := 0; suffix <= 10000; suffix++ {
		name := numberedFileName(source.Name, suffix)
		if !validEntryName(name) || name == fsroot.TrashDirectoryName {
			writeError(w, http.StatusConflict, "no available file name could be reserved")
			return
		}
		target := joinPath(request.Destination, name)
		exists, lookupErr := api.fileOperationTargetExists(request.Destination, target)
		if lookupErr != nil {
			writeFileError(w, lookupErr)
			return
		}
		if exists {
			continue
		}
		if move {
			auditID, ok := beginFileAudit(api, w, r, "manual_move_file", source.Path, target)
			if !ok {
				return
			}
			result, err := operator.MoveFileNoReplace(r.Context(), source.Path, target, source.Fingerprint())
			if err != nil {
				if errors.Is(err, fs.ErrExist) {
					if !finishFileAudit(api, w, r, auditID, "failed", "目标名称已被占用。") {
						return
					}
					continue
				}
				if result.Published && !result.SourceRemoved {
					if !finishFileAudit(api, w, r, auditID, "partial", "文件已发布，来源未完全移除。") {
						return
					}
					writeJSON(w, http.StatusMultiStatus, fileOperationResponse{
						Path: target, Name: name, Status: "partial",
						Message: "文件已复制到目标目录，但源文件未删除；请核对两处文件后重试。",
					})
					return
				}
				if result.Published && result.SourceRemoved {
					if !finishFileAudit(api, w, r, auditID, "needs_review", "文件移动结果待核查。") {
						return
					}
					writeError(w, http.StatusInternalServerError, "文件已移动，但文件系统未能确认目录持久化；请刷新目录核对结果。")
					return
				}
				if !finishFileAudit(api, w, r, auditID, "failed", "文件移动未完成。") {
					return
				}
				writeFileError(w, err)
				return
			}
			if !result.Published || !result.SourceRemoved {
				outcome := "failed"
				if result.Published {
					outcome = "partial"
				}
				if !finishFileAudit(api, w, r, auditID, outcome, "文件移动结果待核查。") {
					return
				}
				writeError(w, http.StatusInternalServerError, "文件系统未确认移动结果，请刷新目录核对。")
				return
			}
			if !finishFileAudit(api, w, r, auditID, "succeeded", "文件移动完成。") {
				return
			}
			writeJSON(w, http.StatusOK, fileOperationResponse{Path: target, Name: name})
			return
		}

		auditID, ok := beginFileAudit(api, w, r, "manual_copy_file", source.Path, target)
		if !ok {
			return
		}
		result, err := operator.CopyFileNoReplace(r.Context(), source.Path, target, source.Fingerprint())
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				if !finishFileAudit(api, w, r, auditID, "failed", "目标名称已被占用。") {
					return
				}
				continue
			}
			if result.Published {
				if !finishFileAudit(api, w, r, auditID, "needs_review", "文件复制结果待核查。") {
					return
				}
				writeError(w, http.StatusInternalServerError, "文件已发布，但无法确认目录持久化；请刷新目录确认后再重试。")
				return
			}
			if !finishFileAudit(api, w, r, auditID, "failed", "文件复制未完成。") {
				return
			}
			writeFileError(w, err)
			return
		}
		if !result.Published {
			if !finishFileAudit(api, w, r, auditID, "needs_review", "文件复制结果待核查。") {
				return
			}
			writeError(w, http.StatusInternalServerError, "文件系统未确认复制结果，请刷新目录核对。")
			return
		}
		if !finishFileAudit(api, w, r, auditID, "succeeded", "文件复制完成。") {
			return
		}
		writeJSON(w, http.StatusCreated, fileOperationResponse{Path: target, Name: name})
		return
	}
	writeError(w, http.StatusConflict, "no available file name could be reserved")
}

// verifyFileOperationVersion 先复核令牌，再由调用方按类型拒绝链接和特殊文件。
func (api *API) verifyFileOperationVersion(relativePath, expected string) (fsroot.Entry, error) {
	entries, err := api.files.List(parentPath(relativePath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fsroot.Entry{}, fsroot.ErrConflict
		}
		return fsroot.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Path != relativePath {
			continue
		}
		if versionForEntry(entry) != expected {
			return fsroot.Entry{}, fsroot.ErrConflict
		}
		return entry, nil
	}
	return fsroot.Entry{}, fsroot.ErrConflict
}

// numberedFileName 为重名文件生成带序号的名称。
func numberedFileName(name string, suffix int) string {
	if suffix == 0 {
		return name
	}
	extension := path.Ext(name)
	base := strings.TrimSuffix(name, extension)
	if base == "" {
		base, extension = name, ""
	}
	return fmt.Sprintf("%s (%d)%s", base, suffix, extension)
}

// beginFileAudit 在文件写入前保存人工操作记录。
func beginFileAudit(api *API, w http.ResponseWriter, r *http.Request, eventType, sourcePath, targetPath string) (int64, bool) {
	id, err := api.beginManualAudit(r.Context(), eventType, sourcePath, targetPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "操作记录暂不可用，未执行文件操作。")
		return 0, false
	}
	return id, true
}

// finishFileAudit 在文件操作结束后写入固定结果。
func finishFileAudit(api *API, w http.ResponseWriter, r *http.Request, id int64, outcome, message string) bool {
	if err := finishManualAuditAfterWrite(api, r.Context(), id, outcome, message); err != nil {
		writeError(w, http.StatusInternalServerError, "文件可能已变更，审计结果待核查。")
		return false
	}
	return true
}

// finishManualAuditAfterWrite 即使请求断开，也给已完成写入留下终结记录的机会。
func finishManualAuditAfterWrite(api *API, ctx context.Context, id int64, outcome, message string) error {
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return api.finishManualAudit(auditCtx, id, outcome, message)
}
