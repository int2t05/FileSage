// Package indexer 负责从图片元数据提取拍摄时间。
package indexer

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"time"

	"filesage/internal/fsroot"
	"github.com/bep/imagemeta"
)

type metadataFileOpener interface {
	OpenRead(string) (io.ReadCloser, fsroot.Entry, error)
}

// captureDate 安全提取图片的拍摄时间。
func captureDate(ctx context.Context, root treeWalker, entry fsroot.Entry) (capturedAt *time.Time) {
	if ctx.Err() != nil || entry.Kind != fsroot.KindFile || entry.Symlink {
		return nil
	}
	format, ok := imageFormat(entry.Name)
	if !ok {
		return nil
	}
	opener, ok := root.(metadataFileOpener)
	if !ok {
		return nil
	}
	file, opened, err := opener.OpenRead(entry.Path)
	if err != nil {
		return nil
	}
	defer file.Close()
	if opened.Kind != fsroot.KindFile || opened.Symlink || !sameFileState(entry, opened) {
		return nil
	}
	reader, ok := file.(io.ReadSeeker)
	if !ok {
		return nil
	}
	return decodeCaptureDate(reader, format)
}

// decodeCaptureDate 读取受限的元数据日期字段。
func decodeCaptureDate(reader io.ReadSeeker, format imagemeta.ImageFormat) (capturedAt *time.Time) {
	defer func() {
		if recover() != nil {
			capturedAt = nil
		}
	}()
	var tags imagemeta.Tags
	sources := imagemeta.EXIF | imagemeta.XMP
	if format == imagemeta.JPEG || format == imagemeta.TIFF || format == imagemeta.DNG || format == imagemeta.CR2 || format == imagemeta.NEF || format == imagemeta.ARW || format == imagemeta.PEF {
		sources |= imagemeta.IPTC
	}
	_, err := imagemeta.Decode(imagemeta.Options{
		R: reader, ImageFormat: format, Sources: sources,
		LimitNumTags: 512, LimitTagSize: 4096,
		ShouldHandleTag: func(tag imagemeta.TagInfo) bool {
			switch tag.Tag {
			case "DateTimeOriginal", "DateTime", "CreateDate", "DateCreated", "TimeCreated":
				return true
			default:
				return false
			}
		},
		HandleTag: func(tag imagemeta.TagInfo) error {
			tags.Add(tag)
			return nil
		},
	})
	if err != nil {
		return nil
	}
	value, err := tags.GetDateTime()
	if err != nil || value.IsZero() {
		return nil
	}
	// EXIF 日期是设备本地日历字段，转成 UTC 载体避免服务器时区改写日期。
	calendar := time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), time.UTC)
	return &calendar
}

// imageFormat 按扩展名返回支持的元数据格式。
func imageFormat(name string) (imagemeta.ImageFormat, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return imagemeta.JPEG, true
	case ".tif", ".tiff":
		return imagemeta.TIFF, true
	case ".png":
		return imagemeta.PNG, true
	case ".webp":
		return imagemeta.WebP, true
	case ".heic", ".heif":
		return imagemeta.HEIF, true
	case ".avif":
		return imagemeta.AVIF, true
	case ".dng":
		return imagemeta.DNG, true
	case ".cr2":
		return imagemeta.CR2, true
	case ".nef":
		return imagemeta.NEF, true
	case ".arw":
		return imagemeta.ARW, true
	case ".pef":
		return imagemeta.PEF, true
	default:
		return imagemeta.ImageFormatAuto, false
	}
}

// sameFileState 防止读取前文件被替换或修改。
func sameFileState(walked, opened fsroot.Entry) bool {
	if walked.Size != opened.Size || !walked.ModifiedAt.Equal(opened.ModifiedAt) {
		return false
	}
	if walked.IdentityAvailable && opened.IdentityAvailable {
		return walked.Device == opened.Device && walked.Inode == opened.Inode
	}
	return true
}
