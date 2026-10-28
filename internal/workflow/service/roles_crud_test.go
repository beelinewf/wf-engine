package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
)

func roleDesc(s string) *string { return &s }

// seedHolder writes the one roles:write holder the tests below refuse to
// remove, returning the service over the shared catalog.
func seedHolder(t *testing.T) (service.AuthService, *auth.Catalog) {
	t.Helper()
	catalog := auth.NewCatalog(nil)
	svc, _ := authServiceFor(t, catalog)
	ctx := context.Background()
	if err := svc.SeedRoles(ctx); err != nil {
		t.Fatalf("seed empty catalog: %v", err)
	}
	if _, _, err := svc.CreateRole(ctx, "admin", roleDesc("admin"), []string{auth.ActionRolesWrite}); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	return svc, catalog
}

// A created role is readable back and immediately authorizes: the write
// refreshes the shared catalog, so a token carrying the new role passes the
// granted gate without a restart.
func TestCreateRolePersistsAndAuthorizesImmediately(t *testing.T) {
	catalog := auth.NewCatalog(nil)
	svc, _ := authServiceFor(t, catalog)
	ctx := context.Background()

	role, perms, err := svc.CreateRole(ctx, "ops", nil, []string{auth.ActionInstancesRead})
	if err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}
	if role.Name != "ops" {
		t.Errorf("role name = %q, want ops", role.Name)
	}
	if role.Description != "Configured role ops" {
		t.Errorf("description = %q, want it derived when omitted", role.Description)
	}
	if len(perms) != 1 || perms[0] != auth.ActionInstancesRead {
		t.Errorf("permissions = %v", perms)
	}
	if role.CreatedAt.IsZero() || role.UpdatedAt.IsZero() {
		t.Error("timestamps are zero, want the write time")
	}

	if !catalog.HasRole("ops") {
		t.Fatal("the shared catalog does not know the created role")
	}
	ops := auth.Principal{Roles: []string{"ops"}}
	if !ops.HasPermission(auth.ActionInstancesRead, catalog) {
		t.Error("a token carrying the new role does not pass the granted gate")
	}
}

func TestCreateRoleBlankNameIsInvalid(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	for _, name := range []string{"", "   "} {
		if _, _, err := svc.CreateRole(ctx, name, nil, []string{auth.ActionInstancesRead}); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("CreateRole(%q) error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestCreateRoleDuplicateIsConflict(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.CreateRole(ctx, "ops", nil, []string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("first CreateRole() error = %v", err)
	}
	if _, _, err := svc.CreateRole(ctx, "ops", nil, []string{auth.ActionInstancesRead}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("second CreateRole() error = %v, want ErrConflict", err)
	}
}

// Permissions are trimmed, deduplicated, and sorted before the write; a
// blank entry is a client mistake, not a grant to drop silently.
func TestCreateRoleNormalizesPermissions(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	_, perms, err := svc.CreateRole(ctx, "ops", nil,
		[]string{"  " + auth.ActionInstancesRead + " ", auth.ActionInputDeliver, auth.ActionInstancesRead})
	if err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}
	if len(perms) != 2 || perms[0] != auth.ActionInputDeliver || perms[1] != auth.ActionInstancesRead {
		t.Errorf("permissions = %v, want trimmed, deduped, sorted", perms)
	}

	if _, _, err := svc.CreateRole(ctx, "bad", nil, []string{auth.ActionInstancesRead, "   "}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("CreateRole(blank entry) error = %v, want ErrInvalid", err)
	}
	if _, _, err := svc.GetRole(ctx, "bad"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetRole(bad) error = %v, want ErrNotFound (the refused write stored nothing)", err)
	}
}

func TestCreateRoleAcceptsUnknownActions(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	_, perms, err := svc.CreateRole(ctx, "future", nil, []string{"future:action"})
	if err != nil {
		t.Fatalf("CreateRole() error = %v, want unknown actions accepted", err)
	}
	if len(perms) != 1 || perms[0] != "future:action" {
		t.Errorf("permissions = %v, want the unknown action stored", perms)
	}
}

func TestUpdateRoleReplacesPermissions(t *testing.T) {
	svc, catalog := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.CreateRole(ctx, "finance", roleDesc("finance"),
		[]string{auth.ActionInstancesRead, auth.ActionInputDeliver}); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}

	role, perms, err := svc.UpdateRole(ctx, "finance", roleDesc("money"), []string{auth.ActionStatisticsRead})
	if err != nil {
		t.Fatalf("UpdateRole() error = %v", err)
	}
	if role.Description != "money" {
		t.Errorf("description = %q, want money", role.Description)
	}
	if len(perms) != 1 || perms[0] != auth.ActionStatisticsRead {
		t.Errorf("permissions = %v, want the replaced set", perms)
	}

	// An empty list revokes every grant, and the gates see it at once.
	if _, perms, err := svc.UpdateRole(ctx, "finance", nil, []string{}); err != nil {
		t.Fatalf("UpdateRole(empty) error = %v", err)
	} else if len(perms) != 0 {
		t.Errorf("permissions = %v, want no grants", perms)
	}
	finance := auth.Principal{Roles: []string{"finance"}}
	if finance.HasPermission(auth.ActionInstancesRead, catalog) {
		t.Error("finance still passes a revoked gate after the update")
	}
}

func TestUpdateRolePreservesDescriptionWhenOmitted(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.CreateRole(ctx, "finance", roleDesc("money"), []string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}
	role, _, err := svc.UpdateRole(ctx, "finance", nil, []string{auth.ActionInstancesRead})
	if err != nil {
		t.Fatalf("UpdateRole() error = %v", err)
	}
	if role.Description != "money" {
		t.Errorf("description = %q, want it preserved when omitted", role.Description)
	}
}

func TestUpdateRoleUnknownIsNotFound(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.UpdateRole(ctx, "wizard", nil, []string{auth.ActionInstancesRead}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("UpdateRole() error = %v, want ErrNotFound", err)
	}
}

func TestUpdateRoleBlankNameAndBlankEntryAreInvalid(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.UpdateRole(ctx, "  ", nil, []string{auth.ActionInstancesRead}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("UpdateRole(blank name) error = %v, want ErrInvalid", err)
	}
	if _, _, err := svc.CreateRole(ctx, "finance", nil, []string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}
	if _, _, err := svc.UpdateRole(ctx, "finance", nil, []string{""}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("UpdateRole(blank entry) error = %v, want ErrInvalid", err)
	}
}

func TestDeleteRoleRemovesAndRefreshes(t *testing.T) {
	svc, catalog := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.CreateRole(ctx, "finance", nil, []string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}
	if err := svc.DeleteRole(ctx, "finance"); err != nil {
		t.Fatalf("DeleteRole() error = %v", err)
	}
	if _, _, err := svc.GetRole(ctx, "finance"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetRole() after delete error = %v, want ErrNotFound", err)
	}
	if catalog.HasRole("finance") {
		t.Error("the shared catalog still knows the deleted role")
	}
}

func TestDeleteRoleUnknownIsNotFound(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	if err := svc.DeleteRole(ctx, "wizard"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("DeleteRole() error = %v, want ErrNotFound", err)
	}
	if err := svc.DeleteRole(ctx, "  "); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("DeleteRole(blank) error = %v, want ErrInvalid", err)
	}
}

// Removing the last roles:write holder is a conflict naming the invariant,
// and the role reads back unchanged: the refusal rolled the write back and
// never touched the shared catalog.
func TestLastHolderWriteIsConflictAndLeavesTheRoleIntact(t *testing.T) {
	svc, catalog := seedHolder(t)
	ctx := context.Background()

	if _, _, err := svc.UpdateRole(ctx, "admin", nil, []string{auth.ActionRolesRead}); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("strip UpdateRole() error = %v, want ErrConflict", err)
	} else if !strings.Contains(err.Error(), "at least one role must retain the roles:write permission") {
		t.Errorf("strip UpdateRole() error = %q, want it to name the invariant", err.Error())
	}
	if err := svc.DeleteRole(ctx, "admin"); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("DeleteRole() error = %v, want ErrConflict", err)
	}

	role, perms, err := svc.GetRole(ctx, "admin")
	if err != nil {
		t.Fatalf("GetRole() error = %v", err)
	}
	if role.Name != "admin" || len(perms) != 1 || perms[0] != auth.ActionRolesWrite {
		t.Errorf("role after refused writes = %+v %v, want it intact", role, perms)
	}
	admin := auth.Principal{Roles: []string{"admin"}}
	if !admin.HasPermission(auth.ActionRolesWrite, catalog) {
		t.Error("the shared catalog lost a grant the refused writes must never touch")
	}
}

// Granting roles:write to a second role unblocks the same writes.
func TestSecondHolderMakesTheSameWriteSucceed(t *testing.T) {
	svc, _ := seedHolder(t)
	ctx := context.Background()
	if _, _, err := svc.CreateRole(ctx, "ops", nil, []string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("CreateRole() error = %v", err)
	}
	if _, _, err := svc.UpdateRole(ctx, "ops", nil, []string{auth.ActionRolesWrite}); err != nil {
		t.Fatalf("grant roles:write to ops: %v", err)
	}
	if _, _, err := svc.UpdateRole(ctx, "admin", nil, []string{auth.ActionRolesRead}); err != nil {
		t.Errorf("strip with a second holder error = %v, want nil", err)
	}
	if err := svc.DeleteRole(ctx, "admin"); err != nil {
		t.Errorf("delete with a second holder error = %v, want nil", err)
	}
}

// RefreshCatalog loads another replica's writes into the shared catalog
// without touching the tables.
func TestRefreshCatalogLoadsDatabaseWrites(t *testing.T) {
	catalog := auth.NewCatalog(nil)
	svc, db := authServiceFor(t, catalog)
	ctx := context.Background()
	roles := repository.NewRoleRepository(db)
	if _, err := roles.Create(ctx, model.Role{Name: "ops", Description: "ops"}, []string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("direct create: %v", err)
	}

	if catalog.HasRole("ops") {
		t.Fatal("catalog knows the role before the refresh")
	}
	if err := svc.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog() error = %v", err)
	}
	if !catalog.HasRole("ops") {
		t.Error("catalog does not know the role after the refresh")
	}
	if got := catalog.PermissionsFor("ops"); len(got) != 1 || got[0] != auth.ActionInstancesRead {
		t.Errorf("PermissionsFor(ops) = %v", got)
	}
}
