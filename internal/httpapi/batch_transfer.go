// 本文件处理经整批预检的批量复制和移动。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"filesage/internal/fsroot"
)

const (
	maxBatchTransferItems      = 256
	maxBatchTransferBodyBytes  = 4 << 20
	maxBatchTransferVersionLen = 256
)

type batchTransferRequest struct {
	Items              []batchTransferInput `json:"items"`
	Destination        string               `json:"destination"`
	DestinationVersion string               `json:"destinationVersion"`
}

type batchTransferInput struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type batchTransferItem struct {
	SourcePath string `json:"sourcePath"`
	TargetPath string `json:"targetPath"`
	Status     string `json:"status"`
	IncludedIn string `json:"includedIn,omitempty"`
	Message    string `json:"message,omitempty"`
}

type batchTransferResponse struct {
	Status string              `json:"status"`
	Items  []batchTransferItem `json:"items"`
}

type batchTransferPlan struct {
	request  batchTransferInput
	entry    fsroot.Entry
	snapshot *fsroot.TreeSnapshot
	included string
	target   string
	suffix   int
}

type batchTransferMountInspector interface {
	MountID(string) (uint64, error)
	CheckTreeMounts(string, uint64) error
}

// batchCopy 批量复制文件和安全目录树。
func (api *API) batchCopy(w http.ResponseWriter, r *http.Request) {
	api.batchTransfer(w, r, false)
}

// batchMove 批量移动文件和安全目录树。
func (api *API) batchMove(w http.ResponseWriter, r *http.Request) {
	api.batchTransfer(w, r, true)
}

func (api *API) batchTransfer(w http.ResponseWriter, r *http.Request, move bool) {
	if !requireSameOrigin(w, r) {
		return
	}
	request, ok := decodeBatchTransferRequest(w, r)
	if !ok {
		return
	}
	plans, paths, ok := validateBatchTransferRequest(w, request)
	if !ok {
		return
	}
	if api.files == nil {
		writeError(w, http.StatusServiceUnavailable, "文件服务暂不可用。")
		return
	}

	release, ok := api.lockMutationPaths(w, r, paths...)
	if !ok {
		return
	}
	defer release()

	destination, err := api.preflightBatchTransfer(r.Context(), request, plans, move)
	if err != nil {
		writeFileError(w, err)
		return
	}

	ancestors := make(map[string]int, len(plans))
	for index := range plans {
		if plans[index].entry.Kind == fsroot.KindDirectory {
			ancestors[plans[index].request.Path] = index
		}
	}
	for index := range plans {
		plans[index].included = selectedBatchTransferAncestor(plans[index].request.Path, ancestors)
	}

	reserved := make(map[string]int, len(plans))
	for index := range plans {
		if plans[index].included != "" {
			continue
		}
		if err := api.assignBatchTransferTarget(request.Destination, &plans[index], index, reserved); err != nil {
			writeFileError(w, err)
			return
		}
	}
	for index := range plans {
		if plans[index].included == "" {
			continue
		}
		parentIndex := ancestors[plans[index].included]
		remainder := strings.TrimPrefix(plans[index].request.Path, plans[parentIndex].request.Path+"/")
		plans[index].target = path.Join(plans[parentIndex].target, remainder)
	}

	response := batchTransferResponse{Status: "complete", Items: make([]batchTransferItem, len(plans))}
	for index, plan := range plans {
		response.Items[index] = batchTransferItem{SourcePath: plan.request.Path, TargetPath: plan.target, Status: "not_started", IncludedIn: plan.included}
	}
	operationStarted := false
	for index := range plans {
		plan := &plans[index]
		if plan.included != "" {
			continue
		}
		status, published, operationErr, audited := api.executeBatchTransferItem(
			r.Context(), request.Destination, destination, plan, index, move, reserved,
		)
		operationStarted = operationStarted || audited
		if status == "audit_finish_failed" {
			writeError(w, http.StatusInternalServerError, "文件可能已变更，审计结果待核查。")
			return
		}
		if status == "audit_unavailable" {
			if !operationStarted {
				writeError(w, http.StatusServiceUnavailable, "操作记录暂不可用，未执行文件操作。")
				return
			}
			response.Status = "partial"
			response.Items[index].TargetPath = plan.target
			response.Items[index].Status = "failed"
			response.Items[index].Message = "操作记录暂不可用，未执行此项。"
			for childIndex := range plans {
				if plans[childIndex].included != plan.request.Path {
					continue
				}
				response.Items[childIndex].TargetPath = path.Join(plan.target, strings.TrimPrefix(plans[childIndex].request.Path, plan.request.Path+"/"))
				response.Items[childIndex].Status = "failed"
				response.Items[childIndex].Message = "所选上级目录未能处理，子项未单独操作。"
			}
			writeJSON(w, http.StatusMultiStatus, response)
			return
		}
		if operationErr != nil {
			response.Status = "partial"
			response.Items[index].TargetPath = plan.target
			response.Items[index].Status = status
			response.Items[index].Message = batchTransferFailureMessage(operationErr, move, published)
			for childIndex := range plans {
				if plans[childIndex].included != plan.request.Path {
					continue
				}
				remainder := strings.TrimPrefix(plans[childIndex].request.Path, plan.request.Path+"/")
				plans[childIndex].target = path.Join(plan.target, remainder)
				response.Items[childIndex].TargetPath = plans[childIndex].target
				if published {
					response.Items[childIndex].Status = "included"
					response.Items[childIndex].Message = "已包含在目标目录中；来源操作未完全完成。"
				} else {
					response.Items[childIndex].Status = "failed"
					response.Items[childIndex].Message = "所选上级目录未能处理，子项未单独操作。"
				}
			}
			writeJSON(w, http.StatusMultiStatus, response)
			return
		}
		response.Items[index].TargetPath = plan.target
		response.Items[index].Status = status
		for childIndex := range plans {
			if plans[childIndex].included == plan.request.Path {
				remainder := strings.TrimPrefix(plans[childIndex].request.Path, plan.request.Path+"/")
				plans[childIndex].target = path.Join(plan.target, remainder)
				response.Items[childIndex].TargetPath = plans[childIndex].target
				response.Items[childIndex].Status = "included"
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func decodeBatchTransferRequest(w http.ResponseWriter, r *http.Request) (batchTransferRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchTransferBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeBatchTransferDecodeError(w, err)
		return batchTransferRequest{}, false
	}
	var request batchTransferRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeBatchTransferDecodeError(w, err)
		return batchTransferRequest{}, false
	}
	return request, true
}

func (request *batchTransferRequest) UnmarshalJSON(data []byte) error {
	fields, err := strictObjectFields(data, "items", "destination", "destinationVersion")
	if err != nil {
		return err
	}
	for _, field := range []string{"items", "destination", "destinationVersion"} {
		if _, exists := fields[field]; !exists {
			return errors.New("required field is missing")
		}
	}
	if err := json.Unmarshal(fields["items"], &request.Items); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["destination"], &request.Destination); err != nil {
		return err
	}
	return json.Unmarshal(fields["destinationVersion"], &request.DestinationVersion)
}

func (item *batchTransferInput) UnmarshalJSON(data []byte) error {
	fields, err := strictObjectFields(data, "path", "version")
	if err != nil {
		return err
	}
	for _, field := range []string{"path", "version"} {
		if _, exists := fields[field]; !exists {
			return errors.New("required item field is missing")
		}
	}
	if err := json.Unmarshal(fields["path"], &item.Path); err != nil {
		return err
	}
	return json.Unmarshal(fields["version"], &item.Version)
}

func writeBatchTransferDecodeError(w http.ResponseWriter, err error) {
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "批量请求内容过大。")
		return
	}
	writeError(w, http.StatusBadRequest, "批量复制／移动请求格式无效。")
}

func validateBatchTransferRequest(w http.ResponseWriter, request batchTransferRequest) ([]batchTransferPlan, []string, bool) {
	if len(request.Items) == 0 || len(request.Items) > maxBatchTransferItems {
		writeError(w, http.StatusBadRequest, "所选项目数量无效。")
		return nil, nil, false
	}
	if !validRelativePath(request.Destination, true) || isReservedPath(request.Destination) {
		writeError(w, http.StatusBadRequest, "目标目录路径无效。")
		return nil, nil, false
	}
	if request.Destination == "" && request.DestinationVersion != "" || request.Destination != "" && (request.DestinationVersion == "" || len(request.DestinationVersion) > maxBatchTransferVersionLen) {
		writeError(w, http.StatusBadRequest, "目标目录版本无效。")
		return nil, nil, false
	}
	plans := make([]batchTransferPlan, len(request.Items))
	paths := make([]string, 0, len(request.Items)+1)
	seen := make(map[string]struct{}, len(request.Items))
	for index, item := range request.Items {
		if !validRelativePath(item.Path, false) || isReservedPath(item.Path) || item.Version == "" || len(item.Version) > maxBatchTransferVersionLen {
			writeError(w, http.StatusBadRequest, "所选路径或版本无效。")
			return nil, nil, false
		}
		if _, exists := seen[item.Path]; exists {
			writeError(w, http.StatusBadRequest, "所选项目不能重复。")
			return nil, nil, false
		}
		seen[item.Path] = struct{}{}
		plans[index].request = item
		paths = append(paths, item.Path)
	}
	paths = append(paths, request.Destination)
	return plans, paths, true
}

type batchTransferDestination struct {
	entry fsroot.Entry
}

func (api *API) preflightBatchTransfer(ctx context.Context, request batchTransferRequest, plans []batchTransferPlan, move bool) (batchTransferDestination, error) {
	destination := batchTransferDestination{}
	if request.Destination == "" {
		if _, err := api.files.List(""); err != nil {
			return destination, err
		}
	} else {
		entry, err := batchTransferEntryAtPath(api.files, request.Destination)
		if err != nil {
			return destination, err
		}
		if entry.Symlink {
			return destination, fsroot.ErrSymlinkPath
		}
		if entry.Kind != fsroot.KindDirectory {
			return destination, fsroot.ErrNotDirectory
		}
		if !entry.IdentityAvailable {
			return destination, fsroot.ErrIdentityUnavailable
		}
		destination = batchTransferDestination{entry: entry}
	}

	for index := range plans {
		entry, err := batchTransferEntryAtPath(api.files, plans[index].request.Path)
		if err != nil {
			return destination, err
		}
		if versionForEntry(entry) != plans[index].request.Version {
			return destination, fsroot.ErrConflict
		}
		if entry.Symlink || entry.Kind == fsroot.KindOther {
			return destination, fsroot.ErrTreeCopyUnsafeEntry
		}
		if !entry.IdentityAvailable {
			return destination, fsroot.ErrIdentityUnavailable
		}
		if move && parentPath(entry.Path) == request.Destination {
			return destination, fsroot.ErrInvalidPath
		}
		switch entry.Kind {
		case fsroot.KindFile:
			if !entry.Mode.IsRegular() {
				return destination, fsroot.ErrNotRegularFile
			}
		case fsroot.KindDirectory:
			if !entry.Mode.IsDir() {
				return destination, fsroot.ErrNotDirectory
			}
			if request.Destination == entry.Path || strings.HasPrefix(request.Destination, entry.Path+"/") {
				return destination, fsroot.ErrTreeCopyIntoSource
			}
			operator, ok := api.files.(directoryTreeOperator)
			if !ok {
				return destination, fsroot.ErrWalkUnsupported
			}
			snapshot, err := operator.SnapshotTree(ctx, entry.Path)
			if err != nil {
				return destination, err
			}
			if err := validateBatchTransferTree(snapshot, entry.Path); err != nil {
				return destination, err
			}
			if versionForEntry(snapshot.Entries[0].Entry) != plans[index].request.Version {
				return destination, fsroot.ErrConflict
			}
			if err := operator.VerifyTreeSnapshot(ctx, snapshot); err != nil {
				return destination, err
			}
			inspector, ok := api.files.(batchTransferMountInspector)
			if !ok {
				return destination, fsroot.ErrWalkUnsupported
			}
			if err := inspector.CheckTreeMounts(entry.Path, snapshot.Entries[0].MountID); err != nil {
				return destination, err
			}
			if move {
				sourceMount, err := inspector.MountID(entry.Path)
				if err != nil {
					return destination, err
				}
				parentMount, err := inspector.MountID(parentPath(entry.Path))
				if err != nil {
					return destination, err
				}
				if sourceMount != parentMount {
					return destination, fsroot.ErrMountRoot
				}
			}
			plans[index].entry = entry
			plans[index].snapshot = &snapshot
		default:
			return destination, fsroot.ErrTreeCopyUnsafeEntry
		}
		if entry.Kind == fsroot.KindFile {
			plans[index].entry = entry
		}
	}
	if request.Destination != "" {
		entry, err := batchTransferEntryAtPath(api.files, request.Destination)
		if err != nil {
			return destination, err
		}
		if versionForEntry(entry) != request.DestinationVersion || !sameBatchDirectoryIdentity(destination.entry, entry) {
			return destination, fsroot.ErrConflict
		}
		destination.entry = entry
	}

	for _, plan := range plans {
		if plan.entry.Kind == fsroot.KindFile {
			if _, ok := api.files.(singleFileOperator); !ok {
				return destination, fsroot.ErrAtomicWriteUnsupported
			}
			continue
		}
		if _, ok := api.files.(directoryTreeOperator); !ok {
			return destination, fsroot.ErrWalkUnsupported
		}
		if move {
			if _, ok := api.files.(directoryTreeMoveOperator); !ok {
				return destination, fsroot.ErrWalkUnsupported
			}
		}
	}
	return destination, nil
}

func validateBatchTransferTree(snapshot fsroot.TreeSnapshot, rootPath string) error {
	if len(snapshot.Entries) == 0 || snapshot.RootPath != rootPath || snapshot.Entries[0].Entry.Path != rootPath || snapshot.Entries[0].Entry.Kind != fsroot.KindDirectory {
		return fsroot.ErrInvalidPath
	}
	rootMount := snapshot.Entries[0].MountID
	for _, item := range snapshot.Entries {
		entry := item.Entry
		if entry.Symlink || entry.LinkIdentityAvailable || entry.LinkTarget != "" || entry.Kind == fsroot.KindOther {
			return fsroot.ErrTreeCopyUnsafeEntry
		}
		if !entry.IdentityAvailable || !item.ChangeTimeAvailable || item.MountID == 0 {
			return fsroot.ErrIdentityUnavailable
		}
		if item.MountID != rootMount {
			return fsroot.ErrNestedMount
		}
		switch entry.Kind {
		case fsroot.KindDirectory:
			if !entry.Mode.IsDir() {
				return fsroot.ErrInvalidPath
			}
		case fsroot.KindFile:
			if !entry.Mode.IsRegular() {
				return fsroot.ErrTreeCopyUnsafeEntry
			}
		default:
			return fsroot.ErrTreeCopyUnsafeEntry
		}
	}
	return nil
}

func selectedBatchTransferAncestor(source string, directories map[string]int) string {
	ancestor := ""
	for candidate := range directories {
		if candidate == source || !strings.HasPrefix(source, candidate+"/") {
			continue
		}
		if ancestor == "" || len(candidate) < len(ancestor) {
			ancestor = candidate
		}
	}
	return ancestor
}

// batchTransferEntryAtPath 按精确相对路径读取当前目录项。
func batchTransferEntryAtPath(files fileSystem, relativePath string) (fsroot.Entry, error) {
	inspector, ok := files.(indexedFileSystem)
	if !ok {
		return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
	}
	return inspector.LstatEntry(relativePath)
}

func (api *API) assignBatchTransferTarget(destination string, plan *batchTransferPlan, owner int, reserved map[string]int) error {
	for suffix := 0; suffix <= 10000; suffix++ {
		name := batchTransferName(plan.entry, suffix)
		if !validEntryName(name) || name == fsroot.TrashDirectoryName {
			return fsroot.ErrInvalidPath
		}
		target := joinPath(destination, name)
		if _, alreadyReserved := reserved[target]; alreadyReserved {
			continue
		}
		exists, err := api.fileOperationTargetExists(destination, target)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		plan.suffix = suffix
		plan.target = target
		reserved[target] = owner
		return nil
	}
	return fsroot.ErrConflict
}

func batchTransferName(entry fsroot.Entry, suffix int) string {
	if entry.Kind == fsroot.KindDirectory {
		return numberedDirectoryName(entry.Name, suffix)
	}
	return numberedFileName(entry.Name, suffix)
}

func (api *API) executeBatchTransferItem(ctx context.Context, destinationPath string, destination batchTransferDestination, plan *batchTransferPlan, owner int, move bool, reserved map[string]int) (string, bool, error, bool) {
	audited := false
	for suffix := plan.suffix; suffix <= 10000; suffix++ {
		if suffix != plan.suffix {
			old := plan.target
			if reserved[old] == owner {
				delete(reserved, old)
			}
			if err := api.assignBatchTransferTargetFrom(destinationPath, plan, owner, reserved, suffix); err != nil {
				return "failed", false, err, audited
			}
		}
		if err := api.verifyBatchTransferDestination(destinationPath, destination); err != nil {
			return "failed", false, err, audited
		}
		if err := api.verifyBatchTransferSource(ctx, plan); err != nil {
			return "failed", false, err, audited
		}
		exists, err := api.fileOperationTargetExists(destinationPath, plan.target)
		if err != nil {
			return "failed", false, err, audited
		}
		if exists {
			continue
		}

		eventType := "manual_batch_copy"
		if move {
			eventType = "manual_batch_move"
		}
		auditID, auditErr := api.beginManualAudit(ctx, eventType, plan.request.Path, plan.target)
		if auditErr != nil {
			return "audit_unavailable", false, auditErr, audited
		}
		audited = true
		status, published, operationErr := api.callBatchTransfer(ctx, plan, move)
		auditOutcome := "succeeded"
		auditMessage := "文件操作完成。"
		if operationErr != nil || status == "" || !published {
			auditOutcome = "failed"
			auditMessage = "文件操作未完成。"
			if published {
				if move {
					auditOutcome = "partial"
					auditMessage = "移动结果部分完成，请核查。"
				} else {
					auditOutcome = "needs_review"
					auditMessage = "复制结果待核查。"
				}
			}
		}
		if finishErr := finishManualAuditAfterWrite(api, ctx, auditID, auditOutcome, auditMessage); finishErr != nil {
			return "audit_finish_failed", published, finishErr, audited
		}
		if operationErr != nil {
			if errors.Is(operationErr, fs.ErrExist) || errors.Is(operationErr, fsroot.ErrConflict) {
				if err := api.verifyBatchTransferSource(ctx, plan); err != nil {
					return "failed", published, err, audited
				}
				if err := api.verifyBatchTransferDestination(destinationPath, destination); err != nil {
					return "failed", published, err, audited
				}
				exists, listErr := api.fileOperationTargetExists(destinationPath, plan.target)
				if listErr != nil {
					return "failed", published, listErr, audited
				}
				if exists {
					continue
				}
			}
			if published {
				return "partial", true, operationErr, audited
			}
			return "failed", false, operationErr, audited
		}
		if status == "" || !published {
			return "failed", published, fs.ErrInvalid, audited
		}
		return status, true, nil, audited
	}
	return "failed", false, fsroot.ErrConflict, audited
}

func (api *API) assignBatchTransferTargetFrom(destination string, plan *batchTransferPlan, owner int, reserved map[string]int, start int) error {
	for suffix := start; suffix <= 10000; suffix++ {
		name := batchTransferName(plan.entry, suffix)
		if !validEntryName(name) || name == fsroot.TrashDirectoryName {
			return fsroot.ErrInvalidPath
		}
		target := joinPath(destination, name)
		if _, exists := reserved[target]; exists {
			continue
		}
		exists, err := api.fileOperationTargetExists(destination, target)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		plan.suffix = suffix
		plan.target = target
		reserved[target] = owner
		return nil
	}
	return fsroot.ErrConflict
}

func (api *API) verifyBatchTransferDestination(destination string, initial batchTransferDestination) error {
	if destination == "" {
		_, err := api.files.List("")
		return err
	}
	entry, err := batchTransferEntryAtPath(api.files, destination)
	if err != nil {
		return err
	}
	if entry.Symlink || entry.Kind != fsroot.KindDirectory || !sameBatchDirectoryIdentity(initial.entry, entry) {
		return fsroot.ErrConflict
	}
	return nil
}

func sameBatchDirectoryIdentity(expected, current fsroot.Entry) bool {
	return expected.Kind == fsroot.KindDirectory && current.Kind == fsroot.KindDirectory && expected.IdentityAvailable && current.IdentityAvailable && expected.Device == current.Device && expected.Inode == current.Inode
}

func (api *API) verifyBatchTransferSource(ctx context.Context, plan *batchTransferPlan) error {
	entry, err := batchTransferEntryAtPath(api.files, plan.request.Path)
	if err != nil {
		return err
	}
	if versionForEntry(entry) != plan.request.Version {
		return fsroot.ErrConflict
	}
	if plan.snapshot != nil {
		operator, ok := api.files.(directoryTreeOperator)
		if !ok {
			return fsroot.ErrWalkUnsupported
		}
		return operator.VerifyTreeSnapshot(ctx, *plan.snapshot)
	}
	return nil
}

func (api *API) callBatchTransfer(ctx context.Context, plan *batchTransferPlan, move bool) (string, bool, error) {
	if plan.entry.Kind == fsroot.KindDirectory {
		if move {
			operator, ok := api.files.(directoryTreeMoveOperator)
			if !ok {
				return "", false, fsroot.ErrWalkUnsupported
			}
			result, err := operator.MoveTreeNoReplace(ctx, plan.entry.Path, plan.target, *plan.snapshot)
			if result.Published && !result.SourceRemoved {
				return "partial", true, errOrPartial(err)
			}
			if result.Published && result.SourceRemoved && err != nil {
				return "partial", true, err
			}
			if err != nil {
				return "", result.Published, err
			}
			if !result.Published || !result.SourceRemoved {
				return "", result.Published, fs.ErrInvalid
			}
			return "moved", true, nil
		}
		operator, ok := api.files.(directoryTreeOperator)
		if !ok {
			return "", false, fsroot.ErrWalkUnsupported
		}
		result, err := operator.CopyTreeNoReplace(ctx, plan.entry.Path, plan.target, *plan.snapshot)
		if err != nil {
			if result.Published {
				return "partial", true, err
			}
			return "", false, err
		}
		if !result.Published {
			return "", false, fs.ErrInvalid
		}
		return "copied", true, nil
	}
	operator, ok := api.files.(singleFileOperator)
	if !ok {
		return "", false, fsroot.ErrAtomicWriteUnsupported
	}
	if move {
		result, err := operator.MoveFileNoReplace(ctx, plan.entry.Path, plan.target, plan.entry.Fingerprint())
		if result.Published && !result.SourceRemoved {
			return "partial", true, errOrPartial(err)
		}
		if result.Published && result.SourceRemoved && err != nil {
			return "partial", true, err
		}
		if err != nil {
			return "", result.Published, err
		}
		if !result.Published || !result.SourceRemoved {
			return "", result.Published, fs.ErrInvalid
		}
		return "moved", true, nil
	}
	result, err := operator.CopyFileNoReplace(ctx, plan.entry.Path, plan.target, plan.entry.Fingerprint())
	if err != nil {
		if result.Published {
			return "partial", true, err
		}
		return "", false, err
	}
	if !result.Published {
		return "", false, fs.ErrInvalid
	}
	return "copied", true, nil
}

func errOrPartial(err error) error {
	if err != nil {
		return err
	}
	return errors.New("source was not safely removed")
}

func batchTransferFailureMessage(err error, move, published bool) string {
	if published {
		if move {
			return "目标副本已发布，但来源未能安全移除；请核对两处文件。"
		}
		return "目标已发布，但持久化状态无法确认；请刷新目录核对。"
	}
	switch {
	case errors.Is(err, fsroot.ErrConflict), errors.Is(err, fs.ErrExist):
		return "文件状态或目标目录已变化，请刷新后重试。"
	case errors.Is(err, fs.ErrPermission):
		return "没有权限完成此项操作。"
	case errors.Is(err, fs.ErrNotExist):
		return "来源或目标目录已不存在。"
	default:
		return "此项未能安全处理；请刷新目录并核对。"
	}
}
