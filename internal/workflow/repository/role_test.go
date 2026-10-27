package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
)

// roleAt builds the role row a write carries, with explicit timestamps so
// the tests pin what is persisted rather than what the clock said.
func roleAt(name, description string, now time.Time) model.Role {
	return model.Role{Name: name, Description: description, CreatedAt: now, UpdatedAt: now}
}

func TestRoleCreateWritesRoleAndPermissions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)

	created, err := repo.Create(ctx, roleAt("finance", "Configured role finance", now),
		[]string{"instances:read", "input:deliver"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Name != "finance" || created.Description != "Configured role finance" {
		t.Errorf("Create() = %+v, want the written row back", created)
	}
	if !created.CreatedAt.Equal(now) || !created.UpdatedAt.Equal(now) {
		t.Errorf("Create() timestamps = %v/%v, want %v", created.CreatedAt, created.UpdatedAt, now)
	}

	role, perms, err := repo.Get(ctx, "finance")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if role.Description != "Configured role finance" {
		t.Errorf("description = %q", role.Description)
	}
	if len(perms) != 2 || perms[0] != "input:deliver" || perms[1] != "instances:read" {
		t.Errorf("permissions = %v, want the ordered grants", perms)
	}
}

func TestRoleCreateDuplicateIsConflict(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("finance", "first", now), []string{"instances:read"}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	if _, err := repo.Create(ctx, roleAt("finance", "second", now), []string{"input:deliver"}); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("second Create() error = %v, want ErrConflict", err)
	}

	role, perms, err := repo.Get(ctx, "finance")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if role.Description != "first" || len(perms) != 1 || perms[0] != "instances:read" {
		t.Errorf("role after duplicate create = %+v %v, want the first write intact", role, perms)
	}
}

func TestRoleUpdateReplacesPermissions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// A second holder so the update under test cannot trip the invariant.
	if _, err := repo.Create(ctx, roleAt("admin", "admin", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := repo.Create(ctx, roleAt("finance", "old", now),
		[]string{"instances:read", "input:deliver"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated, err := repo.Update(ctx, roleAt("finance", "new", now.Add(time.Hour)), []string{"statistics:read"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Description != "new" {
		t.Errorf("Update() description = %q, want new", updated.Description)
	}
	if !updated.UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Errorf("Update() updated_at = %v, want the write time", updated.UpdatedAt)
	}

	_, perms, err := repo.Get(ctx, "finance")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(perms) != 1 || perms[0] != "statistics:read" {
		t.Errorf("permissions = %v, want the replaced set", perms)
	}
}

func TestRoleUpdateUnknownIsNotFound(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)

	if _, err := repo.Update(ctx, roleAt("wizard", "x", time.Now().UTC()), []string{"instances:read"}); err != repository.ErrRoleNotFound {
		t.Errorf("Update() error = %v, want ErrRoleNotFound", err)
	}
}

func TestRoleDeleteRemovesRoleAndPermissions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("admin", "admin", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := repo.Create(ctx, roleAt("finance", "finance", now), []string{"instances:read"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Delete(ctx, "finance"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, _, err := repo.Get(ctx, "finance"); err != repository.ErrRoleNotFound {
		t.Errorf("Get() after delete error = %v, want ErrRoleNotFound", err)
	}
	var permCount int64
	if err := db.Model(&repository.RolePermissionModel{}).Where("role = ?", "finance").Count(&permCount).Error; err != nil {
		t.Fatal(err)
	}
	if permCount != 0 {
		t.Errorf("permission rows for a deleted role = %d, want 0", permCount)
	}
}

func TestRoleDeleteUnknownIsNotFound(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)

	if err := repo.Delete(ctx, "wizard"); err != repository.ErrRoleNotFound {
		t.Errorf("Delete() error = %v, want ErrRoleNotFound", err)
	}
}

// Stripping roles:write from the last holder must fail with a conflict and
// leave the role and its permissions exactly as they were: the check runs
// inside the write transaction, so the refusal rolls the mutation back.
func TestRoleUpdateStrippingLastHolderRollsBack(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("admin", "admin", now),
		[]string{"roles:read", "roles:write"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	if _, err := repo.Update(ctx, roleAt("admin", "admin", now), []string{"roles:read"}); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("Update() error = %v, want ErrConflict", err)
	}
	role, perms, err := repo.Get(ctx, "admin")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if role.Description != "admin" || len(perms) != 2 {
		t.Errorf("role after refused update = %+v %v, want it intact", role, perms)
	}
}

func TestRoleDeleteLastHolderRollsBack(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("admin", "admin", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := repo.Delete(ctx, "admin"); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict", err)
	}
	if _, perms, err := repo.Get(ctx, "admin"); err != nil || len(perms) != 1 {
		t.Errorf("role after refused delete = %v %v, want it intact", perms, err)
	}
}

// The invariant counts holders, not writes: with two holders the same strip
// and delete succeed.
func TestRoleWriteSucceedsWhileAnotherHolderRemains(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("admin", "admin", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := repo.Create(ctx, roleAt("ops", "ops", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed ops: %v", err)
	}
	if _, err := repo.Update(ctx, roleAt("ops", "ops", now), []string{"instances:read"}); err != nil {
		t.Errorf("strip with a second holder error = %v, want nil", err)
	}
	if _, err := repo.Create(ctx, roleAt("ops2", "ops2", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed ops2: %v", err)
	}
	if err := repo.Delete(ctx, "ops2"); err != nil {
		t.Errorf("delete with a second holder error = %v, want nil", err)
	}
}

// A role granted the wildcard satisfies the gate, so it counts as a holder
// exactly like an explicit roles:write grant.
func TestRoleWildcardCountsAsHolder(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("root", "root", now), []string{"*"}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := repo.Create(ctx, roleAt("admin", "admin", now), []string{"roles:write"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	// Stripping the explicit grant is fine while the wildcard holder remains.
	if _, err := repo.Update(ctx, roleAt("admin", "admin", now), []string{"roles:read"}); err != nil {
		t.Errorf("strip with a wildcard holder error = %v, want nil", err)
	}
	// But the wildcard holder is itself protected when it is the last one.
	if err := repo.Delete(ctx, "root"); !errors.Is(err, model.ErrConflict) {
		t.Errorf("Delete() error = %v, want ErrConflict", err)
	}
}

// Creating a role can never remove a grant, so it can never violate the
// invariant, even for a role that grants nothing on an empty table.
func TestRoleCreateCannotViolateTheInvariant(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if _, err := repo.Create(ctx, roleAt("nobody", "nobody", now), nil); err != nil {
		t.Fatalf("Create() error = %v, want nil", err)
	}
	if _, perms, err := repo.Get(ctx, "nobody"); err != nil || len(perms) != 0 {
		t.Errorf("Get() = %v %v, want the grant-nothing role", perms, err)
	}
}
