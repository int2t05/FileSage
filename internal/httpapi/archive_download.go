// 本文件处理经过版本校验的原生 ZIP 下载请求。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
)

const (
	maxArchiveRequestBytes = 4 << 20
	maxArchiveSelections   = 256
	maxArchiveEntries      = 50000
	maxArchiveSourceBytes  = 20 << 30
	archiveTempRetention   = 24 * time.Hour
	archiveTempCleanPeriod = time.Hour
)

var (
	archiveTempJanitorOnce sync.Once
	archiveTempActiveMu    sync.Mutex
	archiveTempActive      = make(map[string]struct{})
)

type archiveSelection struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

func (api *API) downloadArchive(w http.ResponseWriter, r *http.Request) {
	if !requireSameOrigin(w, r) {
		return
	}
	selections, ok := parseArchiveSelections(w, r)
	if !ok {
		return
	}
	select {
	case api.archiveSlots <- struct{}{}:
		defer func() { <-api.archiveSlots }()
	default:
		writeArchiveHTML(w, http.StatusTooManyRequests, "已有一个打包任务正在运行，请稍后重试。")
		return
	}

	paths := make([]string, 0, len(selections))
	for _, selection := range selections {
		paths = append(paths, selection.Path)
	}
	release, err := api.locks.Lock(r.Context(), coordination.PathKeys(paths...)...)
	if err != nil {
		writeArchiveHTML(w, http.StatusRequestTimeout, "等待其他文件操作时已取消，请刷新后重试。")
		return
	}
	defer release()

	for _, selection := range selections {
		if _, err := api.verifyEntryVersion(selection.Path, selection.Version); err != nil {
			status, message := archiveErrorResponse(err)
			writeArchiveHTML(w, status, message)
			return
		}
	}

	files, ok := api.files.(archiveFileSystem)
	if !ok {
		writeArchiveHTML(w, http.StatusServiceUnavailable, "当前服务无法安全打包所选文件。")
		return
	}

	temporaryDirectory, err := os.MkdirTemp("", ".filesage-archive-")
	if err != nil {
		writeArchiveHTML(w, http.StatusInsufficientStorage, "服务器临时空间不足，无法创建压缩包。")
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
		writeArchiveHTML(w, http.StatusInsufficientStorage, "无法安全创建临时压缩包。")
		return
	}
	temporaryPath := filepath.Join(temporaryDirectory, "archive.zip")
	temporary, err = os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		writeArchiveHTML(w, http.StatusInsufficientStorage, "服务器临时空间不足，无法创建压缩包。")
		return
	}
	if err := temporary.Chmod(0o600); err != nil {
		writeArchiveHTML(w, http.StatusInsufficientStorage, "无法安全创建临时压缩包。")
		return
	}

	archivePaths := collapseArchiveSelections(selections)
	limits := fsroot.ArchiveLimits{MaxEntries: maxArchiveEntries, MaxBytes: maxArchiveSourceBytes}
	if err := files.WriteArchive(r.Context(), archivePaths, temporary, limits); err != nil {
		status, message := archiveErrorResponse(err)
		writeArchiveHTML(w, status, message)
		return
	}
	for _, selection := range selections {
		if _, err := api.verifyEntryVersion(selection.Path, selection.Version); err != nil {
			writeArchiveHTML(w, http.StatusConflict, "打包期间文件状态发生变化，压缩包已丢弃。请刷新后重试。")
			return
		}
	}
	if err := r.Context().Err(); err != nil {
		return
	}
	if err := temporary.Sync(); err != nil {
		writeArchiveHTML(w, http.StatusInsufficientStorage, "服务器临时空间不足，压缩包未能完成。")
		return
	}
	info, err := temporary.Stat()
	if err != nil {
		writeArchiveHTML(w, http.StatusInternalServerError, "无法读取已完成的压缩包。")
		return
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		writeArchiveHTML(w, http.StatusInternalServerError, "无法准备压缩包下载。")
		return
	}

	filename := "filesage-selection.zip"
	contentDisposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, temporary)
}

func cleanupArchiveTemps() {
	archiveTempJanitorOnce.Do(func() {
		cleanupOldArchiveTemps()
		go func() {
			ticker := time.NewTicker(archiveTempCleanPeriod)
			defer ticker.Stop()
			for range ticker.C {
				cleanupOldArchiveTemps()
			}
		}()
	})
}

func cleanupOldArchiveTemps() {
	cleanupArchiveTempsIn(os.TempDir(), time.Now().Add(-archiveTempRetention))
}

func trackArchiveTemp(directory string) func() {
	directory = filepath.Clean(directory)
	archiveTempActiveMu.Lock()
	archiveTempActive[directory] = struct{}{}
	archiveTempActiveMu.Unlock()
	return func() {
		archiveTempActiveMu.Lock()
		delete(archiveTempActive, directory)
		archiveTempActiveMu.Unlock()
	}
}

func archiveTempIsActive(directory string) bool {
	archiveTempActiveMu.Lock()
	defer archiveTempActiveMu.Unlock()
	_, active := archiveTempActive[filepath.Clean(directory)]
	return active
}

func cleanupArchiveTempsIn(tempRoot string, cutoff time.Time) {
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".filesage-archive-") {
			continue
		}
		directory := filepath.Join(tempRoot, entry.Name())
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		children, err := os.ReadDir(directory)
		if err != nil || len(children) != 1 || !isArchiveTemporaryName(children[0].Name()) {
			continue
		}
		archivePath := filepath.Join(directory, children[0].Name())
		archiveInfo, err := os.Lstat(archivePath)
		if err != nil || !archiveInfo.Mode().IsRegular() || archiveInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		lastUsed := info.ModTime()
		if archiveInfo.ModTime().After(lastUsed) {
			lastUsed = archiveInfo.ModTime()
		}
		if lastUsed.Before(cutoff) {
			if !archiveTempIsActive(directory) {
				_ = os.RemoveAll(directory)
			}
		}
	}
}

func isArchiveTemporaryName(name string) bool {
	switch name {
	case "archive.zip", "archive.tar", "archive.tar.gz":
		return true
	default:
		return false
	}
}

func parseArchiveSelections(w http.ResponseWriter, r *http.Request) ([]archiveSelection, bool) {
	if r.URL.RawQuery != "" {
		writeArchiveHTML(w, http.StatusBadRequest, "打包请求不能包含 URL 参数。")
		return nil, false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		writeArchiveHTML(w, http.StatusUnsupportedMediaType, "打包请求格式无效。")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxArchiveRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			writeArchiveHTML(w, http.StatusRequestEntityTooLarge, "所选项目过多，请分批打包。")
		} else {
			writeArchiveHTML(w, http.StatusBadRequest, "无法读取打包请求。")
		}
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || len(form) != 1 || len(form["selection"]) != 1 {
		writeArchiveHTML(w, http.StatusBadRequest, "打包请求格式无效。")
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(form.Get("selection")))
	decoder.DisallowUnknownFields()
	var selections []archiveSelection
	if err := decoder.Decode(&selections); err != nil {
		writeArchiveHTML(w, http.StatusBadRequest, "所选项目列表无效。")
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || len(selections) == 0 || len(selections) > maxArchiveSelections {
		writeArchiveHTML(w, http.StatusBadRequest, "所选项目数量无效。")
		return nil, false
	}

	versions := make(map[string]string, len(selections))
	for _, selection := range selections {
		if !validRelativePath(selection.Path, false) || hasTrashPathComponent(selection.Path) || selection.Version == "" || len(selection.Version) > 256 {
			writeArchiveHTML(w, http.StatusBadRequest, "所选路径或版本无效。")
			return nil, false
		}
		if previous, exists := versions[selection.Path]; exists {
			if previous != selection.Version {
				writeArchiveHTML(w, http.StatusConflict, "同一路径包含不同版本，请刷新后重试。")
				return nil, false
			}
			continue
		}
		versions[selection.Path] = selection.Version
	}
	paths := make([]string, 0, len(versions))
	for relativePath := range versions {
		paths = append(paths, relativePath)
	}
	sort.Strings(paths)
	unique := make([]archiveSelection, 0, len(paths))
	for _, relativePath := range paths {
		unique = append(unique, archiveSelection{Path: relativePath, Version: versions[relativePath]})
	}
	return unique, true
}

func hasTrashPathComponent(relativePath string) bool {
	for _, component := range strings.Split(relativePath, "/") {
		if component == fsroot.TrashDirectoryName {
			return true
		}
	}
	return false
}

func collapseArchiveSelections(selections []archiveSelection) []string {
	paths := make([]string, 0, len(selections))
	for _, selection := range selections {
		paths = append(paths, selection.Path)
	}
	sort.Strings(paths)
	collapsed := make([]string, 0, len(paths))
	kept := make(map[string]struct{}, len(paths))
	for _, relativePath := range paths {
		if archiveSelectionCovered(relativePath, kept) {
			continue
		}
		collapsed = append(collapsed, relativePath)
		kept[relativePath] = struct{}{}
	}
	return collapsed
}

func archiveSelectionCovered(relativePath string, kept map[string]struct{}) bool {
	for parent := path.Dir(relativePath); parent != "."; parent = path.Dir(parent) {
		if _, exists := kept[parent]; exists {
			return true
		}
	}
	_, exists := kept[relativePath]
	return exists
}

func archiveErrorResponse(err error) (int, string) {
	switch {
	case errors.Is(err, fsroot.ErrArchiveUnsafeEntry):
		return http.StatusUnprocessableEntity, "所选内容包含软链接、特殊文件或不安全文件名，未生成压缩包。"
	case errors.Is(err, fsroot.ErrArchiveLimitExceeded):
		return http.StatusRequestEntityTooLarge, "所选内容超过当前打包上限，请减少文件或分批下载。"
	case errors.Is(err, fsroot.ErrArchiveStorage):
		return http.StatusInsufficientStorage, "服务器临时空间不足，压缩包未能完成。"
	case errors.Is(err, fsroot.ErrArchiveUnsupported), errors.Is(err, fsroot.ErrWalkUnsupported):
		return http.StatusServiceUnavailable, "当前平台尚不支持安全的目录打包。"
	case errors.Is(err, fsroot.ErrIdentityUnavailable):
		return http.StatusServiceUnavailable, "当前文件系统无法提供打包所需的稳定文件身份。"
	case errors.Is(err, fsroot.ErrPathChanged), errors.Is(err, fsroot.ErrConflict), errors.Is(err, os.ErrNotExist):
		return http.StatusConflict, "打包期间文件状态发生变化，压缩包已丢弃。请刷新后重试。"
	case errors.Is(err, fsroot.ErrInvalidPath), errors.Is(err, fsroot.ErrOutsideRoot), errors.Is(err, fsroot.ErrDanglingLink), errors.Is(err, fsroot.ErrLinkCycle), errors.Is(err, fsroot.ErrNotDirectory), errors.Is(err, fsroot.ErrNotRegularFile):
		return http.StatusBadRequest, "所选路径无效或无法安全访问。"
	case errors.Is(err, os.ErrPermission):
		return http.StatusForbidden, "服务器没有权限读取所选内容。"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout, "打包操作已取消。"
	default:
		return http.StatusInternalServerError, "打包失败，未生成可下载文件。"
	}
}

func writeArchiveHTML(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>FileSage 下载失败</title><body style=\"font:16px system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;color:#26352b\"><h1>未能生成压缩包</h1><p>"+html.EscapeString(message)+"</p><p>可以关闭此标签页并刷新文件列表后重试。</p></body></html>")
}
