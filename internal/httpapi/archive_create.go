// 本文件处理经版本校验并原子发布的归档创建请求。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"filesage/internal/fsroot"
)

const maxArchiveCreateBodyBytes = 4 << 20

type archiveCreateRequest struct {
	Items              []archiveCreateItem  `json:"items"`
	Destination        string               `json:"destination"`
	DestinationVersion string               `json:"destinationVersion"`
	Name               string               `json:"name"`
	Format             fsroot.ArchiveFormat `json:"format,omitempty"`
}

type archiveCreateItem struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type archiveCreateResponse struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type archiveCreateFileWriter interface {
	WriteNewAtomicFrom(string, io.Reader, int64, fs.FileMode) (string, error)
}

type archiveCreateDestination struct {
	entry fsroot.Entry
	root  bool
}

type archiveCreatePublishResult struct {
	target       string
	name         string
	published    bool
	auditStarted bool
	err          error
	auditErr     error
}

func (request *archiveCreateRequest) UnmarshalJSON(data []byte) error {
	fields, err := strictObjectFields(data, "items", "destination", "destinationVersion", "name", "format")
	if err != nil {
		return err
	}
	for _, field := range []string{"items", "destination", "destinationVersion", "name"} {
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
	if err := json.Unmarshal(fields["destinationVersion"], &request.DestinationVersion); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["name"], &request.Name); err != nil {
		return err
	}
	if value, exists := fields["format"]; exists {
		return json.Unmarshal(value, &request.Format)
	}
	request.Format = fsroot.ArchiveFormatZIP
	return nil
}

func (item *archiveCreateItem) UnmarshalJSON(data []byte) error {
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

// createArchive 先完成临时归档和来源复核，再以无覆盖方式发布。
func (api *API) createArchive(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	request, ok := decodeArchiveCreateRequest(w, r)
	if !ok {
		return
	}
	paths, ok := validateArchiveCreateRequest(w, request)
	if !ok {
		return
	}
	if api.files == nil {
		writeError(w, http.StatusServiceUnavailable, "文件服务暂不可用。")
		return
	}
	if api.archiveSlots == nil {
		writeError(w, http.StatusServiceUnavailable, "归档服务暂不可用。")
		return
	}
	if err := r.Context().Err(); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	select {
	case api.archiveSlots <- struct{}{}:
		defer func() { <-api.archiveSlots }()
	default:
		if err := r.Context().Err(); err != nil {
			writeArchiveCreateError(w, err)
			return
		}
		writeError(w, http.StatusTooManyRequests, "已有一个归档任务正在运行，请稍后重试。")
		return
	}

	paths = append(paths, request.Destination)
	release, ok := api.lockMutationPaths(w, r, paths...)
	if !ok {
		return
	}
	defer release()
	if err := r.Context().Err(); err != nil {
		writeArchiveCreateError(w, err)
		return
	}

	selections, destination, err := api.preflightArchiveCreate(request)
	if err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	files, ok := api.files.(archiveFormatFileSystem)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "当前文件系统不支持安全的归档创建。")
		return
	}
	writer, ok := api.files.(archiveCreateFileWriter)
	if !ok {
		writeArchiveCreateError(w, fsroot.ErrAtomicWriteUnsupported)
		return
	}

	temporaryDirectory, err := os.MkdirTemp("", ".filesage-archive-")
	if err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	stopTracking := trackArchiveTemp(temporaryDirectory)
	var temporary *os.File
	defer func() {
		if temporary != nil {
			_ = temporary.Close()
		}
		stopTracking()
		_ = os.RemoveAll(temporaryDirectory)
	}()
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	temporary, err = os.OpenFile(filepath.Join(temporaryDirectory, archiveCreateTemporaryName(request.Format)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	if err := temporary.Chmod(0o600); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	if err := files.WriteArchiveFormat(r.Context(), archiveCreatePaths(selections), temporary, fsroot.ArchiveLimits{
		MaxEntries: maxArchiveEntries,
		MaxBytes:   maxArchiveSourceBytes,
	}, request.Format); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	if err := api.verifyArchiveCreateSources(selections); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	if err := r.Context().Err(); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	if err := temporary.Sync(); err != nil {
		writeArchiveCreateError(w, err)
		return
	}
	info, err := temporary.Stat()
	if err != nil {
		writeArchiveCreateError(w, err)
		return
	}

	publish := api.publishArchiveCreate(r.Context(), writer, temporary, info.Size(), request, selections, destination)
	if publish.published && publish.err != nil || publish.published && publish.auditErr != nil {
		api.refreshArchiveCreateIndex()
	}
	if publish.auditErr != nil {
		if publish.auditStarted {
			writeError(w, http.StatusInternalServerError, "文件可能已变更，审计结果待核查。")
		} else {
			writeError(w, http.StatusServiceUnavailable, "操作记录暂不可用，未执行文件操作。")
		}
		return
	}
	if publish.err != nil {
		if errors.Is(publish.err, fsroot.ErrPublishedNotDurable) {
			if isArchiveCreateStorageError(publish.err) {
				writeError(w, http.StatusInsufficientStorage, "压缩包已发布，但磁盘空间不足，持久化状态无法确认；请刷新目录核对。")
			} else {
				writeError(w, http.StatusInternalServerError, "压缩包已发布，但无法确认持久化状态；请刷新目录核对。")
			}
			return
		}
		writeArchiveCreateError(w, publish.err)
		return
	}
	if !publish.published {
		writeError(w, http.StatusInternalServerError, "文件系统未确认压缩包发布结果，请刷新目录核对。")
		return
	}
	writeJSON(w, http.StatusCreated, archiveCreateResponse{Path: publish.target, Name: publish.name})
}

func decodeArchiveCreateRequest(w http.ResponseWriter, r *http.Request) (archiveCreateRequest, bool) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, http.StatusBadRequest, "归档创建请求不能包含 URL 参数。")
		return archiveCreateRequest{}, false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "归档创建请求必须使用 JSON。")
		return archiveCreateRequest{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxArchiveCreateBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "归档创建请求内容过大。")
		} else {
			writeError(w, http.StatusBadRequest, "无法读取归档创建请求。")
		}
		return archiveCreateRequest{}, false
	}
	var request archiveCreateRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "归档创建请求格式无效。")
		return archiveCreateRequest{}, false
	}
	return request, true
}

func validateArchiveCreateRequest(w http.ResponseWriter, request archiveCreateRequest) ([]string, bool) {
	if len(request.Items) == 0 || len(request.Items) > maxArchiveSelections {
		writeError(w, http.StatusBadRequest, "所选项目数量必须在 1 到 256 项之间。")
		return nil, false
	}
	if !validRelativePath(request.Destination, true) || isReservedPath(request.Destination) {
		writeError(w, http.StatusBadRequest, "目标目录路径无效。")
		return nil, false
	}
	if request.Destination == "" && request.DestinationVersion != "" || request.Destination != "" && (request.DestinationVersion == "" || len(request.DestinationVersion) > maxBatchTransferVersionLen) {
		writeError(w, http.StatusBadRequest, "目标目录版本无效。")
		return nil, false
	}
	if !validArchiveFormat(request.Format) || !validArchiveName(request.Name, request.Format) {
		writeError(w, http.StatusBadRequest, "归档格式或文件名无效。")
		return nil, false
	}
	paths := make([]string, 0, len(request.Items)+1)
	seen := make(map[string]struct{}, len(request.Items))
	for _, item := range request.Items {
		if !validRelativePath(item.Path, false) || isReservedPath(item.Path) || item.Version == "" || len(item.Version) > maxBatchTransferVersionLen {
			writeError(w, http.StatusBadRequest, "所选路径或版本无效。")
			return nil, false
		}
		if _, exists := seen[item.Path]; exists {
			writeError(w, http.StatusBadRequest, "所选路径不能重复。")
			return nil, false
		}
		seen[item.Path] = struct{}{}
		paths = append(paths, item.Path)
	}
	return paths, true
}

func (api *API) preflightArchiveCreate(request archiveCreateRequest) ([]archiveCreateItem, archiveCreateDestination, error) {
	selections := append([]archiveCreateItem(nil), request.Items...)
	for _, item := range selections {
		entry, err := api.archiveCreateEntry(item.Path, item.Version)
		if err != nil {
			return nil, archiveCreateDestination{}, err
		}
		if entry.Symlink || !entry.IdentityAvailable {
			return nil, archiveCreateDestination{}, fsroot.ErrArchiveUnsafeEntry
		}
		switch entry.Kind {
		case fsroot.KindFile:
			if !entry.Mode.IsRegular() {
				return nil, archiveCreateDestination{}, fsroot.ErrArchiveUnsafeEntry
			}
		case fsroot.KindDirectory:
			if !entry.Mode.IsDir() {
				return nil, archiveCreateDestination{}, fsroot.ErrArchiveUnsafeEntry
			}
			if request.Destination == entry.Path || strings.HasPrefix(request.Destination, entry.Path+"/") {
				return nil, archiveCreateDestination{}, fsroot.ErrInvalidPath
			}
		default:
			return nil, archiveCreateDestination{}, fsroot.ErrArchiveUnsafeEntry
		}
	}

	if request.Destination == "" {
		if _, err := api.files.List(""); err != nil {
			return nil, archiveCreateDestination{}, err
		}
		return selections, archiveCreateDestination{root: true}, nil
	}
	entry, err := api.archiveCreateEntry(request.Destination, request.DestinationVersion)
	if err != nil {
		return nil, archiveCreateDestination{}, err
	}
	if entry.Symlink {
		return nil, archiveCreateDestination{}, fsroot.ErrSymlinkPath
	}
	if entry.Kind != fsroot.KindDirectory || !entry.Mode.IsDir() {
		return nil, archiveCreateDestination{}, fsroot.ErrNotDirectory
	}
	if !entry.IdentityAvailable {
		return nil, archiveCreateDestination{}, fsroot.ErrIdentityUnavailable
	}
	return selections, archiveCreateDestination{entry: entry}, nil
}

func (api *API) archiveCreateEntry(relativePath, version string) (fsroot.Entry, error) {
	inspector, ok := api.files.(indexedFileSystem)
	if !ok {
		return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
	}
	entry, err := inspector.LstatEntry(relativePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fsroot.Entry{}, fsroot.ErrConflict
		}
		return fsroot.Entry{}, err
	}
	if entry.Symlink {
		if !entry.LinkIdentityAvailable {
			return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
		}
	} else if !entry.IdentityAvailable {
		return fsroot.Entry{}, fsroot.ErrIdentityUnavailable
	}
	if version != "" && versionForEntry(entry) != version {
		return fsroot.Entry{}, fsroot.ErrConflict
	}
	return entry, nil
}

func (api *API) verifyArchiveCreateSources(selections []archiveCreateItem) error {
	for _, item := range selections {
		entry, err := api.archiveCreateEntry(item.Path, item.Version)
		if err != nil {
			return err
		}
		if entry.Symlink || !entry.IdentityAvailable || (entry.Kind != fsroot.KindFile && entry.Kind != fsroot.KindDirectory) {
			return fsroot.ErrArchiveUnsafeEntry
		}
	}
	return nil
}

func (api *API) verifyArchiveCreateDestination(relativePath string, initial archiveCreateDestination) error {
	if initial.root {
		_, err := api.files.List("")
		return err
	}
	entry, err := api.archiveCreateEntry(relativePath, "")
	if err != nil {
		return err
	}
	if entry.Symlink || entry.Kind != fsroot.KindDirectory || !entry.Mode.IsDir() || !entry.IdentityAvailable || !sameArchiveCreateDirectory(initial.entry, entry) {
		return fsroot.ErrConflict
	}
	return nil
}

func sameArchiveCreateDirectory(first, second fsroot.Entry) bool {
	return first.Kind == fsroot.KindDirectory && second.Kind == fsroot.KindDirectory && first.IdentityAvailable && second.IdentityAvailable && first.Device == second.Device && first.Inode == second.Inode
}

func archiveCreatePaths(selections []archiveCreateItem) []string {
	paths := make([]string, len(selections))
	for index, item := range selections {
		paths[index] = item.Path
	}
	return paths
}

func (api *API) publishArchiveCreate(ctx context.Context, writer archiveCreateFileWriter, temporary *os.File, size int64, request archiveCreateRequest, selections []archiveCreateItem, destination archiveCreateDestination) archiveCreatePublishResult {
	for suffix := 0; suffix <= 10000; suffix++ {
		if err := ctx.Err(); err != nil {
			return archiveCreatePublishResult{err: err}
		}
		if err := api.verifyArchiveCreateSources(selections); err != nil {
			return archiveCreatePublishResult{err: err}
		}
		if err := api.verifyArchiveCreateDestination(request.Destination, destination); err != nil {
			return archiveCreatePublishResult{err: err}
		}
		name := numberedArchiveCreateName(request.Name, suffix, request.Format)
		if !validEntryName(name) || isReservedPath(name) {
			return archiveCreatePublishResult{err: fsroot.ErrConflict}
		}
		target := joinPath(request.Destination, name)
		exists, err := api.archiveCreateTargetExists(request.Destination, target)
		if err != nil {
			return archiveCreatePublishResult{err: err}
		}
		if exists {
			continue
		}
		if _, err := temporary.Seek(0, io.SeekStart); err != nil {
			return archiveCreatePublishResult{target: target, name: name, err: err}
		}
		auditID, auditErr := api.beginManualAudit(ctx, "manual_archive_create", "", target)
		if auditErr != nil {
			return archiveCreatePublishResult{target: target, name: name, auditErr: auditErr}
		}
		digest, err := writer.WriteNewAtomicFrom(target, archiveCreateContextReader{ctx: ctx, reader: temporary}, size, 0o644)
		published := errors.Is(err, fsroot.ErrPublishedNotDurable) && digest != ""
		outcome, message := "succeeded", "压缩包创建完成。"
		if err != nil || digest == "" {
			outcome, message = "failed", "压缩包创建未完成。"
			if published || err == nil {
				outcome, message = "needs_review", "压缩包发布结果待核查。"
			}
		}
		if finishErr := finishManualAuditAfterWrite(api, ctx, auditID, outcome, message); finishErr != nil {
			return archiveCreatePublishResult{
				target: target, name: name, published: published, auditStarted: true, err: err, auditErr: finishErr,
			}
		}
		if errors.Is(err, fs.ErrExist) {
			exists, inspectErr := api.archiveCreateTargetExists(request.Destination, target)
			if inspectErr != nil {
				return archiveCreatePublishResult{target: target, name: name, err: inspectErr}
			}
			if exists {
				continue
			}
		}
		if err != nil {
			return archiveCreatePublishResult{target: target, name: name, published: published, err: err}
		}
		if digest == "" {
			return archiveCreatePublishResult{target: target, name: name, err: errors.New("filesystem did not confirm archive publication")}
		}
		return archiveCreatePublishResult{target: target, name: name, published: true}
	}
	return archiveCreatePublishResult{err: fsroot.ErrConflict}
}

func validArchiveFormat(format fsroot.ArchiveFormat) bool {
	switch format {
	case fsroot.ArchiveFormatZIP, fsroot.ArchiveFormat7Z, fsroot.ArchiveFormatTAR, fsroot.ArchiveFormatTARGZ:
		return true
	default:
		return false
	}
}

func archiveFormatExtension(format fsroot.ArchiveFormat) string {
	switch format {
	case fsroot.ArchiveFormatZIP:
		return ".zip"
	case fsroot.ArchiveFormat7Z:
		return ".7z"
	case fsroot.ArchiveFormatTAR:
		return ".tar"
	case fsroot.ArchiveFormatTARGZ:
		return ".tar.gz"
	default:
		return ""
	}
}

func validArchiveName(name string, format fsroot.ArchiveFormat) bool {
	extension := archiveFormatExtension(format)
	return extension != "" && validEntryName(name) && !isReservedPath(name) && len(name) >= len(extension) && strings.EqualFold(name[len(name)-len(extension):], extension)
}

func archiveCreateTemporaryName(format fsroot.ArchiveFormat) string {
	return "archive" + archiveFormatExtension(format)
}

// numberedArchiveCreateName 自动编号并在 255 字节内保留完整扩展名。
func numberedArchiveCreateName(name string, suffix int, format fsroot.ArchiveFormat) string {
	if suffix == 0 {
		return name
	}
	extension := name[len(name)-len(archiveFormatExtension(format)):]
	stem := strings.TrimSuffix(name, extension)
	marker := fmt.Sprintf(" (%d)", suffix)
	maxStemBytes := 255 - len(marker) - len(extension)
	if len(stem) > maxStemBytes {
		stem = stem[:maxStemBytes]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
	}
	return stem + marker + extension
}

func (api *API) archiveCreateTargetExists(destination, target string) (bool, error) {
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

func (api *API) refreshArchiveCreateIndex() {
	if api.index != nil {
		api.index.Refresh(context.Background())
	}
}

type archiveCreateContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader archiveCreateContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func writeArchiveCreateError(w http.ResponseWriter, err error) {
	if isArchiveCreateStorageError(err) {
		writeError(w, http.StatusInsufficientStorage, "服务器磁盘空间不足，归档未能完成。")
		return
	}
	if errors.Is(err, fsroot.ErrAtomicWriteUnsupported) || errors.Is(err, fsroot.ErrNoReplaceUnsupported) {
		writeError(w, http.StatusNotImplemented, "当前文件系统不支持安全的原子无覆盖发布。")
		return
	}
	if errors.Is(err, fsroot.ErrArchiveDependency) {
		writeError(w, http.StatusServiceUnavailable, "服务器尚未安装 7-Zip，无法创建此格式的压缩包。")
		return
	}
	if errors.Is(err, fsroot.ErrArchiveCommand) {
		writeError(w, http.StatusInternalServerError, "7-Zip 创建压缩包失败，请检查服务器工具状态后重试。")
		return
	}
	if errors.Is(err, fsroot.ErrSymlinkPath) {
		writeError(w, http.StatusBadRequest, "所选项或目标路径不能经过软链接。")
		return
	}
	status, message := archiveErrorResponse(err)
	writeError(w, status, message)
}

func isArchiveCreateStorageError(err error) bool {
	return errors.Is(err, fsroot.ErrArchiveStorage) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
