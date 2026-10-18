//go:build linux

// runner_linux.go 只在隔离沙箱中执行命令，并保留私有工作区边界。
package shellstage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	rootTmpfsBytes  = 16 << 20
	probeTimeout    = 10 * time.Second
	monitorInterval = 100 * time.Millisecond
	processReserve  = 4
)

type toolPaths struct {
	bwrap   string
	prlimit string
	mounts  []systemMount
}

type systemMount struct {
	source string
	target string
}

// platformCheckAvailability 实际探测 Bubblewrap、命名空间和资源限制能力。
func platformCheckAvailability(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	cgroups, err := delegatedCgroup()
	if err != nil {
		return err
	}
	tools, err := discoverTools()
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	processLimit, err := boundedProcessLimit(DefaultLimits().Processes)
	if err != nil {
		return err
	}
	probeCommand := "test -x /bin/sh && test -d /proc && test -c /dev/null && test ! -e /home && test ! -e /root && test ! -e /etc/passwd && test \"$(wc -l < /proc/net/route)\" -le 1 && ! printf x >/probe 2>/dev/null && printf ok >/tmp/probe"
	args := sandboxArgs(tools, 8<<20, ".", probeCommand, true)
	limits := []string{"--cpu=2:3", "--as=536870912:536870912", fmt.Sprintf("--nproc=%d:%d", processLimit, processLimit), "--fsize=1048576:1048576", "--", tools.bwrap}
	limits = append(limits, args...)
	cmd := exec.CommandContext(probeCtx, tools.prlimit, limits...)
	cgroup, err := cgroups.createSandboxCgroup(DefaultLimits())
	if err != nil {
		return err
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	configureKill(cmd)
	cgroup.attach(cmd)
	cmd.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	probeErr := cmd.Run()
	memoryExceeded, memoryErr := cgroup.memoryLimitExceeded()
	cleanupErr := cgroup.close()
	if memoryErr != nil || cleanupErr != nil {
		return errors.Join(unavailableCgroup("verify namespace probe limits", memoryErr), cleanupErr)
	}
	if memoryExceeded {
		return ErrMemoryLimit
	}
	if probeErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: bubblewrap namespace/resource probe failed (%v): %s", ErrUnavailable, probeErr, compactOutput(output.String()))
	}
	return nil
}

// platformExecute 在独立命名空间运行 Shell，仅修改工作区副本。
func platformExecute(ctx context.Context, req Request) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	baseline, baselinePath, err := openPrivateDir(req.BaselineDir)
	if err != nil {
		return Result{}, err
	}
	defer baseline.Close()
	workspace, workspacePath, err := openPrivateDir(req.WorkspaceDir)
	if err != nil {
		return Result{}, err
	}
	defer workspace.Close()
	if sameInodeFromFiles(baseline, workspace) || pathsOverlap(baselinePath, workspacePath) {
		return Result{}, fmt.Errorf("%w: baseline and workspace must be separate directories", ErrInvalidRequest)
	}
	before, err := scanTree(baseline, req.Limits)
	if err != nil {
		return Result{}, err
	}
	workspaceBefore, err := scanTree(workspace, req.Limits)
	if err != nil {
		return Result{}, err
	}
	if !sameTrees(before, workspaceBefore) {
		return Result{}, fmt.Errorf("%w: workspace must exactly match the read-only baseline", ErrInvalidRequest)
	}
	if before.bytes+workspaceBefore.bytes > req.Limits.StagingBytes-req.Limits.TemporaryBytes {
		return Result{}, fmt.Errorf("%w: baseline and workspace leave no bounded temporary-space budget", ErrStagingLimit)
	}
	workingDir := req.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	workdirEntry, exists := workspaceBefore.entries[workingDir]
	if !exists || !workdirEntry.state.IsDirectory {
		return Result{}, fmt.Errorf("%w: working directory is missing or not a directory", ErrInvalidRequest)
	}
	workspaceBudget := req.Limits.StagingBytes - before.bytes - req.Limits.TemporaryBytes
	processLimit, err := boundedProcessLimit(req.Limits.Processes)
	if err != nil {
		return Result{}, err
	}
	tools, err := discoverTools()
	if err != nil {
		return Result{}, err
	}
	if err := platformCheckAvailability(ctx); err != nil {
		return Result{}, err
	}
	workspaceMount, restoreWorkspace, err := stageWorkspaceMount(workspace, workspacePath)
	if err != nil {
		return Result{}, err
	}
	defer workspaceMount.Close()
	cgroups, err := delegatedCgroup()
	if err != nil {
		restoreErr := restoreWorkspace()
		return Result{}, errors.Join(err, restoreErr)
	}
	cgroup, err := cgroups.createSandboxCgroup(req.Limits)
	if err != nil {
		restoreErr := restoreWorkspace()
		return Result{}, errors.Join(err, restoreErr)
	}
	args := sandboxArgs(tools, req.Limits.TemporaryBytes, workingDir, req.Command, false)
	prlimitArgs := []string{
		fmt.Sprintf("--cpu=%d:%d", req.Limits.CPUSeconds, req.Limits.CPUSeconds+1),
		fmt.Sprintf("--as=%d:%d", req.Limits.AddressSpaceBytes, req.Limits.AddressSpaceBytes),
		fmt.Sprintf("--nproc=%d:%d", processLimit, processLimit),
		fmt.Sprintf("--fsize=%d:%d", req.Limits.FileSizeBytes, req.Limits.FileSizeBytes),
		"--", tools.bwrap,
	}
	prlimitArgs = append(prlimitArgs, args...)
	runCtx, cancel := context.WithTimeout(ctx, req.Limits.WallTime)
	defer cancel()
	cmd := exec.CommandContext(runCtx, tools.prlimit, prlimitArgs...)
	cmd.ExtraFiles = []*os.File{baseline, workspaceMount}
	cgroup.attach(cmd)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.Cancel = func() error { return killCommand(cmd) }
	cmd.WaitDelay = 2 * time.Second
	capture := newOutputCapture(req.Limits.OutputBytes)
	capture.setKill(func() { _ = killCommand(cmd) })
	cmd.Stdout = capture.writer(true)
	cmd.Stderr = capture.writer(false)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		cgroupErr := cgroup.close()
		restoreErr := restoreWorkspace()
		return Result{}, errors.Join(fmt.Errorf("%w: start bubblewrap: %v", ErrUnavailable, err), cgroupErr, restoreErr)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	var waitErr error
	var runErr error
	finished := false
	for !finished {
		if capture.overflowed() {
			runErr = ErrOutputLimit
			_ = killCommand(cmd)
			waitErr = <-wait
			finished = true
			continue
		}
		select {
		case waitErr = <-wait:
			finished = true
		case <-runCtx.Done():
			_ = killCommand(cmd)
			waitErr = <-wait
			finished = true
			if ctx.Err() != nil {
				runErr = ctx.Err()
			} else {
				runErr = ErrWallTimeLimit
			}
		case <-ticker.C:
			cpuExceeded, cpuErr := cgroup.cpuLimitExceeded()
			if cpuErr != nil {
				runErr = unavailableCgroup("read aggregate CPU usage", cpuErr)
				_ = killCommand(cmd)
				waitErr = <-wait
				finished = true
				continue
			}
			if cpuExceeded {
				runErr = ErrCPUTimeLimit
				_ = killCommand(cmd)
				waitErr = <-wait
				finished = true
				continue
			}
			used, inspectErr := summarizeWorkspace(workspaceMount, req.Limits.MaxFiles, req.Limits.MaxDepth, workspaceBudget)
			if inspectErr != nil {
				runErr = inspectErr
				_ = killCommand(cmd)
				waitErr = <-wait
				finished = true
				continue
			}
			if used > workspaceBudget {
				runErr = ErrStagingLimit
				_ = killCommand(cmd)
				waitErr = <-wait
				finished = true
			}
		}
	}
	if exceeded, monitorErr := cgroup.memoryLimitExceeded(); monitorErr != nil {
		runErr = errors.Join(runErr, unavailableCgroup("read aggregate memory events", monitorErr))
	} else if exceeded {
		runErr = errors.Join(runErr, ErrMemoryLimit)
	}
	if exceeded, monitorErr := cgroup.cpuLimitExceeded(); monitorErr != nil {
		runErr = errors.Join(runErr, unavailableCgroup("read aggregate CPU usage", monitorErr))
	} else if exceeded {
		runErr = errors.Join(runErr, ErrCPUTimeLimit)
	}
	runErr = errors.Join(runErr, cgroup.close())
	if err := restoreWorkspace(); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if runErr == nil && capture.overflowed() {
		runErr = ErrOutputLimit
	}
	result := Result{Duration: time.Since(started)}
	result.Stdout, result.Stderr = capture.values()
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		result.ExitCode = -1
	}
	if runErr == nil {
		runErr = classifyProcessError(waitErr, result.Stderr)
		if runErr == nil && cpuLimitSignal(waitErr, cmd.ProcessState, req.Limits.CPUSeconds, result.Duration) {
			runErr = ErrCPUTimeLimit
		}
	}
	baselineAfter, scanErr := scanTree(baseline, req.Limits)
	if scanErr != nil || !sameTrees(before, baselineAfter) {
		return result, errors.Join(runErr, ErrInputChanged)
	}
	after, scanErr := scanTree(workspace, req.Limits)
	if scanErr != nil {
		return result, errors.Join(runErr, scanErr)
	}
	if before.bytes+after.bytes > req.Limits.StagingBytes-req.Limits.TemporaryBytes {
		return result, errors.Join(runErr, ErrStagingLimit)
	}
	changes, diffErr := buildChanges(baseline, workspace, before, after, req.Limits.MaxDiffBytesPerFile)
	if diffErr != nil {
		return result, errors.Join(runErr, diffErr)
	}
	if len(changes) > req.Limits.MaxFiles {
		return result, errors.Join(runErr, fmt.Errorf("%w: change manifest exceeds %d entries", ErrStagingLimit, req.Limits.MaxFiles))
	}
	result.Changes = changes
	return result, runErr
}

// stageWorkspaceMount 隔离可被命令 chmod 的挂载根，保护宿主工作区权限。
func stageWorkspaceMount(workspace *os.File, workspacePath string) (*os.File, func() error, error) {
	temporaryPath, err := os.MkdirTemp(workspacePath, ".filesage-")
	if err != nil {
		return nil, nil, fmt.Errorf("%w: create private workspace mount: %v", ErrUnsafeStaging, err)
	}
	temporaryName := filepath.Base(temporaryPath)
	temporaryFD, err := unix.Openat(int(workspace.Fd()), temporaryName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Unlinkat(int(workspace.Fd()), temporaryName, unix.AT_REMOVEDIR)
		return nil, nil, fmt.Errorf("%w: open private workspace mount: %v", ErrUnsafeStaging, err)
	}
	temporary := os.NewFile(uintptr(temporaryFD), temporaryPath)
	restore := func() error {
		if err := unix.Fchmod(temporaryFD, 0o700); err != nil {
			return fmt.Errorf("%w: restore private workspace mount permissions: %v", ErrUnsafeStaging, err)
		}
		var directoryModes []workspaceDirectoryMode
		modeErr := normalizeWorkspaceDirectories(temporaryFD, "", &directoryModes)
		entries, readErr := readDirectoryEntries(temporaryFD)
		restoreErr := modeErr
		if readErr != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("%w: enumerate sandbox workspace: %v", ErrUnsafeStaging, readErr))
		} else {
			for _, entry := range entries {
				name := entry.Name()
				if err := unix.Renameat(temporaryFD, name, int(workspace.Fd()), name); err != nil {
					restoreErr = errors.Join(restoreErr, fmt.Errorf("%w: restore workspace entry %q: %v", ErrUnsafeStaging, name, err))
				}
			}
		}
		restoreErr = errors.Join(restoreErr, applyWorkspaceDirectoryModes(int(workspace.Fd()), directoryModes))
		if err := unix.Unlinkat(int(workspace.Fd()), temporaryName, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("%w: remove private workspace mount: %v", ErrUnsafeStaging, err))
		}
		return restoreErr
	}
	entries, err := readDirectoryEntries(int(workspace.Fd()))
	if err != nil {
		restoreErr := restore()
		temporary.Close()
		return nil, nil, errors.Join(fmt.Errorf("%w: enumerate workspace copy: %v", ErrUnsafeStaging, err), restoreErr)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == temporaryName {
			continue
		}
		if err := unix.Renameat(int(workspace.Fd()), name, temporaryFD, name); err != nil {
			restoreErr := restore()
			temporary.Close()
			return nil, nil, errors.Join(fmt.Errorf("%w: stage workspace entry %q: %v", ErrUnsafeStaging, name, err), restoreErr)
		}
	}
	return temporary, restore, nil
}

type workspaceDirectoryMode struct {
	path string
	mode uint32
}

// normalizeWorkspaceDirectories 暂时放宽权限，确保目录可安全移出挂载点。
func normalizeWorkspaceDirectories(dirFD int, prefix string, modes *[]workspaceDirectoryMode) error {
	entries, err := readDirectoryEntries(dirFD)
	if err != nil {
		return fmt.Errorf("%w: enumerate workspace directory %q: %v", ErrUnsafeStaging, prefix, err)
	}
	var result error
	for _, entry := range entries {
		name := entry.Name()
		var stat unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			result = errors.Join(result, fmt.Errorf("%w: inspect workspace directory entry %q: %v", ErrUnsafeStaging, name, err))
			continue
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		relative := name
		if prefix != "" {
			relative = prefix + "/" + name
		}
		*modes = append(*modes, workspaceDirectoryMode{path: relative, mode: stat.Mode & 0o7777})
		if err := unix.Fchmodat(dirFD, name, 0o700, 0); err != nil {
			result = errors.Join(result, fmt.Errorf("%w: make workspace directory %q accessible: %v", ErrUnsafeStaging, relative, err))
			continue
		}
		childFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("%w: open workspace directory %q: %v", ErrUnsafeStaging, relative, err))
			continue
		}
		walkErr := normalizeWorkspaceDirectories(childFD, relative, modes)
		closeErr := unix.Close(childFD)
		result = errors.Join(result, walkErr, closeErr)
	}
	return result
}

// applyWorkspaceDirectoryModes 按由深到浅的顺序恢复 Shell 设置的权限。
func applyWorkspaceDirectoryModes(rootFD int, modes []workspaceDirectoryMode) error {
	var result error
	for index := len(modes) - 1; index >= 0; index-- {
		mode := modes[index]
		components := strings.Split(mode.path, "/")
		parentFD, err := unix.Dup(rootFD)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("%w: duplicate workspace root: %v", ErrUnsafeStaging, err))
			continue
		}
		for _, component := range components[:len(components)-1] {
			nextFD, openErr := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			_ = unix.Close(parentFD)
			if openErr != nil {
				parentFD = -1
				result = errors.Join(result, fmt.Errorf("%w: open workspace parent of %q: %v", ErrUnsafeStaging, mode.path, openErr))
				break
			}
			parentFD = nextFD
		}
		if parentFD < 0 {
			continue
		}
		directoryFD, openErr := unix.Openat(parentFD, components[len(components)-1], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(parentFD)
		if openErr != nil {
			result = errors.Join(result, fmt.Errorf("%w: open workspace directory %q for permission restore: %v", ErrUnsafeStaging, mode.path, openErr))
			continue
		}
		chmodErr := unix.Fchmod(directoryFD, mode.mode)
		closeErr := unix.Close(directoryFD)
		if chmodErr != nil || closeErr != nil {
			result = errors.Join(result, fmt.Errorf("%w: restore workspace directory %q permissions: %v", ErrUnsafeStaging, mode.path, errors.Join(chmodErr, closeErr)))
		}
	}
	return result
}

// readDirectoryEntries 获取目录快照，不跟随任何目录项。
func readDirectoryEntries(dirFD int) ([]os.DirEntry, error) {
	fd, err := unix.Dup(dirFD)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "directory")
	defer file.Close()
	return file.ReadDir(-1)
}

// discoverTools 只接受 root 拥有且不可被普通用户改写的系统工具。
func discoverTools() (toolPaths, error) {
	bwrap, err := trustedExecutable("bwrap")
	if err != nil {
		return toolPaths{}, err
	}
	prlimit, err := trustedExecutable("prlimit")
	if err != nil {
		return toolPaths{}, err
	}
	paths := toolPaths{bwrap: bwrap, prlimit: prlimit}
	seen := make(map[string]struct{})
	for _, target := range []string{"/bin", "/usr/bin", "/lib", "/lib64", "/usr/lib"} {
		resolved, err := filepath.EvalSymlinks(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return toolPaths{}, fmt.Errorf("%w: inspect system runtime path %q: %v", ErrUnavailable, target, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() || !trustedSystemObject(resolved, true) {
			return toolPaths{}, fmt.Errorf("%w: runtime path %q is not a trusted system directory", ErrUnavailable, target)
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		paths.mounts = append(paths.mounts, systemMount{source: resolved, target: target})
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		return toolPaths{}, fmt.Errorf("%w: /bin/sh is required", ErrUnavailable)
	}
	if _, ok := seen["/bin"]; !ok && !pathExistsMount(paths.mounts, "/usr/bin") {
		return toolPaths{}, fmt.Errorf("%w: no system command directory is available", ErrUnavailable)
	}
	return paths, nil
}

// trustedExecutable 验证 Bubblewrap 和 prlimit 来自不可写系统目录。
func trustedExecutable(name string) (string, error) {
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s was not found", ErrUnavailable, name)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil || !trustedSystemObject(resolved, false) || !insideSystemPrefix(resolved) {
		return "", fmt.Errorf("%w: %s is not a trusted system executable", ErrUnavailable, name)
	}
	return resolved, nil
}

// trustedSystemObject 检查工具及其目录由 root 管理且不可由普通用户写入。
func trustedSystemObject(name string, directory bool) bool {
	info, err := os.Stat(name)
	if err != nil {
		return false
	}
	if directory && !info.IsDir() || !directory && (!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0) {
		return false
	}
	for current := name; ; current = filepath.Dir(current) {
		entry, err := os.Stat(current)
		if err != nil {
			return false
		}
		stat, ok := entry.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || entry.Mode().Perm()&0o022 != 0 {
			return false
		}
		if current == "/" {
			return true
		}
	}
}

// insideSystemPrefix 限定宿主可执行文件到系统二进制目录。
func insideSystemPrefix(name string) bool {
	for _, prefix := range []string{"/usr/bin", "/usr/sbin", "/usr/local/bin", "/bin", "/sbin"} {
		rel, err := filepath.Rel(prefix, name)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

// pathExistsMount 判断运行时目录是否已纳入最小命令环境。
func pathExistsMount(mounts []systemMount, target string) bool {
	for _, mount := range mounts {
		if mount.target == target {
			return true
		}
	}
	return false
}

// sandboxArgs 构建没有宿主根目录、网络和环境继承的 Bubblewrap 参数。
func sandboxArgs(tools toolPaths, temporaryBytes int64, workingDir, command string, probe bool) []string {
	args := []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts",
		"--disable-userns", "--assert-userns-disabled", "--die-with-parent", "--cap-drop", "ALL",
		"--hostname", "filesage-sandbox",
		"--size", fmt.Sprint(rootTmpfsBytes), "--tmpfs", "/",
	}
	created := map[string]bool{"/": true}
	for _, mount := range tools.mounts {
		appendMountDirectories(&args, mount.target, created)
		args = append(args, "--dir", mount.target, "--ro-bind", mount.source, mount.target)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--dir", "/tmp", "--size", fmt.Sprint(temporaryBytes), "--tmpfs", "/tmp")
	if !probe {
		args = append(args, "--dir", "/inputs", "--ro-bind-fd", "3", "/inputs", "--dir", "/workspace", "--bind-fd", "4", "/workspace")
		created["/inputs"], created["/workspace"] = true, true
	}
	args = append(args, "--dir", "/tmp/home", "--remount-ro", "/")
	if probe {
		args = append(args, "--chdir", "/")
	} else {
		args = append(args, "--chdir", sandboxWorkdir(workingDir))
	}
	args = append(args, "--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/tmp/home", "--setenv", "TMPDIR", "/tmp",
		"--setenv", "LANG", "C", "--setenv", "LC_ALL", "C", "--", "/bin/sh", "-c", command)
	return args
}

// appendMountDirectories 逐级建立空根中的只读挂载目标。
func appendMountDirectories(args *[]string, target string, created map[string]bool) {
	components := strings.Split(strings.TrimPrefix(target, "/"), "/")
	current := ""
	for _, component := range components[:len(components)-1] {
		current += "/" + component
		if created[current] {
			continue
		}
		*args = append(*args, "--dir", current)
		created[current] = true
	}
}

// sandboxWorkdir 根目录用 "." 映射到工作区根。
func sandboxWorkdir(relative string) string {
	if relative == "" || relative == "." {
		return "/workspace"
	}
	return "/workspace/" + relative
}

// openPrivateDir 打开无符号链接、私有且归服务账户所有的目录。
func openPrivateDir(name string) (*os.File, string, error) {
	if !filepath.IsAbs(name) {
		return nil, "", fmt.Errorf("%w: staging directory must be absolute", ErrInvalidRequest)
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, "", fmt.Errorf("%w: resolve staging directory", ErrInvalidRequest)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(abs) {
		return nil, "", fmt.Errorf("%w: staging path contains a symlink or is unavailable", ErrUnsafeStaging)
	}
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("%w: open staging directory: %v", ErrUnsafeStaging, err)
	}
	file := os.NewFile(uintptr(fd), abs)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		file.Close()
		return nil, "", fmt.Errorf("%w: staging directory must be mode 0700 or stricter and owned by the service account", ErrUnsafeStaging)
	}
	return file, filepath.Clean(abs), nil
}

// sameInodeFromFiles 判断两个暂存根是否是同一目录 inode。
func sameInodeFromFiles(a, b *os.File) bool {
	var x, y unix.Stat_t
	if unix.Fstat(int(a.Fd()), &x) != nil || unix.Fstat(int(b.Fd()), &y) != nil {
		return true
	}
	return x.Dev == y.Dev && x.Ino == y.Ino
}

// pathsOverlap 拒绝一个暂存根嵌套在另一个暂存根内。
func pathsOverlap(a, b string) bool {
	return containsPath(a, b) || containsPath(b, a)
}

// containsPath 判断 parent 是否等于或包含 child。
func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)))
}

// sameTrees 比较两棵暂存树的完整初始状态。
func sameTrees(a, b fileTree) bool {
	if len(a.entries) != len(b.entries) || a.bytes != b.bytes {
		return false
	}
	for name, old := range a.entries {
		current, ok := b.entries[name]
		if !ok || !sameState(old.state, current.state) {
			return false
		}
	}
	return true
}

// configureKill 让取消信号覆盖 Bubblewrap 所在的宿主进程组。
func configureKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killCommand(cmd) }
}

// killCommand 杀死执行组，Bubblewrap 同时负责清理 PID 命名空间子进程。
func killCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// classifyProcessError 将 Linux 资源限制信号映射为可审计错误。
func classifyProcessError(waitErr error, stderr string) error {
	if strings.Contains(stderr, "bwrap: Creating new namespace failed:") {
		return fmt.Errorf("%w: bubblewrap could not create namespaces: %s", ErrUnavailable, compactOutput(stderr))
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			switch status.Signal() {
			case syscall.SIGXCPU:
				return ErrCPUTimeLimit
			case syscall.SIGXFSZ:
				return ErrFileSizeLimit
			}
		}
	}
	if strings.Contains(stderr, "No space left on device") {
		return ErrTemporaryLimit
	}
	if strings.Contains(stderr, "File size limit exceeded") {
		return ErrFileSizeLimit
	}
	lowerStderr := strings.ToLower(stderr)
	if strings.Contains(stderr, "Resource temporarily unavailable") || strings.Contains(lowerStderr, "can't fork") || strings.Contains(lowerStderr, "cannot fork") {
		return ErrProcessLimit
	}
	return nil
}

// cpuLimitSignal 识别软上限信号或硬上限导致的 SIGKILL。
func cpuLimitSignal(waitErr error, state *os.ProcessState, seconds uint64, duration time.Duration) bool {
	if state == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	if status.Exited() && state.ExitCode() == 128+int(syscall.SIGXCPU) && duration >= time.Duration(seconds)*time.Second {
		return true
	}
	if status.Exited() && state.ExitCode() == 128+int(syscall.SIGKILL) && duration >= time.Duration(seconds+1)*time.Second {
		return true
	}
	if !status.Signaled() || (status.Signal() != syscall.SIGXCPU && status.Signal() != syscall.SIGKILL) {
		return false
	}
	used := state.UserTime() + state.SystemTime()
	return used >= time.Duration(seconds)*time.Second
}

// boundedProcessLimit 将请求额度叠加到服务账户现有线程数，避免误伤同账户进程。
func boundedProcessLimit(requested uint64) (uint64, error) {
	uid := unix.Getuid()
	if uid == 0 {
		return 0, fmt.Errorf("%w: process limits cannot be enforced for uid 0", ErrUnavailable)
	}
	current, err := countUIDTasks(uint32(uid))
	if err != nil {
		return 0, fmt.Errorf("%w: count service-account tasks: %v", ErrUnavailable, err)
	}
	maximum := ^uint64(0)
	if current > maximum-processReserve || requested > maximum-current-processReserve {
		return 0, fmt.Errorf("%w: process limit overflows", ErrInvalidRequest)
	}
	limit := current + requested + processReserve
	var rlimit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NPROC, &rlimit); err != nil {
		return 0, fmt.Errorf("%w: read RLIMIT_NPROC: %v", ErrUnavailable, err)
	}
	if rlimit.Max != unix.RLIM_INFINITY && limit > rlimit.Max {
		return 0, fmt.Errorf("%w: host RLIMIT_NPROC cannot accommodate %d sandbox processes", ErrUnavailable, requested)
	}
	return limit, nil
}

// countUIDTasks 统计当前 UID 在线程级进程表中的占用。
func countUIDTasks(uid uint32) (uint64, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, entry := range entries {
		if _, err := strconv.ParseUint(entry.Name(), 10, 64); err != nil {
			continue
		}
		procPath := filepath.Join("/proc", entry.Name())
		info, err := os.Stat(procPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, fmt.Errorf("unexpected proc stat type")
		}
		if stat.Uid != uid {
			continue
		}
		tasks, err := os.ReadDir(filepath.Join(procPath, "task"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += uint64(len(tasks))
	}
	return total, nil
}

// compactOutput 将探测失败输出压成一行且限制长度。
func compactOutput(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

type outputCapture struct {
	mu       sync.Mutex
	limit    int64
	total    int64
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	overflow bool
	kill     func()
}

type outputStream struct {
	capture *outputCapture
	stdout  bool
}

// newOutputCapture 创建共享总字节数上限的输出缓存。
func newOutputCapture(limit int64) *outputCapture {
	return &outputCapture{limit: limit}
}

// setKill 注册输出超限时的沙箱终止动作。
func (c *outputCapture) setKill(kill func()) {
	c.mu.Lock()
	c.kill = kill
	c.mu.Unlock()
}

// writer 返回对应标准流的限长写入器。
func (c *outputCapture) writer(stdout bool) io.Writer {
	return outputStream{capture: c, stdout: stdout}
}

// Write 保留总输出预算内的字节，并在溢出时终止执行组。
func (w outputStream) Write(data []byte) (int, error) {
	c := w.capture
	c.mu.Lock()
	remaining := c.limit - c.total
	if remaining > int64(len(data)) {
		remaining = int64(len(data))
	}
	if remaining > 0 {
		if w.stdout {
			_, _ = c.stdout.Write(data[:remaining])
		} else {
			_, _ = c.stderr.Write(data[:remaining])
		}
		c.total += remaining
	}
	overflowNow := int64(len(data)) > remaining && !c.overflow
	if overflowNow {
		c.overflow = true
	}
	kill := c.kill
	c.mu.Unlock()
	if overflowNow && kill != nil {
		kill()
	}
	return len(data), nil
}

// overflowed 读取合并输出的超限状态。
func (c *outputCapture) overflowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.overflow
}

// values 返回受总预算约束的标准输出和标准错误。
func (c *outputCapture) values() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdout.String(), c.stderr.String()
}
