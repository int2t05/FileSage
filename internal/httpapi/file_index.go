// 本文件提供索引状态与分页文件接口。
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"filesage/internal/fsroot"
	"filesage/internal/store"
)

const (
	defaultFilePageSize = 100
	maxFilePageSize     = 200
	maxFilePageOffset   = 10_000_000
	maxFilePageScanSize = 10_000
)

var errRecursiveIndexUnavailable = errors.New("recursive file index is unavailable")
var errIndexPageNeedsRefresh = errors.New("file index changed while reading")
var errCapturedDateIndexUnavailable = errors.New("capture date filter requires a file index")

type fileCursor struct {
	Generation int64  `json:"generation"`
	Offset     int    `json:"offset"`
	QueryHash  string `json:"queryHash"`
}

type filePageQuery struct {
	path           string
	keyword        string
	scope          string
	sort           string
	fileType       string
	minSize        *int64
	maxSize        *int64
	modifiedFrom   *time.Time
	modifiedBefore *time.Time
	capturedFrom   *time.Time
	capturedBefore *time.Time
	limit          int
	offset         int
	generation     int64
	state          string
}

func (query filePageQuery) hasCapturedDateFilter() bool {
	return query.capturedFrom != nil || query.capturedBefore != nil
}

func (query filePageQuery) needsCapturedDateIndex() bool {
	return query.hasCapturedDateFilter() || query.sort == "captured-asc" || query.sort == "captured-desc"
}

type filePageEntry struct {
	entry      fsroot.Entry
	capturedAt *time.Time
}

// listFilePage 提供服务端分页和名称／路径搜索。
func (api *API) listFilePage(w http.ResponseWriter, r *http.Request) {
	relativePath, ok := requestPath(w, r)
	if !ok {
		return
	}
	query, err := parseFilePageQuery(r, relativePath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if api.index != nil {
		status, err := api.index.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "无法读取文件索引状态，请稍后重试。")
			return
		}
		query.state = string(status.State)
		if query.offset == 0 && query.generation == 0 {
			query.generation = status.ActiveGeneration
		} else if query.generation > 0 && status.ActiveGeneration == 0 {
			writeError(w, http.StatusConflict, "文件索引已变化，请重新搜索。")
			return
		}
		if query.generation > 0 && query.generation != status.ActiveGeneration {
			query.state = "stale"
		}
	}

	var entries []filePageEntry
	var hasMore bool
	var nextOffset int
	useIndex := query.generation > 0 && api.index != nil
	if useIndex {
		var indexChanged bool
		entries, hasMore, nextOffset, indexChanged, err = api.indexedPage(r.Context(), query)
		if indexChanged {
			query.state = "stale"
		}
		if err != nil {
			if errors.Is(err, fsroot.ErrVolumeUnavailable) {
				writeFileError(w, err)
				return
			}
			if errors.Is(err, store.ErrFileIndexCaptureMetadataUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "拍摄日期排序和筛选需要完成一次新的文件索引，请稍后重试。")
				return
			}
			if errors.Is(err, errRecursiveIndexUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "该位置无法使用递归索引，请稍后重试或缩小搜索范围。")
				return
			}
			if errors.Is(err, errCapturedDateIndexUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "拍摄日期排序和筛选需要文件索引就绪，请稍后重试。")
				return
			}
			if errors.Is(err, errIndexPageNeedsRefresh) {
				writeError(w, http.StatusServiceUnavailable, "文件索引正在校准，请稍后重试。")
				return
			}
			if errors.Is(err, fs.ErrNotExist) {
				writeFileError(w, err)
				return
			}
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrFileIndexGenerationNotReady) {
				writeError(w, http.StatusConflict, "文件索引已变化，请重新搜索。")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "文件索引暂不可用，请稍后重试。")
			return
		}
	} else {
		if query.needsCapturedDateIndex() {
			writeError(w, http.StatusServiceUnavailable, "拍摄日期筛选需要文件索引就绪，请稍后重试。")
			return
		}
		if query.scope == "recursive" {
			writeError(w, http.StatusServiceUnavailable, "文件索引正在建立或校准，递归搜索暂不可用。")
			return
		}
		filesystemEntries, more, err := api.filesystemPage(query)
		if err != nil {
			writeFileError(w, err)
			return
		}
		entries = make([]filePageEntry, 0, len(filesystemEntries))
		for _, entry := range filesystemEntries {
			entries = append(entries, filePageEntry{entry: entry})
		}
		hasMore = more
		nextOffset = query.offset + len(entries)
	}

	response := listingResponse{
		Path: relativePath, Entries: make([]fileEntry, 0, len(entries)),
		Limit: query.limit, HasMore: hasMore, IndexState: query.state,
	}
	for _, indexedEntry := range entries {
		if indexedEntry.entry.Name == fsroot.TrashDirectoryName {
			continue
		}
		response.Entries = append(response.Entries, toFileEntry(indexedEntry.entry, indexedEntry.capturedAt))
	}
	if hasMore {
		response.NextCursor = encodeFileCursor(fileCursor{
			Generation: query.generation,
			Offset:     nextOffset,
			QueryHash:  fileQueryHash(query),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// indexStatus 返回扫描进度，不阻塞基础文件服务。
func (api *API) indexStatus(w http.ResponseWriter, r *http.Request) {
	if api.index == nil {
		writeError(w, http.StatusServiceUnavailable, "文件索引服务未配置。")
		return
	}
	status, err := api.index.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "无法读取文件索引状态。")
		return
	}
	response := indexStatusResponse{
		ActiveGeneration: status.ActiveGeneration, BuildingGeneration: status.BuildingGeneration,
		State: string(status.State), Scanned: status.Scanned, LastError: status.LastError,
	}
	if status.StartedAt != nil {
		response.StartedAt = status.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if status.CompletedAt != nil {
		response.CompletedAt = status.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, response)
}

func parseFilePageQuery(r *http.Request, relativePath string) (filePageQuery, error) {
	params := r.URL.Query()
	query := filePageQuery{path: relativePath, scope: params.Get("scope"), sort: params.Get("sort")}
	if query.scope == "" {
		query.scope = "current"
	}
	if query.scope != "current" && query.scope != "recursive" {
		return query, fmt.Errorf("搜索范围无效")
	}
	query.keyword = strings.TrimSpace(params.Get("q"))
	if utf8.RuneCountInString(query.keyword) > 256 {
		return query, fmt.Errorf("搜索内容不能超过 256 个字符")
	}
	if query.keyword == "" && len(params["scope"]) == 0 {
		query.scope = "current"
	}
	var err error
	if query.fileType, err = parseFileTypeFilter(params); err != nil {
		return query, err
	}
	if query.minSize, err = parseFileSizeFilter(params, "minSize"); err != nil {
		return query, err
	}
	if query.maxSize, err = parseFileSizeFilter(params, "maxSize"); err != nil {
		return query, err
	}
	if query.modifiedFrom, err = parseFileTimeFilter(params, "modifiedFrom"); err != nil {
		return query, err
	}
	if query.modifiedBefore, err = parseFileTimeFilter(params, "modifiedBefore"); err != nil {
		return query, err
	}
	if query.capturedFrom, err = parseFileTimeFilter(params, "capturedFrom"); err != nil {
		return query, err
	}
	if query.capturedBefore, err = parseFileTimeFilter(params, "capturedBefore"); err != nil {
		return query, err
	}
	if query.minSize != nil && query.maxSize != nil && *query.minSize > *query.maxSize {
		return query, fmt.Errorf("最小文件大小不能大于最大文件大小")
	}
	if query.modifiedFrom != nil && query.modifiedBefore != nil && query.modifiedFrom.After(*query.modifiedBefore) {
		return query, fmt.Errorf("修改时间范围无效")
	}
	if query.capturedFrom != nil && query.capturedBefore != nil && query.capturedFrom.After(*query.capturedBefore) {
		return query, fmt.Errorf("拍摄时间范围无效")
	}
	if query.sort == "" {
		query.sort = "name-asc"
	}
	switch query.sort {
	case "name-asc", "name-desc", "size-asc", "size-desc", "modified-asc", "modified-desc", "captured-asc", "captured-desc":
	default:
		return query, fmt.Errorf("排序方式无效")
	}
	query.limit = defaultFilePageSize
	if rawLimit := params.Get("limit"); rawLimit != "" {
		value, err := strconv.Atoi(rawLimit)
		if err != nil || value < 1 || value > maxFilePageSize {
			return query, fmt.Errorf("每页数量须为 1 到 %d", maxFilePageSize)
		}
		query.limit = value
	}
	if rawCursor := params.Get("cursor"); rawCursor != "" {
		cursor, err := decodeFileCursor(rawCursor)
		if err != nil || cursor.Generation < 0 || cursor.Offset < 0 || cursor.Offset > maxFilePageOffset {
			return query, fmt.Errorf("分页游标无效，请刷新目录")
		}
		query.generation, query.offset = cursor.Generation, cursor.Offset
		if cursor.QueryHash != fileQueryHash(query) {
			return query, fmt.Errorf("分页游标与当前查询不匹配")
		}
	}
	return query, nil
}

func parseFileFilterValue(params map[string][]string, name string) (string, bool, error) {
	values, exists := params[name]
	if !exists {
		return "", false, nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", true, fmt.Errorf("筛选参数 %s 只能填写一次且不能为空", name)
	}
	return values[0], true, nil
}

func parseFileTypeFilter(params map[string][]string) (string, error) {
	value, exists, err := parseFileFilterValue(params, "type")
	if err != nil || !exists {
		return "", err
	}
	switch value {
	case "image", "video", "audio", "document", "archive", "directory", "other":
		return value, nil
	default:
		return "", fmt.Errorf("文件类型筛选无效")
	}
}

func parseFileSizeFilter(params map[string][]string, name string) (*int64, error) {
	value, exists, err := parseFileFilterValue(params, name)
	if err != nil || !exists {
		return nil, err
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("筛选参数 %s 必须是非负整数字节数", name)
	}
	return &size, nil
}

func parseFileTimeFilter(params map[string][]string, name string) (*time.Time, error) {
	value, exists, err := parseFileFilterValue(params, name)
	if err != nil || !exists {
		return nil, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("筛选参数 %s 必须使用 RFC3339 时间", name)
	}
	utc := parsed.UTC()
	return &utc, nil
}

func fileQueryHash(query filePageQuery) string {
	data, _ := json.Marshal([]any{
		query.path, query.keyword, query.scope, query.sort, query.limit, query.generation,
		query.fileType, query.minSize, query.maxSize, query.modifiedFrom, query.modifiedBefore,
		query.capturedFrom, query.capturedBefore,
	})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func encodeFileCursor(cursor fileCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeFileCursor(value string) (fileCursor, error) {
	var cursor fileCursor
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, err
	}
	err = json.Unmarshal(data, &cursor)
	return cursor, err
}

func (api *API) indexedPage(ctx context.Context, query filePageQuery) ([]filePageEntry, bool, int, bool, error) {
	filesystem, ok := api.files.(indexedFileSystem)
	if !ok {
		if query.needsCapturedDateIndex() {
			return nil, false, query.offset, false, errCapturedDateIndexUnavailable
		}
		if query.scope == "recursive" {
			return nil, false, query.offset, false, errRecursiveIndexUnavailable
		}
		filesystemEntries, hasMore, err := api.filesystemPage(query)
		entries := make([]filePageEntry, 0, len(filesystemEntries))
		for _, entry := range filesystemEntries {
			entries = append(entries, filePageEntry{entry: entry})
		}
		return entries, hasMore, query.offset + len(entries), false, err
	}
	if query.path != "" {
		parent, err := filesystem.LstatEntry(query.path)
		if err == nil && parent.MountUnavailable {
			return nil, false, query.offset, false, fsroot.ErrVolumeUnavailable
		}
		if err != nil || parent.Symlink {
			if query.needsCapturedDateIndex() {
				return nil, false, query.offset, false, errCapturedDateIndexUnavailable
			}
			if query.scope == "recursive" {
				return nil, false, query.offset, false, errRecursiveIndexUnavailable
			}
			filesystemEntries, hasMore, err := api.filesystemPage(query)
			entries := make([]filePageEntry, 0, len(filesystemEntries))
			for _, entry := range filesystemEntries {
				entries = append(entries, filePageEntry{entry: entry})
			}
			return entries, hasMore, query.offset + len(entries), false, err
		}
	}
	batchSize := max(query.limit+1, 64)
	entries := make([]filePageEntry, 0, query.limit)
	nextOffset := query.offset
	scanned := 0
	indexChanged := false
	refreshRequested := false
	for scanned < maxFilePageScanSize {
		records, err := api.index.Query(ctx, store.FileIndexQuery{
			Generation:   query.generation,
			ParentPath:   query.path,
			Recursive:    query.scope == "recursive",
			Keyword:      query.keyword,
			Type:         query.fileType,
			MinSize:      query.minSize,
			MaxSize:      query.maxSize,
			ModifiedFrom: query.modifiedFrom, ModifiedBefore: query.modifiedBefore,
			CapturedFrom: query.capturedFrom, CapturedBefore: query.capturedBefore,
			Sort:   query.sort,
			Limit:  batchSize,
			Offset: nextOffset,
		})
		if err != nil {
			return nil, false, nextOffset, indexChanged, err
		}
		hasMoreRecords := len(records) > batchSize
		if hasMoreRecords {
			records = records[:batchSize]
		}
		for _, record := range records {
			if len(entries) == query.limit {
				return entries, true, nextOffset, indexChanged, nil
			}
			entry, err := filesystem.LstatEntry(record.RelativePath)
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsroot.ErrVolumeUnavailable) {
				indexChanged = true
				if !refreshRequested {
					api.index.Refresh(context.Background())
					refreshRequested = true
				}
				nextOffset++
				scanned++
				continue
			}
			if err != nil {
				return nil, false, nextOffset, indexChanged, err
			}
			if entry.MountUnavailable {
				entries = append(entries, filePageEntry{entry: entry, capturedAt: record.CapturedAt})
				nextOffset++
				scanned++
				continue
			}
			if !matchesIndexedEntry(record, entry) {
				indexChanged = true
				if !refreshRequested {
					api.index.Refresh(context.Background())
					refreshRequested = true
				}
				nextOffset++
				scanned++
				continue
			}
			entries = append(entries, filePageEntry{entry: entry, capturedAt: record.CapturedAt})
			nextOffset++
			scanned++
		}
		if !hasMoreRecords {
			return entries, false, nextOffset, indexChanged, nil
		}
	}
	if len(entries) == 0 {
		return nil, false, nextOffset, indexChanged, errIndexPageNeedsRefresh
	}
	return entries, true, nextOffset, indexChanged, nil
}

func matchesIndexedEntry(record store.FileIndexRecord, entry fsroot.Entry) bool {
	if entry.Name != record.Name || entry.Symlink != (record.Kind == "symlink") {
		return false
	}
	if entry.Symlink {
		if record.Device != nil && entry.LinkIdentityAvailable && *record.Device != entry.LinkDevice {
			return false
		}
		if record.Inode != nil && entry.LinkIdentityAvailable && *record.Inode != entry.LinkInode {
			return false
		}
		return true
	}
	if string(entry.Kind) != record.Kind || entry.Size != record.SizeBytes || !entry.ModifiedAt.Equal(record.ModifiedAt) || uint32(entry.Mode) != record.Mode {
		return false
	}
	if record.Device != nil && entry.IdentityAvailable && *record.Device != entry.Device {
		return false
	}
	if record.Inode != nil && entry.IdentityAvailable && *record.Inode != entry.Inode {
		return false
	}
	return true
}

func (api *API) filesystemPage(query filePageQuery) ([]fsroot.Entry, bool, error) {
	if query.needsCapturedDateIndex() {
		return nil, false, errCapturedDateIndexUnavailable
	}
	entries, err := api.files.List(query.path)
	if err != nil {
		return nil, false, err
	}
	if query.keyword != "" {
		keyword := strings.ToLower(query.keyword)
		matching := entries[:0]
		for _, entry := range entries {
			if strings.Contains(strings.ToLower(entry.Name), keyword) || strings.Contains(strings.ToLower(entry.Path), keyword) {
				matching = append(matching, entry)
			}
		}
		entries = matching
	}
	matching := entries[:0]
	for _, entry := range entries {
		kind := string(entry.Kind)
		if entry.Symlink {
			kind = "symlink"
		}
		if query.fileType != "" && store.FileIndexCategory(entry.Name, "", kind) != query.fileType {
			continue
		}
		if query.minSize != nil || query.maxSize != nil {
			if kind != "file" || query.minSize != nil && entry.Size < *query.minSize || query.maxSize != nil && entry.Size > *query.maxSize {
				continue
			}
		}
		if query.modifiedFrom != nil && entry.ModifiedAt.Before(*query.modifiedFrom) {
			continue
		}
		if query.modifiedBefore != nil && !entry.ModifiedAt.Before(*query.modifiedBefore) {
			continue
		}
		matching = append(matching, entry)
	}
	entries = matching
	sortFileEntries(entries, query.sort)
	if query.offset >= len(entries) {
		return []fsroot.Entry{}, false, nil
	}
	end := min(query.offset+query.limit, len(entries))
	return entries[query.offset:end], end < len(entries), nil
}

func sortFileEntries(entries []fsroot.Entry, sortMode string) {
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.Kind == fsroot.KindDirectory && right.Kind != fsroot.KindDirectory {
			return true
		}
		if right.Kind == fsroot.KindDirectory && left.Kind != fsroot.KindDirectory {
			return false
		}
		var comparison int
		switch sortMode {
		case "name-desc":
			comparison = strings.Compare(strings.ToLower(right.Name), strings.ToLower(left.Name))
		case "size-asc":
			comparison = compareInt64(left.Size, right.Size)
		case "size-desc":
			comparison = compareInt64(right.Size, left.Size)
		case "modified-asc":
			comparison = compareInt64(left.ModifiedAt.UnixNano(), right.ModifiedAt.UnixNano())
		case "modified-desc":
			comparison = compareInt64(right.ModifiedAt.UnixNano(), left.ModifiedAt.UnixNano())
		default:
			comparison = strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
		}
		if comparison == 0 {
			return left.Path < right.Path
		}
		return comparison < 0
	})
}

func compareInt64(left, right int64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func toFileEntry(entry fsroot.Entry, capturedAt *time.Time) fileEntry {
	response := fileEntry{
		Name: entry.Name, Path: entry.Path, Kind: entry.Kind, Size: entry.Size,
		ModifiedAt: entry.ModifiedAt.UTC().Format(time.RFC3339Nano),
		Mode:       uint32(entry.Mode.Perm()), OwnerUID: entry.OwnerUID, GroupGID: entry.GroupGID,
		OwnershipAvailable: entry.OwnershipAvailable, Symlink: entry.Symlink,
		LinkStatus: entry.LinkStatus, MountUnavailable: entry.MountUnavailable,
		ReadOnly: entry.ReadOnly, Version: versionForEntry(entry),
	}
	if capturedAt != nil {
		value := capturedAt.UTC().Format(time.RFC3339Nano)
		response.CapturedAt = &value
	}
	return response
}

func refreshIndexAfterMutation(next http.Handler, index fileIndexer) http.Handler {
	if index == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isIndexedMutation(r) {
			next.ServeHTTP(w, r)
			return
		}
		writer := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		if writer.status >= http.StatusOK && writer.status < http.StatusMultipleChoices {
			index.Refresh(context.Background())
		}
	})
}

func isIndexedMutation(r *http.Request) bool {
	switch r.Method {
	case http.MethodPut:
		return r.URL.Path == "/api/v1/files/content"
	case http.MethodPatch:
		return r.URL.Path == "/api/v1/files/metadata"
	case http.MethodDelete:
		return strings.HasPrefix(r.URL.Path, "/api/v1/files") || strings.HasPrefix(r.URL.Path, "/api/v1/trash")
	case http.MethodPost:
		if strings.HasPrefix(r.URL.Path, "/api/v1/files/directories") || strings.HasPrefix(r.URL.Path, "/api/v1/files/copy") || strings.HasPrefix(r.URL.Path, "/api/v1/files/move") || strings.HasPrefix(r.URL.Path, "/api/v1/files/rename") || strings.HasPrefix(r.URL.Path, "/api/v1/files/batch-rename") || r.URL.Path == "/api/v1/files/batch-copy" || r.URL.Path == "/api/v1/files/batch-move" || r.URL.Path == "/api/v1/files/archive/extract" || strings.HasPrefix(r.URL.Path, "/api/v1/trash/") && strings.HasSuffix(r.URL.Path, "/restore") {
			return true
		}
		return strings.HasPrefix(r.URL.Path, "/api/v1/agent/plans/") && strings.HasSuffix(r.URL.Path, "/execute")
	default:
		return false
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
