// Package config 读取并校验 FileSage 服务配置。
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultListenAddr     = "127.0.0.1:8080"
	defaultUploadMaxBytes = 20 << 30
)

type Config struct {
	RootDir        string
	DataDir        string
	WebDir         string
	ListenAddr     string
	AllowedHosts   []string
	UploadMaxBytes int64
	AgentModel     AgentModelConfig
}

// AgentModelConfig 仅在内存中保存模型连接参数。
type AgentModelConfig struct {
	BaseURL  string
	Model    string
	Provider string
	APIKey   string
}

func Load() (Config, error) {
	rootDir := strings.TrimSpace(os.Getenv("FILESAGE_ROOT"))
	dataDir := strings.TrimSpace(os.Getenv("FILESAGE_DATA_DIR"))
	webDir := strings.TrimSpace(os.Getenv("FILESAGE_WEB_DIR"))
	listenAddr := strings.TrimSpace(os.Getenv("FILESAGE_LISTEN_ADDR"))
	allowedHosts := os.Getenv("FILESAGE_ALLOWED_HOSTS")
	uploadMaxBytes, err := parseUploadMaxBytes(os.Getenv("FILESAGE_UPLOAD_MAX_BYTES"))
	if err != nil {
		return Config{}, err
	}
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	cfg, err := newConfig(rootDir, dataDir, webDir, listenAddr, allowedHosts)
	if err != nil {
		return Config{}, err
	}
	cfg.UploadMaxBytes = uploadMaxBytes
	cfg.AgentModel = AgentModelConfig{
		BaseURL:  strings.TrimSpace(os.Getenv("FILESAGE_AGENT_BASE_URL")),
		Model:    strings.TrimSpace(os.Getenv("FILESAGE_AGENT_MODEL")),
		Provider: strings.TrimSpace(os.Getenv("FILESAGE_AGENT_PROVIDER")),
		APIKey:   strings.TrimSpace(os.Getenv("FILESAGE_AGENT_API_KEY")),
	}
	return cfg, nil
}

func New(rootDir, dataDir, listenAddr string) (Config, error) {
	return newConfig(rootDir, dataDir, "", listenAddr, "")
}

// newConfig 校验服务目录、监听地址和信任主机配置。
func newConfig(rootDir, dataDir, webDir, listenAddr, allowedHostsValue string) (Config, error) {
	if rootDir == "" {
		return Config{}, fmt.Errorf("FILESAGE_ROOT is required")
	}
	if dataDir == "" {
		return Config{}, fmt.Errorf("FILESAGE_DATA_DIR is required and must be outside FILESAGE_ROOT")
	}
	if !filepath.IsAbs(rootDir) {
		return Config{}, fmt.Errorf("FILESAGE_ROOT must be an absolute path")
	}
	if !filepath.IsAbs(dataDir) {
		return Config{}, fmt.Errorf("FILESAGE_DATA_DIR must be an absolute path")
	}
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	if err := validateListenAddr(listenAddr); err != nil {
		return Config{}, err
	}
	allowedHosts, err := parseAllowedHosts(listenAddr, allowedHostsValue)
	if err != nil {
		return Config{}, err
	}

	rootPath, err := filepath.EvalSymlinks(filepath.Clean(rootDir))
	if err != nil {
		return Config{}, fmt.Errorf("resolve FILESAGE_ROOT: %w", err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return Config{}, fmt.Errorf("inspect FILESAGE_ROOT: %w", err)
	}
	if !rootInfo.IsDir() {
		return Config{}, fmt.Errorf("FILESAGE_ROOT must be an existing directory")
	}

	dataPath := filepath.Clean(dataDir)
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		return Config{}, fmt.Errorf("create FILESAGE_DATA_DIR: %w", err)
	}
	dataPath, err = filepath.EvalSymlinks(dataPath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve FILESAGE_DATA_DIR: %w", err)
	}
	if pathWithin(rootPath, dataPath) {
		return Config{}, fmt.Errorf("FILESAGE_DATA_DIR must be outside FILESAGE_ROOT")
	}
	webPath, err := resolveWebDir(webDir, rootPath, dataPath)
	if err != nil {
		return Config{}, err
	}

	return Config{
		RootDir: rootPath, DataDir: dataPath, WebDir: webPath, ListenAddr: listenAddr,
		AllowedHosts: allowedHosts, UploadMaxBytes: defaultUploadMaxBytes,
	}, nil
}

// parseUploadMaxBytes 解析单文件上传上限；零表示不限制。
func parseUploadMaxBytes(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return defaultUploadMaxBytes, nil
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("FILESAGE_UPLOAD_MAX_BYTES must be a non-negative integer")
	}
	return limit, nil
}

// resolveWebDir 解析可选构建目录，并避免将用户文件或服务状态公开为静态资源。
func resolveWebDir(webDir, rootPath, dataPath string) (string, error) {
	if webDir == "" {
		return "", nil
	}
	if !filepath.IsAbs(webDir) {
		return "", fmt.Errorf("FILESAGE_WEB_DIR must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(webDir))
	if err != nil {
		return "", fmt.Errorf("resolve FILESAGE_WEB_DIR: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect FILESAGE_WEB_DIR: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("FILESAGE_WEB_DIR must be an existing directory")
	}
	if pathsOverlap(rootPath, resolved) {
		return "", fmt.Errorf("FILESAGE_WEB_DIR must be separate from FILESAGE_ROOT")
	}
	if pathsOverlap(dataPath, resolved) {
		return "", fmt.Errorf("FILESAGE_WEB_DIR must be separate from FILESAGE_DATA_DIR")
	}
	return resolved, nil
}

// pathsOverlap 检查两个目录是否相同或互相包含。
func pathsOverlap(first, second string) bool {
	return pathWithin(first, second) || pathWithin(second, first)
}

// parseAllowedHosts 生成默认主机并合并显式配置。
func parseAllowedHosts(listenAddr, value string) ([]string, error) {
	listenHost, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, fmt.Errorf("FILESAGE_LISTEN_ADDR must be valid before deriving allowed hosts: %w", err)
	}
	defaults := []string{listenHost, "localhost", "127.0.0.1", "::1"}
	allowed := make([]string, 0, len(defaults)+4)
	seen := make(map[string]struct{}, len(defaults)+4)
	add := func(host string) error {
		normalized, err := normalizeConfiguredHost(host)
		if err != nil {
			return err
		}
		if _, exists := seen[normalized]; !exists {
			allowed = append(allowed, normalized)
			seen[normalized] = struct{}{}
		}
		return nil
	}
	for _, host := range defaults {
		if err := add(host); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(value) == "" {
		return allowed, nil
	}
	for _, item := range strings.Split(value, ",") {
		host := strings.TrimSpace(item)
		if host == "" {
			return nil, fmt.Errorf("FILESAGE_ALLOWED_HOSTS must contain comma-separated hostnames or IP addresses")
		}
		if err := add(host); err != nil {
			return nil, fmt.Errorf("invalid FILESAGE_ALLOWED_HOSTS entry %q: %w", host, err)
		}
	}
	return allowed, nil
}

// normalizeConfiguredHost 规范化单个主机名或 IP。
func normalizeConfiguredHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "[]/\\@?#") {
		return "", fmt.Errorf("expected a hostname or IP address without a port")
	}
	if strings.Contains(host, ":") {
		address, zone, hasZone := strings.Cut(host, "%")
		if hasZone && zone == "" {
			return "", fmt.Errorf("invalid IPv6 zone")
		}
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("invalid IP address")
		}
		return ip.String(), nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) == 0 || len(host) > 253 {
		return "", fmt.Errorf("invalid hostname length")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid hostname label")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", fmt.Errorf("invalid hostname character")
			}
		}
	}
	return host, nil
}

func validateListenAddr(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("FILESAGE_LISTEN_ADDR must be an explicit IP:port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("FILESAGE_LISTEN_ADDR port must be between 1 and 65535")
	}
	ipHost, zone, hasZone := strings.Cut(host, "%")
	ip := net.ParseIP(ipHost)
	if ip == nil {
		return fmt.Errorf("FILESAGE_LISTEN_ADDR host must be an explicit IP address")
	}
	if hasZone && (zone == "" || !ip.IsLinkLocalUnicast()) {
		return fmt.Errorf("FILESAGE_LISTEN_ADDR may use an interface zone only for a link-local address")
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = net.IP(ipv4)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("FILESAGE_LISTEN_ADDR cannot bind an unspecified address")
	}
	if !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
		return fmt.Errorf("FILESAGE_LISTEN_ADDR must use a loopback, private, or link-local address")
	}
	return nil
}

func pathWithin(rootPath, candidate string) bool {
	rel, err := filepath.Rel(rootPath, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
