// filesage 启动文件管理 API，并可选托管 React 构建页面。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"filesage/internal/agent"
	"filesage/internal/config"
	"filesage/internal/coordination"
	"filesage/internal/documenttext"
	"filesage/internal/fsroot"
	"filesage/internal/httpapi"
	"filesage/internal/imagepreview"
	"filesage/internal/indexer"
	"filesage/internal/store"
	trashsvc "filesage/internal/trash"
	"filesage/internal/upload"
	"filesage/internal/videoposter"
	"filesage/internal/webui"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	if err := documenttext.CleanupStaleTemporaryData(); err != nil {
		slog.Warn("cannot clean stale OCR temporary data", "error", err)
	}

	metadata, err := store.Open(context.Background(), filepath.Join(cfg.DataDir, "filesage.sqlite"))
	if err != nil {
		slog.Error("cannot open service metadata store", "error", err)
		os.Exit(1)
	}
	defer metadata.Close()

	root, err := fsroot.Open(cfg.RootDir)
	if err != nil {
		slog.Error("cannot open managed filesystem root", "error", err)
		os.Exit(1)
	}
	defer root.Close()
	trashManager, err := trashsvc.Open(context.Background(), root, metadata)
	if err != nil {
		slog.Error("cannot recover trash operations", "error", err)
		os.Exit(1)
	}

	locks := coordination.New()
	agentService, err := agent.NewWithStaging(metadata, root, locks,
		filepath.Join(cfg.DataDir, "agent-text-staging"), filepath.Join(cfg.DataDir, "agent-shell-staging"), trashManager)
	if err != nil {
		slog.Error("cannot create Agent candidate staging", "error", err)
		os.Exit(1)
	}
	if err := agentService.Recover(context.Background()); err != nil {
		slog.Error("cannot recover Agent operations", "error", err)
		os.Exit(1)
	}
	if err := metadata.RecoverConversations(context.Background(), time.Now().UTC()); err != nil {
		slog.Error("cannot recover Agent conversations", "error", err)
		os.Exit(1)
	}
	if err := metadata.RecoverDuplicateScans(context.Background(), time.Now().UTC()); err != nil {
		slog.Error("cannot recover duplicate scans", "error", err)
		os.Exit(1)
	}
	fileIndex, err := indexer.Open(context.Background(), root, metadata)
	if err != nil {
		slog.Error("cannot start filesystem index", "error", err)
		os.Exit(1)
	}
	defer fileIndex.Close()
	conversationService := agent.NewConversationService(metadata, root, agentService, cfg.AgentModel, fileIndex)
	defer conversationService.Close()
	uploads, err := upload.New(upload.Config{
		StagingDir:  filepath.Join(cfg.DataDir, "upload-staging"),
		ManagedRoot: cfg.RootDir,
	})
	if err != nil {
		slog.Error("cannot create upload handler", "error", err)
		os.Exit(1)
	}
	defer uploads.Close()

	publication := newPublicationManager(uploads, root, locks, metadata)
	publication.SetIndexRefresh(fileIndex.Refresh)
	mux := http.NewServeMux()
	mux.Handle(uploadStatusPath, publication)
	mux.Handle("/api/v1/uploads/", uploads)
	apiHandler := httpapi.NewWithAgentAndIndexAndConversation(root, metadata, trashManager, agentService, fileIndex, conversationService, metadata.Health, locks)
	videoPoster := videoposter.New(videoposter.Options{})
	mux.Handle("GET /api/v1/files/video-poster", httpapi.NewVideoPosterHandler(root, videoPoster))
	imagePreviewer := imagepreview.New(imagepreview.Options{})
	mux.Handle("GET /api/v1/files/image-preview", httpapi.NewImagePreviewHandler(root, imagePreviewer))
	// 静态处理器仅接管 API 之外的 GET／HEAD。
	webHandler, err := webui.New(cfg.WebDir, apiHandler)
	if err != nil {
		slog.Error("cannot open web build directory", "directory", cfg.WebDir, "error", err)
		os.Exit(1)
	}
	defer webHandler.Close()
	mux.Handle("/", webHandler)
	trustedMux, err := httpapi.NewTrustedHostHandler(mux, cfg.AllowedHosts)
	if err != nil {
		slog.Error("invalid allowed hosts", "error", err)
		os.Exit(2)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           trustedMux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		slog.Error("cannot listen", "address", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	publisherDone := make(chan error, 1)
	go func() {
		publisherDone <- publication.run(ctx)
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("graceful shutdown failed", "error", err)
		}
	}()

	slog.Info("FileSage listening", "address", cfg.ListenAddr)
	serveErr := server.Serve(listener)
	stop()
	uploads.Close()
	if err := <-publisherDone; err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("upload publication coordinator stopped", "error", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		slog.Error("HTTP server stopped", "error", serveErr)
		os.Exit(1)
	}
}
