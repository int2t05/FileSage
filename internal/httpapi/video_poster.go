package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"filesage/internal/fsroot"
	"filesage/internal/videoposter"
)

type videoPosterFileSystem interface {
	OpenRead(string) (io.ReadCloser, fsroot.Entry, error)
}

type videoPosterGenerator interface {
	Generate(context.Context, io.Reader) ([]byte, error)
}

// NewVideoPosterHandler 创建有版本校验的只读视频海报接口。
func NewVideoPosterHandler(files videoPosterFileSystem, generator videoPosterGenerator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET 请求。")
			return
		}
		if files == nil {
			writeError(w, http.StatusServiceUnavailable, "视频海报服务未启用。")
			return
		}
		if r.URL.Path != "/api/v1/files/video-poster" {
			http.NotFound(w, r)
			return
		}
		relativePath, version, ok := videoPosterQuery(w, r)
		if !ok {
			return
		}
		if !isVideoPosterPath(relativePath) {
			writeError(w, http.StatusUnsupportedMediaType, "仅支持 MP4、MOV、MKV、WebM、AVI 和 M4V 视频。")
			return
		}

		file, entry, err := files.OpenRead(relativePath)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if entry.Kind != fsroot.KindFile {
			_ = file.Close()
			writeError(w, http.StatusBadRequest, "仅支持普通视频文件。")
			return
		}
		if versionForEntry(entry) != version {
			_ = file.Close()
			writeError(w, http.StatusConflict, "文件版本已变化，请刷新后重试。")
			return
		}
		etag := `"` + version + `"`
		if versionedImageNotModified(r, etag, entry.ModifiedAt) {
			if err := file.Close(); err != nil {
				writeError(w, http.StatusInternalServerError, "无法关闭视频文件。")
				return
			}
			setVersionedJPEGHeaders(w, version, etag, entry.ModifiedAt)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if generator == nil {
			_ = file.Close()
			writeError(w, http.StatusServiceUnavailable, "视频海报服务未启用。")
			return
		}

		image, generateErr := generator.Generate(r.Context(), file)
		closeErr := file.Close()
		if r.Context().Err() != nil {
			return
		}
		if generateErr != nil {
			writeVideoPosterError(w, generateErr)
			return
		}
		if closeErr != nil {
			writeError(w, http.StatusInternalServerError, "无法安全关闭视频文件。")
			return
		}
		current, currentEntry, err := files.OpenRead(relativePath)
		if err != nil {
			writeError(w, http.StatusConflict, "文件在生成海报期间发生变化，请刷新后重试。")
			return
		}
		currentVersion := versionForEntry(currentEntry)
		currentCloseErr := current.Close()
		if currentVersion != version {
			writeError(w, http.StatusConflict, "文件在生成海报期间发生变化，请刷新后重试。")
			return
		}
		if currentCloseErr != nil {
			writeError(w, http.StatusInternalServerError, "无法关闭视频文件。")
			return
		}
		setVersionedJPEGHeaders(w, version, etag, entry.ModifiedAt)
		http.ServeContent(w, r, filepath.Base(relativePath)+".jpg", entry.ModifiedAt, bytes.NewReader(image))
	})
}

func videoPosterQuery(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "视频海报查询参数无效。")
		return "", "", false
	}
	for key, value := range values {
		if (key != "path" && key != "version") || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "仅接受一个 path 和一个 version。")
			return "", "", false
		}
	}
	paths, versions := values["path"], values["version"]
	if len(paths) != 1 || !validRelativePath(paths[0], false) || isReservedPath(paths[0]) {
		writeError(w, http.StatusBadRequest, "path 必须是管理根目录内的相对路径。")
		return "", "", false
	}
	if len(versions) != 1 || versions[0] == "" || len(versions[0]) > 256 {
		writeError(w, http.StatusBadRequest, "version 必须是有效的文件版本。")
		return "", "", false
	}
	return paths[0], versions[0], true
}

func isVideoPosterPath(relativePath string) bool {
	switch strings.ToLower(filepath.Ext(relativePath)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v":
		return true
	default:
		return false
	}
}

func writeVideoPosterError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, videoposter.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "视频海报未启用，请安装 ffmpeg 后重试。")
	case errors.Is(err, videoposter.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "视频海报生成超时。")
	case errors.Is(err, videoposter.ErrOutputLimit), errors.Is(err, videoposter.ErrInvalidJPEG):
		writeError(w, http.StatusUnprocessableEntity, "视频格式无法生成有效海报。")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "视频海报生成已取消。")
	default:
		writeError(w, http.StatusUnprocessableEntity, "视频格式无法解码或文件内容无效。")
	}
}
