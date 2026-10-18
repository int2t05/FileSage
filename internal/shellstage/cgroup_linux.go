//go:build linux

// cgroup_linux.go 为 Shell 子进程设置内核级聚合资源边界。
package shellstage

import (
	"crypto/rand"
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
	controlCgroupName = ".filesage-control"
	shellCgroupPrefix = "filesage-shell-"
	cpuQuotaMicros    = 100_000
	cpuPeriodMicros   = 100_000
)

type cgroupManager struct {
	dir  *os.File
	path string
}

type sandboxCgroup struct {
	dir           *os.File
	path          string
	oomKills      uint64
	oomGroupKills uint64
	cpuInitial    uint64
	cpuBudget     uint64
}

var cgroupState struct {
	once    sync.Once
	manager *cgroupManager
	err     error
}

// delegatedCgroup 返回启用所需控制器的服务委托目录。
func delegatedCgroup() (*cgroupManager, error) {
	cgroupState.once.Do(func() {
		cgroupState.manager, cgroupState.err = prepareDelegatedCgroup()
	})
	return cgroupState.manager, cgroupState.err
}

// prepareDelegatedCgroup 将服务进程移入叶组，释放委托根供任务组使用。
func prepareDelegatedCgroup() (*cgroupManager, error) {
	current, err := currentCgroupDirectory()
	if err != nil {
		return nil, err
	}
	basePath, controlPath := current, filepath.Join(current, controlCgroupName)
	if filepath.Base(current) == controlCgroupName {
		basePath = filepath.Dir(current)
		controlPath = current
	}
	base, err := openCgroupDirectory(basePath)
	if err != nil {
		return nil, err
	}
	keepBase := false
	defer func() {
		if !keepBase {
			_ = base.Close()
		}
	}()
	if err := requireControllers(base); err != nil {
		return nil, err
	}
	if err := removeEmptyStaleGroups(base); err != nil {
		return nil, err
	}
	control, err := openCgroupDirectory(controlPath)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(controlPath, 0o700); err != nil {
			return nil, unavailableCgroup("create service control group", err)
		}
		created = true
		control, err = openCgroupDirectory(controlPath)
	}
	if err != nil {
		return nil, err
	}
	defer control.Close()
	pid := strconv.Itoa(os.Getpid())
	currentPIDs, err := readCgroupPIDs(control)
	if err != nil {
		return nil, unavailableCgroup("read service control group", err)
	}
	if filepath.Base(current) == controlCgroupName {
		if len(currentPIDs) != 1 || currentPIDs[0] != pid {
			return nil, unavailableCgroup("service control group contains unexpected processes", nil)
		}
	} else {
		basePIDs, err := readCgroupPIDs(base)
		if err != nil {
			return nil, unavailableCgroup("read delegated group", err)
		}
		if len(basePIDs) != 1 || basePIDs[0] != pid || len(currentPIDs) != 0 {
			return nil, unavailableCgroup("delegated group must contain only the service process", nil)
		}
		if err := writeCgroupFile(control, "cgroup.procs", pid); err != nil {
			return nil, unavailableCgroup("move service into control group", err)
		}
		if err := enableControllers(base); err != nil {
			_ = writeCgroupFile(base, "cgroup.procs", pid)
			if created {
				_ = os.Remove(controlPath)
			}
			return nil, err
		}
	}
	if err := requireEnabledControllers(base); err != nil {
		return nil, err
	}
	basePIDs, err := readCgroupPIDs(base)
	if err != nil || len(basePIDs) != 0 {
		return nil, unavailableCgroup("delegated group must be empty after service isolation", err)
	}
	keepBase = true
	return &cgroupManager{dir: base, path: basePath}, nil
}

// createSandboxCgroup 在启动命令前设置内核硬上限。
func (m *cgroupManager) createSandboxCgroup(limits Limits) (*sandboxCgroup, error) {
	var name string
	var dir *os.File
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		suffix, err := randomCgroupSuffix()
		if err != nil {
			return nil, unavailableCgroup("generate command cgroup name", err)
		}
		name = shellCgroupPrefix + suffix
		path := filepath.Join(m.path, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, unavailableCgroup("create command cgroup", err)
		}
		dir, err = openCgroupDirectory(path)
		if err != nil {
			_ = os.Remove(path)
			return nil, err
		}
		break
	}
	if dir == nil {
		return nil, unavailableCgroup("allocate unique command cgroup", nil)
	}
	path := filepath.Join(m.path, name)
	group := &sandboxCgroup{dir: dir, path: path, cpuBudget: limits.CPUSeconds * 1_000_000}
	if err := checkCgroupFileWritable(dir, "cgroup.kill"); err != nil {
		_ = group.close()
		return nil, unavailableCgroup("verify command cleanup support", err)
	}
	// 给沙箱启动进程和命令解释器保留固定额度。
	pidsMax := limits.Processes + processReserve
	settings := []struct{ file, value string }{
		{"cpu.max", fmt.Sprintf("%d %d", cpuQuotaMicros, cpuPeriodMicros)},
		{"memory.max", strconv.FormatUint(limits.MemoryBytes, 10)},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
		{"pids.max", strconv.FormatUint(pidsMax, 10)},
	}
	for _, setting := range settings {
		if err := writeCgroupFile(dir, setting.file, setting.value); err != nil {
			_ = group.close()
			return nil, unavailableCgroup("set "+setting.file, err)
		}
	}
	group.oomKills, err = readCgroupEvent(dir, "memory.events", "oom_kill")
	if err != nil {
		_ = group.close()
		return nil, unavailableCgroup("read memory events", err)
	}
	group.oomGroupKills, err = readCgroupEvent(dir, "memory.events", "oom_group_kill")
	if err != nil {
		_ = group.close()
		return nil, unavailableCgroup("read grouped memory events", err)
	}
	group.cpuInitial, err = readCgroupEvent(dir, "cpu.stat", "usage_usec")
	if err != nil {
		_ = group.close()
		return nil, unavailableCgroup("read aggregate CPU baseline", err)
	}
	return group, nil
}

// attach 通过 clone3 原子放入任务组。
func (g *sandboxCgroup) attach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:     true,
		UseCgroupFD: true,
		CgroupFD:    int(g.dir.Fd()),
	}
}

// cpuLimitExceeded 汇总任务组内所有进程的 CPU 用量。
func (g *sandboxCgroup) cpuLimitExceeded() (bool, error) {
	usage, err := readCgroupEvent(g.dir, "cpu.stat", "usage_usec")
	if err != nil {
		return false, err
	}
	if usage < g.cpuInitial {
		return false, fmt.Errorf("aggregate CPU counter moved backwards")
	}
	return usage-g.cpuInitial >= g.cpuBudget, nil
}

// memoryLimitExceeded 检查聚合内存上限是否触发 OOM。
func (g *sandboxCgroup) memoryLimitExceeded() (bool, error) {
	current, err := readCgroupEvent(g.dir, "memory.events", "oom_kill")
	if err != nil {
		return false, err
	}
	groupKills, err := readCgroupEvent(g.dir, "memory.events", "oom_group_kill")
	if err != nil {
		return false, err
	}
	return current > g.oomKills || groupKills > g.oomGroupKills, nil
}

// close 终止残余进程，并只移除空任务组。
func (g *sandboxCgroup) close() error {
	if g.dir == nil {
		return nil
	}
	var result error
	if err := writeCgroupFile(g.dir, "cgroup.kill", "1"); err != nil {
		result = errors.Join(result, unavailableCgroup("kill remaining command processes", err))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pids, err := readCgroupPIDs(g.dir)
		if err != nil {
			result = errors.Join(result, unavailableCgroup("verify command cgroup cleanup", err))
			break
		}
		if len(pids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			result = errors.Join(result, unavailableCgroup("command processes remained after cgroup kill", nil))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(readPIDsIgnoringError(g.dir)) == 0 {
		if err := os.Remove(g.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, unavailableCgroup("remove command cgroup", err))
		}
	}
	if err := g.dir.Close(); err != nil {
		result = errors.Join(result, err)
	}
	g.dir = nil
	return result
}

// currentCgroupDirectory 映射进程的 v2 组路径到挂载点。
func currentCgroupDirectory() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", unavailableCgroup("read process cgroup", err)
	}
	var relative string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			relative = filepath.Clean(parts[2])
			break
		}
	}
	if relative == "" || !filepath.IsAbs(relative) {
		return "", unavailableCgroup("unified cgroup v2 membership is unavailable", nil)
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", unavailableCgroup("read cgroup mount information", err)
	}
	for _, line := range strings.Split(string(mounts), "\n") {
		sections := strings.SplitN(line, " - ", 2)
		if len(sections) != 2 {
			continue
		}
		before, after := strings.Fields(sections[0]), strings.Fields(sections[1])
		if len(before) < 5 || len(after) < 1 || after[0] != "cgroup2" {
			continue
		}
		root, mount := unescapeMountPath(before[3]), unescapeMountPath(before[4])
		rel, err := filepath.Rel(root, relative)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		return filepath.Join(mount, rel), nil
	}
	return "", unavailableCgroup("cgroup v2 mount is unavailable", nil)
}

// openCgroupDirectory 不跟随末段符号链接打开组目录。
func openCgroupDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, unavailableCgroup("open delegated cgroup", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// requireControllers 检查服务是否收到所需控制器。
func requireControllers(dir *os.File) error {
	data, err := readCgroupFile(dir, "cgroup.controllers")
	if err != nil {
		return unavailableCgroup("read delegated controllers", err)
	}
	available := stringSet(strings.Fields(data))
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !available[controller] {
			return unavailableCgroup("delegated controller is missing: "+controller, nil)
		}
	}
	return nil
}

// enableControllers 在服务移出委托根后启用所需控制器。
func enableControllers(dir *os.File) error {
	enabled, err := readCgroupFile(dir, "cgroup.subtree_control")
	if err != nil {
		return unavailableCgroup("read delegated controller state", err)
	}
	active := stringSet(strings.Fields(enabled))
	var missing []string
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !active[controller] {
			missing = append(missing, "+"+controller)
		}
	}
	if len(missing) > 0 {
		if err := writeCgroupFile(dir, "cgroup.subtree_control", strings.Join(missing, " ")); err != nil {
			return unavailableCgroup("enable delegated controllers", err)
		}
	}
	return nil
}

// requireEnabledControllers 复核委托控制器仍处于启用状态。
func requireEnabledControllers(dir *os.File) error {
	data, err := readCgroupFile(dir, "cgroup.subtree_control")
	if err != nil {
		return unavailableCgroup("verify delegated controllers", err)
	}
	active := stringSet(strings.Fields(data))
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !active[controller] {
			return unavailableCgroup("delegated controller is not enabled: "+controller, nil)
		}
	}
	return nil
}

// removeEmptyStaleGroups 拒绝未知子组并清理空任务组。
func removeEmptyStaleGroups(base *os.File) error {
	entries, err := os.ReadDir(base.Name())
	if err != nil {
		return unavailableCgroup("inspect delegated child groups", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		if name == controlCgroupName {
			continue
		}
		if !strings.HasPrefix(name, shellCgroupPrefix) {
			return unavailableCgroup("unexpected child in delegated group: "+name, nil)
		}
		child, err := openCgroupDirectory(filepath.Join(base.Name(), name))
		if err != nil {
			return err
		}
		pids, readErr := readCgroupPIDs(child)
		_ = child.Close()
		if readErr != nil || len(pids) != 0 {
			return unavailableCgroup("stale command cgroup is still populated: "+name, readErr)
		}
		if err := os.Remove(filepath.Join(base.Name(), name)); err != nil {
			return unavailableCgroup("remove stale command cgroup", err)
		}
	}
	return nil
}

// readCgroupPIDs 读取当前组中的进程 ID。
func readCgroupPIDs(dir *os.File) ([]string, error) {
	data, err := readCgroupFile(dir, "cgroup.procs")
	if err != nil {
		return nil, err
	}
	var pids []string
	for _, value := range strings.Fields(data) {
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid cgroup process id")
		}
		pids = append(pids, value)
	}
	return pids, nil
}

// readPIDsIgnoringError 清理阶段尽力读取进程列表。
func readPIDsIgnoringError(dir *os.File) []string {
	pids, _ := readCgroupPIDs(dir)
	return pids
}

// readCgroupEvent 读取资源统计文件中的指定数值。
func readCgroupEvent(dir *os.File, file, key string) (uint64, error) {
	data, err := readCgroupFile(dir, file)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != key {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return value, nil
	}
	return 0, fmt.Errorf("cgroup metric %s is missing from %s", key, file)
}

// readCgroupFile 相对已打开的组目录读取控制文件。
func readCgroupFile(dir *os.File, name string) (string, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// writeCgroupFile 相对已打开的组目录写入控制值。
func writeCgroupFile(dir *os.File, name, value string) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	data := []byte(value)
	n, err := unix.Write(fd, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write to cgroup control file")
	}
	return nil
}

// checkCgroupFileWritable 验证控制文件可写但不触发其操作。
func checkCgroupFileWritable(dir *os.File, name string) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// unavailableCgroup 标记委托资源控制能力不可用。
func unavailableCgroup(action string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, action)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, action, err)
}

// stringSet 将控制器名称转换为集合。
func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// unescapeMountPath 解码 mountinfo 中的路径转义。
func unescapeMountPath(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

// randomCgroupSuffix 生成随机任务组名后缀。
func randomCgroupSuffix() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
}
