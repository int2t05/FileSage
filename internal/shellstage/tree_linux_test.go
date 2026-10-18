//go:build linux

package shellstage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestRequestRejectsTraversalAndMalformedTrees 验证路径和双阶段输入布局必须明确。
func TestRequestRejectsTraversalAndMalformedTrees(t *testing.T) {
	for _, workdir := range []string{"../outside", "a/../b", "/etc", "a\\b"} {
		req := Request{WorkingDir: workdir, Command: "true", Limits: testLimits()}
		if err := validateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("working directory %q error = %v, want ErrInvalidRequest", workdir, err)
		}
	}
	basePath, workPath := privateTrees(t)
	writeTreeFile(t, basePath, "source.txt", "baseline")
	req := Request{BaselineDir: basePath, WorkspaceDir: workPath, WorkingDir: ".", Command: "true", Limits: testLimits()}
	if _, err := Execute(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("mismatched trees error = %v, want ErrInvalidRequest", err)
	}
	if _, err := Execute(context.Background(), Request{BaselineDir: basePath, WorkspaceDir: basePath, WorkingDir: ".", Command: "true", Limits: testLimits()}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("same root error = %v, want ErrInvalidRequest", err)
	}
}

// TestOpenPrivateDirRejectsSymlinkAndPublicMode 拒绝别名根目录和非私有权限。
func TestOpenPrivateDirRejectsSymlinkAndPublicMode(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openPrivateDir(link); !errors.Is(err, ErrUnsafeStaging) {
		t.Fatalf("symlink root error = %v, want ErrUnsafeStaging", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openPrivateDir(root); !errors.Is(err, ErrUnsafeStaging) {
		t.Fatalf("public root error = %v, want ErrUnsafeStaging", err)
	}
}

// TestDiscoverToolsFailsClosedWithoutPath 验证缺失工具不会转为宿主 Shell 回退。
func TestDiscoverToolsFailsClosedWithoutPath(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := discoverTools(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("tool discovery error = %v, want ErrUnavailable", err)
	}
}

// TestBuildChangesReturnsCompleteSortedManifest 验证增删改均产生排序后的状态和文本预览。
func TestBuildChangesReturnsCompleteSortedManifest(t *testing.T) {
	basePath, workPath := privateTrees(t)
	writeTreeFile(t, basePath, "keep.txt", "same\n")
	writeTreeFile(t, basePath, "edit.txt", "before\n")
	writeTreeFile(t, basePath, "remove.txt", "gone\n")
	if err := os.Mkdir(filepath.Join(basePath, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	copyTree(t, basePath, workPath)
	writeTreeFile(t, workPath, "edit.txt", "after\n")
	if err := os.Remove(filepath.Join(workPath, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, workPath, "folder/new.txt", "new\n")

	base, err := openTestRoot(basePath)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	work, err := openTestRoot(workPath)
	if err != nil {
		t.Fatal(err)
	}
	defer work.Close()
	limits := testLimits()
	before, err := scanTree(base, limits)
	if err != nil {
		t.Fatal(err)
	}
	after, err := scanTree(work, limits)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := buildChanges(base, work, before, after, limits.MaxDiffBytesPerFile)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"edit.txt", "folder/new.txt", "remove.txt"}
	gotPaths := make([]string, len(changes))
	for i, change := range changes {
		gotPaths[i] = change.Path
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("paths = %#v, want %#v", gotPaths, wantPaths)
	}
	if changes[0].Operation != Modified || changes[0].Before == nil || changes[0].After == nil ||
		!strings.Contains(changes[0].TextDiff, "-before") || !strings.Contains(changes[0].TextDiff, "+after") {
		t.Fatalf("modified text change = %#v", changes[0])
	}
	if changes[1].Operation != Added || changes[1].Before != nil || changes[1].After == nil {
		t.Fatalf("added change = %#v", changes[1])
	}
	if changes[2].Operation != Deleted || changes[2].Before == nil || changes[2].After != nil {
		t.Fatalf("deleted change = %#v", changes[2])
	}
}

// TestTreeRejectsLinksAndSpecialFiles 拒绝符号链接、硬链接和 FIFO。
func TestTreeRejectsLinksAndSpecialFiles(t *testing.T) {
	cases := []struct {
		name string
		make func(*testing.T, string)
	}{
		{
			name: "symlink",
			make: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink("/etc/passwd", filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			make: func(t *testing.T, root string) {
				t.Helper()
				writeTreeFile(t, root, "source", "content")
				if err := os.Link(filepath.Join(root, "source"), filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo",
			make: func(t *testing.T, root string) {
				t.Helper()
				if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rootPath, _ := privateTrees(t)
			test.make(t, rootPath)
			root, err := openTestRoot(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			_, err = scanTree(root, testLimits())
			if !errors.Is(err, ErrUnsafeStaging) {
				t.Fatalf("scan error = %v, want ErrUnsafeStaging", err)
			}
		})
	}
}

// TestTreeBoundsDepthCountAndSize 验证树遍历和内容摘要均受配置约束。
func TestTreeBoundsDepthCountAndSize(t *testing.T) {
	rootPath, _ := privateTrees(t)
	if err := os.Mkdir(filepath.Join(rootPath, "one"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, rootPath, "one/file", strings.Repeat("x", 32))
	root, err := openTestRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	limits := testLimits()
	limits.MaxDepth = 1
	if _, err := scanTree(root, limits); !errors.Is(err, ErrUnsafeStaging) {
		t.Fatalf("depth error = %v, want ErrUnsafeStaging", err)
	}
	limits = testLimits()
	limits.MaxFiles = 1
	if _, err := scanTree(root, limits); !errors.Is(err, ErrUnsafeStaging) {
		t.Fatalf("count error = %v, want ErrUnsafeStaging", err)
	}
	limits = testLimits()
	limits.FileSizeBytes = 16
	if _, err := scanTree(root, limits); !errors.Is(err, ErrUnsafeStaging) {
		t.Fatalf("file-size error = %v, want ErrUnsafeStaging", err)
	}
}

// TestTextDiffIsBounded 标记大文本预览截断且不超过单文件预算。
func TestTextDiffIsBounded(t *testing.T) {
	oldText := []byte(strings.Repeat("old line\n", 64))
	newText := []byte(strings.Repeat("new line\n", 64))
	diff, truncated := formatTextDiff(oldText, newText, 256, true, true)
	if !truncated {
		t.Fatal("expected truncated diff")
	}
	if len(diff) > 256 {
		t.Fatalf("diff length = %d, limit 256", len(diff))
	}
}

// openTestRoot 打开测试用私有目录。
func openTestRoot(name string) (*os.File, error) {
	return os.Open(name)
}

// privateTrees 创建相互分离的 mode 0700 暂存根目录。
func privateTrees(t *testing.T) (string, string) {
	t.Helper()
	parent := t.TempDir()
	base := filepath.Join(parent, "baseline")
	work := filepath.Join(parent, "workspace")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return base, work
}

// writeTreeFile 建立指定相对路径并写入私有测试内容。
func writeTreeFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyTree 复制常规测试树并保留文件模式。
func copyTree(t *testing.T, source, target string) {
	t.Helper()
	err := filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == source {
			return nil
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.Mkdir(destination, info.Mode().Perm())
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// testLimits 提供快速单测使用的显式资源上限。
func testLimits() Limits {
	return Limits{
		WallTime:            5 * time.Second,
		CPUSeconds:          3,
		AddressSpaceBytes:   256 << 20,
		MemoryBytes:         256 << 20,
		Processes:           128,
		FileSizeBytes:       1 << 20,
		StagingBytes:        16 << 20,
		TemporaryBytes:      2 << 20,
		OutputBytes:         4096,
		MaxFiles:            128,
		MaxDepth:            16,
		MaxDiffBytesPerFile: 512,
	}
}
