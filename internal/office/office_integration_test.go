//go:build linux

package office

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOfficeSandboxIntegration 在 Linux 服务器验证真实格式转换和临时文件清理。
func TestOfficeSandboxIntegration(t *testing.T) {
	if os.Getenv("FILESAGE_OFFICE_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_OFFICE_INTEGRATION=1 to run real LibreOffice conversion")
	}
	paths, err := discoverPaths()
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("office preview dependencies are unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	converter, err := newConverter(paths, converterOptions{tempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"preview.docx", "preview.pptx"} {
		t.Run(strings.TrimPrefix(filepath.Ext(name), "."), func(t *testing.T) {
			sample, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			extension := filepath.Ext(name)
			pdf, err := converter.Convert(context.Background(), sample, extension)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
				t.Fatalf("output has no PDF signature: %q", pdf[:min(len(pdf), 16)])
			}
			assertTempRootEmpty(t, converter.tempDir)
		})
	}
}

// TestOfficeSandboxCancellationStopsChildProcess 核实取消请求会终止真实沙箱子进程。
func TestOfficeSandboxCancellationStopsChildProcess(t *testing.T) {
	if os.Getenv("FILESAGE_OFFICE_INTEGRATION") != "1" {
		t.Skip("set FILESAGE_OFFICE_INTEGRATION=1 to run Bubblewrap process-tree test")
	}
	paths, err := discoverPaths()
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("office preview dependencies are unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is required for the sandbox process-tree test")
	}
	sleepPath, err = filepath.EvalSymlinks(sleepPath)
	if err != nil {
		t.Fatal(err)
	}
	processName := fmt.Sprintf("s%010d", os.Getpid())
	target := "/usr/lib/filesage-test/" + processName
	paths.readOnlyFile = append(paths.readOnlyFile, bindMount{source: sleepPath, target: target})
	args, err := sandboxArgsWithCommand(paths, t.TempDir(), []string{target, "30"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- runSandbox(ctx, paths, args) }()
	if !waitForOfficeProcess(processName, 4*time.Second) {
		cancel()
		<-finished
		t.Fatal("sandbox child did not start before the deadline")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sandbox cancellation error = %v, want context canceled", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("sandbox command did not stop after cancellation")
	}
	if waitForOfficeProcess(processName, 2*time.Second) {
		t.Fatal("sandbox child remained after cancellation")
	}
}

// waitForOfficeProcess 等待具有唯一进程名的沙箱测试子进程出现或退出。
func waitForOfficeProcess(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
			if err == nil && strings.TrimSpace(string(comm)) == name {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
