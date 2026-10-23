// Package safecmd 为不可信媒体与归档输入启动最小环境子进程。
package safecmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// CommandContext 清除服务密钥环境；Linux 子进程同时清除 ambient capabilities。
func CommandContext(ctx context.Context, binary string, args ...string) (*exec.Cmd, error) {
	path := binary
	commandArgs := args
	if runtime.GOOS == "linux" {
		setpriv, err := trustedSetpriv()
		if err != nil {
			return nil, fmt.Errorf("locate setpriv: %w", err)
		}
		path = setpriv
		commandArgs = append([]string{"--ambient-caps=-all", "--", binary}, args...)
	}

	command := exec.CommandContext(ctx, path, commandArgs...)
	command.Env = minimalEnvironment(binary)
	return command, nil
}

// minimalEnvironment 为子进程仅设置稳定的本地化与可执行搜索路径。
func minimalEnvironment(binary string) []string {
	environment := []string{"HOME=/nonexistent", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	switch runtime.GOOS {
	case "windows":
		searchPaths := []string{filepath.Dir(binary)}
		if root := os.Getenv("SystemRoot"); root != "" {
			environment = append(environment, "SystemRoot="+root, "WINDIR="+root)
			searchPaths = append(searchPaths, filepath.Join(root, "System32"))
		}
		environment = append(environment, "PATH="+strings.Join(searchPaths, string(os.PathListSeparator)))
	case "darwin":
		environment = append(environment, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=/tmp")
	default:
		environment = append(environment, "PATH=/usr/bin:/bin")
	}
	return environment
}

// trustedSetpriv 只接受系统目录内可执行的 util-linux setpriv。
func trustedSetpriv() (string, error) {
	found, err := exec.LookPath("setpriv")
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(found)
	if err != nil || !filepath.IsAbs(resolved) || !strings.HasPrefix(resolved, "/usr/") {
		return "", errors.New("setpriv path is invalid")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("setpriv executable is invalid")
	}
	return resolved, nil
}
