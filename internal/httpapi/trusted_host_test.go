package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedHostRejectsMatchingMaliciousOrigin(t *testing.T) {
	called := false
	handler, err := NewTrustedHostHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}), []string{"filesage.lan", "10.77.88.99"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "http://attacker.example/api/v1/files/content", nil)
	request.Host = "attacker.example:8080"
	request.Header.Set("Origin", "http://attacker.example:8080")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421", response.Code)
	}
	if called {
		t.Fatal("request with an untrusted matching Host and Origin reached the application")
	}
}

func TestTrustedHostAllowsCustomHostnameRegardlessOfPort(t *testing.T) {
	handler, err := NewTrustedHostHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), []string{"filesage.example"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://filesage.example/health", nil)
	request.Host = "FileSage.Example.:8099"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", response.Code, response.Body.String())
	}
}

func TestTrustedHostNormalizesIPAndIPv6Authority(t *testing.T) {
	tests := []struct {
		name    string
		allowed string
		host    string
	}{
		{name: "ipv4 with port", allowed: "10.77.88.99", host: "10.77.88.99:8080"},
		{name: "ipv6 with port", allowed: "::1", host: "[::1]:8080"},
		{name: "ipv6 without port", allowed: "::1", host: "[::1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := NewTrustedHostHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), []string{test.allowed})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			request.Host = test.host
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestTrustedHostRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewTrustedHostHandler(http.NotFoundHandler(), []string{"filesage.example:8080"}); err == nil {
		t.Fatal("host with port was accepted in allowlist")
	}
	if _, err := NewTrustedHostHandler(http.NotFoundHandler(), nil); err == nil {
		t.Fatal("empty allowlist was accepted")
	}
}
