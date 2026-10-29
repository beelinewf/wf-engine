package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/pkg/ids"
)

// AuthService resolves authenticated identities onto user rows, seeds the
// role tables from configuration on a first boot, and reads and writes them
// afterwards. Every committed write refreshes the shared in-memory catalog
// the request gates consult. Authorization itself is decided from the live
// token claims by the auth package; nothing here grants access.
type AuthService interface {
	// ResolvePrincipal maps a verified principal onto its users.id uuid,
	// inserting the identity on first sight. A service principal already
	// carries the configured system user id and is returned unchanged.
	ResolvePrincipal(ctx context.Context, p auth.Principal) (auth.Principal, error)
	// SeedRoles writes the shared in-memory catalog into the role and
	// role_permissions tables. It runs only while the tables are empty;
	// afterwards the tables are the source of truth and the API owns them.
	SeedRoles(ctx context.Context) error
	// ListRoles returns every role with its permissions.
	ListRoles(ctx context.Context) ([]model.Role, map[string][]string, error)
	// GetRole returns one role with its permissions.
	GetRole(ctx context.Context, name string) (model.Role, []string, error)
	// CreateRole inserts one role. A blank name is ErrInvalid, a duplicate
	// is ErrConflict. A nil or blank description is derived; permissions
	// are trimmed, deduplicated, and sorted, and a blank entry is
	// ErrInvalid.
	CreateRole(ctx context.Context, name string, description *string, actions []string) (model.Role, []string, error)
	// UpdateRole fully replaces a role's permissions and, unless the
	// description is omitted, its description. An unknown name is
	// ErrNotFound; stripping roles:write from the last holder is
	// ErrConflict and rolls back.
	UpdateRole(ctx context.Context, name string, description *string, actions []string) (model.Role, []string, error)
	// DeleteRole removes a role. An unknown name is ErrNotFound; deleting
	// the last roles:write holder is ErrConflict and rolls back.
	DeleteRole(ctx context.Context, name string) error
	// RefreshCatalog reloads the shared in-memory catalog from the role
	// tables: the startup load, the post-write swap, and the background
	// refresh share it.
	RefreshCatalog(ctx context.Context) error
	// Me returns the identity row behind a principal, for GET /v1/auth/me.
	Me(ctx context.Context, p auth.Principal) (model.User, error)
	// ListUsers returns one page of users matching q, with the total
	// matching row count, for GET /v1/users.
	ListUsers(ctx context.Context, q repository.UserListQuery) ([]model.User, int64, error)
	// GetUser returns one user by id, for GET /v1/users/{id}.
	GetUser(ctx context.Context, id string) (model.User, error)
	// CreateUser inserts a local user with a password credential and the
	// given roles, for POST /v1/users. Roles must name catalog roles; a
	// duplicate email is ErrConflict.
	CreateUser(ctx context.Context, name, email, password string, roles []string) (model.User, error)
	// UpdateUser replaces a local user's profile, for PUT
	// /v1/users/{id}. A caller with users:write edits every field; the
	// user itself edits name, email, and password only (a password
	// rotation proves the current one) and any other change is
	// ErrForbidden.
	UpdateUser(ctx context.Context, actor auth.Principal, id string, upd UpdateUserParams) (model.User, error)
	// DisableUser soft-deletes a local user, for DELETE /v1/users/{id}.
	// The system user and the caller's own account refuse with ErrInvalid.
	DisableUser(ctx context.Context, actor auth.Principal, id string) (model.User, error)
	// Register creates a self-registered account, for POST
	// /v1/auth/register. With email verification on the account stays
	// pending and the result carries no token; otherwise it carries a
	// login token at once.
	Register(ctx context.Context, name, email, password string) (RegisterResult, error)
	// Login authenticates a local user by email and password, for POST
	// /v1/auth/login. Unknown email, wrong password, and disabled
	// account all answer ErrInvalidCredentials; a pending account answers
	// ErrEmailNotVerified.
	Login(ctx context.Context, email, password string) (LoginResult, error)
	// VerifyEmail consumes an email verification token, for POST
	// /v1/auth/verify-email, and returns a login token for the verified
	// account.
	VerifyEmail(ctx context.Context, rawToken string) (LoginResult, error)
	// ResendVerification mails a fresh verification link, for POST
	// /v1/auth/resend-verification. It always succeeds: unknown email,
	// verified account, disabled account, and cooldown suppression are
	// all silent, so the endpoint cannot probe for accounts.
	ResendVerification(ctx context.Context, email string) error
	// ResendVerificationForUser is the admin variant, for POST
	// /v1/users/{id}/resend-verification: unknown id is ErrNotFound and
	// a verified account is a silent success.
	ResendVerificationForUser(ctx context.Context, actor auth.Principal, id string) error
}

type authService struct {
	users   repository.UserRepository
	roles   repository.RoleRepository
	catalog *auth.Catalog
	// systemUserID is the audit actor a service principal acts as.
	systemUserID string
	// local wires the local-user surface; nil disables it.
	local *LocalAuthDeps
	// now is injectable so tests get deterministic seed timestamps.
	now func() time.Time
}

// NewAuthService builds the auth service. systemUserID is the configured
// system user the API-token and broker bypasses act as.
func NewAuthService(
	users repository.UserRepository,
	roles repository.RoleRepository,
	catalog *auth.Catalog,
	systemUserID string,
	opts ...AuthServiceOption,
) AuthService {
	s := &authService{
		users:        users,
		roles:        roles,
		catalog:      catalog,
		systemUserID: systemUserID,
		now:          nowUTC,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *authService) ResolvePrincipal(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	if p.Service {
		// The bypass always acts as the configured system user, so the
		// audit trail of a service call is the same as an engine-side
		// effect.
		p.UserID = s.systemUserID
		return p, nil
	}
	if p.Issuer == model.LocalIssuer {
		// Local JWTs resolve to their stored row instead of upserting:
		// the account already exists, and this is where a disabled or
		// unverified account stops an otherwise valid token.
		return s.resolveLocalPrincipal(ctx, p)
	}
	// Failing closed: (subject, issuer) is the identity key, so a
	// principal missing either cannot be persisted. Writing it anyway would
	// mint a row that every later anonymous request collides on, and would
	// attribute audit rows to a user id nobody owns.
	if p.Subject == "" || p.Issuer == "" {
		return p, fmt.Errorf("service: resolve principal: missing subject or issuer: %w", model.ErrInvalid)
	}
	now := s.now()
	id, err := s.users.UpsertBySubject(ctx, model.User{
		// The uuid is allocated before the write so a retry after a failed
		// upsert resolves to the same row instead of forking the identity.
		ID:       mustNewID(),
		Subject:  p.Subject,
		Issuer:   p.Issuer,
		Name:     p.Name,
		Email:    p.Email,
		Metadata: auth.MarshalPrincipalUser(p),
		IsActive: true,
		// An OIDC caller proved its mailbox to the provider, so the
		// cached row verifies at write; only a pending self-registration
		// is ever unverified.
		EmailVerifiedAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		return p, fmt.Errorf("service: resolve principal: %w", err)
	}
	p.UserID = id
	return p, nil
}

func (s *authService) SeedRoles(ctx context.Context) error {
	seed := s.catalog.Seed()
	if len(seed.Roles) == 0 {
		return nil
	}
	now := s.now()
	roles := make([]model.Role, 0, len(seed.Roles))
	permissions := make([]model.RolePermission, 0, len(seed.Roles))
	for _, name := range seed.Roles {
		roles = append(roles, model.Role{
			Name:        name,
			Description: roleDescription(name),
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		for _, action := range seed.Permissions[name] {
			permissions = append(permissions, model.RolePermission{
				Role:      name,
				Action:    action,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}
	}
	if err := s.roles.Seed(ctx, roles, permissions); err != nil {
		return fmt.Errorf("service: seed roles: %w", err)
	}
	return nil
}

// ListRoles serves the role tables, which are the source of truth every
// committed write refreshes the shared catalog from, so the read API and the
// request gates never diverge. The timestamps are the real row timestamps.
func (s *authService) ListRoles(ctx context.Context) ([]model.Role, map[string][]string, error) {
	roles, perms, err := s.roles.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("service: list roles: %w", err)
	}
	if perms == nil {
		perms = make(map[string][]string, len(roles))
	}
	for _, role := range roles {
		if perms[role.Name] == nil {
			perms[role.Name] = []string{}
		}
	}
	return roles, perms, nil
}

// GetRole serves one row of the role tables. A blank name is ErrInvalid and
// an unknown name is ErrNotFound, so the handler answers 422 and 404.
func (s *authService) GetRole(ctx context.Context, name string) (model.Role, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.Role{}, nil, fmt.Errorf("%w: role name is required", model.ErrInvalid)
	}
	role, actions, err := s.roles.Get(ctx, name)
	if err != nil {
		if errors.Is(err, repository.ErrRoleNotFound) {
			return model.Role{}, nil, fmt.Errorf("%w: role %q", model.ErrNotFound, name)
		}
		return model.Role{}, nil, fmt.Errorf("service: get role: %w", err)
	}
	if actions == nil {
		actions = []string{}
	}
	return role, actions, nil
}

// CreateRole inserts one role and refreshes the shared catalog from the
// committed tables, so the new grants authorize immediately. A blank name is
// ErrInvalid and a duplicate is ErrConflict.
func (s *authService) CreateRole(ctx context.Context, name string, description *string, actions []string) (model.Role, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.Role{}, nil, fmt.Errorf("%w: role name is required", model.ErrInvalid)
	}
	grants, err := normalizeRoleActions(actions)
	if err != nil {
		return model.Role{}, nil, err
	}
	warnUnknownRoleActions(name, grants)
	desc := ""
	if description != nil {
		desc = strings.TrimSpace(*description)
	}
	if desc == "" {
		desc = roleDescription(name)
	}
	now := s.now()
	role, err := s.roles.Create(ctx, model.Role{
		Name: name, Description: desc, CreatedAt: now, UpdatedAt: now,
	}, grants)
	if err != nil {
		return model.Role{}, nil, fmt.Errorf("service: create role %q: %w", name, err)
	}
	if err := s.refreshAfterWrite(ctx, fmt.Sprintf("create role %q", name)); err != nil {
		return model.Role{}, nil, err
	}
	return role, grants, nil
}

// UpdateRole fully replaces a role's permission list: an empty list revokes
// every grant. An omitted description keeps the stored one. An unknown name
// is ErrNotFound; stripping the last roles:write holder is ErrConflict and
// rolls back, leaving the catalog untouched.
func (s *authService) UpdateRole(ctx context.Context, name string, description *string, actions []string) (model.Role, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.Role{}, nil, fmt.Errorf("%w: role name is required", model.ErrInvalid)
	}
	grants, err := normalizeRoleActions(actions)
	if err != nil {
		return model.Role{}, nil, err
	}
	warnUnknownRoleActions(name, grants)
	existing, _, err := s.roles.Get(ctx, name)
	if err != nil {
		if errors.Is(err, repository.ErrRoleNotFound) {
			return model.Role{}, nil, fmt.Errorf("%w: role %q", model.ErrNotFound, name)
		}
		return model.Role{}, nil, fmt.Errorf("service: get role: %w", err)
	}
	desc := existing.Description
	if description != nil {
		desc = strings.TrimSpace(*description)
	}
	role, err := s.roles.Update(ctx, model.Role{
		Name: name, Description: desc, UpdatedAt: s.now(),
	}, grants)
	if err != nil {
		if errors.Is(err, repository.ErrRoleNotFound) {
			return model.Role{}, nil, fmt.Errorf("%w: role %q", model.ErrNotFound, name)
		}
		return model.Role{}, nil, fmt.Errorf("service: update role %q: %w", name, err)
	}
	if err := s.refreshAfterWrite(ctx, fmt.Sprintf("update role %q", name)); err != nil {
		return model.Role{}, nil, err
	}
	return role, grants, nil
}

// DeleteRole removes a role and refreshes the shared catalog, so the grants
// stop authorizing at once. An unknown name is ErrNotFound; deleting the
// last roles:write holder is ErrConflict and rolls back. Tokens and input
// nodes naming the deleted role keep resolving the name, it simply grants
// nothing, exactly like a role the catalog never knew.
func (s *authService) DeleteRole(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: role name is required", model.ErrInvalid)
	}
	if err := s.roles.Delete(ctx, name); err != nil {
		if errors.Is(err, repository.ErrRoleNotFound) {
			return fmt.Errorf("%w: role %q", model.ErrNotFound, name)
		}
		return fmt.Errorf("service: delete role %q: %w", name, err)
	}
	return s.refreshAfterWrite(ctx, fmt.Sprintf("delete role %q", name))
}

// RefreshCatalog reloads the shared in-memory catalog from the role tables:
// the startup load when the tables are already seeded, the post-write swap
// after every committed role change, and the background refresh that bounds
// cross-replica staleness.
func (s *authService) RefreshCatalog(ctx context.Context) error {
	roles, perms, err := s.roles.List(ctx)
	if err != nil {
		return fmt.Errorf("service: refresh catalog: %w", err)
	}
	if perms == nil {
		perms = make(map[string][]string, len(roles))
	}
	// Roles with no grants keep no permission rows, but they are still
	// catalog roles: an empty grant set must replace, not drop, them.
	for _, role := range roles {
		if _, ok := perms[role.Name]; !ok {
			perms[role.Name] = []string{}
		}
	}
	s.catalog.Replace(perms)
	return nil
}

// refreshAfterWrite reloads the shared catalog after a committed write. A
// reload failure is a plain error, so the handler answers 500: the write is
// committed, but this replica may now authorize from a stale catalog.
func (s *authService) refreshAfterWrite(ctx context.Context, what string) error {
	if err := s.RefreshCatalog(ctx); err != nil {
		return fmt.Errorf("service: %s committed but the catalog refresh failed: %w", what, err)
	}
	return nil
}

// normalizeRoleActions trims, dedupes, and sorts a permission list. A blank
// entry is ErrInvalid: silently dropping it would store a grant set the
// caller did not ask for. Unknown actions stay, as the catalog allows: they
// resolve but never match a gate, and the write warns about them.
func normalizeRoleActions(actions []string) ([]string, error) {
	seen := make(map[string]bool, len(actions))
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		action = strings.TrimSpace(action)
		if action == "" {
			return nil, fmt.Errorf("%w: permission entries must not be blank", model.ErrInvalid)
		}
		if seen[action] {
			continue
		}
		seen[action] = true
		out = append(out, action)
	}
	sort.Strings(out)
	return out, nil
}

// warnUnknownRoleActions logs the same warning warnAboutCatalog emits at
// boot: an unknown action is accepted but grants nothing, so a typo would
// otherwise be invisible to the operator who wrote it.
func warnUnknownRoleActions(role string, actions []string) {
	known := make(map[string]bool, len(auth.KnownActions()))
	for _, action := range auth.KnownActions() {
		known[action] = true
	}
	for _, action := range actions {
		if !known[action] && action != auth.WildcardAction {
			slog.Warn("auth: role grants unknown action; no route gates on it", "role", role, "action", action)
		}
	}
}

// Me returns the identity row behind a principal, for GET /v1/auth/me.
// A principal that never resolved to a user row is a credential problem,
// not a missing resource: ErrInvalid so the handler answers 401, because a
// caller that cannot name a user is not a 404 for a user the client asked
// about.
func (s *authService) Me(ctx context.Context, p auth.Principal) (model.User, error) {
	if p.UserID == "" {
		return model.User{}, fmt.Errorf("%w: principal is not resolved to a user", model.ErrInvalid)
	}
	user, err := s.users.GetByID(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return model.User{}, fmt.Errorf("%w: user %s", model.ErrNotFound, p.UserID)
		}
		return model.User{}, err
	}
	return user, nil
}

// ListUsers is a pass-through onto the users table: unlike roles, users are
// rows rather than catalog entries, so there is no live configuration to
// answer from.
func (s *authService) ListUsers(ctx context.Context, q repository.UserListQuery) ([]model.User, int64, error) {
	return s.users.List(ctx, q)
}

// GetUser returns one user by id. A malformed id is ErrInvalid rather than a
// missing row: the repository answers ErrUserNotFound for both, so the shape
// is checked here before the lookup. A well-formed id that matches no row is
// ErrNotFound.
func (s *authService) GetUser(ctx context.Context, id string) (model.User, error) {
	if !ids.Valid(id) {
		return model.User{}, fmt.Errorf("%w: user id %q is not a valid uuid", model.ErrInvalid, id)
	}
	user, err := s.users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return model.User{}, fmt.Errorf("%w: user %s", model.ErrNotFound, id)
		}
		return model.User{}, err
	}
	return user, nil
}

// roleDescription derives the description of a role created without one.
// A role is an operator-facing bundle of resource-actions; the API accepts
// an authored description, and this is the fallback when it is omitted.
func roleDescription(name string) string {
	return "Configured role " + name
}
