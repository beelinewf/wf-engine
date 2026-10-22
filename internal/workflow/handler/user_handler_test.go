package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/pkg/configuration"
)

// The user directory is gated on users:read: a role that grants it is
// admitted to both routes, and a role that grants nothing is refused both.
func TestRouterUserRoutesRequireUsersRead(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"auditor": {auth.ActionUsersRead},
	})
	paths := []string{"/v1/users", "/v1/users/" + testSystemUserID}

	admitted := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &fakeAuthSvc{},
		AuthSettings: configuration.Auth{},
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: []string{"auditor"}}},
		Catalog:      catalog,
	})
	for _, path := range paths {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(BearerHeader, "Bearer [REDACTED]")
		admitted.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("auditor GET %s status = %d, want 200", path, w.Code)
		}
	}

	refused := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &fakeAuthSvc{},
		AuthSettings: configuration.Auth{},
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: []string{"finance"}}},
		Catalog:      catalog,
	})
	for _, path := range paths {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(BearerHeader, "Bearer [REDACTED]")
		refused.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("finance GET %s status = %d, want 403", path, w.Code)
		}
	}
}

// Both user routes sit behind the global authentication gate: no credential
// is 401 before any permission is consulted.
func TestRouterUserRoutesRequireCredential(t *testing.T) {
	r := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &fakeAuthSvc{},
		AuthSettings: authConfigForTest(),
	})
	for _, path := range []string{"/v1/users", "/v1/users/" + testSystemUserID} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401", path, w.Code)
		}
	}
}

// The list answers the paginated envelope with the page echoed back. The
// identity-cache metadata must never leak: the item shape carries no such
// field.
func TestRouterUsersListEnvelope(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &fakeAuthSvc{}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Items      *[]UserResponse `json:"items"`
		Page       int             `json:"page"`
		PerPage    int             `json:"per_page"`
		Total      int64           `json:"total"`
		TotalPages int64           `json:"total_pages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /v1/users: %v", err)
	}
	if body.Items == nil {
		t.Fatalf("items = null, want [] (body: %s)", w.Body.String())
	}
	if len(*body.Items) != 1 || (*body.Items)[0].ID != testSystemUserID {
		t.Errorf("items = %+v, want the one fake user", *body.Items)
	}
	if body.Page != 1 || body.PerPage != 50 || body.Total != 1 || body.TotalPages != 1 {
		t.Errorf("page/per_page/total/total_pages = %d/%d/%d/%d, want 1/50/1/1",
			body.Page, body.PerPage, body.Total, body.TotalPages)
	}

	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode /v1/users items: %v", err)
	}
	if _, ok := raw.Items[0]["metadata"]; ok {
		t.Errorf("item leaks metadata: %v", raw.Items[0])
	}
}

// An empty directory is still a directory: items is an empty array rather
// than null even when the service answers nil.
func TestRouterUsersListReportsEmptyAsArray(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &emptyUserListSvc{}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Items *[]UserResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /v1/users: %v", err)
	}
	if body.Items == nil {
		t.Fatalf("items = null, want [] (body: %s)", w.Body.String())
	}
	if len(*body.Items) != 0 {
		t.Errorf("items = %+v, want an empty array", *body.Items)
	}
}

// emptyUserListSvc answers a nil page, so the empty-array contract pins the
// handler's own envelope rather than the service's slice.
type emptyUserListSvc struct{ fakeAuthSvc }

func (emptyUserListSvc) ListUsers(context.Context, int, int) ([]model.User, int64, error) {
	return nil, 0, nil
}

func TestRouterUsersListRejectsBadPagination(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &fakeAuthSvc{}})
	for _, query := range []string{"?page=0", "?page=nope", "?per_page=0", "?per_page=201"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/users"+query, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET /v1/users%s status = %d, want 400", query, w.Code)
		}
	}
}

// A well-formed id that matches no row is a missing resource. The fake
// answers exactly what the real service answers for an unknown id, so the
// status under test is the production mapping.
func TestRouterUserGetUnknownIsNotFound(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &fakeAuthSvc{}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/99999999-9999-7999-8999-999999999999", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

// A malformed id names no user for any caller, which is a malformed request
// rather than a missing resource: the documented 422 is pinned here so the
// annotation and the handler cannot drift apart.
func TestRouterUserGetMalformedIDIsUnprocessable(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &invalidUserIDSvc{}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/not-a-uuid", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (body: %s)", w.Code, w.Body.String())
	}
}

// invalidUserIDSvc mirrors the real service's distinction: a malformed id is
// ErrInvalid. The service test covers the real validation; this pins the
// status it maps to.
type invalidUserIDSvc struct{ fakeAuthSvc }

func (invalidUserIDSvc) GetUser(context.Context, string) (model.User, error) {
	return model.User{}, fmt.Errorf("%w: user id is not a valid uuid", model.ErrInvalid)
}

// A stored user without a provider identity, like the system user, reports
// null subject and issuer rather than empty strings.
func TestRouterUserGetReturnsUser(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &fakeAuthSvc{}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/"+testSystemUserID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body UserResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /v1/users/{id}: %v", err)
	}
	if body.ID != testSystemUserID || body.Name != "system" || body.Email != "system@localhost" {
		t.Errorf("user = %+v, want the fake system user", body)
	}
	if body.Subject != nil || body.Issuer != nil {
		t.Errorf("subject/issuer = %v/%v, want null for an identity-less user", body.Subject, body.Issuer)
	}
}
