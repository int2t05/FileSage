// 本文件处理经审核记录的人工软链接创建。
package httpapi

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"filesage/internal/fsroot"
)

type symlinkFileSystem interface {
	SymlinkNoReplace(targetPath, linkPath string) error
}

type symlinkCreateRequest struct {
	Parent *string `json:"parent"`
	Name   string  `json:"name"`
	Target string  `json:"target"`
}

// createSymlink 在真实目录内创建指向根内既有文件或目录的软链接。
func (api *API) createSymlink(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	var request symlinkCreateRequest
	if !decodeRequestJSON(w, r, &request) {
		return
	}
	if request.Parent == nil || !validRelativePath(*request.Parent, true) || isReservedPath(*request.Parent) {
		writeError(w, http.StatusBadRequest, "parent must be a clean path relative to the managed root")
		return
	}
	if !validEntryName(request.Name) || request.Name == fsroot.TrashDirectoryName {
		writeError(w, http.StatusBadRequest, "name must be a single valid file name")
		return
	}
	if !validRelativePath(request.Target, false) || isReservedPath(request.Target) {
		writeError(w, http.StatusBadRequest, "target must identify a path within the managed root")
		return
	}
	fileSystem, ok := api.files.(symlinkFileSystem)
	if !ok {
		writeError(w, http.StatusNotImplemented, "safe symbolic-link creation is not supported by this filesystem")
		return
	}

	parent := *request.Parent
	for suffix := 1; suffix <= 10000; suffix++ {
		name := request.Name
		if suffix > 1 {
			name = fmt.Sprintf("%s (%d)", request.Name, suffix)
		}
		if !validEntryName(name) {
			writeError(w, http.StatusConflict, "no available symbolic-link name could be reserved")
			return
		}
		createdPath := joinPath(parent, name)
		release, ok := api.lockMutationPaths(w, r, createdPath, request.Target)
		if !ok {
			return
		}
		entries, err := api.files.List(parent)
		if err != nil {
			release()
			writeFileError(w, err)
			return
		}
		occupied := false
		for _, entry := range entries {
			if entry.Name == name {
				occupied = true
				break
			}
		}
		if occupied {
			release()
			continue
		}
		auditID, ok := beginFileAudit(api, w, r, "manual_create_symlink", request.Target, createdPath)
		if !ok {
			release()
			return
		}
		if err := fileSystem.SymlinkNoReplace(request.Target, createdPath); err != nil {
			if !finishFileAudit(api, w, r, auditID, "failed", "软链接创建未完成。") {
				release()
				return
			}
			release()
			if errors.Is(err, fsroot.ErrConflict) || errors.Is(err, fs.ErrExist) {
				continue
			}
			writeFileError(w, err)
			return
		}
		if !finishFileAudit(api, w, r, auditID, "succeeded", "软链接已创建。") {
			release()
			return
		}
		release()
		writeJSON(w, http.StatusCreated, pathResponse{Path: createdPath, Name: name})
		return
	}
	writeError(w, http.StatusConflict, "no available symbolic-link name could be reserved")
}
