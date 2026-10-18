package indexer

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"filesage/internal/fsroot"
	"filesage/internal/store"
	"github.com/bep/imagemeta"
)

func TestOpenBuildsNoFollowIndexAndSkipsTrash(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("safe tree walking is currently implemented on Linux")
	}

	managedRoot := filepath.Join(t.TempDir(), "managed")
	outsideRoot := filepath.Join(t.TempDir(), "outside")
	for _, directory := range []string{
		filepath.Join(managedRoot, "nested", "deep"),
		filepath.Join(managedRoot, fsroot.TrashDirectoryName),
		outsideRoot,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		filepath.Join(managedRoot, "root.txt"):                              "root",
		filepath.Join(managedRoot, "nested", "deep", "inside.txt"):          "inside",
		filepath.Join(managedRoot, fsroot.TrashDirectoryName, "hidden.txt"): "hidden",
		filepath.Join(outsideRoot, "secret.txt"):                            "secret",
	} {
		if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	photoPath := filepath.Join(managedRoot, "camera.jpg")
	if err := os.WriteFile(photoPath, jpegWithCaptureDate("2020:01:02 03:04:05"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideRoot, filepath.Join(managedRoot, "outside-link")); err != nil {
		t.Fatalf("create test symlink: %v", err)
	}

	root, err := fsroot.Open(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	service, err := Open(context.Background(), root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	deadline := time.Now().Add(10 * time.Second)
	var status store.FileIndexStatus
	for time.Now().Before(deadline) {
		status, err = service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.State == store.FileIndexReady && status.ActiveGeneration > 0 && status.BuildingGeneration == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.State != store.FileIndexReady || status.ActiveGeneration == 0 {
		t.Fatalf("index status = %+v, want completed generation", status)
	}

	records, err := service.Query(context.Background(), store.FileIndexQuery{
		Generation: status.ActiveGeneration, Recursive: true, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]store.FileIndexRecord, len(records))
	for _, record := range records {
		paths[record.RelativePath] = record
	}
	for _, expected := range []string{"root.txt", "nested", "nested/deep", "nested/deep/inside.txt", "outside-link"} {
		if _, ok := paths[expected]; !ok {
			t.Errorf("index omitted %q", expected)
		}
	}
	for _, omitted := range []string{
		fsroot.TrashDirectoryName,
		fsroot.TrashDirectoryName + "/hidden.txt",
		"outside-link/secret.txt",
	} {
		if _, ok := paths[omitted]; ok {
			t.Errorf("index included excluded path %q", omitted)
		}
	}
	if paths["outside-link"].Kind != "symlink" {
		t.Errorf("symlink record kind = %q, want symlink", paths["outside-link"].Kind)
	}
	wantCapture := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := paths["camera.jpg"].CapturedAt; got == nil || !got.Equal(wantCapture) {
		t.Errorf("capture date = %v, want %v", got, wantCapture)
	}
}

func TestDecodeCaptureDateFromJPEG(t *testing.T) {
	got := decodeCaptureDate(bytes.NewReader(jpegWithCaptureDate("2020:01:02 03:04:05")), imagemeta.JPEG)
	want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Fatalf("capture date = %v, want %v", got, want)
	}
}

func jpegWithCaptureDate(value string) []byte {
	tiff := make([]byte, 64)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x8769)
	binary.LittleEndian.PutUint16(tiff[12:14], 4)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint32(tiff[18:22], 26)
	binary.LittleEndian.PutUint16(tiff[26:28], 1)
	binary.LittleEndian.PutUint16(tiff[28:30], 0x9003)
	binary.LittleEndian.PutUint16(tiff[30:32], 2)
	binary.LittleEndian.PutUint32(tiff[32:36], 20)
	binary.LittleEndian.PutUint32(tiff[36:40], 44)
	copy(tiff[44:64], value+"\x00")

	payload := append([]byte("Exif\x00\x00"), tiff...)
	segmentLength := uint16(len(payload) + 2)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, byte(segmentLength >> 8), byte(segmentLength)}
	jpeg = append(jpeg, payload...)
	return append(jpeg, 0xff, 0xda)
}
