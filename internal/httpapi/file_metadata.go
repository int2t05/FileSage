package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"filesage/internal/fsroot"
)

type metadataPatchRequest struct {
	Path    string
	Version string
	Update  fsroot.MetadataUpdate
}

type metadataPatchResponse struct {
	Entry fileEntry `json:"entry"`
}

// updateFileMetadata 按当前版本修改权限或属主，并记录前后值。
func (api *API) updateFileMetadata(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	request, ok := readMetadataPatchRequest(w, r)
	if !ok {
		return
	}
	if !validRelativePath(request.Path, false) || isReservedPath(request.Path) {
		writeError(w, http.StatusBadRequest, "path must identify an entry within the managed root")
		return
	}
	if request.Version == "" || len(request.Version) > 256 {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}
	filesystem, supported := api.files.(metadataFileSystem)
	if !supported {
		writeError(w, http.StatusNotImplemented, "safe metadata updates are not supported by this filesystem")
		return
	}

	release, ok := api.lockMutationPathsWithInodePaths(w, r, []string{request.Path}, request.Path)
	if !ok {
		return
	}
	defer release()

	entry, err := api.verifyEntryVersion(request.Path, request.Version)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if entry.Symlink || entry.Kind != fsroot.KindFile && entry.Kind != fsroot.KindDirectory {
		writeError(w, http.StatusBadRequest, "only regular files and directories support metadata updates")
		return
	}
	eventType := "manual_update_ownership"
	if request.Update.Mode != nil {
		eventType = "manual_update_permissions"
	}
	auditID, ok := beginFileAudit(api, w, r, eventType, request.Path, request.Path)
	if !ok {
		return
	}
	updated, err := filesystem.UpdateMetadata(request.Path, entry.Fingerprint(), request.Update)
	if err != nil {
		outcome := "failed"
		message := metadataAuditMessage(entry, entry, request.Update, false)
		if errors.Is(err, fsroot.ErrMetadataNeedsReview) {
			outcome = "needs_review"
			message = metadataAuditUncertainMessage(entry, request.Update)
		}
		if !finishFileAudit(api, w, r, auditID, outcome, message) {
			return
		}
		writeFileError(w, err)
		return
	}
	if !finishFileAudit(api, w, r, auditID, "succeeded", metadataAuditMessage(entry, updated, request.Update, true)) {
		return
	}
	writeJSON(w, http.StatusOK, metadataPatchResponse{Entry: toFileEntry(updated, nil)})
}

func readMetadataPatchRequest(w http.ResponseWriter, r *http.Request) (metadataPatchRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON request")
		}
		return metadataPatchRequest{}, false
	}
	fields, err := strictObjectFields(data, "path", "version", "mode", "ownerUid", "groupGid")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return metadataPatchRequest{}, false
	}
	var request metadataPatchRequest
	for _, field := range []string{"path", "version"} {
		if _, exists := fields[field]; !exists {
			writeError(w, http.StatusBadRequest, "path and version are required")
			return metadataPatchRequest{}, false
		}
	}
	if json.Unmarshal(fields["path"], &request.Path) != nil || json.Unmarshal(fields["version"], &request.Version) != nil {
		writeError(w, http.StatusBadRequest, "path and version must be strings")
		return metadataPatchRequest{}, false
	}
	modeRaw, modeSet := fields["mode"]
	ownerRaw, ownerSet := fields["ownerUid"]
	groupRaw, groupSet := fields["groupGid"]
	ownershipSet := ownerSet || groupSet
	if modeSet == ownershipSet || !modeSet && !ownershipSet {
		writeError(w, http.StatusBadRequest, "provide either mode or ownerUid/groupGid")
		return metadataPatchRequest{}, false
	}
	if modeSet {
		mode, ok := parseMetadataUint(modeRaw, 0o777)
		if !ok {
			writeError(w, http.StatusBadRequest, "mode must be a decimal permission value from 0 to 511")
			return metadataPatchRequest{}, false
		}
		permission := fs.FileMode(mode)
		request.Update.Mode = &permission
		return request, true
	}
	if ownerSet {
		owner, ok := parseMetadataUint(ownerRaw, ^uint32(0))
		if !ok {
			writeError(w, http.StatusBadRequest, "ownerUid must be a non-negative integer")
			return metadataPatchRequest{}, false
		}
		request.Update.OwnerUID = &owner
	}
	if groupSet {
		group, ok := parseMetadataUint(groupRaw, ^uint32(0))
		if !ok {
			writeError(w, http.StatusBadRequest, "groupGid must be a non-negative integer")
			return metadataPatchRequest{}, false
		}
		request.Update.GroupGID = &group
	}
	return request, true
}

func parseMetadataUint(raw json.RawMessage, maximum uint32) (uint32, bool) {
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
	if err != nil || value > uint64(maximum) {
		return 0, false
	}
	return uint32(value), true
}

func metadataAuditMessage(before, after fsroot.Entry, update fsroot.MetadataUpdate, changed bool) string {
	if update.Mode != nil {
		newMode := before.Mode.Perm()
		if changed {
			newMode = after.Mode.Perm()
		}
		return "权限 mode: " + strconv.FormatUint(uint64(before.Mode.Perm()), 8) + " → " + strconv.FormatUint(uint64(newMode), 8)
	}
	newUID, newGID := before.OwnerUID, before.GroupGID
	newMode := before.Mode.Perm()
	if changed {
		newUID, newGID = after.OwnerUID, after.GroupGID
		newMode = after.Mode.Perm()
	}
	return "属主 uid/gid: " + strconv.FormatUint(uint64(before.OwnerUID), 10) + "/" + strconv.FormatUint(uint64(before.GroupGID), 10) + " → " + strconv.FormatUint(uint64(newUID), 10) + "/" + strconv.FormatUint(uint64(newGID), 10) + "; mode: " + strconv.FormatUint(uint64(before.Mode.Perm()), 8) + " → " + strconv.FormatUint(uint64(newMode), 8)
}

func metadataAuditUncertainMessage(before fsroot.Entry, update fsroot.MetadataUpdate) string {
	if update.Mode != nil {
		return "权限修改已执行但无法复核；原 mode: " + strconv.FormatUint(uint64(before.Mode.Perm()), 8) + "；新值未知。"
	}
	return "属主修改已执行但无法复核；原 uid/gid: " + strconv.FormatUint(uint64(before.OwnerUID), 10) + "/" + strconv.FormatUint(uint64(before.GroupGID), 10) + "；新值未知。"
}
