// Package origincheck 集中校验写请求的浏览器来源元数据。
package origincheck

import (
	"net/http"
	"net/url"
	"strings"
)

// AllowsMutation 拒绝跨站来源；无来源头的非浏览器客户端保持兼容。
func AllowsMutation(r *http.Request) bool {
	if r == nil {
		return false
	}
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	if fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return sameOrigin(origins[0], scheme, r.Host)
}

// sameOrigin 校验来源与当前请求的协议、主机和有效端口。
func sameOrigin(origin, requestScheme, requestHost string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery || strings.ContainsAny(origin, "?#") || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	requestScheme = strings.ToLower(requestScheme)
	if (scheme != "http" && scheme != "https") || scheme != requestScheme {
		return false
	}
	originHost, originPort, ok := normalizedAuthority(parsed.Host, scheme)
	if !ok {
		return false
	}
	requestHost, requestPort, ok := normalizedAuthority(requestHost, requestScheme)
	return ok && strings.EqualFold(originHost, requestHost) && originPort == requestPort
}

// normalizedAuthority 解析主机地址，并补齐协议默认端口。
func normalizedAuthority(authority, scheme string) (host, port string, ok bool) {
	parsed, err := url.Parse("//" + authority)
	if err != nil || parsed.Host != authority || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	host = strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", "", false
	}
	port = parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port, true
}
