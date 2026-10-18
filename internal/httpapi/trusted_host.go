// 本文件限制 HTTP 请求只能使用显式信任的主机名或 IP。
package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// NewTrustedHostHandler 在路由前校验请求 Host，不依赖端口匹配。
func NewTrustedHostHandler(next http.Handler, allowedHosts []string) (http.Handler, error) {
	if next == nil {
		return nil, fmt.Errorf("trusted host handler requires a next handler")
	}
	allowed := make(map[string]struct{}, len(allowedHosts))
	for _, value := range allowedHosts {
		host, err := normalizeHost(value)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed host %q: %w", value, err)
		}
		allowed[host] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("trusted host handler requires at least one allowed host")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, err := hostFromAuthority(r.Host)
		if err != nil {
			http.Error(w, "unrecognized host", http.StatusMisdirectedRequest)
			return
		}
		if _, ok := allowed[host]; !ok {
			http.Error(w, "unrecognized host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

// hostFromAuthority 提取 Host 中的主机部分并忽略有效端口。
func hostFromAuthority(authority string) (string, error) {
	if authority == "" || strings.TrimSpace(authority) != authority {
		return "", fmt.Errorf("invalid host authority")
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", fmt.Errorf("invalid bracketed host")
		}
		tail := authority[end+1:]
		if tail != "" {
			if !strings.HasPrefix(tail, ":") || !validHostPort(tail[1:]) {
				return "", fmt.Errorf("invalid host port")
			}
		}
		bracketHost := authority[1:end]
		address, _, _ := strings.Cut(bracketHost, "%")
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("brackets require an IPv6 address")
		}
		return normalizeHost(bracketHost)
	}
	if net.ParseIP(authority) != nil {
		return normalizeHost(authority)
	}
	if strings.Count(authority, ":") == 1 {
		host, port, err := net.SplitHostPort(authority)
		if err != nil || !validHostPort(port) {
			return "", fmt.Errorf("invalid host port")
		}
		return normalizeHost(host)
	}
	return normalizeHost(authority)
}

// validHostPort 检查 Host 中的可选十进制端口。
func validHostPort(port string) bool {
	if port == "" {
		return false
	}
	for _, char := range port {
		if char < '0' || char > '9' {
			return false
		}
	}
	number, err := strconv.Atoi(port)
	return err == nil && number <= 65535
}

// normalizeHost 统一主机名大小写、尾点和 IP 格式。
func normalizeHost(host string) (string, error) {
	if host == "" || strings.ContainsAny(host, "[]/\\@?#") {
		return "", fmt.Errorf("invalid hostname or IP")
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
