package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/mail"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/pkg/ids"
)

// ErrInvalidCredentials is a login that must not say why it failed:
// unknown email, wrong password, and disabled account share it, so the
// endpoint cannot probe for accounts. The handler maps it to 401.
var ErrInvalidCredentials = errors.New("service: invalid email or password")

// ErrEmailNotVerified is a login with the right password on a pending
// account. It is checked only after the password succeeds, so it never
// leaks account existence to password-guessers. The handler maps it to 403.
var ErrEmailNotVerified = errors.New("service: email not verified")

// LocalAuthDeps wires the local-user surface of the auth service. A nil
// Issuer disables every local operation: the routes are only registered
// when local auth is on, so that state is unreachable from HTTP and fails
// closed here.
type LocalAuthDeps struct {
	// Issuer mints login JWTs. Required for every local operation.
	Issuer *auth.LocalAuthenticator
	// Mail delivers verification links. Required only when verification
	// is on.
	Mail mail.Sender
	// AllowRegistration opens Register; otherwise it is ErrForbidden.
	AllowRegistration bool
	// DefaultRole is assigned to a self-registered user. Empty assigns
	// no roles.
	DefaultRole string
	// VerificationEnabled keeps a registered account pending until its
	// mailed token is consumed.
	VerificationEnabled bool
	// VerificationTokenTTL bounds a verification token.
	VerificationTokenTTL time.Duration
	// ResendCooldown is the per-email quiet period between sends.
	ResendCooldown time.Duration
	// PublicBaseURL is the frontend origin the verification link is
	// built from.
	PublicBaseURL string
}

// AuthServiceOption customizes NewAuthService.
type AuthServiceOption func(*authService)

// WithLocalAuth wires the local-user surface.
func WithLocalAuth(deps LocalAuthDeps) AuthServiceOption {
	return func(s *authService) { s.local = &deps }
}

// LoginResult is the authenticated session: the user row plus its JWT.
type LoginResult struct {
	User      model.User
	Token     string
	ExpiresAt time.Time
}

// RegisterResult is a self-registration: either a login session at once,
// or a pending account with no token until its email verifies.
type RegisterResult struct {
	User                model.User
	Token               string
	ExpiresAt           time.Time
	PendingVerification bool
}

// UpdateUserParams carries the editable fields of a local user. A nil
// field keeps the stored value; a non-nil Roles replaces the membership
// set (an empty list revokes every membership), and a non-nil,
// non-empty Password rotates the credential.
type UpdateUserParams struct {
	Name            *string
	Email           *string
	Roles           *[]string
	Password        *string
	CurrentPassword *string
	IsActive        *bool
	EmailVerified   *bool
}

// Password policy: 6..72 bytes (bcrypt refuses past 72), with at least
// one uppercase letter, one number, and one symbol. A symbol is any
// rune that is not a letter, a number, or whitespace.
const (
	minPasswordLength = 6
	maxPasswordLength = 72
)

// localDeps returns the local-auth wiring, or ErrForbidden when local
// auth is off: the routes are only registered when it is on, so reaching
// here disabled is a wiring bug, and a 403 fails closed.
func (s *authService) localDeps() (*LocalAuthDeps, error) {
	if s.local == nil || s.local.Issuer == nil {
		return nil, fmt.Errorf("%w: local auth is disabled", model.ErrForbidden)
	}
	return s.local, nil
}

// canWriteUsers reports whether the actor may administer local users: the
// service principal, or a caller whose resolved permissions hold
// users:write (explicitly or via the wildcard).
func canWriteUsers(ctx context.Context, actor auth.Principal) bool {
	if actor.Service {
		return true
	}
	perms, ok := auth.PermissionsFromContext(ctx)
	if !ok {
		return false
	}
	return perms[auth.ActionUsersWrite] || perms[auth.WildcardAction]
}

// normalizeEmail lowercases and trims a login identifier. Local emails
// are stored normalized, so the unique index is effectively
// case-insensitive.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validEmail is deliberately shallow: local@domain with no spaces. Proof
// of mailbox is the verification mail, not the shape check. CR and LF are
// rejected outright: the address is interpolated into SMTP envelopes and
// headers, where a newline would inject commands.
func validEmail(email string) bool {
	if email == "" || strings.ContainsAny(email, " \t\n\r") {
		return false
	}
	local, domain, found := strings.Cut(email, "@")
	return found && local != "" && domain != "" && !strings.Contains(domain, "@")
}

func validatePassword(password string) error {
	if len(password) < minPasswordLength || len(password) > maxPasswordLength {
		return fmt.Errorf("%w: password must be %d..%d characters", model.ErrInvalid, minPasswordLength, maxPasswordLength)
	}
	var upper, number, symbol bool
	for _, r := range password {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			number = true
		case !unicode.IsLetter(r) && !unicode.IsSpace(r):
			symbol = true
		}
	}
	if !upper || !number || !symbol {
		return fmt.Errorf("%w: password must contain at least one uppercase letter, one number, and one symbol", model.ErrInvalid)
	}
	return nil
}

// validateLocalRoles trims, dedupes, and sorts a membership set and
// rejects any role the catalog does not know: a member of an unknown role
// would silently hold nothing.
func (s *authService) validateLocalRoles(roles []string) ([]string, error) {
	grants, err := normalizeRoleActions(roles)
	if err != nil {
		return nil, err
	}
	for _, role := range grants {
		if !s.catalog.HasRole(role) {
			return nil, fmt.Errorf("%w: role %q does not exist", model.ErrInvalid, role)
		}
	}
	return grants, nil
}

func sameRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, role := range a {
		seen[role] = true
	}
	for _, role := range b {
		if !seen[role] {
			return false
		}
	}
	return true
}

// CreateUser inserts a local user. Admin-created accounts verify at
// write: only self-registration can pend on a mailbox.
func (s *authService) CreateUser(ctx context.Context, name, email, password string, roles []string) (model.User, error) {
	if _, err := s.localDeps(); err != nil {
		return model.User{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return model.User{}, fmt.Errorf("%w: name is required", model.ErrInvalid)
	}
	email = normalizeEmail(email)
	if !validEmail(email) {
		return model.User{}, fmt.Errorf("%w: a valid email is required", model.ErrInvalid)
	}
	if err := validatePassword(password); err != nil {
		return model.User{}, err
	}
	grants, err := s.validateLocalRoles(roles)
	if err != nil {
		return model.User{}, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return model.User{}, fmt.Errorf("service: create user: hash password: %w", err)
	}
	now := s.now()
	id := mustNewID()
	created, err := s.users.Create(ctx, model.User{
		ID: id, Subject: id, Issuer: model.LocalIssuer,
		Name: name, Email: email,
		PasswordHash: hash, IsActive: true, EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}, grants)
	if err != nil {
		return model.User{}, fmt.Errorf("service: create user: %w", err)
	}
	return created, nil
}

// UpdateUser replaces a local user's profile. Writers (service or
// users:write) edit every field; the user itself edits name, email, and
// password only, proving the current password for a rotation. An email
// change re-pends the account while verification is on, unless the writer
// explicitly verifies in the same call.
func (s *authService) UpdateUser(ctx context.Context, actor auth.Principal, id string, upd UpdateUserParams) (model.User, error) {
	deps, err := s.localDeps()
	if err != nil {
		return model.User{}, err
	}
	if !ids.Valid(id) {
		return model.User{}, fmt.Errorf("%w: user id %q is not a valid uuid", model.ErrInvalid, id)
	}
	current, err := s.users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return model.User{}, fmt.Errorf("%w: user %s", model.ErrNotFound, id)
		}
		return model.User{}, err
	}
	if !current.IsLocal() {
		return model.User{}, fmt.Errorf("%w: user %s is not a local user", model.ErrInvalid, id)
	}
	writer := canWriteUsers(ctx, actor)
	self := !writer && actor.UserID != "" && actor.UserID == id
	if !writer && !self {
		return model.User{}, fmt.Errorf("%w: the users:write permission is required", model.ErrForbidden)
	}
	if self {
		if upd.Roles != nil {
			// Normalized before comparing, so echoing the current
			// membership with different spacing still reads as no
			// change; anything else is a privilege change.
			grants, err := s.validateLocalRoles(*upd.Roles)
			if err != nil || !sameRoles(grants, current.Roles) {
				return model.User{}, fmt.Errorf("%w: only a users:write holder may change roles", model.ErrForbidden)
			}
		}
		if upd.IsActive != nil && *upd.IsActive != current.IsActive {
			return model.User{}, fmt.Errorf("%w: only a users:write holder may change active state", model.ErrForbidden)
		}
		if upd.EmailVerified != nil && *upd.EmailVerified != current.EmailVerified() {
			return model.User{}, fmt.Errorf("%w: only a users:write holder may change verification", model.ErrForbidden)
		}
	}

	next := current
	if upd.Name != nil {
		name := strings.TrimSpace(*upd.Name)
		if name == "" {
			return model.User{}, fmt.Errorf("%w: name is required", model.ErrInvalid)
		}
		next.Name = name
	}
	emailChanged := false
	if upd.Email != nil {
		email := normalizeEmail(*upd.Email)
		if !validEmail(email) {
			return model.User{}, fmt.Errorf("%w: a valid email is required", model.ErrInvalid)
		}
		if email != current.Email {
			next.Email = email
			emailChanged = true
		}
	}
	roles := current.Roles
	if writer && upd.Roles != nil {
		roles, err = s.validateLocalRoles(*upd.Roles)
		if err != nil {
			return model.User{}, err
		}
	}
	if upd.Password != nil && *upd.Password != "" {
		if err := validatePassword(*upd.Password); err != nil {
			return model.User{}, err
		}
		if self {
			if upd.CurrentPassword == nil || !auth.CheckPassword(current.PasswordHash, *upd.CurrentPassword) {
				return model.User{}, fmt.Errorf("%w: current password is incorrect", model.ErrInvalid)
			}
		}
		hash, err := auth.HashPassword(*upd.Password)
		if err != nil {
			return model.User{}, fmt.Errorf("service: update user: hash password: %w", err)
		}
		next.PasswordHash = hash
	}
	if writer && upd.IsActive != nil {
		next.IsActive = *upd.IsActive
	}
	// Explicit verification wins over an email change in the same call:
	// operator intent beats the re-pend. The verdict rides inside the
	// single Update below, which clears the token in the same statement,
	// so a crash cannot pair a new verdict with a live old link.
	explicitVerified := writer && upd.EmailVerified != nil
	if explicitVerified {
		if *upd.EmailVerified {
			now := s.now()
			next.EmailVerifiedAt = &now
		} else {
			next.EmailVerifiedAt = nil
		}
	} else if emailChanged {
		if deps.VerificationEnabled {
			next.EmailVerifiedAt = nil
		} else {
			now := s.now()
			next.EmailVerifiedAt = &now
		}
	}
	next.UpdatedAt = s.now()
	updated, err := s.users.Update(ctx, next, roles)
	if err != nil {
		return model.User{}, fmt.Errorf("service: update user: %w", err)
	}
	if !explicitVerified && emailChanged && deps.VerificationEnabled {
		if err := s.issueVerification(ctx, deps, id, next.Email); err != nil {
			slog.Warn("auth: verification mail failed after an email change; the resend path recovers", "user_id", id, "error", err)
		}
	}
	return updated, nil
}

// DisableUser soft-deletes a local user. Both the system account, which
// keeps the audit trail anchored, and the caller's own account refuse,
// so an admin cannot lock themselves out.
func (s *authService) DisableUser(ctx context.Context, actor auth.Principal, id string) (model.User, error) {
	if _, err := s.localDeps(); err != nil {
		return model.User{}, err
	}
	if !canWriteUsers(ctx, actor) {
		return model.User{}, fmt.Errorf("%w: the users:write permission is required", model.ErrForbidden)
	}
	if !ids.Valid(id) {
		return model.User{}, fmt.Errorf("%w: user id %q is not a valid uuid", model.ErrInvalid, id)
	}
	if id == s.systemUserID {
		return model.User{}, fmt.Errorf("%w: the system user cannot be disabled", model.ErrInvalid)
	}
	if actor.UserID != "" && actor.UserID == id {
		return model.User{}, fmt.Errorf("%w: an account cannot disable itself", model.ErrInvalid)
	}
	current, err := s.users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return model.User{}, fmt.Errorf("%w: user %s", model.ErrNotFound, id)
		}
		return model.User{}, err
	}
	if !current.IsLocal() {
		return model.User{}, fmt.Errorf("%w: user %s is not a local user", model.ErrInvalid, id)
	}
	disabled, err := s.users.Disable(ctx, id, s.now())
	if err != nil {
		return model.User{}, fmt.Errorf("service: disable user: %w", err)
	}
	return disabled, nil
}

// Register creates a self-registered account. With verification on the
// account pends and the result carries no token; the mailed link is the
// only way forward.
func (s *authService) Register(ctx context.Context, name, email, password string) (RegisterResult, error) {
	deps, err := s.localDeps()
	if err != nil {
		return RegisterResult{}, err
	}
	if !deps.AllowRegistration {
		return RegisterResult{}, fmt.Errorf("%w: registration is disabled", model.ErrForbidden)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return RegisterResult{}, fmt.Errorf("%w: name is required", model.ErrInvalid)
	}
	email = normalizeEmail(email)
	if !validEmail(email) {
		return RegisterResult{}, fmt.Errorf("%w: a valid email is required", model.ErrInvalid)
	}
	if err := validatePassword(password); err != nil {
		return RegisterResult{}, err
	}
	var grants []string
	if deps.DefaultRole != "" {
		grants, err = s.validateLocalRoles([]string{deps.DefaultRole})
		if err != nil {
			return RegisterResult{}, err
		}
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("service: register: hash password: %w", err)
	}
	now := s.now()
	u := model.User{
		ID: "", Name: name, Email: email,
		PasswordHash: hash, IsActive: true,
		CreatedAt: now, UpdatedAt: now,
	}
	u.ID = mustNewID()
	u.Subject = u.ID
	u.Issuer = model.LocalIssuer
	if !deps.VerificationEnabled {
		u.EmailVerifiedAt = &now
	}
	created, err := s.users.Create(ctx, u, grants)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("service: register: %w", err)
	}
	if !deps.VerificationEnabled {
		token, expires, err := deps.Issuer.Issue(created.ID, created.Name, created.Email, created.Roles)
		if err != nil {
			return RegisterResult{}, fmt.Errorf("service: register: issue token: %w", err)
		}
		return RegisterResult{User: created, Token: token, ExpiresAt: expires}, nil
	}
	// The account exists already; a mail failure must not roll it back
	// into a retry-duplicate trap. The resend path is the recovery.
	if err := s.issueVerification(ctx, deps, created.ID, created.Email); err != nil {
		slog.Warn("auth: verification mail failed after register; the resend path recovers", "user_id", created.ID, "error", err)
	}
	return RegisterResult{User: created, PendingVerification: true}, nil
}

// Login authenticates by email and password. Unknown email, wrong
// password, and disabled account share ErrInvalidCredentials; the mailbox
// check runs only after the password succeeds.
func (s *authService) Login(ctx context.Context, email, password string) (LoginResult, error) {
	deps, err := s.localDeps()
	if err != nil {
		return LoginResult{}, err
	}
	email = normalizeEmail(email)
	user, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			auth.DummyPasswordCompare(password)
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, err
	}
	if !auth.CheckPassword(user.PasswordHash, password) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if !user.IsActive {
		return LoginResult{}, ErrInvalidCredentials
	}
	if deps.VerificationEnabled && !user.EmailVerified() {
		return LoginResult{}, ErrEmailNotVerified
	}
	token, expires, err := deps.Issuer.Issue(user.ID, user.Name, user.Email, user.Roles)
	if err != nil {
		return LoginResult{}, fmt.Errorf("service: login: issue token: %w", err)
	}
	return LoginResult{User: user, Token: token, ExpiresAt: expires}, nil
}

// VerifyEmail consumes a verification token and returns a login session,
// so the frontend needs no second call after the link.
func (s *authService) VerifyEmail(ctx context.Context, rawToken string) (LoginResult, error) {
	deps, err := s.localDeps()
	if err != nil {
		return LoginResult{}, err
	}
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return LoginResult{}, fmt.Errorf("%w: verification token is required", model.ErrInvalid)
	}
	user, err := s.users.ConsumeVerificationToken(ctx, auth.HashVerificationToken(rawToken), s.now())
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return LoginResult{}, fmt.Errorf("%w: invalid verification link", model.ErrInvalid)
		}
		if errors.Is(err, repository.ErrVerificationExpired) {
			return LoginResult{}, fmt.Errorf("%w: verification link expired; request a new one", model.ErrInvalid)
		}
		return LoginResult{}, err
	}
	if !user.IsActive {
		return LoginResult{}, ErrInvalidCredentials
	}
	token, expires, err := deps.Issuer.Issue(user.ID, user.Name, user.Email, user.Roles)
	if err != nil {
		return LoginResult{}, fmt.Errorf("service: verify email: issue token: %w", err)
	}
	return LoginResult{User: user, Token: token, ExpiresAt: expires}, nil
}

// ResendVerification mails a fresh link to a pending account. Every other
// case — unknown email, verified, disabled, cooling down, verification
// off — is a silent success, so the endpoint cannot probe for accounts.
func (s *authService) ResendVerification(ctx context.Context, email string) error {
	deps, err := s.localDeps()
	if err != nil {
		return err
	}
	if !deps.VerificationEnabled {
		return nil
	}
	email = normalizeEmail(email)
	if email == "" {
		return nil
	}
	user, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		// Only a missing row is generic: a database failure must
		// surface as a 500 rather than masquerade as success.
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil
		}
		return fmt.Errorf("service: resend verification: %w", err)
	}
	if user.EmailVerified() || !user.IsActive {
		return nil
	}
	if user.VerificationSentAt != nil && s.now().Sub(*user.VerificationSentAt) < deps.ResendCooldown {
		return nil
	}
	if err := s.issueVerification(ctx, deps, user.ID, user.Email); err != nil {
		slog.Warn("auth: verification resend failed", "user_id", user.ID, "error", err)
	}
	return nil
}

// ResendVerificationForUser is the admin variant: an unknown id is
// ErrNotFound and the cooldown does not apply, but a verified account is
// still a silent success.
func (s *authService) ResendVerificationForUser(ctx context.Context, actor auth.Principal, id string) error {
	deps, err := s.localDeps()
	if err != nil {
		return err
	}
	if !canWriteUsers(ctx, actor) {
		return fmt.Errorf("%w: the users:write permission is required", model.ErrForbidden)
	}
	if !deps.VerificationEnabled {
		return fmt.Errorf("%w: email verification is disabled", model.ErrInvalid)
	}
	if !ids.Valid(id) {
		return fmt.Errorf("%w: user id %q is not a valid uuid", model.ErrInvalid, id)
	}
	user, err := s.users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return fmt.Errorf("%w: user %s", model.ErrNotFound, id)
		}
		return err
	}
	if !user.IsLocal() {
		return fmt.Errorf("%w: user %s is not a local user", model.ErrInvalid, id)
	}
	if user.EmailVerified() {
		return nil
	}
	if !user.IsActive {
		return fmt.Errorf("%w: user %s is disabled", model.ErrInvalid, id)
	}
	if err := s.issueVerification(ctx, deps, user.ID, user.Email); err != nil {
		return fmt.Errorf("service: resend verification: %w", err)
	}
	return nil
}

// issueVerification records a fresh token and mails its link. Only the
// newest link works; the raw token never touches the database.
func (s *authService) issueVerification(ctx context.Context, deps *LocalAuthDeps, id, email string) error {
	raw, hash, err := auth.GenerateVerificationToken()
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.users.SetVerificationToken(ctx, id, hash, now.Add(deps.VerificationTokenTTL), now); err != nil {
		return err
	}
	if deps.Mail == nil {
		return nil
	}
	link := strings.TrimRight(deps.PublicBaseURL, "/") + "/verify-email?token=" + raw
	return deps.Mail.SendVerification(ctx, email, link, deps.VerificationTokenTTL)
}

// resolveLocalPrincipal maps a verified local JWT onto its users.id uuid.
// The token already carries the stored roles, so this only gates on live
// state: the account must exist, stay active, and (while verification is
// on) stay verified.
func (s *authService) resolveLocalPrincipal(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	deps, err := s.localDeps()
	if err != nil {
		return p, fmt.Errorf("service: resolve principal: %w", err)
	}
	user, err := s.users.GetByID(ctx, p.Subject)
	if err != nil {
		// Only a missing row is an authentication failure; anything
		// else (a database error) must surface as a 500, like the
		// OIDC upsert path.
		if errors.Is(err, repository.ErrUserNotFound) {
			return p, fmt.Errorf("service: resolve principal: principal is not resolved to a user: %w", model.ErrInvalid)
		}
		return p, fmt.Errorf("service: resolve principal: %w", err)
	}
	if !user.IsActive {
		return p, fmt.Errorf("service: resolve principal: account is disabled: %w", model.ErrInvalid)
	}
	if deps.VerificationEnabled && !user.EmailVerified() {
		return p, fmt.Errorf("service: resolve principal: email is not verified: %w", model.ErrInvalid)
	}
	p.UserID = user.ID
	return p, nil
}
