// 本文件验证回收站游标接口契约。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filesage/internal/fsroot"
	trashsvc "filesage/internal/trash"
)

type cursorTrashStub struct {
	limit  int
	cursor string
}

func (s *cursorTrashStub) List(_ context.Context, limit int, cursor string) (trashsvc.Page, error) {
	s.limit, s.cursor = limit, cursor
	return trashsvc.Page{
		Items: []trashsvc.Item{{ID: "9", OriginalPath: "docs/a.pdf", Name: "a.pdf", Kind: "file"}},
		Limit: limit, HasMore: true, Snapshot: 71, NextCursor: "opaque-next",
	}, nil
}

func (*cursorTrashStub) Get(context.Context, string) (trashsvc.Item, error) {
	return trashsvc.Item{}, errors.New("unused")
}

func (*cursorTrashStub) LockIdentity(context.Context, string) (fsroot.Entry, error) {
	return fsroot.Entry{}, errors.New("unused")
}

func (*cursorTrashStub) MoveToTrash(context.Context, fsroot.Entry) (trashsvc.Item, error) {
	return trashsvc.Item{}, errors.New("unused")
}

func (*cursorTrashStub) PermanentRemove(context.Context, fsroot.Entry) error {
	return errors.New("unused")
}
func (*cursorTrashStub) Restore(context.Context, string) (string, error) {
	return "", errors.New("unused")
}
func (*cursorTrashStub) Purge(context.Context, string) error { return errors.New("unused") }
func (*cursorTrashStub) PurgeAtSnapshot(context.Context, string, int64) error {
	return errors.New("unused")
}
func (*cursorTrashStub) PurgeCandidates(context.Context, int64, int64, int) ([]trashsvc.Item, error) {
	return nil, errors.New("unused")
}

func TestTrashListingUsesOpaqueKeysetCursor(t *testing.T) {
	service := &cursorTrashStub{}
	handler := NewWithTrash(nil, nil, service, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/trash?limit=7&cursor=opaque-token", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if service.limit != 7 || service.cursor != "opaque-token" {
		t.Fatalf("List() received limit=%d cursor=%q", service.limit, service.cursor)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["offset"]; ok {
		t.Fatalf("response retains offset pagination: %s", response.Body.String())
	}
	var nextCursor string
	if err := json.Unmarshal(body["nextCursor"], &nextCursor); err != nil || nextCursor != "opaque-next" {
		t.Fatalf("nextCursor = %q, error = %v", nextCursor, err)
	}
}

func TestTrashListingRejectsOffsetAndOversizedCursor(t *testing.T) {
	service := &cursorTrashStub{}
	handler := NewWithTrash(nil, nil, service, nil, nil)
	for _, target := range []string{
		"/api/v1/trash?offset=10",
		"/api/v1/trash?cursor=" + strings.Repeat("a", 65),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %q status = %d, want 400", target, response.Code)
		}
	}
}
