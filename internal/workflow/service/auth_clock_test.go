package service

import (
	"context"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
)

// A role served from the database carries real row timestamps, not the read
// time: the row has its own creation now that the API owns the tables. Pin
// the clock so the test proves the reads ignore it, and keep the seed
// stamping on the injected clock.
func TestRoleReadsCarryRowTimestamps(t *testing.T) {
	stored := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	read := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	roles := &fixedRoleRepo{
		rows: []model.Role{{
			Name: "finance", Description: "Configured role finance",
			CreatedAt: stored, UpdatedAt: stored,
		}},
		perms: map[string][]string{"finance": {auth.ActionInputDeliver}},
	}
	svc := NewAuthService(nil, roles, auth.NewCatalog(nil), "sys").(*authService)
	svc.now = func() time.Time { return read }

	list, perms, err := svc.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("ListRoles() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListRoles() returned %d roles, want 1", len(list))
	}
	if !list[0].CreatedAt.Equal(stored) || !list[0].UpdatedAt.Equal(stored) {
		t.Errorf("ListRoles timestamps = %v/%v, want the row time %v, not the read time %v",
			list[0].CreatedAt, list[0].UpdatedAt, stored, read)
	}
	if len(perms["finance"]) != 1 {
		t.Errorf("ListRoles permissions = %v, want the stored grants", perms)
	}

	role, _, err := svc.GetRole(context.Background(), "finance")
	if err != nil {
		t.Fatalf("GetRole() error = %v", err)
	}
	if !role.CreatedAt.Equal(stored) || !role.UpdatedAt.Equal(stored) {
		t.Errorf("GetRole timestamps = %v/%v, want the row time %v",
			role.CreatedAt, role.UpdatedAt, stored)
	}

	// The seed written on a first boot keeps stamping the injected clock,
	// so the write path and the read path stay visibly distinct.
	seedSvc := NewAuthService(nil, roles, auth.NewCatalog(map[string][]string{
		"finance": {auth.ActionInputDeliver},
	}), "sys").(*authService)
	seedSvc.now = func() time.Time { return read }
	if err := seedSvc.SeedRoles(context.Background()); err != nil {
		t.Fatalf("SeedRoles() error = %v", err)
	}
	if !roles.seededAt.Equal(read) {
		t.Errorf("seeded created_at = %v, want the injected clock %v", roles.seededAt, read)
	}
}

// fixedRoleRepo serves canned rows; the embedded interface keeps it a
// RoleRepository while the write methods stay unimplemented, so a test
// calling one fails loudly instead of silently succeeding.
type fixedRoleRepo struct {
	repository.RoleRepository
	rows     []model.Role
	perms    map[string][]string
	seededAt time.Time
}

func (f *fixedRoleRepo) Seed(_ context.Context, roles []model.Role, _ []model.RolePermission) error {
	if len(roles) > 0 {
		f.seededAt = roles[0].CreatedAt
	}
	return nil
}

func (f *fixedRoleRepo) List(context.Context) ([]model.Role, map[string][]string, error) {
	return f.rows, f.perms, nil
}

func (f *fixedRoleRepo) Get(_ context.Context, name string) (model.Role, []string, error) {
	for _, role := range f.rows {
		if role.Name == name {
			return role, f.perms[name], nil
		}
	}
	return model.Role{}, nil, repository.ErrRoleNotFound
}
