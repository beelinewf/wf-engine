package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

// writeFakeAuthSvc behaves like fakeAuthSvc but fails writes on demand and,
// on success, swaps the shared catalog the way the real service's refresh
// does, so the router tests prove the gates see a write without a restart.
type writeFakeAuthSvc struct {
	fakeAuthSvc
	catalog        *auth.Catalog
	createErr      error
	updateErr      error
	deleteErr      error
	lastCreate     CreateRoleRequest
	lastUpdateName string
	lastUpdate     UpdateRoleRequest
	lastDelete     string
}

func (w *writeFakeAuthSvc) swap(mut func(perms map[string][]string)) {
	seed := w.catalog.Seed()
	perms := make(map[string][]string, len(seed.Permissions)+1)
	for role, actions := range seed.Permissions {
		perms[role] = actions
	}
	mut(perms)
	w.catalog.Replace(perms)
}

func (w *writeFakeAuthSvc) CreateRole(_ context.Context, name string, description *string, actions []string) (model.Role, []string, error) {
	if w.createErr != nil {
		return model.Role{}, nil, w.createErr
	}
	w.lastCreate = CreateRoleRequest{Name: name, Description: description, Permissions: actions}
	w.swap(func(perms map[string][]string) { perms[name] = actions })
	return model.Role{Name: name}, actions, nil
}

func (w *writeFakeAuthSvc) UpdateRole(_ context.Context, name string, description *string, actions []string) (model.Role, []string, error) {
	if w.updateErr != nil {
		return model.Role{}, nil, w.updateErr
	}
	w.lastUpdateName = name
	w.lastUpdate = UpdateRoleRequest{Description: description, Permissions: actions}
	w.swap(func(perms map[string][]string) { perms[name] = actions })
	return model.Role{Name: name}, actions, nil
}

func (w *writeFakeAuthSvc) DeleteRole(_ context.Context, name string) error {
	if w.deleteErr != nil {
		return w.deleteErr
	}
	w.lastDelete = name
	w.swap(func(perms map[string][]string) { delete(perms, name) })
	return nil
}

func rolesWriteRouter(catalog *auth.Catalog, roles []string, svc service.AuthService) *gin.Engine {
	return NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         svc,
		AuthSettings: configuration.Auth{},
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: roles}},
		Catalog:      catalog,
	})
}

func serveRoleWrite(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

// A caller holding only roles:read is refused every write with 403; reads
// stay on roles:read.
func TestRouterRoleWritesRequireRolesWrite(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"auditor": {auth.ActionRolesRead},
	})
	svc := &writeFakeAuthSvc{catalog: catalog}
	r := rolesWriteRouter(catalog, []string{"auditor"}, svc)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/roles", `{"name":"ops","permissions":["instances:read"]}`},
		{http.MethodPut, "/v1/roles/ops", `{"permissions":["instances:read"]}`},
		{http.MethodDelete, "/v1/roles/ops", ""},
	} {
		if w := serveRoleWrite(t, r, tc.method, tc.path, tc.body); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as roles:read-only caller status = %d, want 403", tc.method, tc.path, w.Code)
		}
	}

	// A caller with no grant at all is refused the same way.
	r = rolesWriteRouter(catalog, []string{"finance"}, svc)
	if w := serveRoleWrite(t, r, http.MethodPost, "/v1/roles", `{"name":"ops"}`); w.Code != http.StatusForbidden {
		t.Errorf("POST /v1/roles as ungranted caller status = %d, want 403", w.Code)
	}
}

func TestRouterRoleWritesSucceedWithRolesWrite(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"writer": {auth.ActionRolesWrite},
	})
	svc := &writeFakeAuthSvc{catalog: catalog}
	r := rolesWriteRouter(catalog, []string{"writer"}, svc)

	w := serveRoleWrite(t, r, http.MethodPost, "/v1/roles", `{"name":"ops","description":"on-call","permissions":["instances:read"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/roles status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	if svc.lastCreate.Name != "ops" || len(svc.lastCreate.Permissions) != 1 {
		t.Errorf("created = %+v, want the posted role", svc.lastCreate)
	}
	if svc.lastCreate.Description == nil || *svc.lastCreate.Description != "on-call" {
		t.Errorf("created description = %+v, want on-call", svc.lastCreate.Description)
	}
	if !strings.Contains(w.Body.String(), `"name":"ops"`) || !strings.Contains(w.Body.String(), `"permissions":["instances:read"]`) {
		t.Errorf("POST body = %s, want the created role", w.Body.String())
	}

	w = serveRoleWrite(t, r, http.MethodPut, "/v1/roles/ops", `{"permissions":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /v1/roles/ops status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if svc.lastUpdateName != "ops" || svc.lastUpdate.Description != nil {
		t.Errorf("update = %q %+v, want path name with omitted description", svc.lastUpdateName, svc.lastUpdate)
	}

	w = serveRoleWrite(t, r, http.MethodDelete, "/v1/roles/ops", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/roles/ops status = %d, want 204 (body: %s)", w.Code, w.Body.String())
	}
	if svc.lastDelete != "ops" {
		t.Errorf("deleted = %q, want ops", svc.lastDelete)
	}
	if w.Body.Len() != 0 {
		t.Errorf("DELETE body = %q, want empty", w.Body.String())
	}
}

// The wildcard counts as the grant, the API token bypasses with no role
// grant at all, and an auth-disabled deployment serves the writes openly.
func TestRouterRoleWriteBypasses(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{"op": {auth.WildcardAction}})
	svc := &writeFakeAuthSvc{catalog: catalog}
	r := rolesWriteRouter(catalog, []string{"op"}, svc)
	if w := serveRoleWrite(t, r, http.MethodPost, "/v1/roles", `{"name":"ops"}`); w.Code != http.StatusCreated {
		t.Errorf("POST as wildcard holder status = %d, want 201", w.Code)
	}

	tokenRouter := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         svc,
		AuthSettings: authConfigForTest(),
		Catalog:      auth.NewCatalog(nil),
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", strings.NewReader(`{"name":"ops"}`))
	req.Header.Set(APIKeyHeader, "test-token")
	req.Header.Set("Content-Type", "application/json")
	tokenRouter.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Errorf("POST as service principal status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}

	openRouter := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: svc})
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/roles/ops", nil)
	openRouter.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE with auth disabled status = %d, want 204", w.Code)
	}
}

func TestRouterRoleWriteErrorsMap(t *testing.T) {
	conflict := fmt.Errorf("%w: at least one role must retain the roles:write permission", model.ErrConflict)
	cases := []struct {
		name   string
		svc    *writeFakeAuthSvc
		method string
		path   string
		body   string
		want   int
	}{
		{
			"duplicate create is 409", &writeFakeAuthSvc{createErr: model.ErrConflict},
			http.MethodPost, "/v1/roles", `{"name":"ops"}`, http.StatusConflict,
		},
		{
			"invalid create is 422", &writeFakeAuthSvc{createErr: model.ErrInvalid},
			http.MethodPost, "/v1/roles", `{"name":""}`, http.StatusUnprocessableEntity,
		},
		{
			"lockout strip is 409", &writeFakeAuthSvc{updateErr: conflict},
			http.MethodPut, "/v1/roles/admin", `{"permissions":[]}`, http.StatusConflict,
		},
		{
			"unknown update is 404", &writeFakeAuthSvc{updateErr: model.ErrNotFound},
			http.MethodPut, "/v1/roles/wizard", `{"permissions":[]}`, http.StatusNotFound,
		},
		{
			"lockout delete is 409", &writeFakeAuthSvc{deleteErr: conflict},
			http.MethodDelete, "/v1/roles/admin", "", http.StatusConflict,
		},
		{
			"unknown delete is 404", &writeFakeAuthSvc{deleteErr: model.ErrNotFound},
			http.MethodDelete, "/v1/roles/wizard", "", http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		catalog := auth.NewCatalog(map[string][]string{"writer": {auth.ActionRolesWrite}})
		tc.svc.catalog = catalog
		r := rolesWriteRouter(catalog, []string{"writer"}, tc.svc)
		if w := serveRoleWrite(t, r, tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body: %s)", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
}

// A malformed body is 400, never a service call.
func TestRouterRoleWriteMalformedBodyIsBadRequest(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{"writer": {auth.ActionRolesWrite}})
	svc := &writeFakeAuthSvc{catalog: catalog}
	r := rolesWriteRouter(catalog, []string{"writer"}, svc)
	if w := serveRoleWrite(t, r, http.MethodPost, "/v1/roles", `{"name":`); w.Code != http.StatusBadRequest {
		t.Errorf("POST malformed body status = %d, want 400", w.Code)
	}
	if w := serveRoleWrite(t, r, http.MethodPut, "/v1/roles/ops", `{"permissions":`); w.Code != http.StatusBadRequest {
		t.Errorf("PUT malformed body status = %d, want 400", w.Code)
	}
}

// End to end through the shared catalog: creating a role over the API lets
// a token carrying it pass the granted gate at once, with no restart.
func TestRouterCreatedRoleAuthorizesWithoutRestart(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"writer": {auth.ActionRolesWrite},
	})
	svc := &writeFakeAuthSvc{catalog: catalog}
	writer := rolesWriteRouter(catalog, []string{"writer"}, svc)
	reader := rolesWriteRouter(catalog, []string{"reader"}, svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	reader.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/roles before the write status = %d, want 403", w.Code)
	}

	w = serveRoleWrite(t, writer, http.MethodPost, "/v1/roles", `{"name":"reader","permissions":["roles:read"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/roles status = %d, want 201", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	reader.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /v1/roles after the write status = %d, want 200", w.Code)
	}
}
