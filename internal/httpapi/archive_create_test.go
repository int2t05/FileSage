// 本文件验证 ZIP 创建接口的校验、原子发布和失败恢复。
package httpapi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"filesage/internal/coordination"
	"filesage/internal/fsroot"
	"filesage/internal/store"
)

func TestCreateArchivePublishesReadableZIPAndRefreshesIndex(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "reports", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "reports", "brief.txt"), []byte("approved content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	index := &archiveCreateTestIndexer{}
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithAgentAndIndex(root, metadata, nil, nil, index, nil, coordination.New())
	request := archiveCreateRequest{
		Items:              []archiveCreateItem{{Path: "reports", Version: archiveCreateVersion(t, root, "reports")}},
		Destination:        "output",
		DestinationVersion: archiveCreateVersion(t, root, "output"),
		Name:               "review.zip",
	}
	response := postArchiveCreate(t, handler, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	var result archiveCreateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "output/review.zip" || result.Name != "review.zip" {
		t.Fatalf("create result = %#v", result)
	}
	data := readArchiveCreateFile(t, root, result.Path)
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open published ZIP: %v", err)
	}
	contents := make(map[string]string, len(archive.File))
	for _, item := range archive.File {
		if item.FileInfo().IsDir() {
			contents[item.Name] = ""
			continue
		}
		file, err := item.Open()
		if err != nil {
			t.Fatal(err)
		}
		value, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read ZIP entry %q: read=%v close=%v", item.Name, readErr, closeErr)
		}
		contents[item.Name] = string(value)
	}
	if contents["reports/brief.txt"] != "approved content" {
		t.Fatalf("ZIP content = %#v", contents)
	}
	if _, exists := contents["reports/empty/"]; !exists {
		t.Fatalf("ZIP omitted the empty directory: %#v", contents)
	}
	if index.refreshes != 1 {
		t.Fatalf("index refresh count = %d, want 1", index.refreshes)
	}
	page, err := metadata.AuditHistoryPage(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var events []store.AuditRecord
	for _, event := range page.Events {
		if event.EventType == "manual_archive_create" {
			events = append(events, event)
		}
	}
	if len(events) != 1 || events[0].SourcePath != "" || events[0].TargetPath != "output/review.zip" || events[0].Outcome != "succeeded" {
		t.Fatalf("archive creation audit events = %#v", events)
	}
}

func TestCreateArchiveSupportsTARFormatsAndNumbersConflicts(t *testing.T) {
	requireLinuxArchiveCreate(t)
	for _, format := range []fsroot.ArchiveFormat{fsroot.ArchiveFormat7Z, fsroot.ArchiveFormatTAR, fsroot.ArchiveFormatTARGZ} {
		t.Run(string(format), func(t *testing.T) {
			if format == fsroot.ArchiveFormat7Z {
				if _, err := exec.LookPath("7z"); err != nil {
					t.Skip("requires the 7-Zip command")
				}
			}
			rootPath := t.TempDir()
			if err := os.Mkdir(filepath.Join(rootPath, "output"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(rootPath, "reports", "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, "reports", "brief.txt"), []byte("approved content"), 0o600); err != nil {
				t.Fatal(err)
			}
			extension := archiveFormatExtension(format)
			name := "review" + extension
			if err := os.WriteFile(filepath.Join(rootPath, "output", name), []byte("keep existing"), 0o600); err != nil {
				t.Fatal(err)
			}
			root := openArchiveCreateRoot(t, rootPath)
			index := &archiveCreateTestIndexer{}
			handler := newArchiveCreateHandler(root, index)
			response := postArchiveCreate(t, handler, archiveCreateRequest{
				Items:              []archiveCreateItem{{Path: "reports", Version: archiveCreateVersion(t, root, "reports")}},
				Destination:        "output",
				DestinationVersion: archiveCreateVersion(t, root, "output"),
				Name:               name,
				Format:             format,
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
			}
			var result archiveCreateResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			wantName := "review (1)" + extension
			if result.Path != "output/"+wantName || result.Name != wantName {
				t.Fatalf("create result = %#v, want name %q", result, wantName)
			}
			if got := string(readArchiveCreateFile(t, root, "output/"+name)); got != "keep existing" {
				t.Fatalf("existing archive changed to %q", got)
			}
			entries := readCreateTestArchive(t, format, readArchiveCreateFile(t, root, result.Path))
			if entries["reports/brief.txt"] != "approved content" {
				t.Fatalf("archive content = %#v", entries)
			}
			if _, exists := entries["reports/empty/"]; !exists {
				t.Fatalf("archive omitted empty directory: %#v", entries)
			}
			if index.refreshes != 1 {
				t.Fatalf("index refresh count = %d, want 1", index.refreshes)
			}
		})
	}
}

func TestCreateArchiveUsesRootDestinationWithoutVersion(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("root target"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	handler := newArchiveCreateHandler(root, nil)
	response := postArchiveCreate(t, handler, archiveCreateRequest{
		Items: []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
		Name:  "root.zip",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("root create status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := root.LstatEntry("root.zip"); err != nil {
		t.Fatalf("root ZIP was not published: %v", err)
	}
}

func TestCreateArchiveStrictJSONAndRequestValidation(t *testing.T) {
	handler := New(nil, nil)
	valid := `{"items":[{"path":"source.txt","version":"v1"}],"destination":"","destinationVersion":"","name":"review.zip"}`
	for name, body := range map[string]string{
		"unknown field":          `{"items":[],"destination":"","destinationVersion":"","name":"x.zip","extra":true}`,
		"unknown format":         `{"items":[{"path":"source.txt","version":"v1"}],"destination":"","destinationVersion":"","name":"x.zip","format":"rar"}`,
		"empty format":           `{"items":[{"path":"source.txt","version":"v1"}],"destination":"","destinationVersion":"","name":"x.zip","format":""}`,
		"mismatched extension":   `{"items":[{"path":"source.txt","version":"v1"}],"destination":"","destinationVersion":"","name":"x.tar.gz","format":"tar"}`,
		"duplicate field":        `{"items":[],"items":[],"destination":"","destinationVersion":"","name":"x.zip"}`,
		"missing field":          `{"items":[],"destination":"","destinationVersion":""}`,
		"duplicate item key":     `{"items":[{"path":"a","path":"b","version":"v1"}],"destination":"","destinationVersion":"","name":"x.zip"}`,
		"trailing JSON":          valid + ` {}`,
		"name extension":         `{"items":[{"path":"source.txt","version":"v1"}],"destination":"","destinationVersion":"","name":"review.txt"}`,
		"root target version":    `{"items":[{"path":"source.txt","version":"v1"}],"destination":"","destinationVersion":"stale","name":"review.zip"}`,
		"missing target version": `{"items":[{"path":"source.txt","version":"v1"}],"destination":"folder","destinationVersion":"","name":"review.zip"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := serveArchiveCreateBody(handler, body, "application/json", "/api/v1/files/archive/create")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
		})
	}
	wrongType := serveArchiveCreateBody(handler, valid, "text/plain", "/api/v1/files/archive/create")
	if wrongType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status = %d, want %d", wrongType.Code, http.StatusUnsupportedMediaType)
	}
	query := serveArchiveCreateBody(handler, valid, "application/json", "/api/v1/files/archive/create?unexpected=true")
	if query.Code != http.StatusBadRequest {
		t.Fatalf("query status = %d, want %d", query.Code, http.StatusBadRequest)
	}
}

func TestCreateArchiveRejectsSourceAndDestinationConflicts(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "folder", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "folder", "item.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	handler := newArchiveCreateHandler(root, nil)
	folder := archiveCreateItem{Path: "folder", Version: archiveCreateVersion(t, root, "folder")}
	for _, destination := range []string{"folder", "folder/child"} {
		response := postArchiveCreate(t, handler, archiveCreateRequest{
			Items:              []archiveCreateItem{folder},
			Destination:        destination,
			DestinationVersion: archiveCreateVersion(t, root, destination),
			Name:               "nested.zip",
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("destination %q status = %d, want %d: %s", destination, response.Code, http.StatusBadRequest, response.Body.String())
		}
	}
	if _, err := root.LstatEntry("folder/nested.zip"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source directory contains an unexpected ZIP: %v", err)
	}
}

func TestCreateArchiveRejectsSymbolicLinkSourcesAndTargets(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "real-output"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source.txt", filepath.Join(rootPath, "source-link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-output", filepath.Join(rootPath, "output-link")); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	handler := newArchiveCreateHandler(root, nil)
	sourceLink := archiveCreateItem{Path: "source-link.txt", Version: archiveCreateVersion(t, root, "source-link.txt")}
	sourceResponse := postArchiveCreate(t, handler, archiveCreateRequest{Items: []archiveCreateItem{sourceLink}, Name: "unsafe.zip"})
	if sourceResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("link source status = %d, want %d: %s", sourceResponse.Code, http.StatusUnprocessableEntity, sourceResponse.Body.String())
	}
	targetResponse := postArchiveCreate(t, handler, archiveCreateRequest{
		Items:              []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
		Destination:        "output-link",
		DestinationVersion: archiveCreateVersion(t, root, "output-link"),
		Name:               "unsafe.zip",
	})
	if targetResponse.Code != http.StatusBadRequest {
		t.Fatalf("link destination status = %d, want %d: %s", targetResponse.Code, http.StatusBadRequest, targetResponse.Body.String())
	}
	if _, err := root.LstatEntry("real-output/unsafe.zip"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("link target wrote through to real directory: %v", err)
	}
}

func TestCreateArchiveRejectsStaleSourceAndDestinationVersions(t *testing.T) {
	requireLinuxArchiveCreate(t)
	for _, staleTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "destination"}[staleTarget], func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.Mkdir(filepath.Join(rootPath, "output"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			root := openArchiveCreateRoot(t, rootPath)
			handler := newArchiveCreateHandler(root, nil)
			sourceVersion := archiveCreateVersion(t, root, "source.txt")
			destinationVersion := archiveCreateVersion(t, root, "output")
			if staleTarget {
				destinationVersion = "stale"
			} else {
				sourceVersion = "stale"
			}
			response := postArchiveCreate(t, handler, archiveCreateRequest{
				Items:              []archiveCreateItem{{Path: "source.txt", Version: sourceVersion}},
				Destination:        "output",
				DestinationVersion: destinationVersion,
				Name:               "stale.zip",
			})
			if response.Code != http.StatusConflict {
				t.Fatalf("stale status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
			}
			if _, err := root.LstatEntry("output/stale.zip"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("stale request published a ZIP: %v", err)
			}
		})
	}
}

func TestCreateArchiveRejectsSourceLargerThanConfiguredLimit(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	source, err := os.Create(filepath.Join(rootPath, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Truncate(maxArchiveSourceBytes + 1); err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	handler := newArchiveCreateHandler(root, nil)
	response := postArchiveCreate(t, handler, archiveCreateRequest{
		Items: []archiveCreateItem{{Path: "large.bin", Version: archiveCreateVersion(t, root, "large.bin")}},
		Name:  "large.zip",
	})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large source status = %d, want %d: %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
	if _, err := root.LstatEntry("large.zip"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("oversized source published a ZIP: %v", err)
	}
}

func TestCreateArchiveDiscardsOutputWhenSourceChangesDuringArchive(t *testing.T) {
	requireLinuxArchiveCreate(t)
	for _, format := range []fsroot.ArchiveFormat{fsroot.ArchiveFormatZIP, fsroot.ArchiveFormatTAR, fsroot.ArchiveFormatTARGZ} {
		t.Run(string(format), func(t *testing.T) {
			rootPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			root := openArchiveCreateRoot(t, rootPath)
			files := &archiveCreateMutatingRoot{Root: root, sourcePath: filepath.Join(rootPath, "source.txt")}
			handler := newArchiveCreateHandler(files, nil)
			name := "changed" + archiveFormatExtension(format)
			response := postArchiveCreate(t, handler, archiveCreateRequest{
				Items:  []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
				Name:   name,
				Format: format,
			})
			if response.Code != http.StatusConflict {
				t.Fatalf("changed source status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
			}
			if _, err := root.LstatEntry(name); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("changed source left a published archive: %v", err)
			}
		})
	}
}

func TestCreateArchiveRetriesExternalNameCompetition(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("archive body"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	files := &archiveCreateRaceRoot{Root: root}
	handler := newArchiveCreateHandler(files, nil)
	response := postArchiveCreate(t, handler, archiveCreateRequest{
		Items:              []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
		Destination:        "output",
		DestinationVersion: archiveCreateVersion(t, root, "output"),
		Name:               "race.zip",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("race create status = %d: %s", response.Code, response.Body.String())
	}
	var result archiveCreateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "output/race (1).zip" || !files.raced {
		t.Fatalf("race result = %#v, raced=%t", result, files.raced)
	}
	if string(readArchiveCreateFile(t, root, "output/race.zip")) != "external file" {
		t.Fatal("external file was overwritten")
	}
	archiveData := readArchiveCreateFile(t, root, result.Path)
	if _, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData))); err != nil {
		t.Fatalf("numbered ZIP is invalid: %v", err)
	}
}

func TestCreateArchivePreservesUppercaseZIPExtensionWhenNumbering(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("archive body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "review.ZIP"), []byte("keep existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	handler := newArchiveCreateHandler(root, nil)
	response := postArchiveCreate(t, handler, archiveCreateRequest{
		Items: []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
		Name:  "review.ZIP",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("uppercase extension status = %d: %s", response.Code, response.Body.String())
	}
	var result archiveCreateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "review (1).ZIP" || result.Name != "review (1).ZIP" {
		t.Fatalf("uppercase extension result = %#v", result)
	}
	if string(readArchiveCreateFile(t, root, "review.ZIP")) != "keep existing" {
		t.Fatal("existing uppercase ZIP name was overwritten")
	}
}

func TestCreateArchiveNumbersMaxLengthNameWithoutExceedingLimit(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("archive body"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	handler := newArchiveCreateHandler(root, nil)
	name := strings.Repeat("x", 251) + ".zip"
	request := archiveCreateRequest{
		Items: []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
		Name:  name,
	}
	for attempt, expected := range []string{name, strings.Repeat("x", 247) + " (1).zip"} {
		response := postArchiveCreate(t, handler, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("attempt %d status = %d: %s", attempt+1, response.Code, response.Body.String())
		}
		var result archiveCreateResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Name != expected || len(result.Name) > 255 {
			t.Fatalf("attempt %d result name = %q (%d bytes), want %q", attempt+1, result.Name, len(result.Name), expected)
		}
	}
}

func TestCreateArchivePassesResourceLimitsAndReportsLimitFailure(t *testing.T) {
	requireLinuxArchiveCreate(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openArchiveCreateRoot(t, rootPath)
	files := &archiveCreateLimitRoot{Root: root}
	handler := newArchiveCreateHandler(files, nil)
	response := postArchiveCreate(t, handler, archiveCreateRequest{
		Items: []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
		Name:  "limited.zip",
	})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("limit status = %d, want %d: %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
	if files.limits.MaxEntries != maxArchiveEntries || files.limits.MaxBytes != maxArchiveSourceBytes {
		t.Fatalf("archive limits = %#v", files.limits)
	}
	if _, err := root.LstatEntry("limited.zip"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("limit failure published a ZIP: %v", err)
	}
}

func TestCreateArchiveCancellationAndDiskFullDoNotPublish(t *testing.T) {
	requireLinuxArchiveCreate(t)
	t.Run("cancel during atomic copy", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		root := openArchiveCreateRoot(t, rootPath)
		ctx, cancel := context.WithCancel(context.Background())
		files := &archiveCreateCancelPublishRoot{Root: root, cancel: cancel}
		handler := newArchiveCreateHandler(files, nil)
		request := archiveCreateRequest{
			Items: []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
			Name:  "cancelled.zip",
		}
		response := serveArchiveCreateRequest(t, handler, request, ctx)
		if response.Code != http.StatusRequestTimeout {
			t.Fatalf("cancel status = %d, want %d: %s", response.Code, http.StatusRequestTimeout, response.Body.String())
		}
		if _, err := root.LstatEntry("cancelled.zip"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("cancelled request published a ZIP: %v", err)
		}
	})
	t.Run("disk full during atomic copy", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.WriteFile(filepath.Join(rootPath, "source.txt"), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		root := openArchiveCreateRoot(t, rootPath)
		index := &archiveCreateTestIndexer{}
		files := &archiveCreateNoSpaceRoot{Root: root}
		handler := newArchiveCreateHandler(files, index)
		response := postArchiveCreate(t, handler, archiveCreateRequest{
			Items: []archiveCreateItem{{Path: "source.txt", Version: archiveCreateVersion(t, root, "source.txt")}},
			Name:  "full.zip",
		})
		if response.Code != http.StatusInsufficientStorage {
			t.Fatalf("disk full status = %d, want %d: %s", response.Code, http.StatusInsufficientStorage, response.Body.String())
		}
		if _, err := root.LstatEntry("full.zip"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("disk full left a published ZIP: %v", err)
		}
		if index.refreshes != 0 {
			t.Fatalf("failed write refreshed index %d times", index.refreshes)
		}
	})
}

func TestCreateArchiveRejectsTooManySelections(t *testing.T) {
	items := make([]archiveCreateItem, maxArchiveSelections+1)
	for index := range items {
		items[index] = archiveCreateItem{Path: filepath.ToSlash(filepath.Join("source", string(rune('a'+index%26))+".txt")), Version: "v1"}
	}
	request := archiveCreateRequest{Items: items, Name: "many.zip"}
	response := postArchiveCreate(t, New(nil, nil), request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("selection count status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func newArchiveCreateHandler(files fileSystem, index fileIndexer) http.Handler {
	return NewWithAgentAndIndex(files, nil, nil, nil, index, nil, coordination.New())
}

func openArchiveCreateRoot(t *testing.T, rootPath string) *fsroot.Root {
	t.Helper()
	root, err := fsroot.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func archiveCreateVersion(t *testing.T, root *fsroot.Root, relativePath string) string {
	t.Helper()
	entry, err := root.LstatEntry(relativePath)
	if err != nil {
		t.Fatalf("inspect %q: %v", relativePath, err)
	}
	return versionForEntry(entry)
}

func postArchiveCreate(t *testing.T, handler http.Handler, request archiveCreateRequest) *httptest.ResponseRecorder {
	t.Helper()
	return serveArchiveCreateRequest(t, handler, request, context.Background())
}

func serveArchiveCreateRequest(t *testing.T, handler http.Handler, request archiveCreateRequest, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/api/v1/files/archive/create", bytes.NewReader(data)).WithContext(ctx)
	httpRequest.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	return response
}

func serveArchiveCreateBody(handler http.Handler, body, contentType, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func readArchiveCreateFile(t *testing.T, root *fsroot.Root, relativePath string) []byte {
	t.Helper()
	file, _, err := root.OpenRead(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readCreateTestArchive(t *testing.T, format fsroot.ArchiveFormat, data []byte) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	if format == fsroot.ArchiveFormat7Z {
		binary, err := exec.LookPath("7z")
		if err != nil {
			t.Skip("requires the 7-Zip command")
		}
		workspace := t.TempDir()
		archivePath := filepath.Join(workspace, "input.7z")
		destination := filepath.Join(workspace, "output")
		if err := os.Mkdir(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "x", "-bd", "-y", "-o"+destination, archivePath)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("extract test 7z: %v: %s", err, output)
		}
		if err := filepath.WalkDir(destination, func(filePath string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if filePath == destination {
				return nil
			}
			relativePath, err := filepath.Rel(destination, filePath)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(relativePath)
			if entry.IsDir() {
				entries[name+"/"] = ""
				return nil
			}
			contents, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}
			entries[name] = string(contents)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return entries
	}
	if format == fsroot.ArchiveFormatZIP {
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range archive.File {
			if item.FileInfo().IsDir() {
				entries[item.Name] = ""
				continue
			}
			file, err := item.Open()
			if err != nil {
				t.Fatal(err)
			}
			contents, readErr := io.ReadAll(file)
			closeErr := file.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read ZIP item %q: read=%v close=%v", item.Name, readErr, closeErr)
			}
			entries[item.Name] = string(contents)
		}
		return entries
	}
	reader := io.Reader(bytes.NewReader(data))
	if format == fsroot.ArchiveFormatTARGZ {
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			t.Fatal(err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			entries[header.Name] = ""
			continue
		}
		contents, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(contents)
	}
	return entries
}

func requireLinuxArchiveCreate(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("安全 ZIP 遍历和原子无覆盖发布当前仅在 Linux 实现")
	}
}

type archiveCreateTestIndexer struct{ refreshes int }

func (*archiveCreateTestIndexer) Status(context.Context) (store.FileIndexStatus, error) {
	return store.FileIndexStatus{}, nil
}

func (*archiveCreateTestIndexer) Query(context.Context, store.FileIndexQuery) ([]store.FileIndexRecord, error) {
	return nil, nil
}

func (index *archiveCreateTestIndexer) Refresh(context.Context) { index.refreshes++ }

type archiveCreateRaceRoot struct {
	*fsroot.Root
	raced bool
}

func (root *archiveCreateRaceRoot) WriteNewAtomicFrom(relativePath string, source io.Reader, size int64, mode fs.FileMode) (string, error) {
	if !root.raced && relativePath == "output/race.zip" {
		root.raced = true
		contents := []byte("external file")
		if _, err := root.Root.WriteNewAtomicFrom(relativePath, bytes.NewReader(contents), int64(len(contents)), 0o644); err != nil {
			return "", err
		}
		return "", fs.ErrExist
	}
	return root.Root.WriteNewAtomicFrom(relativePath, source, size, mode)
}

type archiveCreateLimitRoot struct {
	*fsroot.Root
	limits fsroot.ArchiveLimits
}

func (root *archiveCreateLimitRoot) WriteArchive(_ context.Context, _ []string, _ io.Writer, limits fsroot.ArchiveLimits) error {
	root.limits = limits
	return fsroot.ErrArchiveLimitExceeded
}

func (root *archiveCreateLimitRoot) WriteArchiveFormat(_ context.Context, _ []string, _ io.Writer, limits fsroot.ArchiveLimits, _ fsroot.ArchiveFormat) error {
	root.limits = limits
	return fsroot.ErrArchiveLimitExceeded
}

type archiveCreateMutatingRoot struct {
	*fsroot.Root
	sourcePath string
}

func (root *archiveCreateMutatingRoot) WriteArchive(ctx context.Context, paths []string, output io.Writer, limits fsroot.ArchiveLimits) error {
	if err := root.Root.WriteArchive(ctx, paths, output, limits); err != nil {
		return err
	}
	return os.WriteFile(root.sourcePath, []byte("changed after ZIP completion"), 0o600)
}

func (root *archiveCreateMutatingRoot) WriteArchiveFormat(ctx context.Context, paths []string, output io.Writer, limits fsroot.ArchiveLimits, format fsroot.ArchiveFormat) error {
	if err := root.Root.WriteArchiveFormat(ctx, paths, output, limits, format); err != nil {
		return err
	}
	return os.WriteFile(root.sourcePath, []byte("changed after archive completion"), 0o600)
}

type archiveCreateCancelPublishRoot struct {
	*fsroot.Root
	cancel context.CancelFunc
}

func (root *archiveCreateCancelPublishRoot) WriteNewAtomicFrom(_ string, source io.Reader, _ int64, _ fs.FileMode) (string, error) {
	root.cancel()
	_, err := io.Copy(io.Discard, source)
	return "", err
}

type archiveCreateNoSpaceRoot struct{ *fsroot.Root }

func (*archiveCreateNoSpaceRoot) WriteNewAtomicFrom(string, io.Reader, int64, fs.FileMode) (string, error) {
	return "", syscall.ENOSPC
}
