package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"gorm.io/gorm"
)

const (
	localSvcSecret = "0123456789abcdef0123456789abcdef"
	localAdminID   = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
)

// fakeMail records verification sends without touching the network.
type fakeMail struct {
	mu    sync.Mutex
	sends []fakeSend
	err   error
}

type fakeSend struct {
	to   string
	link string
	ttl  time.Duration
}

func (f *fakeMail) SendVerification(_ context.Context, to, link string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sends = append(f.sends, fakeSend{to: to, link: link, ttl: ttl})
	return nil
}

func (f *fakeMail) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeMail) last() fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends[len(f.sends)-1]
}

var localSvcCatalog = map[string][]string{
	"admin":  {auth.ActionUsersWrite, auth.ActionUsersRead, auth.ActionRolesRead},
	"viewer": {auth.ActionUsersRead},
}

// localSvcFor builds a real auth service with local auth wired, over a
// fresh test database with the catalog roles seeded.
func localSvcFor(t *testing.T, deps service.LocalAuthDeps) (service.AuthService, *gorm.DB, *auth.Catalog) {
	t.Helper()
	db := setupSvcDB(t)
	catalog := auth.NewCatalog(localSvcCatalog)
	now := time.Now().UTC()
	for role := range localSvcCatalog {
		if err := db.Create(&repository.RoleModel{Name: role, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatalf("seed role %s: %v", role, err)
		}
	}
	issuer, err := auth.NewLocalAuthenticator(localSvcSecret, auth.LocalIssuerName, "beelinewf", time.Hour)
	if err != nil {
		t.Fatalf("NewLocalAuthenticator() error = %v", err)
	}
	deps.Issuer = issuer
	if deps.VerificationTokenTTL == 0 {
		deps.VerificationTokenTTL = time.Hour
	}
	if deps.ResendCooldown == 0 {
		deps.ResendCooldown = time.Minute
	}
	if deps.PublicBaseURL == "" {
		deps.PublicBaseURL = "https://app.example.test"
	}
	svc := service.NewAuthService(
		repository.NewUserRepository(db),
		repository.NewRoleRepository(db),
		catalog,
		svcSysUserID,
		service.WithLocalAuth(deps),
	)
	return svc, db, catalog
}

// adminActor is a users:write holder that is nobody's account: its id is
// fixed and never created, so self-guards never trip for it by accident.
func adminActor() auth.Principal {
	return auth.Principal{Subject: "admin-1", Issuer: authSvcIssuer, Name: "Admin", UserID: localAdminID, Roles: []string{"admin"}}
}

func ctxFor(catalog *auth.Catalog, p auth.Principal) context.Context {
	return auth.ContextWithPrincipal(context.Background(), p, catalog)
}

func TestLocalDisabledFailsClosed(t *testing.T) {
	svc, _ := authServiceFor(t, auth.NewCatalog(localSvcCatalog))
	ctx := context.Background()
	admin := adminActor()

	if _, err := svc.CreateUser(ctx, "N", "n@example.test", "Password-1", nil); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("CreateUser() error = %v, want ErrForbidden", err)
	}
	if _, err := svc.UpdateUser(ctx, admin, localAdminID, service.UpdateUserParams{}); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("UpdateUser() error = %v, want ErrForbidden", err)
	}
	if _, err := svc.DisableUser(ctx, admin, localAdminID); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("DisableUser() error = %v, want ErrForbidden", err)
	}
	if _, err := svc.Register(ctx, "N", "n@example.test", "Password-1"); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("Register() error = %v, want ErrForbidden", err)
	}
	if _, err := svc.Login(ctx, "n@example.test", "Password-1"); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("Login() error = %v, want ErrForbidden", err)
	}
	if _, err := svc.VerifyEmail(ctx, "token"); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("VerifyEmail() error = %v, want ErrForbidden", err)
	}
	if err := svc.ResendVerification(ctx, "n@example.test"); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("ResendVerification() error = %v, want ErrForbidden", err)
	}
	if err := svc.ResendVerificationForUser(ctx, admin, localAdminID); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("ResendVerificationForUser() error = %v, want ErrForbidden", err)
	}
}

func TestCreateUserRoundTrip(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{})
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, "Ada", "Ada@Example.Test", "Password-1", []string{"viewer"})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if !created.IsLocal() || !created.IsActive || !created.EmailVerified() {
		t.Errorf("created = %+v, want an active verified local", created)
	}
	if created.Email != "ada@example.test" {
		t.Errorf("email = %q, want normalized", created.Email)
	}
	if len(created.Roles) != 1 || created.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", created.Roles)
	}
	// The stored credential is a hash, and it authenticates.
	if created.PasswordHash == "Password-1" || created.PasswordHash == "" {
		t.Error("PasswordHash is plaintext or empty")
	}
	login, err := svc.Login(ctx, "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if login.User.ID != created.ID || login.Token == "" {
		t.Errorf("login = %+v", login)
	}
}

func TestCreateUserValidation(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{})
	ctx := context.Background()

	cases := map[string]func() error{
		"blank name":     func() error { _, err := svc.CreateUser(ctx, "  ", "a@example.test", "Password-1", nil); return err },
		"blank email":    func() error { _, err := svc.CreateUser(ctx, "A", "  ", "Password-1", nil); return err },
		"shapeless mail": func() error { _, err := svc.CreateUser(ctx, "A", "not-an-email", "Password-1", nil); return err },
		"cr mail":        func() error { _, err := svc.CreateUser(ctx, "A", "a@exam\rple.test", "Password-1", nil); return err },
		"short password": func() error { _, err := svc.CreateUser(ctx, "A", "a@example.test", "short", nil); return err },
		"long password": func() error {
			_, err := svc.CreateUser(ctx, "A", "a@example.test", strings.Repeat("x", 73), nil)
			return err
		},
		"no uppercase": func() error { _, err := svc.CreateUser(ctx, "A", "a@example.test", "password-1", nil); return err },
		"no digit":     func() error { _, err := svc.CreateUser(ctx, "A", "a@example.test", "Password-x", nil); return err },
		"no symbol":    func() error { _, err := svc.CreateUser(ctx, "A", "a@example.test", "Password1", nil); return err },
		"unknown role": func() error {
			_, err := svc.CreateUser(ctx, "A", "a@example.test", "Password-1", []string{"wizard"})
			return err
		},
		"blank role": func() error {
			_, err := svc.CreateUser(ctx, "A", "a@example.test", "Password-1", []string{"  "})
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}

	if _, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", nil); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	// A six-character password with all three classes passes the floor.
	if _, err := svc.CreateUser(ctx, "Bo", "bo@example.test", "P-a1xy", nil); err != nil {
		t.Errorf("six-character password error = %v, want nil", err)
	}
	// Case-insensitive duplicate: the stored email is normalized.
	if _, err := svc.CreateUser(ctx, "Ada 2", "ADA@EXAMPLE.TEST", "Password-2", nil); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate email error = %v, want ErrConflict", err)
	}
}

// Unknown email, wrong password, and disabled account share one generic
// error, so login cannot probe for accounts.
func TestLoginFailuresAreGeneric(t *testing.T) {
	svc, _, catalog := localSvcFor(t, service.LocalAuthDeps{})
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if _, err := svc.DisableUser(ctxFor(catalog, adminActor()), adminActor(), created.ID); err != nil {
		t.Fatalf("DisableUser() error = %v", err)
	}
	if _, err := svc.CreateUser(ctx, "Grace", "grace@example.test", "Password-1", nil); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	for name, creds := range map[string][2]string{
		"unknown email":  {"nobody@example.test", "Password-1"},
		"wrong password": {"grace@example.test", "wrong-password"},
		"disabled":       {"ada@example.test", "Password-1"},
	} {
		_, err := svc.Login(ctx, creds[0], creds[1])
		if !errors.Is(err, service.ErrInvalidCredentials) {
			t.Errorf("%s: error = %v, want ErrInvalidCredentials", name, err)
		}
	}
}

// A login token verifies through the local verifier and carries the
// users.id subject with the stored roles.
func TestLoginTokenVerifies(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{})
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", []string{"viewer"})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	login, err := svc.Login(ctx, "ADA@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	verifier, err := auth.NewLocalAuthenticator(localSvcSecret, auth.LocalIssuerName, "beelinewf", time.Hour)
	if err != nil {
		t.Fatalf("NewLocalAuthenticator() error = %v", err)
	}
	p, err := verifier.Verify(ctx, login.Token)
	if err != nil {
		t.Fatalf("Verify(token) error = %v", err)
	}
	if p.Subject != created.ID || p.Issuer != model.LocalIssuer {
		t.Errorf("subject/issuer = %q/%q", p.Subject, p.Issuer)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", p.Roles)
	}
}

func TestRegisterClosedIsForbidden(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: false})
	if _, err := svc.Register(context.Background(), "A", "a@example.test", "Password-1"); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("error = %v, want ErrForbidden", err)
	}
}

func TestRegisterImmediateWhenVerificationOff(t *testing.T) {
	mailer := &fakeMail{}
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true, Mail: mailer})
	ctx := context.Background()

	got, err := svc.Register(ctx, "Ada", "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if got.PendingVerification || got.Token == "" {
		t.Errorf("pending/has-token = %v/%v, want an immediate session", got.PendingVerification, got.Token != "")
	}
	if !got.User.EmailVerified() {
		t.Error("EmailVerified() = false, want true")
	}
	if mailer.count() != 0 {
		t.Errorf("mails = %d, want none with verification off", mailer.count())
	}
	if _, err := svc.Login(ctx, "ada@example.test", "Password-1"); err != nil {
		t.Errorf("Login() after register error = %v", err)
	}
}

func TestRegisterAssignsDefaultRole(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true, DefaultRole: "viewer"})
	got, err := svc.Register(context.Background(), "Ada", "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if len(got.User.Roles) != 1 || got.User.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", got.User.Roles)
	}
}

func TestRegisterUnknownDefaultRoleIsInvalid(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true, DefaultRole: "wizard"})
	if _, err := svc.Register(context.Background(), "Ada", "ada@example.test", "Password-1"); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
}

func TestRegisterDuplicateEmailIsConflict(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true})
	ctx := context.Background()
	if _, err := svc.Register(ctx, "Ada", "ada@example.test", "Password-1"); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := svc.Register(ctx, "Ada 2", "ada@example.test", "Password-2"); !errors.Is(err, model.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
}

// The full pending journey: register holds the token, login refuses with
// the verified-only error, the mailed link verifies, and login works.
func TestRegisterPendingJourney(t *testing.T) {
	mailer := &fakeMail{}
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true, VerificationEnabled: true, Mail: mailer})
	ctx := context.Background()

	got, err := svc.Register(ctx, "Ada", "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if !got.PendingVerification || got.Token != "" {
		t.Fatalf("pending/token = %v/%q, want pending with no token", got.PendingVerification, got.Token)
	}
	if got.User.EmailVerified() {
		t.Fatal("EmailVerified() = true, want pending")
	}
	if _, err := svc.Login(ctx, "ada@example.test", "Password-1"); !errors.Is(err, service.ErrEmailNotVerified) {
		t.Fatalf("Login() error = %v, want ErrEmailNotVerified", err)
	}
	if mailer.count() != 1 {
		t.Fatalf("mails = %d, want 1", mailer.count())
	}
	link := mailer.last().link
	if !strings.HasPrefix(link, "https://app.example.test/verify-email?token=") {
		t.Fatalf("link = %q", link)
	}
	raw := strings.TrimPrefix(link, "https://app.example.test/verify-email?token=")
	if raw == "" {
		t.Fatal("link carries no token")
	}

	verified, err := svc.VerifyEmail(ctx, raw)
	if err != nil {
		t.Fatalf("VerifyEmail() error = %v", err)
	}
	if verified.Token == "" || verified.User.ID != got.User.ID {
		t.Errorf("verified = %+v", verified)
	}
	if _, err := svc.Login(ctx, "ada@example.test", "Password-1"); err != nil {
		t.Errorf("Login() after verify error = %v", err)
	}
	// Single-use: the consumed link never verifies twice.
	if _, err := svc.VerifyEmail(ctx, raw); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("second verify error = %v, want ErrInvalid", err)
	}
}

func TestVerifyEmailRejectsGarbage(t *testing.T) {
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true, VerificationEnabled: true, Mail: &fakeMail{}})
	for _, raw := range []string{"", "  ", "not-a-token"} {
		if _, err := svc.VerifyEmail(context.Background(), raw); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("VerifyEmail(%q) error = %v, want ErrInvalid", raw, err)
		}
	}
}

// A registration that cannot mail still registers: the account exists and
// the resend path is the recovery, not a retry into a 409 trap.
func TestRegisterMailFailureStillRegisters(t *testing.T) {
	mailer := &fakeMail{err: errors.New("relay down")}
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{AllowRegistration: true, VerificationEnabled: true, Mail: mailer})
	got, err := svc.Register(context.Background(), "Ada", "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v, want nil despite the mail failure", err)
	}
	if !got.PendingVerification {
		t.Error("PendingVerification = false, want pending")
	}
}

// The public resend always succeeds and only ever sends to a pending
// account inside its cooldown: unknown email, verified, disabled, and
// cooldown suppression are all silent.
func TestResendVerificationIsGeneric(t *testing.T) {
	mailer := &fakeMail{}
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{
		AllowRegistration: true, VerificationEnabled: true, Mail: mailer, ResendCooldown: time.Hour,
	})
	ctx := context.Background()

	if err := svc.ResendVerification(ctx, "nobody@example.test"); err != nil {
		t.Errorf("unknown email error = %v, want nil", err)
	}
	pending, err := svc.Register(ctx, "Ada", "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if mailer.count() != 1 {
		t.Fatalf("mails after register = %d, want 1", mailer.count())
	}
	// Inside the cooldown the resend is a silent no-op.
	if err := svc.ResendVerification(ctx, "ada@example.test"); err != nil {
		t.Errorf("cooldown resend error = %v, want nil", err)
	}
	if mailer.count() != 1 {
		t.Errorf("mails = %d, want still 1 inside the cooldown", mailer.count())
	}
	_ = pending

	verified, err := svc.CreateUser(ctx, "Grace", "grace@example.test", "Password-1", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	_ = verified
	if err := svc.ResendVerification(ctx, "grace@example.test"); err != nil {
		t.Errorf("verified resend error = %v, want nil", err)
	}
	if mailer.count() != 1 {
		t.Errorf("mails = %d, want still 1", mailer.count())
	}
}

func TestAdminResendVerification(t *testing.T) {
	mailer := &fakeMail{}
	svc, _, catalog := localSvcFor(t, service.LocalAuthDeps{
		AllowRegistration: true, VerificationEnabled: true, Mail: mailer, ResendCooldown: time.Hour,
	})
	ctx := context.Background()
	adminCtx := ctxFor(catalog, adminActor())

	pending, err := svc.Register(ctx, "Ada", "ada@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	// The admin path skips the cooldown: operator intent, not a flood.
	if err := svc.ResendVerificationForUser(adminCtx, adminActor(), pending.User.ID); err != nil {
		t.Fatalf("ResendVerificationForUser() error = %v", err)
	}
	if mailer.count() != 2 {
		t.Errorf("mails = %d, want 2", mailer.count())
	}
	verified, err := svc.CreateUser(ctx, "Grace", "grace@example.test", "Password-1", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if err := svc.ResendVerificationForUser(adminCtx, adminActor(), verified.ID); err != nil {
		t.Errorf("verified resend error = %v, want nil", err)
	}
	if mailer.count() != 2 {
		t.Errorf("mails = %d, want still 2", mailer.count())
	}
	// Unknown id is honest here: this is an admin API, not a probe.
	if err := svc.ResendVerificationForUser(adminCtx, adminActor(), localAdminID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown id error = %v, want ErrNotFound", err)
	}
	// Without the permission the call is forbidden.
	viewer := auth.Principal{Subject: "v-1", Issuer: authSvcIssuer, UserID: verified.ID, Roles: []string{"viewer"}}
	if err := svc.ResendVerificationForUser(ctxFor(catalog, viewer), viewer, pending.User.ID); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("viewer error = %v, want ErrForbidden", err)
	}
}

func TestUpdateUserAdmin(t *testing.T) {
	mailer := &fakeMail{}
	svc, _, catalog := localSvcFor(t, service.LocalAuthDeps{VerificationEnabled: true, Mail: mailer})
	ctx := context.Background()
	adminCtx := ctxFor(catalog, adminActor())

	created, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", []string{"viewer"})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	roles := []string{"admin"}
	updated, err := svc.UpdateUser(adminCtx, adminActor(), created.ID, service.UpdateUserParams{
		Name: strptr("Ada Renamed"), Roles: &roles, Password: strptr("Password-2"),
	})
	if err != nil {
		t.Fatalf("UpdateUser() error = %v", err)
	}
	if updated.Name != "Ada Renamed" || len(updated.Roles) != 1 || updated.Roles[0] != "admin" {
		t.Errorf("updated = %+v", updated)
	}
	if _, err := svc.Login(ctx, "ada@example.test", "Password-2"); err != nil {
		t.Errorf("Login() with rotated password error = %v", err)
	}
	// An email change re-pends the account and mails the new address.
	email := "ada.new@example.test"
	remailed, err := svc.UpdateUser(adminCtx, adminActor(), created.ID, service.UpdateUserParams{Email: &email})
	if err != nil {
		t.Fatalf("UpdateUser(email) error = %v", err)
	}
	if remailed.EmailVerified() {
		t.Error("EmailVerified() = true after an email change, want pending")
	}
	if mailer.count() != 1 || mailer.last().to != "ada.new@example.test" {
		t.Errorf("mails = %+v, want one to the new address", mailer.sends)
	}
	// The admin can verify by hand instead of waiting for the link.
	yes := true
	marked, err := svc.UpdateUser(adminCtx, adminActor(), created.ID, service.UpdateUserParams{EmailVerified: &yes})
	if err != nil {
		t.Fatalf("UpdateUser(verified) error = %v", err)
	}
	if !marked.EmailVerified() {
		t.Error("EmailVerified() = false, want true")
	}
}

func TestUpdateUserSelf(t *testing.T) {
	svc, _, catalog := localSvcFor(t, service.LocalAuthDeps{})
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", []string{"viewer"})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	self := auth.Principal{Subject: created.ID, Issuer: model.LocalIssuer, UserID: created.ID, Roles: []string{"viewer"}}
	selfCtx := ctxFor(catalog, self)

	updated, err := svc.UpdateUser(selfCtx, self, created.ID, service.UpdateUserParams{
		Name: strptr("Ada Me"), Password: strptr("Password-2"), CurrentPassword: strptr("Password-1"),
	})
	if err != nil {
		t.Fatalf("self UpdateUser() error = %v", err)
	}
	if updated.Name != "Ada Me" {
		t.Errorf("name = %q", updated.Name)
	}
	if _, err := svc.Login(ctx, "ada@example.test", "Password-2"); err != nil {
		t.Errorf("Login() with rotated password error = %v", err)
	}
	// A rotation must prove the current password.
	if _, err := svc.UpdateUser(selfCtx, self, created.ID, service.UpdateUserParams{
		Password: strptr("Password-3"), CurrentPassword: strptr("wrong"),
	}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("wrong current password error = %v, want ErrInvalid", err)
	}
	// Privilege fields are forbidden to the account itself.
	roles := []string{"admin"}
	if _, err := svc.UpdateUser(selfCtx, self, created.ID, service.UpdateUserParams{Roles: &roles}); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("self role grant error = %v, want ErrForbidden", err)
	}
	active := false
	if _, err := svc.UpdateUser(selfCtx, self, created.ID, service.UpdateUserParams{IsActive: &active}); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("self disable error = %v, want ErrForbidden", err)
	}
	// And another user's account is entirely out of reach.
	other, err := svc.CreateUser(ctx, "Grace", "grace@example.test", "Password-1", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if _, err := svc.UpdateUser(selfCtx, self, other.ID, service.UpdateUserParams{Name: strptr("x")}); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("cross-user edit error = %v, want ErrForbidden", err)
	}
}

func TestUpdateUserUnknownOrNonLocal(t *testing.T) {
	svc, _, catalog := localSvcFor(t, service.LocalAuthDeps{})
	adminCtx := ctxFor(catalog, adminActor())

	if _, err := svc.UpdateUser(adminCtx, adminActor(), "not-a-uuid", service.UpdateUserParams{}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("malformed id error = %v, want ErrInvalid", err)
	}
	if _, err := svc.UpdateUser(adminCtx, adminActor(), localAdminID, service.UpdateUserParams{}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown id error = %v, want ErrNotFound", err)
	}
	oidc, err := svc.ResolvePrincipal(context.Background(), auth.Principal{Subject: "o-1", Issuer: authSvcIssuer, Name: "O"})
	if err != nil {
		t.Fatalf("ResolvePrincipal() error = %v", err)
	}
	if _, err := svc.UpdateUser(adminCtx, adminActor(), oidc.UserID, service.UpdateUserParams{}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("OIDC row error = %v, want ErrInvalid", err)
	}
}

func TestDisableUser(t *testing.T) {
	svc, _, catalog := localSvcFor(t, service.LocalAuthDeps{})
	ctx := context.Background()
	adminCtx := ctxFor(catalog, adminActor())

	created, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	// A viewer cannot disable anyone.
	viewer := auth.Principal{Subject: created.ID, Issuer: model.LocalIssuer, UserID: created.ID, Roles: []string{"viewer"}}
	if _, err := svc.DisableUser(ctxFor(catalog, viewer), viewer, created.ID); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("viewer error = %v, want ErrForbidden", err)
	}
	disabled, err := svc.DisableUser(adminCtx, adminActor(), created.ID)
	if err != nil {
		t.Fatalf("DisableUser() error = %v", err)
	}
	if disabled.IsActive {
		t.Error("IsActive = true, want false")
	}
	if _, err := svc.Login(ctx, "ada@example.test", "Password-1"); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("login after disable error = %v, want ErrInvalidCredentials", err)
	}
	// Neither the system user nor the caller's own account disables.
	if _, err := svc.DisableUser(adminCtx, adminActor(), svcSysUserID); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("system disable error = %v, want ErrInvalid", err)
	}
	adminUser, err := svc.CreateUser(ctx, "Root", "root@example.test", "Password-1", []string{"admin"})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	selfAdmin := auth.Principal{Subject: adminUser.ID, Issuer: model.LocalIssuer, UserID: adminUser.ID, Roles: []string{"admin"}}
	if _, err := svc.DisableUser(ctxFor(catalog, selfAdmin), selfAdmin, adminUser.ID); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("self disable error = %v, want ErrInvalid", err)
	}
}

func TestResolvePrincipalLocal(t *testing.T) {
	mailer := &fakeMail{}
	svc, _, _ := localSvcFor(t, service.LocalAuthDeps{
		AllowRegistration: true, VerificationEnabled: true, Mail: mailer,
	})
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, "Ada", "ada@example.test", "Password-1", []string{"viewer"})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	p, err := svc.ResolvePrincipal(ctx, auth.Principal{
		Subject: created.ID, Issuer: model.LocalIssuer, Name: "Ada", Roles: []string{"viewer"},
	})
	if err != nil {
		t.Fatalf("ResolvePrincipal() error = %v", err)
	}
	if p.UserID != created.ID {
		t.Errorf("UserID = %q, want %q", p.UserID, created.ID)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want the token roles kept", p.Roles)
	}
	// Unknown, disabled, and pending principals fail closed.
	if _, err := svc.ResolvePrincipal(ctx, auth.Principal{Subject: localAdminID, Issuer: model.LocalIssuer}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("unknown error = %v, want ErrInvalid", err)
	}
	pending, err := svc.Register(ctx, "Pen", "pen@example.test", "Password-1")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := svc.ResolvePrincipal(ctx, auth.Principal{Subject: pending.User.ID, Issuer: model.LocalIssuer}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("pending error = %v, want ErrInvalid", err)
	}
}
