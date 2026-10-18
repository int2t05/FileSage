// Package webui 为 API 服务提供可选的 React 单页应用静态资源。
package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
)

// Handler 只读取配置目录中的静态文件；API 路径始终交给后续处理器。
type Handler struct {
	root     *os.Root
	fallback http.Handler
}

// New 打开构建产物目录，并将 API 请求交给 fallback。
func New(directory string, fallback http.Handler) (*Handler, error) {
	if fallback == nil {
		fallback = http.NotFoundHandler()
	}
	handler := &Handler{fallback: fallback}
	if strings.TrimSpace(directory) == "" {
		return handler, nil
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	index, err := root.Open("index.html")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open web build index.html: %w", err)
	}
	info, err := index.Stat()
	_ = index.Close()
	if err != nil || !info.Mode().IsRegular() {
		_ = root.Close()
		return nil, fmt.Errorf("web build index.html must be a regular file")
	}
	handler.root = root
	return handler, nil
}

// Close 释放构建目录句柄。
func (handler *Handler) Close() error {
	if handler.root == nil {
		return nil
	}
	return handler.root.Close()
}

// ServeHTTP 将网页请求交给静态资源处理，并保留 API 原路由。
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler.root == nil || isAPIPath(r.URL.Path) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		handler.fallback.ServeHTTP(w, r)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		http.NotFound(w, r)
		return
	}
	served, missing := handler.serveFile(w, r, name)
	if served {
		return
	}
	if !missing || handler.hasExistingParent(name) || path.Ext(name) != "" || !acceptsHTML(r.Header.Get("Accept")) {
		http.NotFound(w, r)
		return
	}
	if served, _ := handler.serveFile(w, r, "index.html"); !served {
		http.NotFound(w, r)
	}
}

// hasExistingParent 避免将现存静态目录中的缺失资源回退成页面。
func (handler *Handler) hasExistingParent(name string) bool {
	for parent := path.Dir(name); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if _, err := handler.root.Stat(parent); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// serveFile 只返回目录内的普通文件，并标记真实的缺失路径。
func (handler *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string) (served, missing bool) {
	file, err := handler.root.Open(name)
	if err != nil {
		if _, lstatErr := handler.root.Lstat(name); lstatErr == nil || !errors.Is(lstatErr, fs.ErrNotExist) {
			return false, false
		}
		return false, errors.Is(err, fs.ErrNotExist)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, false
	}
	if contentType := contentTypeFor(name); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return true, false
}

// contentTypeFor 固定前端关键资源类型，其余类型使用系统 MIME 表。
func contentTypeFor(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	default:
		return mime.TypeByExtension(path.Ext(name))
	}
}

// isAPIPath 识别 API 与上传路由前缀。
func isAPIPath(requestPath string) bool {
	return requestPath == "/api" || strings.HasPrefix(requestPath, "/api/")
}

// acceptsHTML 仅允许浏览器页面请求触发 SPA 回退。
func acceptsHTML(value string) bool {
	for _, item := range strings.Split(value, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(item, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/html") || strings.EqualFold(mediaType, "application/xhtml+xml") {
			return true
		}
	}
	return false
}
