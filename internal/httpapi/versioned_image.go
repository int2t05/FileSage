// 本文件统一版本化图片预览的缓存响应行为。
package httpapi

import (
	"net/http"
	"strings"
	"time"
)

// setVersionedJPEGHeaders 固定版本化 JPEG 预览的缓存与安全响应头。
func setVersionedJPEGHeaders(w http.ResponseWriter, version, etag string, modified time.Time) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300, must-revalidate")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-FileSage-Version", version)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !modified.IsZero() {
		w.Header().Set("Last-Modified", modified.UTC().Truncate(time.Second).Format(http.TimeFormat))
	}
}

// versionedImageNotModified 校验条件请求，避免重复生成缩略图。
func versionedImageNotModified(r *http.Request, etag string, modified time.Time) bool {
	if values := r.Header.Values("If-None-Match"); len(values) > 0 {
		for _, value := range values {
			for _, candidate := range strings.Split(value, ",") {
				candidate = strings.TrimSpace(candidate)
				if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
					return true
				}
			}
		}
		return false
	}
	if value := r.Header.Get("If-Modified-Since"); value != "" && !modified.IsZero() {
		if since, err := http.ParseTime(value); err == nil && !modified.UTC().Truncate(time.Second).After(since) {
			return true
		}
	}
	return false
}
