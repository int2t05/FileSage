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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const staleOfficeDirectoryAge = 24 * time.Hour

// 每个进程限制 2 GiB 虚拟地址空间；复杂 Office 样本容量尚未基准测试。
const officeAddressSpaceLimit int64 = 2 << 30

// privateOfficeTempRoot 创建只允许当前服务账户访问的临时根目录。
func privateOfficeTempRoot() (string, error) {
	parent := "/tmp"
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: private temporary parent is unavailable", ErrUnavailable)
	}
	root := filepath.Join(parent, fmt.Sprintf("filesage-office-%d", os.Geteuid()))
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create private office temporary root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return "", fmt.Errorf("%w: private temporary root has unsafe ownership or permissions", ErrUnavailable)
	}
	if err := cleanupStaleOfficeDirs(root, time.Now()); err != nil {
		return "", fmt.Errorf("clean stale office temporary directories: %w", err)
	}
	return root, nil
}

// cleanupStaleOfficeDirs 只清理由本账户创建且超过保留期的任务目录。
func cleanupStaleOfficeDirs(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	cutoff := now.Add(-staleOfficeDirectoryAge)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		name := filepath.Join(root, entry.Name())
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(name); err != nil {
			return fmt.Errorf("remove stale directory %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// ownedByCurrentUser 确认临时目录属于当前服务账户。
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func discoverPaths() (executionPaths, error) {
	libreOffice, err := exec.LookPath("libreoffice")
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: libreoffice was not found", ErrUnavailable)
	}
	libreOffice, err = filepath.EvalSymlinks(libreOffice)
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: libreoffice path is invalid", ErrUnavailable)
	}
	programDir := filepath.Dir(libreOffice)
	installDir := filepath.Dir(programDir)
	if filepath.Base(programDir) != "program" || filepath.Base(installDir) != "libreoffice" || !allowedSystemPath(installDir) {
		return executionPaths{}, fmt.Errorf("%w: LibreOffice must be installed in a private system directory", ErrUnavailable)
	}
	oosplash := filepath.Join(programDir, "oosplash")
	info, err := os.Stat(oosplash)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return executionPaths{}, fmt.Errorf("%w: LibreOffice launcher was not found", ErrUnavailable)
	}
	sofficeBinary := filepath.Join(programDir, "soffice.bin")
	info, err = os.Stat(sofficeBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return executionPaths{}, fmt.Errorf("%w: LibreOffice process was not found", ErrUnavailable)
	}

	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: bubblewrap was not found", ErrUnavailable)
	}
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: prlimit was not found", ErrUnavailable)
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: setpriv was not found", ErrUnavailable)
	}
	bwrap, err = filepath.EvalSymlinks(bwrap)
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: bubblewrap path is invalid", ErrUnavailable)
	}
	prlimit, err = filepath.EvalSymlinks(prlimit)
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: prlimit path is invalid", ErrUnavailable)
	}
	setpriv, err = filepath.EvalSymlinks(setpriv)
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: setpriv path is invalid", ErrUnavailable)
	}

	paths := executionPaths{
		oosplash: oosplash, programDir: programDir, bwrap: bwrap, prlimit: prlimit, setpriv: setpriv,
	}
	if err := addReadOnlyDir(&paths, installDir, installDir); err != nil {
		return executionPaths{}, err
	}
	if err := addReadOnlyDirIfPresent(&paths, "/usr/share/libreoffice"); err != nil {
		return executionPaths{}, err
	}
	if err := addReadOnlyDirIfPresent(&paths, "/etc/libreoffice/registry"); err != nil {
		return executionPaths{}, err
	}
	if err := addReadOnlyDirIfPresent(&paths, "/etc/fonts"); err != nil {
		return executionPaths{}, err
	}
	if err := addReadOnlyDirIfPresent(&paths, "/usr/share/fonts"); err != nil {
		return executionPaths{}, err
	}
	if err := addReadOnlyDirIfPresent(&paths, "/usr/share/fontconfig"); err != nil {
		return executionPaths{}, err
	}
	triplet, loader := systemLibraryLayout(runtime.GOARCH)
	if triplet == "" {
		return executionPaths{}, fmt.Errorf("%w: unsupported Linux architecture", ErrUnavailable)
	}
	for _, prefix := range []string{"/lib", "/usr/lib"} {
		if err := addReadOnlyDirIfPresent(&paths, filepath.Join(prefix, triplet)); err != nil {
			return executionPaths{}, err
		}
	}
	loaderSource, err := filepath.EvalSymlinks(loader)
	if err != nil {
		return executionPaths{}, fmt.Errorf("%w: system loader was not found", ErrUnavailable)
	}
	if err := addReadOnlyFile(&paths, loaderSource, loader); err != nil {
		return executionPaths{}, err
	}
	paths.loaderPath = strings.Join([]string{
		programDir,
		filepath.Join("/lib", triplet),
		filepath.Join("/usr/lib", triplet),
	}, ":")
	return paths, nil
}

func systemLibraryLayout(architecture string) (string, string) {
	switch architecture {
	case "amd64":
		return "x86_64-linux-gnu", "/lib64/ld-linux-x86-64.so.2"
	case "arm64":
		return "aarch64-linux-gnu", "/lib/ld-linux-aarch64.so.1"
	case "arm":
		return "arm-linux-gnueabihf", "/lib/ld-linux-armhf.so.3"
	case "386":
		return "i386-linux-gnu", "/lib/ld-linux.so.2"
	default:
		return "", ""
	}
}

func addReadOnlyDirIfPresent(paths *executionPaths, directory string) error {
	info, err := os.Stat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect office dependency directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: office dependency is not a directory", ErrUnavailable)
	}
	return addReadOnlyDir(paths, directory, directory)
}

func addReadOnlyDir(paths *executionPaths, source, target string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("%w: office dependency directory is unavailable", ErrUnavailable)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() || !filepath.IsAbs(target) {
		return fmt.Errorf("%w: office dependency directory is invalid", ErrUnavailable)
	}
	for _, mount := range paths.readOnlyDir {
		if mount.target == target {
			return nil
		}
	}
	paths.readOnlyDir = append(paths.readOnlyDir, bindMount{source: resolved, target: target})
	return nil
}

func addReadOnlyFile(paths *executionPaths, source, target string) error {
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(target) {
		return fmt.Errorf("%w: office dependency file is invalid", ErrUnavailable)
	}
	paths.readOnlyFile = append(paths.readOnlyFile, bindMount{source: source, target: target})
	return nil
}

func runConverter(ctx context.Context, paths executionPaths, workDir string, format officeFormatInfo) error {
	args, err := sandboxArgs(paths, workDir, format)
	if err != nil {
		return err
	}
	if err := runSandbox(ctx, paths, args); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(workDir, "out", "source.pdf")); err != nil {
		return fmt.Errorf("sandboxed LibreOffice produced no PDF")
	}
	return nil
}

// runSandbox 通过资源限制器启动 Bubblewrap，并收集有限长度的错误输出。
func runSandbox(ctx context.Context, paths executionPaths, args []string) error {
	commandArgs := []string{
		"--as=" + strconv.FormatInt(officeAddressSpaceLimit, 10), "--cpu=45", "--fsize=" + strconv.FormatInt(MaxOutputBytes, 10),
		"--nofile=128", "--nproc=512", "--", paths.setpriv, "--ambient-caps=-all", "--", paths.bwrap,
	}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, paths.prlimit, commandArgs...)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	stderr := &limitedBuffer{limit: 8 << 10}
	command.Stdout = stderr
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("sandboxed LibreOffice exited: %w: %s", err, strings.TrimSpace(stderr.buffer.String()))
	}
	return nil
}

func sandboxArgs(paths executionPaths, workDir string, format officeFormatInfo) ([]string, error) {
	return sandboxArgsWithCommand(paths, workDir, []string{
		paths.oosplash,
		"--headless", "--nodefault", "--nologo", "--nolockcheck", "--norestore",
		"-env:UserInstallation=file:///work/profile",
		"--convert-to", "pdf:" + format.export,
		"--outdir", "/work/out", "/work/" + format.inputName,
	})
}

// sandboxArgsWithCommand 为固定可执行文件构造无网络、最小只读挂载参数。
func sandboxArgsWithCommand(paths executionPaths, workDir string, command []string) ([]string, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, fmt.Errorf("%w: sandbox command is empty", ErrUnavailable)
	}
	args := []string{
		"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session",
		"--cap-drop", "ALL", "--tmpfs", "/",
	}
	targetDirs := make(map[string]struct{})
	for _, mount := range paths.readOnlyDir {
		if !allowedSystemPath(mount.target) || !filepath.IsAbs(mount.source) {
			return nil, fmt.Errorf("%w: invalid read-only dependency mount", ErrUnavailable)
		}
		collectTargetDirs(targetDirs, mount.target)
	}
	for _, mount := range paths.readOnlyFile {
		if !allowedSystemPath(mount.target) || !filepath.IsAbs(mount.source) {
			return nil, fmt.Errorf("%w: invalid read-only dependency mount", ErrUnavailable)
		}
		collectTargetDirs(targetDirs, filepath.Dir(mount.target))
	}
	for _, directory := range orderedTargetDirs(targetDirs) {
		args = append(args, "--dir", directory)
	}
	for _, mount := range paths.readOnlyDir {
		args = append(args, "--ro-bind", mount.source, mount.target)
	}
	for _, mount := range paths.readOnlyFile {
		args = append(args, "--ro-bind", mount.source, mount.target)
	}
	args = append(args,
		"--dev", "/dev", "--proc", "/proc",
		"--dir", "/tmp", "--tmpfs", "/tmp",
		"--dir", "/home", "--dir", "/work",
		"--bind", workDir, "/work", "--chdir", "/work",
		"--clearenv",
		"--setenv", "HOME", "/work/home",
		"--setenv", "TMPDIR", "/work/tmp",
		"--setenv", "XDG_CACHE_HOME", "/work/cache",
		"--setenv", "XDG_CONFIG_HOME", "/work/config",
		"--setenv", "XDG_DATA_HOME", "/work/data",
		"--setenv", "XDG_RUNTIME_DIR", "/work/runtime",
		"--setenv", "URE_BOOTSTRAP", "file://"+filepath.Join(paths.programDir, "fundamentalrc"),
		"--setenv", "UNO_PATH", paths.programDir,
		"--setenv", "LD_LIBRARY_PATH", paths.loaderPath,
		"--setenv", "SAL_ENABLE_FILE_LOCKING", "1",
		"--setenv", "LC_ALL", "C",
		"--setenv", "LANG", "C",
		"--",
	)
	args = append(args, command...)
	return args, nil
}

func allowedSystemPath(target string) bool {
	clean := filepath.Clean(target)
	for _, prefix := range []string{"/usr/lib", "/usr/share", "/lib", "/lib64", "/etc"} {
		if clean == prefix || strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func collectTargetDirs(result map[string]struct{}, directory string) {
	directory = filepath.Clean(directory)
	for directory != "/" && directory != "." {
		result[directory] = struct{}{}
		directory = filepath.Dir(directory)
	}
}

func orderedTargetDirs(directories map[string]struct{}) []string {
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && pathDepth(result[j]) < pathDepth(result[j-1]); j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

func pathDepth(value string) int {
	return strings.Count(filepath.Clean(value), string(filepath.Separator))
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if remaining := b.limit - b.buffer.Len(); remaining > 0 {
		if len(data) > remaining {
			_, _ = b.buffer.Write(data[:remaining])
		} else {
			_, _ = b.buffer.Write(data)
		}
	}
	return len(data), nil
}
