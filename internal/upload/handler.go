// Package upload 将可续传文件接收到私有暂存区；接收完成不代表已发布。
package upload

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"filesage/internal/fsroot"
	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

const (
	defaultBasePath      = "/api/v1/uploads/"
	defaultEventBuffer   = 64
	maxMetadataHeaderLen = 16 * 1024
	maxMetadataValueLen  = 4096
	maxRelativePathLen   = 4096
	maxFileNameLen       = 255
)

type EventKind string

const (
	UploadCreated    EventKind = "created"
	UploadCompleted  EventKind = "completed"
	UploadTerminated EventKind = "terminated"
)

// Metadata 仅包含通过校验的客户端元数据，路径均相对管理根目录。
type Metadata struct {
	FileName     string `json:"filename"`
	Target       string `json:"target"`
	RelativePath string `json:"relativePath"`
	ContentType  string `json:"contentType,omitempty"`
}

// Event 描述暂存接收状态；完成事件不代表文件已发布到管理根目录。
type Event struct {
	Kind     EventKind `json:"kind"`
	UploadID string    `json:"uploadId"`
	Size     int64     `json:"size"`
	Offset   int64     `json:"offset"`
	Metadata Metadata  `json:"metadata"`
}

type Config struct {
	// StagingDir 必须是管理根目录外的绝对路径，权限仅允许服务用户访问。
	StagingDir string
	// ManagedRoot 用于拒绝与浏览根目录重叠的暂存路径。
	ManagedRoot string
	// BasePath 是上传处理器的挂载路径，默认值为 /api/v1/uploads/。
	BasePath string
	// MaxUploadSize 是单个上传的字节上限，零表示不限制。
	MaxUploadSize int64
	// EventBuffer 限制待消费事件数量，零默认使用 64。
	EventBuffer int
}

// Server 封装 tus 暂存处理器，并向协调器提供校验后的事件。
type Server struct {
	httpHandler   http.Handler
	stagingDir    string
	store         filestore.FileStore
	events        chan Event
	stop          context.CancelFunc
	done          chan struct{}
	closeOnce     sync.Once
	hookMu        sync.RWMutex
	terminateHook func(string)
}

// New 创建 tus 1.0 私有暂存处理器。调用方应持续消费事件，并在 Close 前停止接收请求。
func New(config Config) (*Server, error) {
	if config.MaxUploadSize < 0 {
		return nil, errors.New("maximum upload size cannot be negative")
	}
	basePath, err := normalizeBasePath(config.BasePath)
	if err != nil {
		return nil, err
	}
	eventBuffer := config.EventBuffer
	if eventBuffer == 0 {
		eventBuffer = defaultEventBuffer
	}
	if eventBuffer < 0 {
		return nil, errors.New("event buffer cannot be negative")
	}
	stagingDir, err := ensurePrivateStaging(config.StagingDir, config.ManagedRoot)
	if err != nil {
		return nil, err
	}

	store := filestore.New(stagingDir)
	store.DirModePerm = 0o700
	store.FileModePerm = 0o600
	composer := tusd.NewStoreComposer()
	composer.UseCore(store)
	composer.UseTerminater(store)
	locker := filelocker.New(stagingDir)
	locker.UseIn(composer)

	var application *Server
	protocolHandler, err := tusd.NewHandler(tusd.Config{
		BasePath:                basePath,
		StoreComposer:           composer,
		MaxSize:                 config.MaxUploadSize,
		DisableDownload:         true,
		DisableConcatenation:    true,
		NotifyCreatedUploads:    true,
		NotifyCompleteUploads:   true,
		NotifyTerminatedUploads: true,
		Cors:                    &tusd.CorsConfig{Disable: true},
		PreUploadCreateCallback: func(event tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
			metadata, err := validateRequestMetadata(event)
			if err != nil {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("INVALID_UPLOAD_METADATA", err.Error(), http.StatusBadRequest)
			}
			if event.Upload.SizeIsDeferred || event.Upload.IsPartial || event.Upload.IsFinal {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("UNSUPPORTED_UPLOAD_MODE", "deferred and concatenated uploads are not supported", http.StatusBadRequest)
			}
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{MetaData: metadata.toTusd()}, nil
		},
		PreUploadTerminateCallback: func(event tusd.HookEvent) (tusd.HTTPResponse, error) {
			if application != nil {
				application.notifyTerminating(event.Upload.ID)
			}
			return tusd.HTTPResponse{}, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create tus handler: %w", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	server := &Server{
		httpHandler: http.StripPrefix(strings.TrimSuffix(basePath, "/"), protocolHandler),
		stagingDir:  stagingDir,
		store:       store,
		events:      make(chan Event, eventBuffer),
		stop:        stop,
		done:        make(chan struct{}),
	}
	application = server
	go server.forwardEvents(ctx, protocolHandler)
	return server, nil
}

// ServeHTTP 处理配置上传路径下的 tus 请求。
func (server *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete) && !sameOriginRequest(r) {
		http.Error(w, "cross-origin upload changes are not allowed", http.StatusForbidden)
		return
	}
	server.httpHandler.ServeHTTP(w, r)
}

// Events 返回只读事件流；调用方须持续消费，避免事件积压阻塞 tus 请求。
func (server *Server) Events() <-chan Event {
	return server.events
}

// SetTerminateHook 设置 tus 删除前的取消回调。
func (server *Server) SetTerminateHook(hook func(string)) {
	server.hookMu.Lock()
	defer server.hookMu.Unlock()
	server.terminateHook = hook
}

// SafeErrorMessage 隐藏暂存目录的绝对路径。
func (server *Server) SafeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), server.stagingDir, "[暂存目录]")
}

// OpenCompleted 打开与完整接收事件匹配的暂存文件，不会发布或移动文件。
func (server *Server) OpenCompleted(event Event) (io.ReadCloser, Metadata, int64, error) {
	if event.Kind != UploadCompleted {
		return nil, Metadata{}, 0, fmt.Errorf("upload event is not completed: %q", event.Kind)
	}
	if !validTusUploadID(event.UploadID) {
		return nil, Metadata{}, 0, errors.New("upload ID is not a safe tus ID")
	}
	if event.Size < 0 {
		return nil, Metadata{}, 0, errors.New("completed upload size is invalid")
	}

	rootInfo, err := os.Lstat(server.stagingDir)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("staging root is not a real directory")
		}
		return nil, Metadata{}, 0, fmt.Errorf("inspect staging root: %w", err)
	}
	stagedPath := filepath.Join(server.stagingDir, event.UploadID)
	infoPath := filepath.Join(server.stagingDir, event.UploadID+".info")
	if filepath.Dir(stagedPath) != server.stagingDir || filepath.Dir(infoPath) != server.stagingDir {
		return nil, Metadata{}, 0, errors.New("upload ID does not resolve to a staging file")
	}

	infoFile, err := os.Lstat(infoPath)
	if err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("inspect staged upload info: %w", err)
	}
	if !infoFile.Mode().IsRegular() {
		return nil, Metadata{}, 0, errors.New("staged upload info is not a regular file")
	}
	infoBytes, err := os.ReadFile(infoPath)
	if err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("read staged upload info: %w", err)
	}
	var storedInfo tusd.FileInfo
	if err := json.Unmarshal(infoBytes, &storedInfo); err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("decode staged upload info: %w", err)
	}
	if err := validateStoredInfo(storedInfo, event, stagedPath); err != nil {
		return nil, Metadata{}, 0, err
	}

	stagedInfo, err := os.Lstat(stagedPath)
	if err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("inspect staged upload data: %w", err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return nil, Metadata{}, 0, errors.New("staged upload data is not a regular file")
	}
	if stagedInfo.Size() != event.Size {
		return nil, Metadata{}, 0, fmt.Errorf("staged upload size changed: got %d, event has %d", stagedInfo.Size(), event.Size)
	}

	storedUpload, err := server.store.GetUpload(context.Background(), event.UploadID)
	if err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("load tus upload: %w", err)
	}
	info, err := storedUpload.GetInfo(context.Background())
	if err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("load tus upload state: %w", err)
	}
	metadata, err := validateCompletedInfo(info, event, stagedPath)
	if err != nil {
		return nil, Metadata{}, 0, err
	}

	reader, err := storedUpload.GetReader(context.Background())
	if err != nil {
		return nil, Metadata{}, 0, fmt.Errorf("open staged upload data: %w", err)
	}
	file, ok := reader.(*os.File)
	if !ok {
		_ = reader.Close()
		return nil, Metadata{}, 0, errors.New("tus file store returned an unexpected reader")
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, Metadata{}, 0, fmt.Errorf("inspect opened upload data: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(stagedInfo, openedInfo) || openedInfo.Size() != event.Size {
		_ = file.Close()
		return nil, Metadata{}, 0, errors.New("staged upload data changed while opening")
	}
	return file, metadata, info.Size, nil
}

// TransferStatus 返回已校验的 tus 接收状态。
func (server *Server) TransferStatus(uploadID string) (Event, error) {
	if !validTusUploadID(uploadID) {
		return Event{}, errors.New("upload ID is not a safe tus ID")
	}
	storedUpload, err := server.store.GetUpload(context.Background(), uploadID)
	if err != nil {
		return Event{}, fmt.Errorf("load tus upload: %w", err)
	}
	info, err := storedUpload.GetInfo(context.Background())
	if err != nil {
		return Event{}, fmt.Errorf("load tus upload state: %w", err)
	}
	if info.ID != uploadID || info.Size < 0 || info.Offset < 0 || info.Offset > info.Size || info.SizeIsDeferred || info.IsPartial || info.IsFinal || len(info.PartialUploads) != 0 {
		return Event{}, errors.New("tus upload state is invalid")
	}
	metadata, err := validateDecodedMetadata(info.MetaData)
	if err != nil {
		return Event{}, fmt.Errorf("tus upload metadata is invalid: %w", err)
	}
	kind := UploadCreated
	if info.Offset == info.Size {
		kind = UploadCompleted
	}
	return Event{Kind: kind, UploadID: uploadID, Size: info.Size, Offset: info.Offset, Metadata: metadata}, nil
}

// CompletedUploads 枚举并验证暂存区内仍完整的上传。
func (server *Server) CompletedUploads() ([]Event, error) {
	entries, err := os.ReadDir(server.stagingDir)
	if err != nil {
		return nil, fmt.Errorf("list upload staging directory: %w", err)
	}
	events := make([]Event, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".info") {
			continue
		}
		uploadID := strings.TrimSuffix(entry.Name(), ".info")
		if !validTusUploadID(uploadID) {
			continue
		}
		infoPath := filepath.Join(server.stagingDir, entry.Name())
		fileInfo, err := os.Lstat(infoPath)
		if err != nil || !fileInfo.Mode().IsRegular() {
			if err == nil {
				err = errors.New("staged upload info is not a regular file")
			}
			slog.Warn("skipping invalid staged upload info", "upload_id", uploadID, "error", err)
			continue
		}
		event, err := server.TransferStatus(uploadID)
		if err != nil {
			slog.Warn("skipping staged upload with invalid tus state", "upload_id", uploadID, "error", err)
			continue
		}
		if event.Kind != UploadCompleted {
			continue
		}
		reader, _, _, err := server.OpenCompleted(event)
		if err != nil {
			slog.Warn("skipping staged upload that failed validation", "upload_id", uploadID, "error", err)
			continue
		}
		if err := reader.Close(); err != nil {
			slog.Warn("cannot close validated staged upload", "upload_id", uploadID, "error", err)
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

// DiscardCompleted 删除已成功发布的完整暂存上传。
func (server *Server) DiscardCompleted(event Event) error {
	reader, _, _, err := server.OpenCompleted(event)
	if err != nil {
		return err
	}
	if err := reader.Close(); err != nil {
		return fmt.Errorf("close staged upload before cleanup: %w", err)
	}
	storedUpload, err := server.store.GetUpload(context.Background(), event.UploadID)
	if err != nil {
		return fmt.Errorf("load staged upload for cleanup: %w", err)
	}
	if err := server.store.AsTerminatableUpload(storedUpload).Terminate(context.Background()); err != nil {
		return fmt.Errorf("remove published upload from staging: %w", err)
	}
	return nil
}

// Close 停止事件转发；调用前应停止接收请求并等待在途请求完成。
func (server *Server) Close() {
	server.closeOnce.Do(func() {
		server.stop()
		<-server.done
	})
}

func (server *Server) forwardEvents(ctx context.Context, handler *tusd.Handler) {
	defer close(server.done)
	defer close(server.events)
	for {
		select {
		case <-ctx.Done():
			return
		case raw := <-handler.CreatedUploads:
			server.forwardEvent(ctx, UploadCreated, raw)
		case raw := <-handler.CompleteUploads:
			server.forwardEvent(ctx, UploadCompleted, raw)
		case raw := <-handler.TerminatedUploads:
			server.forwardEvent(ctx, UploadTerminated, raw)
		}
	}
}

func (server *Server) forwardEvent(ctx context.Context, kind EventKind, raw tusd.HookEvent) {
	metadata, err := validateDecodedMetadata(raw.Upload.MetaData)
	if err != nil || !validTusUploadID(raw.Upload.ID) || raw.Upload.Size < 0 || raw.Upload.SizeIsDeferred || raw.Upload.Offset < 0 || raw.Upload.Offset > raw.Upload.Size {
		if err == nil {
			err = errors.New("tusd emitted an incomplete upload event")
		}
		// 元数据在创建前已校验；此处再次校验后才向调用方转发。
		slog.Error("discarding invalid tusd upload event", "kind", kind, "error", err)
		return
	}
	select {
	case server.events <- Event{
		Kind:     kind,
		UploadID: raw.Upload.ID,
		Size:     raw.Upload.Size,
		Offset:   raw.Upload.Offset,
		Metadata: metadata,
	}:
	case <-ctx.Done():
	}
}

func (server *Server) notifyTerminating(uploadID string) {
	server.hookMu.RLock()
	hook := server.terminateHook
	server.hookMu.RUnlock()
	if hook != nil && validTusUploadID(uploadID) {
		hook(uploadID)
	}
}

func sameOriginRequest(r *http.Request) bool {
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	if fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.Opaque != "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.ForceQuery || strings.ContainsAny(origins[0], "?#") || (origin.Path != "" && origin.Path != "/") {
		return false
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	if !strings.EqualFold(origin.Scheme, requestScheme) {
		return false
	}
	originHost, originPort, ok := normalizedAuthority(origin.Host, requestScheme)
	if !ok {
		return false
	}
	requestHost, requestPort, ok := normalizedAuthority(r.Host, requestScheme)
	return ok && strings.EqualFold(originHost, requestHost) && originPort == requestPort
}

func normalizedAuthority(authority, scheme string) (host, port string, ok bool) {
	parsed, err := url.Parse("//" + authority)
	if err != nil || parsed.Host != authority || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	host = strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", "", false
	}
	port = parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port, true
}

func validateStoredInfo(info tusd.FileInfo, event Event, stagedPath string) error {
	if info.ID != event.UploadID || info.Size != event.Size || info.Size < 0 || info.SizeIsDeferred || info.IsPartial || info.IsFinal || len(info.PartialUploads) != 0 {
		return errors.New("tus upload state does not match the completed event")
	}
	if info.Storage == nil || info.Storage["Type"] != "filestore" || !filepath.IsAbs(info.Storage[filestore.StorageKeyPath]) || filepath.Clean(info.Storage[filestore.StorageKeyPath]) != filepath.Clean(stagedPath) {
		return errors.New("tus upload data path does not match private staging storage")
	}
	metadata, err := validateDecodedMetadata(info.MetaData)
	if err != nil {
		return fmt.Errorf("stored upload metadata is invalid: %w", err)
	}
	if metadata != event.Metadata {
		return errors.New("tus upload metadata does not match the completed event")
	}
	return nil
}

func validateCompletedInfo(info tusd.FileInfo, event Event, stagedPath string) (Metadata, error) {
	if err := validateStoredInfo(info, event, stagedPath); err != nil {
		return Metadata{}, err
	}
	if info.Offset != info.Size {
		return Metadata{}, fmt.Errorf("upload is incomplete: offset %d of %d bytes", info.Offset, info.Size)
	}
	return validateDecodedMetadata(info.MetaData)
}

func validTusUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, value := range id {
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')) {
			return false
		}
	}
	return true
}

func validateRequestMetadata(event tusd.HookEvent) (Metadata, error) {
	rawHeader := event.HTTPRequest.Header.Get("Upload-Metadata")
	parsed, err := parseMetadataHeader(rawHeader)
	if err != nil {
		return Metadata{}, err
	}
	if !equalMetadata(parsed, event.Upload.MetaData) {
		return Metadata{}, errors.New("metadata could not be parsed consistently")
	}
	return validateDecodedMetadata(parsed)
}

func parseMetadataHeader(header string) (map[string]string, error) {
	if len(header) > maxMetadataHeaderLen {
		return nil, errors.New("upload metadata is too large")
	}
	metadata := make(map[string]string)
	if strings.TrimSpace(header) == "" {
		return metadata, nil
	}
	for _, rawItem := range strings.Split(header, ",") {
		item := strings.TrimSpace(rawItem)
		parts := strings.Split(item, " ")
		if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
			return nil, errors.New("upload metadata must use key and base64-value pairs")
		}
		key := parts[0]
		if !allowedMetadataKey(key) {
			return nil, fmt.Errorf("unsupported upload metadata key %q", key)
		}
		if _, exists := metadata[key]; exists {
			return nil, fmt.Errorf("duplicate upload metadata key %q", key)
		}
		value := ""
		if len(parts) == 2 {
			decodedBytes, err := decodeStandardBase64(parts[1])
			if err != nil {
				return nil, fmt.Errorf("invalid base64 value for upload metadata key %q", key)
			}
			value = string(decodedBytes)
		}
		if len(value) > maxMetadataValueLen || !utf8.ValidString(value) || containsControl(value) {
			return nil, fmt.Errorf("invalid value for upload metadata key %q", key)
		}
		metadata[key] = value
	}
	return metadata, nil
}

func decodeStandardBase64(value string) ([]byte, error) {
	return base64.StdEncoding.Strict().DecodeString(value)
}

func equalMetadata(left, right tusd.MetaData) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func validateDecodedMetadata(values map[string]string) (Metadata, error) {
	for key := range values {
		if !allowedMetadataKey(key) {
			return Metadata{}, fmt.Errorf("unsupported upload metadata key %q", key)
		}
	}
	filename, ok := values["filename"]
	if !ok || !validPathComponent(filename) {
		return Metadata{}, errors.New("filename metadata must be a single safe file name")
	}
	target := values["target"]
	if target == "" {
		target = "."
	}
	if !validRelativeDirectory(target) {
		return Metadata{}, errors.New("target metadata must be a safe relative directory path")
	}
	relativePath := values["relativepath"]
	if relativePath == "" {
		relativePath = filename
	}
	if !validRelativeFilePath(relativePath) || path.Base(relativePath) != filename {
		return Metadata{}, errors.New("relativepath metadata must be a safe relative file path ending in filename")
	}
	contentType := values["filetype"]
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || len(contentType) > 255 {
			return Metadata{}, errors.New("filetype metadata must be a valid media type")
		}
		contentType = mediaType
	}
	return Metadata{
		FileName:     filename,
		Target:       target,
		RelativePath: relativePath,
		ContentType:  contentType,
	}, nil
}

func (metadata Metadata) toTusd() tusd.MetaData {
	values := tusd.MetaData{
		"filename":     metadata.FileName,
		"target":       metadata.Target,
		"relativepath": metadata.RelativePath,
	}
	if metadata.ContentType != "" {
		values["filetype"] = metadata.ContentType
	}
	return values
}

func allowedMetadataKey(key string) bool {
	switch key {
	case "filename", "target", "relativepath", "filetype":
		return true
	default:
		return false
	}
}

// validPathComponent 校验单个文件名并拒绝内部保留目录。
func validPathComponent(value string) bool {
	return value != "" && value != "." && value != ".." && value != fsroot.TrashDirectoryName && len(value) <= maxFileNameLen &&
		utf8.ValidString(value) && !strings.ContainsAny(value, "/\\") && !containsControl(value)
}

func validRelativeDirectory(value string) bool {
	if value == "." {
		return true
	}
	return validRelativePath(value, false)
}

func validRelativeFilePath(value string) bool {
	return validRelativePath(value, true)
}

func validRelativePath(value string, includeFilename bool) bool {
	if value == "" || len(value) > maxRelativePathLen || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || !utf8.ValidString(value) || containsControl(value) || path.Clean(value) != value {
		return false
	}
	parts := strings.Split(value, "/")
	for index, component := range parts {
		if !validPathComponent(component) {
			return false
		}
		if !includeFilename && index == len(parts)-1 && component == "." {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func normalizeBasePath(basePath string) (string, error) {
	if basePath == "" {
		basePath = defaultBasePath
	}
	if !strings.HasPrefix(basePath, "/") || strings.ContainsAny(basePath, "?#") || strings.Contains(basePath, "\\") {
		return "", errors.New("upload base path must be an absolute URL path without query or fragment")
	}
	cleaned := path.Clean(basePath)
	if cleaned == "/" || cleaned != strings.TrimSuffix(basePath, "/") {
		return "", errors.New("upload base path must not contain empty or dot path segments")
	}
	return cleaned + "/", nil
}

func ensurePrivateStaging(stagingDir, managedRoot string) (string, error) {
	if !filepath.IsAbs(stagingDir) || !filepath.IsAbs(managedRoot) {
		return "", errors.New("staging directory and managed root must be absolute paths")
	}
	rootPath, err := filepath.EvalSymlinks(filepath.Clean(managedRoot))
	if err != nil {
		return "", fmt.Errorf("resolve managed root: %w", err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return "", fmt.Errorf("inspect managed root: %w", err)
	}
	if !rootInfo.IsDir() {
		return "", errors.New("managed root must be a directory")
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return "", fmt.Errorf("make managed root absolute: %w", err)
	}

	stagingPath, err := filepath.Abs(filepath.Clean(stagingDir))
	if err != nil {
		return "", fmt.Errorf("make staging directory absolute: %w", err)
	}
	prospectivePath, err := resolveProspectivePath(stagingPath)
	if err != nil {
		return "", fmt.Errorf("resolve staging directory: %w", err)
	}
	if pathsOverlap(rootPath, prospectivePath) {
		return "", errors.New("upload staging directory must be separate from the managed root")
	}
	if err := os.MkdirAll(stagingPath, 0o700); err != nil {
		return "", fmt.Errorf("create upload staging directory: %w", err)
	}
	stagingPath, err = filepath.EvalSymlinks(stagingPath)
	if err != nil {
		return "", fmt.Errorf("resolve created staging directory: %w", err)
	}
	stagingPath, err = filepath.Abs(stagingPath)
	if err != nil {
		return "", fmt.Errorf("make staging directory absolute: %w", err)
	}
	if pathsOverlap(rootPath, stagingPath) {
		return "", errors.New("upload staging directory must be separate from the managed root")
	}
	if err := os.Chmod(stagingPath, 0o700); err != nil {
		return "", fmt.Errorf("restrict upload staging directory permissions: %w", err)
	}
	return stagingPath, nil
}

func resolveProspectivePath(candidate string) (string, error) {
	current := filepath.Clean(candidate)
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent for %q", candidate)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathsOverlap(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
