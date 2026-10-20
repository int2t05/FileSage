// 本文件验证 Agent 设置接口的持久化和同源保护。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"filesage/internal/store"
)

func TestAgentSettingsAPIReadsDefaultAndUpdatesPersistedMode(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(nil, metadata, nil, nil)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/settings/agent", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var settings struct {
		BypassPermission bool   `json:"bypassPermission"`
		UpdatedAt        string `json:"updatedAt"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &settings); response.Code != http.StatusOK || err != nil || !settings.BypassPermission || settings.UpdatedAt == "" {
		t.Fatalf("default settings response=(%d, %s, %v)", response.Code, response.Body.String(), err)
	}

	request = httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/settings/agent", strings.NewReader(`{"bypassPermission":false}`))
	request.Header.Set("Origin", "http://filesage.local")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &settings); response.Code != http.StatusOK || err != nil || settings.BypassPermission {
		t.Fatalf("updated settings response=(%d, %s, %v)", response.Code, response.Body.String(), err)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/settings/agent", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if err := json.Unmarshal(response.Body.Bytes(), &settings); response.Code != http.StatusOK || err != nil || settings.BypassPermission {
		t.Fatalf("persisted settings response=(%d, %s, %v)", response.Code, response.Body.String(), err)
	}
}

func TestAgentSettingsUpdateRequiresBooleanAndSameOrigin(t *testing.T) {
	metadata, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "filesage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	handler := NewWithTaskStore(nil, metadata, nil, nil)
	for _, test := range []struct {
		name, body, origin string
		wantStatus         int
	}{
		{name: "missing value", body: `{}`, origin: "http://filesage.local", wantStatus: http.StatusBadRequest},
		{name: "wrong origin", body: `{"bypassPermission":false}`, origin: "http://elsewhere.local", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "http://filesage.local/api/v1/settings/agent", strings.NewReader(test.body))
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("settings update status=%d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}
