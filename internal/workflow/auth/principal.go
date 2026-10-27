// Package auth resolves the caller of a request into a Principal and answers
// authorization questions about it. It is transport-independent: the Gin
// middleware only moves the resolved Principal in and out of the request
// context, so the same rules apply to a bearer token, an API token, and the
// broker consumers that never carry a credential at all.
package auth

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/beelinewf/wf-engine/pkg/configuration"
)

// Context keys used to carry the principal and the resolved permissions
// through a request context.
type contextKey int

const (
	principalContextKey contextKey = iota
	permissionsContextKey
)

// WildcardAction is a catalog spelling of the bypass: a role granted it
// authorizes every resource-action. The service principal bypasses on its
// own flag instead.
const WildcardAction = "*"

// Resource-action permissions. One action per route, so a role grants a
// capability rather than a path. They are configured, so an operator may
// bind whatever subset of these an endpoint exposes; a role naming an action
// the build does not know still resolves, it simply never matches a gate.
const (
	ActionDefinitionsRead        = "definitions:read"
	ActionDefinitionsWrite       = "definitions:write"
	ActionSecretsRead            = "secrets:read"
	ActionSecretsWrite           = "secrets:write"
	ActionFormsRead              = "forms:read"
	ActionFormsWrite             = "forms:write"
	ActionInstancesCreate        = "instances:create"
	ActionInstancesRead          = "instances:read"
	ActionInstancesUpdateContext = "instances:update-context"
	// ActionInputDeliver is the endpoint half of the input authorization.
	// The input node's allowed_roles is the independent second half, so a
	// caller may hold this and still be refused by a node that does not
	// list one of its roles.
	ActionInputDeliver     = "input:deliver"
	ActionInstancesControl = "instances:control"
	ActionInstancesMigrate = "instances:migrate"
	ActionStatisticsRead   = "statistics:read"
	ActionRolesRead        = "roles:read"
	ActionRolesWrite       = "roles:write"
	ActionUsersRead        = "users:read"
	ActionSchedulesRead    = "schedules:read"
	ActionSchedulesWrite   = "schedules:write"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	// Subject and Issuer identify the principal at the provider. Both are
	// empty for the service principal.
	Subject string
	Issuer  string
	Name    string
	Email   string
	// Roles are the roles the token asserted. They drive both the endpoint
	// permission union and the input-node role gate.
	Roles []string
	// Service marks the API-token / broker principal: it bypasses the
	// endpoint permission and the per-node role gate, and is recorded as
	// the system user on every audit write it causes.
	Service bool
	// UserID is the users.id uuid this principal is recorded as. It is
	// resolved from the identity cache, so it is empty only for a principal
	// that has never been seen.
	UserID string
}

// Permissions returns the union of the permissions granted by the
// principal's roles. A service principal holds every known action, so a
// listing of its permissions shows what the bypass covers. The bypass
// itself lives in HasPermission, not in this set.
func (p Principal) Permissions(catalog *Catalog) map[string]bool {
	out := make(map[string]bool)
	if p.Service {
		for _, action := range KnownActions() {
			out[action] = true
		}
		return out
	}
	for _, role := range p.Roles {
		for _, action := range catalog.PermissionsFor(role) {
			out[action] = true
		}
	}
	return out
}

// HasPermission reports whether the principal may perform action. A service
// principal may perform anything. A role granted the wildcard in the
// catalog may also perform anything, so admin: ["*"] is a configuration
// spelling of the same bypass.
// A role name that is not in the catalog grants nothing, so an unknown
// claim role denies by default while still matching an input node's
// allowed_roles.
func (p Principal) HasPermission(action string, catalog *Catalog) bool {
	if action == "" {
		return false
	}
	if p.Service {
		return true
	}
	perms := p.Permissions(catalog)
	if perms[WildcardAction] {
		return true
	}
	return perms[action]
}

// CanSatisfyRoles reports whether any of the principal's roles appears in
// allowed. An empty allowed list is open to every caller, including one
// without roles. A service principal always satisfies the gate.
func (p Principal) CanSatisfyRoles(allowed []string) bool {
	if p.Service || len(allowed) == 0 {
		return true
	}
	for _, role := range p.Roles {
		for _, want := range allowed {
			if role == want {
				return true
			}
		}
	}
	return false
}

// MatchesUserID reports whether the principal's resolved user id appears in
// the input node's user gate. It is a pure matcher with no open-gate or
// service semantics: a listed user passes however their roles read, and a
// principal with no resolved user id never matches, so the gate fails
// closed rather than admitting an unknown caller. On a node carrying both
// gates it is one half of the union — an unlisted caller still passes on a
// listed role.
func (p Principal) MatchesUserID(allowed []string) bool {
	if p.UserID == "" {
		return false
	}
	for _, want := range allowed {
		if p.UserID == want {
			return true
		}
	}
	return false
}

// ContextWithPrincipal returns a context carrying the principal and its
// resolved permissions.
func ContextWithPrincipal(ctx context.Context, p Principal, catalog *Catalog) context.Context {
	ctx = context.WithValue(ctx, principalContextKey, p)
	return context.WithValue(ctx, permissionsContextKey, p.Permissions(catalog))
}

// FromContext returns the principal carried by ctx, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey).(Principal)
	return p, ok
}

// PermissionsFromContext returns the permissions carried by ctx, if any.
func PermissionsFromContext(ctx context.Context) (map[string]bool, bool) {
	perms, ok := ctx.Value(permissionsContextKey).(map[string]bool)
	return perms, ok
}

// Catalog is the shared in-memory role catalog. It is consulted on every
// request and replaced wholesale whenever the role tables change: at
// startup from the database (or from configuration on a first boot), after
// every committed role write, and on the background refresh. One instance
// is shared by every holder, so a replacement is visible to the request
// gates without a restart. Reads take the read lock and copy what they
// return, so concurrent requests never observe a half-swapped map and can
// never mutate the shared state. The zero value is an empty catalog and is
// usable as-is; build a populated one with NewCatalog. Always share the
// pointer, never a copy: copying a Catalog copies its lock.
type Catalog struct {
	mu     sync.RWMutex
	byRole map[string][]string
}

// NewCatalog builds a catalog from role permissions. Actions are copied so
// later mutation of the input map cannot affect the catalog.
//
// Role names are trimmed like actions are. A role name is compared against
// the names a token asserts, so a name carrying stray whitespace is a role no
// token can ever reach, and the operator sees a configured role that silently
// grants nothing. A name that is blank once trimmed names nobody and is
// dropped.
func NewCatalog(rolePermissions map[string][]string) *Catalog {
	return &Catalog{byRole: buildCatalogMap(rolePermissions)}
}

// CatalogFromConfig builds the catalog from the loaded configuration.
func CatalogFromConfig(cfg configuration.Auth) *Catalog {
	return NewCatalog(cfg.RolePermissions)
}

// Replace swaps the whole role map for a freshly built one. It is how a
// committed role write and the background refresh become visible to the
// request gates. The input map is copied with the same trimming NewCatalog
// applies, so callers may reuse it afterwards.
func (c *Catalog) Replace(rolePermissions map[string][]string) {
	fresh := buildCatalogMap(rolePermissions)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byRole = fresh
}

// buildCatalogMap copies a role-permission map into catalog shape, trimming
// role names and actions and dropping blank entries of either.
func buildCatalogMap(rolePermissions map[string][]string) map[string][]string {
	byRole := make(map[string][]string, len(rolePermissions))
	for role, actions := range rolePermissions {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		trimmed := make([]string, 0, len(actions))
		for _, action := range actions {
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			trimmed = append(trimmed, action)
		}
		byRole[role] = trimmed
	}
	return byRole
}

// PermissionsFor returns the actions granted by a role, copied so the caller
// cannot mutate the shared catalog. A role that is not in the catalog
// returns nil.
func (c *Catalog) PermissionsFor(role string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	actions := c.byRole[role]
	if actions == nil {
		return nil
	}
	out := make([]string, len(actions))
	copy(out, actions)
	return out
}

// HasRole reports whether the catalog knows a role.
func (c *Catalog) HasRole(role string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.byRole[role]
	return ok
}

// Roles returns every role name in the catalog, sorted for stable output.
func (c *Catalog) Roles() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.byRole))
	for role := range c.byRole {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// HasRolesWriteHolder reports whether any role in perms grants the role-write
// gate, explicitly via roles:write or via the wildcard. It is the one
// definition of "holder" shared by the boot warning, the role-write path,
// and the tests; the repository enforces the same condition in SQL inside
// the write transaction.
func HasRolesWriteHolder(perms map[string][]string) bool {
	for _, actions := range perms {
		for _, action := range actions {
			action = strings.TrimSpace(action)
			if action == ActionRolesWrite || action == WildcardAction {
				return true
			}
		}
	}
	return false
}

// KnownActions returns every resource-action this build gates on, so a
// deployment can be checked against the same list the gates use rather than
// a second copy that drifts. The wildcard is not included: it is the
// deliberate bypass, not an endpoint.
func KnownActions() []string {
	actions := []string{
		ActionDefinitionsRead,
		ActionDefinitionsWrite,
		ActionSecretsRead,
		ActionSecretsWrite,
		ActionFormsRead,
		ActionFormsWrite,
		ActionInstancesCreate,
		ActionInstancesRead,
		ActionInstancesUpdateContext,
		ActionInputDeliver,
		ActionInstancesControl,
		ActionInstancesMigrate,
		ActionStatisticsRead,
		ActionRolesRead,
		ActionRolesWrite,
		ActionUsersRead,
		ActionSchedulesRead,
		ActionSchedulesWrite,
	}
	sort.Strings(actions)
	return actions
}

// Seed describes one role and the actions it grants, as stored in the
// role_permissions table. It is the first-boot seed: configuration fills the
// tables only while they are empty, and the database is the source of truth
// afterwards.
type Seed struct {
	Roles       []string
	Permissions map[string][]string
}

// Seed returns the role/action rows to upsert on a first boot. Permissions
// are deduplicated and sorted so the seed is deterministic.
func (c *Catalog) Seed() Seed {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Listed inline rather than via Roles: a read lock must not nest.
	roles := make([]string, 0, len(c.byRole))
	for role := range c.byRole {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	perms := make(map[string][]string, len(roles))
	for _, role := range roles {
		seen := make(map[string]bool, len(c.byRole[role]))
		var actions []string
		for _, action := range c.byRole[role] {
			if seen[action] {
				continue
			}
			seen[action] = true
			actions = append(actions, action)
		}
		sort.Strings(actions)
		perms[role] = actions
	}
	return Seed{Roles: roles, Permissions: perms}
}

// SystemPrincipal is the service principal used by the API-token path and by
// the broker input consumers, which carry no credential of their own. The
// caller supplies the audit actor the bypass acts as. It carries the admin
// role so identity listings name a role; authorization still bypasses on
// the Service flag rather than on that role.
func SystemPrincipal(userID string) Principal {
	return Principal{
		Name:    "system",
		Roles:   []string{"admin"},
		Service: true,
		UserID:  userID,
	}
}

// UserMetadata is the identity-cache metadata row for a principal: the
// provider subject plus the last-seen roles. It is debug-only and is never
// read back for authorization, which always reads the live token claims.
type UserMetadata struct {
	Subject string   `json:"subject,omitempty"`
	Issuer  string   `json:"issuer,omitempty"`
	Roles   []string `json:"roles,omitempty"`
}

// MarshalPrincipalUser renders the identity-cache metadata for a principal.
func MarshalPrincipalUser(p Principal) json.RawMessage {
	raw, err := json.Marshal(UserMetadata{Subject: p.Subject, Issuer: p.Issuer, Roles: p.Roles})
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}
