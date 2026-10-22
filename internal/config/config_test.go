// 本文件验证配置默认值、路径边界和环境变量解析。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRequiresSeparateAbsoluteDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	cfg, err := New(root, filepath.Join(outside, "state"), "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.UploadMaxBytes != defaultUploadMaxBytes {
		t.Fatalf("UploadMaxBytes = %d, want %d", cfg.UploadMaxBytes, defaultUploadMaxBytes)
	}
	if cfg.UploadStagingMaxBytes != defaultUploadStagingMaxBytes {
		t.Fatalf("UploadStagingMaxBytes = %d, want %d", cfg.UploadStagingMaxBytes, defaultUploadStagingMaxBytes)
	}
	if got, want := strings.Join(cfg.AllowedHosts, ","), "127.0.0.1,localhost,::1"; got != want {
		t.Fatalf("AllowedHosts = %q, want %q", got, want)
	}
	if _, err := os.Stat(cfg.DataDir); err != nil {
		t.Fatalf("data directory was not created: %v", err)
	}

	if _, err := New(root, root, "127.0.0.1:8080"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("New(root as data dir) error = %v, want outside-root error", err)
	}
	if _, err := New("relative-root", outside, "127.0.0.1:8080"); err == nil {
		t.Fatal("New(relative root) succeeded")
	}
	if _, err := New(root, outside, "0.0.0.0:8080"); err == nil {
		t.Fatal("New(wildcard listener) succeeded")
	}
}

func TestDefaultListenAddressBuildsDefaultAllowedHosts(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	cfg, err := New(root, filepath.Join(state, "data"), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if got, want := strings.Join(cfg.AllowedHosts, ","), "127.0.0.1,localhost,::1"; got != want {
		t.Fatalf("AllowedHosts = %q, want %q", got, want)
	}
}

func TestLoadAddsConfiguredAllowedHosts(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	t.Setenv("FILESAGE_WEB_DIR", "")
	t.Setenv("FILESAGE_ROOT", root)
	t.Setenv("FILESAGE_DATA_DIR", filepath.Join(state, "data"))
	t.Setenv("FILESAGE_LISTEN_ADDR", "10.77.88.99:8080")
	t.Setenv("FILESAGE_ALLOWED_HOSTS", " FILESAGE.LAN. , 192.168.2.50 ")
	t.Setenv("FILESAGE_AGENT_BASE_URL", " http://127.0.0.1:11434/v1 ")
	t.Setenv("FILESAGE_AGENT_MODEL", " qwen3:8b ")
	t.Setenv("FILESAGE_AGENT_PROVIDER", " ollama ")
	t.Setenv("FILESAGE_AGENT_API_KEY", " local-secret ")
	t.Setenv("FILESAGE_UPLOAD_MAX_BYTES", "")
	t.Setenv("FILESAGE_UPLOAD_STAGING_MAX_BYTES", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(cfg.AllowedHosts, ","), "10.77.88.99,localhost,127.0.0.1,::1,filesage.lan,192.168.2.50"; got != want {
		t.Fatalf("AllowedHosts = %q, want %q", got, want)
	}
	if cfg.AgentModel.BaseURL != "http://127.0.0.1:11434/v1" || cfg.AgentModel.Model != "qwen3:8b" || cfg.AgentModel.Provider != "ollama" || cfg.AgentModel.APIKey != "local-secret" {
		t.Fatalf("AgentModel = %#v", cfg.AgentModel)
	}
}

func TestLoadConfiguresUploadSizeLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  int64
	}{
		{name: "default", value: "", want: defaultUploadMaxBytes},
		{name: "custom", value: "1048576", want: 1 << 20},
		{name: "unlimited", value: "0", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			state := t.TempDir()
			t.Setenv("FILESAGE_ROOT", root)
			t.Setenv("FILESAGE_DATA_DIR", filepath.Join(state, "data"))
			t.Setenv("FILESAGE_WEB_DIR", "")
			t.Setenv("FILESAGE_LISTEN_ADDR", "127.0.0.1:8080")
			t.Setenv("FILESAGE_ALLOWED_HOSTS", "")
			t.Setenv("FILESAGE_UPLOAD_MAX_BYTES", test.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.UploadMaxBytes != test.want {
				t.Fatalf("UploadMaxBytes = %d, want %d", cfg.UploadMaxBytes, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid "+value, func(t *testing.T) {
			t.Setenv("FILESAGE_UPLOAD_MAX_BYTES", value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted FILESAGE_UPLOAD_MAX_BYTES=%q", value)
			}
		})
	}
}

func TestLoadConfiguresUploadStagingLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  int64
	}{
		{name: "default", value: "", want: defaultUploadStagingMaxBytes},
		{name: "custom", value: "1048576", want: 1 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			state := t.TempDir()
			t.Setenv("FILESAGE_ROOT", root)
			t.Setenv("FILESAGE_DATA_DIR", filepath.Join(state, "data"))
			t.Setenv("FILESAGE_WEB_DIR", "")
			t.Setenv("FILESAGE_LISTEN_ADDR", "127.0.0.1:8080")
			t.Setenv("FILESAGE_ALLOWED_HOSTS", "")
			t.Setenv("FILESAGE_UPLOAD_STAGING_MAX_BYTES", test.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.UploadStagingMaxBytes != test.want {
				t.Fatalf("UploadStagingMaxBytes = %d, want %d", cfg.UploadStagingMaxBytes, test.want)
			}
		})
	}

	for _, value := range []string{"0", "-1", "invalid"} {
		t.Run("invalid "+value, func(t *testing.T) {
			root := t.TempDir()
			state := t.TempDir()
			t.Setenv("FILESAGE_ROOT", root)
			t.Setenv("FILESAGE_DATA_DIR", filepath.Join(state, "data"))
			t.Setenv("FILESAGE_WEB_DIR", "")
			t.Setenv("FILESAGE_LISTEN_ADDR", "127.0.0.1:8080")
			t.Setenv("FILESAGE_ALLOWED_HOSTS", "")
			t.Setenv("FILESAGE_UPLOAD_STAGING_MAX_BYTES", value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted FILESAGE_UPLOAD_STAGING_MAX_BYTES=%q", value)
			}
		})
	}
}

// TestLoadAcceptsOptionalWebBuildDirectory 验证静态构建目录可由环境变量配置。
func TestLoadAcceptsOptionalWebBuildDirectory(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	web := t.TempDir()
	t.Setenv("FILESAGE_ROOT", root)
	t.Setenv("FILESAGE_DATA_DIR", filepath.Join(state, "data"))
	t.Setenv("FILESAGE_WEB_DIR", web)
	t.Setenv("FILESAGE_LISTEN_ADDR", "127.0.0.1:8080")
	t.Setenv("FILESAGE_ALLOWED_HOSTS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDir != web {
		t.Fatalf("WebDir = %q, want %q", cfg.WebDir, web)
	}
}

// TestResolveWebDirRequiresAnIsolatedExistingDirectory 拒绝缺失、非目录和重叠路径。
func TestResolveWebDirRequiresAnIsolatedExistingDirectory(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	web := t.TempDir()
	file := filepath.Join(web, "index.html")
	if err := os.WriteFile(file, []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := resolveWebDir(web, root, state); err != nil || got != web {
		t.Fatalf("resolveWebDir(valid) = %q, %v", got, err)
	}
	for _, candidate := range []string{
		"relative/build",
		filepath.Join(web, "missing"),
		file,
		root,
		filepath.Dir(root),
		state,
		filepath.Dir(state),
	} {
		t.Run(candidate, func(t *testing.T) {
			if _, err := resolveWebDir(candidate, root, state); err == nil {
				t.Fatalf("resolveWebDir(%q) succeeded", candidate)
			}
		})
	}
}

func TestParseAllowedHostsRejectsInvalidEntries(t *testing.T) {
	for _, value := range []string{
		"filesage.lan:8080",
		"*.filesage.lan",
		"filesage.lan,,other.lan",
		"filesage.lan/",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseAllowedHosts("127.0.0.1:8080", value); err == nil {
				t.Fatalf("parseAllowedHosts(%q) succeeded", value)
			}
		})
	}
}

func TestListenAddrMustBeLocal(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:8080",
		"[::1]:8080",
		"10.77.88.99:8080",
		"10.0.0.1:8080",
		"[fd00::1]:8080",
		"169.254.10.20:8080",
		"[fe80::1%eth0]:8080",
	} {
		t.Run(address, func(t *testing.T) {
			if err := validateListenAddr(address); err != nil {
				t.Fatalf("validateListenAddr(%q) error = %v", address, err)
			}
		})
	}
	for _, address := range []string{
		"0.0.0.0:8080",
		"[::]:8080",
		"8.8.8.8:8080",
		"[2606:4700:4700::1111]:8080",
		"[2001:4860:4860::8888%eth0]:8080",
	} {
		t.Run(address, func(t *testing.T) {
			if err := validateListenAddr(address); err == nil {
				t.Fatalf("validateListenAddr(%q) succeeded for a non-local address", address)
			}
		})
	}
}
