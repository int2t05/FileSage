// 本文件验证子进程最小环境和 Linux 启动参数。
package safecmd

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestCommandContextUsesMinimalEnvironment 验证服务密钥不会传给子进程。
func TestCommandContextUsesMinimalEnvironment(t *testing.T) {
	t.Setenv("FILESAGE_AGENT_API_KEY", "test-secret")
	command, err := CommandContext(context.Background(), "/usr/bin/parser", "--version")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(command.Env, "\n")
	if strings.Contains(got, "FILESAGE_AGENT_API_KEY") || strings.Contains(got, "test-secret") {
		t.Fatal("child environment contains the parent API key")
	}
	for _, want := range []string{"HOME=/nonexistent", "LC_ALL=C.UTF-8"} {
		if !strings.Contains(got, want) {
			t.Errorf("child environment missing %q: %q", want, command.Env)
		}
	}
	if !strings.Contains(got, "PATH=") {
		t.Fatalf("child environment has no PATH: %q", command.Env)
	}
	if runtime.GOOS == "linux" {
		if !strings.Contains(got, "PATH=/usr/bin:/bin") {
			t.Errorf("Linux child PATH is not restricted: %q", command.Env)
		}
		if !strings.HasSuffix(command.Path, "/setpriv") {
			t.Fatalf("Linux launcher = %q, want setpriv", command.Path)
		}
		if len(command.Args) < 4 || command.Args[1] != "--ambient-caps=-all" || command.Args[2] != "--" || command.Args[3] != "/usr/bin/parser" {
			t.Fatalf("Linux command arguments = %#v", command.Args)
		}
	}
	if os.Getenv("FILESAGE_AGENT_API_KEY") != "test-secret" {
		t.Fatal("test did not set the parent API key")
	}
}
