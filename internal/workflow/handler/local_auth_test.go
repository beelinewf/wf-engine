package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/pkg/configuration"
)

// scriptedAuthSvc answers the local-user surface from fields, capturing
// update calls so tests assert what the handler forwarded.
type scriptedAuthSvc struct {
	fakeAuthSvc
	createResult model.User
	createErr    error
	updateResult model.User
	updateErr    error
	lastActor    auth.Principal
	lastID       string
	lastParams   service.UpdateUserParams
	disableErr   error
	register     service.RegisterResult
	registerErr  error
	login        service.LoginResult
	loginErr     error
	verify       service.LoginResult
	verifyErr    error
	resendErr    error
	adminResend  error
}

func (s *scriptedAuthSvc) CreateUser(context.Context, string, string, string, []string) (model.User, error) {
	return s.createResult, s.createErr
}

func (s *scriptedAuthSvc) UpdateUser(_ context.Context, actor auth.Principal, id string, upd service.UpdateUserParams) (model.User, error) {
	s.lastActor, s.lastID, s.lastParams = actor, id, upd
	return s.updateResult, s.updateErr
}

func (s *scriptedAuthSvc) DisableUser(context.Context, auth.Principal, string) (model.User, error) {
	return model.User{}, s.disableErr
}

func (s *scriptedAuthSvc) Register(context.Context, string, string, string) (service.RegisterResult, error) {
	return s.register, s.registerErr
}

func (s *scriptedAuthSvc) Login(context.Context, string, string) (service.LoginResult, error) {
	return s.login, s.loginErr
}

func (s *scriptedAuthSvc) VerifyEmail(context.Context, string) (service.LoginResult, error) {
	return s.verify, s.verifyErr
}

func (s *scriptedAuthSvc) ResendVerification(context.Context, string) error {
	return s.resendErr
}

func (s *scriptedAuthSvc) ResendVerificationForUser(context.Context, auth.Principal, string) error {
	return s.adminResend
}

func localAuthSettings() configuration.Auth {
	return configuration.Auth{
		Enabled: true,
		Local: configuration.LocalAuth{
			Enabled:           true,
			AllowRegistration: true,
			RateLimit:         configuration.LocalRateLimit{Enabled: false},
		},
	}
}

func scriptedLocalUser() model.User {
	now := time.Now().UTC()
	verified := now
	return model.User{
		ID: "22222222-2222-7222-8222-222222222222", Subject: "22222222-2222-7222-8222-222222222222",
		Issuer: model.LocalIssuer, Name: "Ada", Email: "ada@example.test",
		PasswordHash: "hashed", IsActive: true, EmailVerifiedAt: &verified,
		Roles: []string{"viewer"}, CreatedAt: now, UpdatedAt: now,
	}
}

func postLocal(t *testing.T, r interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// The four local-auth endpoints are public: a caller without a token
// reaches all of them, exactly like the login contract.
func TestRouterLocalAuthIsPublic(t *testing.T) {
	user := scriptedLocalUser()
	svc := &scriptedAuthSvc{
		register: service.RegisterResult{User: user, Token: "tok", ExpiresAt: time.Now().Add(time.Hour)},
		login:    service.LoginResult{User: user, Token: "tok", ExpiresAt: time.Now().Add(time.Hour)},
		verify:   service.LoginResult{User: user, Token: "tok", ExpiresAt: time.Now().Add(time.Hour)},
	}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: svc, AuthSettings: localAuthSettings()})

	if w := postLocal(t, r, "/v1/auth/register", `{"name":"Ada","email":"ada@example.test","password":"Password-1"}`); w.Code != http.StatusCreated {
		t.Errorf("register status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	for path, body := range map[string]string{
		"/v1/auth/login":               `{"email":"ada@example.test","password":"Password-1"}`,
		"/v1/auth/verify-email":        `{"token":"raw-token"}`,
		"/v1/auth/resend-verification": `{"email":"ada@example.test"}`,
	} {
		if w := postLocal(t, r, path, body); w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 (body: %s)", path, w.Code, w.Body.String())
		}
	}

	var login LoginResponse
	w := postLocal(t, r, "/v1/auth/login", `{"email":"ada@example.test","password":"Password-1"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if login.AccessToken != "tok" || login.TokenType != "Bearer" || login.ExpiresIn <= 0 {
		t.Errorf("login = %+v, want a Bearer [REDACTED] a positive ttl", login)
	}
	if login.User.ID != user.ID || !login.User.IsLocal || !login.User.IsActive || !login.User.EmailVerified {
		t.Errorf("login user = %+v", login.User)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode login raw: %v", err)
	}
	if _, ok := raw["user"].(map[string]any)["password_hash"]; ok {
		t.Error("login user leaks password_hash")
	}
}

// A pending registration carries no token: the mailed link is the only
// way forward, and the body says so.
func TestRouterRegisterPendingCarriesNoToken(t *testing.T) {
	user := scriptedLocalUser()
	user.EmailVerifiedAt = nil
	svc := &scriptedAuthSvc{register: service.RegisterResult{User: user, PendingVerification: true}}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: svc, AuthSettings: localAuthSettings()})

	w := postLocal(t, r, "/v1/auth/register", `{"name":"Ada","email":"ada@example.test","password":"Password-1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	var body RegisterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if !body.PendingVerification || body.AccessToken != "" {
		t.Errorf("register = %+v, want pending with no token", body)
	}
}

// With local auth off the four endpoints do not exist: 404, not 403.
func TestRouterLocalAuthDisabledIs404(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: &fakeAuthSvc{}, AuthSettings: authConfigForTest()})
	for path, body := range map[string]string{
		"/v1/auth/register":            `{"name":"A","email":"a@example.test","password":"Password-1"}`,
		"/v1/auth/login":               `{"email":"a@example.test","password":"Password-1"}`,
		"/v1/auth/verify-email":        `{"token":"x"}`,
		"/v1/auth/resend-verification": `{"email":"a@example.test"}`,
	} {
		if w := postLocal(t, r, path, body); w.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, w.Code)
		}
	}
}

// The local-auth sentinels map to fixed statuses with fixed generic
// details; domain errors keep the standard mapping.
func TestRouterLocalAuthErrorMapping(t *testing.T) {
	login401 := &scriptedAuthSvc{loginErr: service.ErrInvalidCredentials}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: login401, AuthSettings: localAuthSettings()})
	w := postLocal(t, r, "/v1/auth/login", `{"email":"a@example.test","password":"x"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad login status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid email or password") {
		t.Errorf("bad login body = %s, want the generic detail", w.Body.String())
	}

	login403 := &scriptedAuthSvc{loginErr: service.ErrEmailNotVerified}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: login403, AuthSettings: localAuthSettings()})
	if w := postLocal(t, r, "/v1/auth/login", `{"email":"a@example.test","password":"x"}`); w.Code != http.StatusForbidden {
		t.Errorf("pending login status = %d, want 403", w.Code)
	}

	conflict := &scriptedAuthSvc{registerErr: fmt.Errorf("%w: taken", model.ErrConflict)}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: conflict, AuthSettings: localAuthSettings()})
	if w := postLocal(t, r, "/v1/auth/register", `{"name":"A","email":"a@example.test","password":"Password-1"}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate register status = %d, want 409", w.Code)
	}

	closed := &scriptedAuthSvc{registerErr: fmt.Errorf("%w: registration is disabled", model.ErrForbidden)}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: closed, AuthSettings: localAuthSettings()})
	if w := postLocal(t, r, "/v1/auth/register", `{"name":"A","email":"a@example.test","password":"Password-1"}`); w.Code != http.StatusForbidden {
		t.Errorf("closed register status = %d, want 403", w.Code)
	}

	badLink := &scriptedAuthSvc{verifyErr: fmt.Errorf("%w: invalid verification link", model.ErrInvalid)}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: badLink, AuthSettings: localAuthSettings()})
	if w := postLocal(t, r, "/v1/auth/verify-email", `{"token":"x"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad link status = %d, want 422", w.Code)
	}

	ok := &scriptedAuthSvc{}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: ok, AuthSettings: localAuthSettings()})
	for path := range map[string]string{
		"/v1/auth/register": `{}`, "/v1/auth/login": `{}`, "/v1/auth/verify-email": `{}`, "/v1/auth/resend-verification": `{}`,
	} {
		if w := postLocal(t, r, path, `{oops`); w.Code != http.StatusBadRequest {
			t.Errorf("%s malformed status = %d, want 400", path, w.Code)
		}
	}
}

// User writes are gated on users:write, and the gate matrix covers the
// credential itself: no credential is 401 before any permission consult.
func TestRouterUserWritesRequireUsersWrite(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"writer": {auth.ActionUsersRead, auth.ActionUsersWrite},
		"reader": {auth.ActionUsersRead},
	})
	user := scriptedLocalUser()

	admitted := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &scriptedAuthSvc{createResult: user},
		AuthSettings: localAuthSettings(),
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: []string{"writer"}}},
		Catalog:      catalog,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(`{"name":"N","email":"n@example.test","password":"Password-1"}`))
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	admitted.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Errorf("writer POST status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/users/"+user.ID, nil)
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	admitted.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("writer DELETE status = %d, want 204 (body: %s)", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/users/"+user.ID+"/resend-verification", nil)
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	admitted.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("writer resend status = %d, want 204 (body: %s)", w.Code, w.Body.String())
	}

	refused := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &scriptedAuthSvc{createResult: user},
		AuthSettings: localAuthSettings(),
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: []string{"reader"}}},
		Catalog:      catalog,
	})
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/users", `{"name":"N","email":"n@example.test","password":"Password-1"}`},
		{http.MethodDelete, "/v1/users/" + user.ID, ""},
		{http.MethodPost, "/v1/users/" + user.ID + "/resend-verification", ""},
	} {
		w := httptest.NewRecorder()
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		}
		req.Header.Set(BearerHeader, "Bearer [REDACTED]")
		refused.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("reader %s %s status = %d, want 403", tc.method, tc.path, w.Code)
		}
	}

	creds := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &scriptedAuthSvc{createResult: user},
		AuthSettings: localAuthSettings(),
		Catalog:      catalog,
	})
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/users", `{"name":"N","email":"n@example.test","password":"Password-1"}`},
		{http.MethodPut, "/v1/users/" + user.ID, `{"name":"N"}`},
		{http.MethodDelete, "/v1/users/" + user.ID, ""},
		{http.MethodPost, "/v1/users/" + user.ID + "/resend-verification", ""},
	} {
		w := httptest.NewRecorder()
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		}
		creds.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s status = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}

// PUT carries no resource-action gate of its own: any authenticated caller
// reaches the service, which admits a users:write holder for every field
// and the account itself for its profile. The handler forwards the actor,
// the id, and the untouched fields.
func TestRouterUserUpdateForwardsToService(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"reader": {auth.ActionUsersRead},
	})
	user := scriptedLocalUser()
	svc := &scriptedAuthSvc{updateResult: user}
	r := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         svc,
		AuthSettings: localAuthSettings(),
		OIDC:         stubVerifier{principal: auth.Principal{Subject: user.ID, Issuer: model.LocalIssuer, UserID: user.ID, Roles: []string{"reader"}}},
		Catalog:      catalog,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/users/"+user.ID, strings.NewReader(`{"name":"Ada Me","password":"Password-2","current_password":"Password-1"}`))
	req.Header.Set(BearerHeader, "Bearer [REDACTED]")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if svc.lastID != user.ID {
		t.Errorf("forwarded id = %q, want %q", svc.lastID, user.ID)
	}
	if svc.lastActor.UserID != user.ID {
		t.Errorf("forwarded actor = %+v, want the caller", svc.lastActor)
	}
	if svc.lastParams.Name == nil || *svc.lastParams.Name != "Ada Me" {
		t.Errorf("forwarded name = %+v", svc.lastParams.Name)
	}
	if svc.lastParams.Password == nil || *svc.lastParams.Password != "Password-2" {
		t.Errorf("forwarded password = %+v", svc.lastParams.Password)
	}
	if svc.lastParams.CurrentPassword == nil || *svc.lastParams.CurrentPassword != "Password-1" {
		t.Errorf("forwarded current password = %+v", svc.lastParams.CurrentPassword)
	}
	if svc.lastParams.Roles != nil || svc.lastParams.IsActive != nil {
		t.Errorf("forwarded roles/active = %+v/%+v, want nil", svc.lastParams.Roles, svc.lastParams.IsActive)
	}
}

// The create body reports the account shape and never the credential: no
// password_hash key, roles as [] when empty.
func TestRouterUserCreateShape(t *testing.T) {
	user := scriptedLocalUser()
	user.Roles = nil
	svc := &scriptedAuthSvc{createResult: user}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: svc})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/users", strings.NewReader(`{"name":"Ada","email":"ada@example.test","password":"Password-1"}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	var body UserResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !body.IsLocal || !body.IsActive || !body.EmailVerified {
		t.Errorf("flags = %+v", body)
	}
	if body.Roles == nil {
		t.Errorf("roles = null, want []")
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode create raw: %v", err)
	}
	for _, key := range []string{"password_hash", "password"} {
		if _, ok := raw[key]; ok {
			t.Errorf("body leaks %q", key)
		}
	}
}

// The login contract reports the local fragment, so a frontend renders
// the right form without probing.
func TestRouterAuthConfigReportsLocal(t *testing.T) {
	settings := localAuthSettings()
	settings.Local.EmailVerification.Enabled = true
	r := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Auth:         &fakeAuthSvc{},
		AuthSettings: settings,
		Catalog:      auth.NewCatalog(map[string][]string{"admin": {auth.ActionUsersWrite}}),
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/config", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body AuthConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if !body.LocalEnabled || !body.RegistrationEnabled || !body.EmailVerificationEnabled {
		t.Errorf("local fragment = %+v", body)
	}
}

// The limiter is wired through the router, not just unit-tested: with a
// budget of one, the second failing login to the full engine is 429,
// while successes never spend the budget.
func TestRouterLocalLimiterWired(t *testing.T) {
	svc := &scriptedAuthSvc{loginErr: service.ErrInvalidCredentials}
	settings := localAuthSettings()
	settings.Local.RateLimit = configuration.LocalRateLimit{Enabled: true, MaxAttempts: 1, Window: time.Minute, BlockDuration: time.Minute}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: svc, AuthSettings: settings})

	body := `{"email":"ada@example.test","password":"wrong"}`
	if w := postLocal(t, r, "/v1/auth/login", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("first login status = %d, want 401", w.Code)
	}
	w := postLocal(t, r, "/v1/auth/login", body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second login status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}

	user := scriptedLocalUser()
	ok := &scriptedAuthSvc{login: service.LoginResult{User: user, Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Auth: ok, AuthSettings: settings})
	good := `{"email":"ada@example.test","password":"Password-1"}`
	for i := 0; i < 3; i++ {
		if w := postLocal(t, r, "/v1/auth/login", good); w.Code != http.StatusOK {
			t.Fatalf("success %d status = %d, want 200", i+1, w.Code)
		}
	}
}
