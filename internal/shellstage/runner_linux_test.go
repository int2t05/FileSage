//go:build linux

// runner_linux_test.go 覆盖 Linux 沙箱与资源限制。
package shellstage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestExecuteKeepsHostAndBaselineOutsideSandbox 验证文件差异、只读输入、隐藏主机路径和网络隔离。
func TestExecuteKeepsHostAndBaselineOutsideSandbox(t *testing.T) {
	requireSandbox(t)
	root := t.TempDir()
	base, work := makeIntegrationTrees(t, root)
	writeTreeFile(t, base, "edit.txt", "before\n")
	writeTreeFile(t, base, "remove.txt", "remove\n")
	writeTreeFile(t, base, "keep.txt", "keep\n")
	copyTree(t, base, work)
	outside := filepath.Join(t.TempDir(), "outside-marker")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	networkProbe := "test \"$(wc -l < /proc/net/route)\" -le 1"
	if _, err := os.Stat("/bin/bash"); err == nil {
		networkProbe = fmt.Sprintf("if /bin/bash -c 'exec 3<>/dev/tcp/127.0.0.1/%d' >/dev/null 2>&1; then exit 43; fi", port)
	}
	command := strings.Join([]string{
		"printf 'after\\n' > /workspace/edit.txt",
		"printf 'added\\n' > /workspace/added.txt",
		"rm /workspace/remove.txt",
		"test ! -e /home && test ! -e /root && test ! -e /etc/passwd",
		"if printf bad >> /inputs/edit.txt 2>/dev/null; then exit 41; fi",
		"if ln /inputs/edit.txt /workspace/input-hardlink 2>/dev/null; then exit 44; fi",
		"if printf bad > " + shellQuote(outside) + " 2>/dev/null; then exit 42; fi",
		networkProbe,
		"test ! -e /proc/self/fd/3 && test ! -e /proc/self/fd/4",
		"chmod 0777 /workspace",
	}, " && ")
	result, err := Execute(context.Background(), integrationRequest(t, base, work, command, testLimits()))
	if err != nil {
		t.Fatalf("Execute error: %v; stderr=%q", err, result.Stderr)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr=%q", result.ExitCode, result.Stderr)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host path was changed or exposed: stat error = %v", err)
	}
	if info, err := os.Stat(work); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("workspace root permissions changed: info=%v error=%v", info, err)
	}
	for name, want := range map[string]string{"edit.txt": "before\n", "remove.txt": "remove\n", "keep.txt": "keep\n"} {
		data, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("baseline %s = %q, want %q", name, data, want)
		}
	}
	want := []string{"added.txt", "edit.txt", "remove.txt"}
	got := make([]string, len(result.Changes))
	for i, change := range result.Changes {
		got[i] = change.Path
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("manifest paths = %v, want %v", got, want)
	}
}

// TestExecuteRestoresWorkspaceAfterPermissionChange 验证 chmod 000 后仍能恢复私有暂存内容。
func TestExecuteRestoresWorkspaceAfterPermissionChange(t *testing.T) {
	requireSandbox(t)
	base, work := freshIntegrationTrees(t)
	writeTreeFile(t, base, "keep.txt", "baseline\n")
	copyTree(t, base, work)
	result, err := Execute(context.Background(), integrationRequest(t, base, work, "chmod 000 /workspace; sleep 0.3", testLimits()))
	if err != nil && !errors.Is(err, ErrUnsafeStaging) {
		t.Fatalf("Execute error = %v; stderr=%q", err, result.Stderr)
	}
	if err == nil && result.ExitCode != 0 {
		t.Fatalf("exit code = %d; stderr=%q", result.ExitCode, result.Stderr)
	}
	info, err := os.Stat(work)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("workspace root permissions = %v; error=%v", info, err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Fatalf("workspace entries after restore = %v; error=%v", entries, err)
	}
	contents, err := os.ReadFile(filepath.Join(work, "keep.txt"))
	if err != nil || string(contents) != "baseline\n" {
		t.Fatalf("workspace content after restore = %q; error=%v", contents, err)
	}
	root, err := openTestRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := scanTree(root, testLimits()); err != nil {
		t.Fatalf("restored workspace is not safely readable: %v", err)
	}
}

// TestExecutePreservesNestedReadOnlyDirectoryModes 验证只读目录仍可恢复并审阅。
func TestExecutePreservesNestedReadOnlyDirectoryModes(t *testing.T) {
	requireSandbox(t)
	base, work := freshIntegrationTrees(t)
	defer func() {
		_ = os.Chmod(filepath.Join(work, "sealed", "nested"), 0o700)
		_ = os.Chmod(filepath.Join(work, "sealed"), 0o700)
	}()
	result, err := Execute(context.Background(), integrationRequest(t, base, work, "mkdir -p sealed/nested && printf approved > sealed/nested/note.txt && chmod 0555 sealed/nested && chmod 0555 sealed", testLimits()))
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Execute() = (%#v, %v)", result, err)
	}
	for _, name := range []string{"sealed", filepath.Join("sealed", "nested")} {
		info, statErr := os.Stat(filepath.Join(work, name))
		if statErr != nil || info.Mode().Perm() != 0o555 {
			t.Fatalf("directory %q mode = (%v, %v), want 0555", name, info, statErr)
		}
	}
	if content, readErr := os.ReadFile(filepath.Join(work, "sealed", "nested", "note.txt")); readErr != nil || string(content) != "approved" {
		t.Fatalf("restored file = (%q, %v)", content, readErr)
	}
}

// TestExecuteEnforcesProcessLimit 验证 RLIMIT_NPROC 限制沙箱派生进程。
func TestExecuteEnforcesProcessLimit(t *testing.T) {
	requireSandbox(t)
	base, work := freshIntegrationTrees(t)
	limits := testLimits()
	limits.Processes = 4
	command := "i=0; while [ $i -lt 20 ]; do sleep 3 & i=$((i+1)); done; wait"
	result, err := Execute(context.Background(), integrationRequest(t, base, work, command, limits))
	if !errors.Is(err, ErrProcessLimit) {
		t.Fatalf("error = %v, want ErrProcessLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
	}
}

// TestExecuteEnforcesOutputAndTemporaryLimits 验证输出缓存和临时盘分别受限。
func TestExecuteEnforcesOutputAndTemporaryLimits(t *testing.T) {
	requireSandbox(t)
	t.Run("output", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.OutputBytes = 128
		result, err := Execute(context.Background(), integrationRequest(t, base, work, "while :; do printf 0123456789; done", limits))
		if !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("error = %v, want ErrOutputLimit", err)
		}
		if int64(len(result.Stdout)+len(result.Stderr)) > limits.OutputBytes {
			t.Fatalf("captured output exceeds limit: stdout=%d stderr=%d", len(result.Stdout), len(result.Stderr))
		}
	})
	t.Run("temporary", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.TemporaryBytes = 1 << 20
		limits.StagingBytes = 16 << 20
		limits.FileSizeBytes = 8 << 20
		command := "dd if=/dev/zero of=/tmp/large bs=65536 count=32"
		result, err := Execute(context.Background(), integrationRequest(t, base, work, command, limits))
		if !errors.Is(err, ErrTemporaryLimit) {
			t.Fatalf("error = %v, want ErrTemporaryLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
		}
	})
}

// TestExecuteEnforcesTimeCancellationAndFileSize 验证超时、取消和单文件限制。
func TestExecuteEnforcesTimeCancellationAndFileSize(t *testing.T) {
	requireSandbox(t)
	t.Run("wall time", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.WallTime = time.Second
		result, err := Execute(context.Background(), integrationRequest(t, base, work, "sleep 10", limits))
		if !errors.Is(err, ErrWallTimeLimit) {
			t.Fatalf("error = %v, want ErrWallTimeLimit; exit=%d", err, result.ExitCode)
		}
	})
	t.Run("context cancellation", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(150*time.Millisecond, cancel)
		command := "(while :; do printf x >> /workspace/heartbeat; sleep 0.02; done) & wait"
		_, err := Execute(ctx, integrationRequest(t, base, work, command, testLimits()))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		sizeAfterReturn, err := os.Stat(filepath.Join(work, "heartbeat"))
		if err != nil {
			t.Fatalf("heartbeat was not created: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
		sizeAfterWait, err := os.Stat(filepath.Join(work, "heartbeat"))
		if err != nil {
			t.Fatal(err)
		}
		if sizeAfterWait.Size() != sizeAfterReturn.Size() {
			t.Fatalf("sandbox child survived cancellation: size %d -> %d", sizeAfterReturn.Size(), sizeAfterWait.Size())
		}
	})
	t.Run("file size", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.FileSizeBytes = 1 << 20
		result, err := Execute(context.Background(), integrationRequest(t, base, work, "dd if=/dev/zero of=/workspace/large bs=1048576 count=3", limits))
		if !errors.Is(err, ErrFileSizeLimit) {
			t.Fatalf("error = %v, want ErrFileSizeLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
		}
	})
}

// TestExecuteEnforcesCPUAndStagingLimits 验证 CPU 时间和工作区总量限制。
func TestExecuteEnforcesCPUAndStagingLimits(t *testing.T) {
	requireSandbox(t)
	t.Run("CPU time", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.CPUSeconds = 1
		limits.WallTime = 5 * time.Second
		result, err := Execute(context.Background(), integrationRequest(t, base, work, "while :; do :; done", limits))
		if !errors.Is(err, ErrCPUTimeLimit) {
			t.Fatalf("error = %v, want ErrCPUTimeLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
		}
	})
	t.Run("aggregate CPU time", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.CPUSeconds = 1
		limits.WallTime = 8 * time.Second
		command := "yes >/dev/null & yes >/dev/null & wait"
		result, err := Execute(context.Background(), integrationRequest(t, base, work, command, limits))
		if !errors.Is(err, ErrCPUTimeLimit) {
			t.Fatalf("error = %v, want aggregate ErrCPUTimeLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
		}
	})
	t.Run("staging space", func(t *testing.T) {
		base, work := freshIntegrationTrees(t)
		limits := testLimits()
		limits.StagingBytes = 1 << 20
		limits.TemporaryBytes = 1 << 20
		limits.FileSizeBytes = 4 << 20
		command := "dd if=/dev/zero of=/workspace/large bs=1048576 count=2"
		result, err := Execute(context.Background(), integrationRequest(t, base, work, command, limits))
		if !errors.Is(err, ErrStagingLimit) {
			t.Fatalf("error = %v, want ErrStagingLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
		}
	})
}

// TestExecuteEnforcesAddressSpaceLimit 验证子命令无法越过地址空间上限。
func TestExecuteEnforcesAddressSpaceLimit(t *testing.T) {
	requireSandbox(t)
	if _, err := os.Stat("/usr/bin/mawk"); err != nil {
		t.Fatalf("mawk is required for the memory-limit probe: %v", err)
	}
	base, work := freshIntegrationTrees(t)
	limits := testLimits()
	limits.AddressSpaceBytes = 64 << 20
	result, err := Execute(context.Background(), integrationRequest(t, base, work, "mawk 'BEGIN { value=sprintf(\"%*s\", 134217728, \"x\"); print length(value) }'", limits))
	if err != nil {
		t.Fatalf("Execute returned infrastructure error: %v; stderr=%q", err, result.Stderr)
	}
	if result.ExitCode == 0 {
		t.Fatalf("memory allocation succeeded despite 64 MiB address-space limit: stdout=%q", result.Stdout)
	}
}

// TestExecuteEnforcesAggregateMemoryLimit 验证并发进程共享内存上限。
func TestExecuteEnforcesAggregateMemoryLimit(t *testing.T) {
	requireSandbox(t)
	base, work := freshIntegrationTrees(t)
	limits := testLimits()
	limits.AddressSpaceBytes = 256 << 20
	limits.MemoryBytes = 128 << 20
	limits.FileSizeBytes = 96 << 20
	limits.StagingBytes = 256 << 20
	limits.TemporaryBytes = 256 << 20
	command := "dd if=/dev/zero of=/tmp/a bs=1M count=80 status=none & dd if=/dev/zero of=/tmp/b bs=1M count=80 status=none & wait"
	result, err := Execute(context.Background(), integrationRequest(t, base, work, command, limits))
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("error = %v, want ErrMemoryLimit; exit=%d stderr=%q", err, result.ExitCode, result.Stderr)
	}
}

// TestAvailabilityFailsClosedWithoutDelegation 验证无委托时拒绝运行。
func TestAvailabilityFailsClosedWithoutDelegation(t *testing.T) {
	_, err := delegatedCgroup()
	if err == nil {
		t.Skip("host has a writable delegated cgroup; real sandbox tests cover execution")
	}
	t.Logf("delegated cgroup unavailable: %v", err)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("delegatedCgroup error = %v, want ErrUnavailable", err)
	}
	if checkErr := CheckAvailability(context.Background()); !errors.Is(checkErr, ErrUnavailable) {
		t.Fatalf("CheckAvailability error = %v, want ErrUnavailable", checkErr)
	}
}

// TestCgroupAtomicProcessPlacement 验证进程从启动起受 cgroup 限制。
func TestCgroupAtomicProcessPlacement(t *testing.T) {
	manager, err := delegatedCgroup()
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	group, err := manager.createSandboxCgroup(testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if group.dir != nil {
			_ = group.close()
		}
	})
	cmd := exec.Command("/bin/sleep", "10")
	group.attach(cmd)
	startErr := cmd.Start()
	if startErr != nil {
		t.Fatalf("start limited process: %v", startErr)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	pids, err := readCgroupPIDs(group.dir)
	if err != nil {
		t.Fatalf("read command cgroup: %v", err)
	}
	containsCommand := false
	for _, pid := range pids {
		if pid == strconv.Itoa(cmd.Process.Pid) {
			containsCommand = true
			break
		}
	}
	if !containsCommand {
		membership, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", cmd.Process.Pid))
		t.Fatalf("command PID %d is missing from limited cgroup %v; proc cgroup=%q", cmd.Process.Pid, pids, strings.TrimSpace(string(membership)))
	}
	if err := group.close(); err != nil {
		t.Fatalf("close command cgroup: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("command survived cgroup cleanup")
	}
	if cmd.ProcessState == nil {
		t.Fatalf("command exit state = %v, want killed by cgroup cleanup", cmd.ProcessState)
	}
	waitStatus := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGKILL {
		t.Fatalf("command status = %v, want SIGKILL", waitStatus)
	}
	if group.dir != nil {
		if err := group.close(); err != nil {
			t.Fatalf("cleanup command cgroup: %v", err)
		}
	}
}

// TestCgroupEnforcesAggregatePids 验证 pids 控制器拒绝超额派生进程。
func TestCgroupEnforcesAggregatePids(t *testing.T) {
	manager, err := delegatedCgroup()
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	limits := testLimits()
	limits.Processes = 2
	group, err := manager.createSandboxCgroup(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if group.dir != nil {
			_ = group.close()
		}
	})
	pidsMax, err := readCgroupFile(group.dir, "pids.max")
	if err != nil || strings.TrimSpace(pidsMax) != strconv.Itoa(int(limits.Processes+processReserve)) {
		t.Fatalf("pids.max = %q, error = %v", strings.TrimSpace(pidsMax), err)
	}
	cmd := exec.Command("/bin/sh", "-c", "i=0; while [ $i -lt 20 ]; do sleep 10 & i=$((i+1)); done; wait")
	group.attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start limited process tree: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if exceeded, err := readCgroupEvent(group.dir, "pids.events", "max"); err != nil {
			t.Fatalf("read pids.events: %v", err)
		} else if exceeded > 0 {
			if err := group.close(); err != nil {
				t.Fatalf("close command cgroup: %v", err)
			}
			_ = cmd.Wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pids controller did not reject excess processes")
}

// TestCgroupEnforcesAggregateCPUQuota 验证并发进程共享 CPU 配额。
func TestCgroupEnforcesAggregateCPUQuota(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("host exposes fewer than two CPUs for quota contention")
	}
	manager, err := delegatedCgroup()
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	group, err := manager.createSandboxCgroup(testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if group.dir != nil {
			_ = group.close()
		}
	})
	cpuMax, err := readCgroupFile(group.dir, "cpu.max")
	if err != nil || strings.TrimSpace(cpuMax) != "100000 100000" {
		t.Fatalf("cpu.max = %q, error = %v", strings.TrimSpace(cpuMax), err)
	}
	cmd := exec.Command("/bin/sh", "-c", "while :; do :; done & while :; do :; done & wait")
	group.attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start CPU-bound process tree: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		throttled, err := readCgroupEvent(group.dir, "cpu.stat", "nr_throttled")
		if err != nil {
			t.Fatalf("read CPU throttling counter: %v", err)
		}
		if throttled > 0 {
			if err := group.close(); err != nil {
				t.Fatalf("close command cgroup: %v", err)
			}
			_ = cmd.Wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CPU-bound processes were not throttled by cpu.max")
}

// requireSandbox 仅在实际 Bubblewrap 探测明确失败时跳过集成测试。
func requireSandbox(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	err := CheckAvailability(ctx)
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("required Bubblewrap/cgroup capability unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Bubblewrap capability probe failed unexpectedly: %v", err)
	}
}

// freshIntegrationTrees 创建隔离集成测试使用的空基线和副本。
func freshIntegrationTrees(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	return makeIntegrationTrees(t, root)
}

// makeIntegrationTrees 创建两个互不嵌套的私有暂存根。
func makeIntegrationTrees(t *testing.T, root string) (string, string) {
	t.Helper()
	base := filepath.Join(root, "baseline")
	work := filepath.Join(root, "workspace")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return base, work
}

// integrationRequest 组装显式资源预算和相对工作目录。
func integrationRequest(t *testing.T, base, work, command string, limits Limits) Request {
	t.Helper()
	return Request{BaselineDir: base, WorkspaceDir: work, WorkingDir: ".", Command: command, Limits: limits}
}

// shellQuote 为测试命令构造单引号参数。
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
