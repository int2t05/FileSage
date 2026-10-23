package origincheck

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowsMutation(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		urlScheme string
		host      string
		origin    string
		fetchSite string
		second    string
		want      bool
	}{
		{name: "originless client", url: "http://filesage.test/api", want: true},
		{name: "same origin with default port", url: "http://filesage.test/api", host: "filesage.test:80", origin: "http://FILESAGE.test", fetchSite: "same-origin", want: true},
		{name: "same origin with root path", url: "http://filesage.test/api", origin: "http://filesage.test/", want: true},
		{name: "same origin without fetch metadata", url: "http://filesage.test/api", origin: "http://filesage.test", want: true},
		{name: "browser navigation metadata", url: "http://filesage.test/api", fetchSite: "none", want: true},
		{name: "cross-site fetch without origin", url: "http://filesage.test/api", fetchSite: "cross-site", want: false},
		{name: "same-site is not same-origin", url: "http://filesage.test/api", fetchSite: "same-site", want: false},
		{name: "different host", url: "http://filesage.test/api", origin: "http://attacker.test", want: false},
		{name: "different scheme", url: "http://filesage.test/api", origin: "https://filesage.test", want: false},
		{name: "different port", url: "http://filesage.test/api", origin: "http://filesage.test:8080", want: false},
		{name: "opaque null origin", url: "http://filesage.test/api", origin: "null", want: false},
		{name: "origin path", url: "http://filesage.test/api", origin: "http://filesage.test/other", want: false},
		{name: "origin query", url: "http://filesage.test/api", origin: "http://filesage.test?x=1", want: false},
		{name: "origin fragment", url: "http://filesage.test/api", origin: "http://filesage.test#section", want: false},
		{name: "origin user information", url: "http://filesage.test/api", origin: "http://user@filesage.test", want: false},
		{name: "malformed port", url: "http://filesage.test/api", origin: "http://filesage.test:invalid", want: false},
		{name: "multiple origins", url: "http://filesage.test/api", origin: "http://filesage.test", second: "http://attacker.test", want: false},
		{name: "request URL scheme is not transport security", url: "http://filesage.test/api", urlScheme: "https", origin: "https://filesage.test", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.url, nil)
			if test.urlScheme != "" {
				request.URL.Scheme = test.urlScheme
			}
			if test.host != "" {
				request.Host = test.host
			}
			if test.origin != "" {
				request.Header.Add("Origin", test.origin)
			}
			if test.second != "" {
				request.Header.Add("Origin", test.second)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if got := AllowsMutation(request); got != test.want {
				t.Fatalf("AllowsMutation() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAllowsHTTPSOnlyForTLSRequests(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://filesage.test/api", nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Origin", "https://filesage.test:443")
	if !AllowsMutation(request) {
		t.Fatal("same HTTPS origin should be allowed for a TLS request")
	}
}

func TestAllowsMutationRejectsNilRequest(t *testing.T) {
	if AllowsMutation(nil) {
		t.Fatal("nil request should be rejected")
	}
}
