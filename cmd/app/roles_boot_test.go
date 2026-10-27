package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/internal/workflow/testdb"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// rolesBootDB opens the app scratch database with empty role tables.
func rolesBootDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testdb.Open(t, "app")
	if err := db.AutoMigrate(&repository.RoleModel{}, &repository.RolePermissionModel{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	if err := db.Exec("TRUNCATE role_permissions, roles").Error; err != nil {
		t.Fatalf("truncate roles: %v", err)
	}
	return db
}

func testBootLogger() (*logrus.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	return logger, &buf
}

// On a first boot the tables are empty, so configuration seeds them and the
// shared catalog serves the seed.
func TestLoadRoleCatalogSeedsWhenEmpty(t *testing.T) {
	db := rolesBootDB(t)
	ctx := context.Background()
	config := map[string][]string{
		"admin":   {auth.ActionRolesWrite, auth.ActionRolesRead},
		"finance": {auth.ActionInstancesRead},
	}
	catalog := auth.NewCatalog(config)
	authSvc := service.NewAuthService(repository.NewUserRepository(db), repository.NewRoleRepository(db), catalog, "sys")
	logger, _ := testBootLogger()

	if err := loadRoleCatalog(ctx, repository.NewRoleRepository(db), authSvc, config, logger); err != nil {
		t.Fatalf("loadRoleCatalog() error = %v", err)
	}

	roles, perms, err := repository.NewRoleRepository(db).List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("roles = %d, want the seeded 2", len(roles))
	}
	if len(perms["admin"]) != 2 || len(perms["finance"]) != 1 {
		t.Errorf("permissions = %v, want the configured seed", perms)
	}
	if !catalog.HasRole("finance") {
		t.Error("the shared catalog does not serve the seed")
	}
}

// Once the tables hold roles, configuration is ignored: the database wins
// and the shared catalog loads from it, not from the file.
func TestLoadRoleCatalogLoadsFromDatabaseWhenSeeded(t *testing.T) {
	db := rolesBootDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	repo := repository.NewRoleRepository(db)
	if _, err := repo.Create(ctx, model.Role{Name: "custom", Description: "via API", CreatedAt: now, UpdatedAt: now},
		[]string{auth.ActionInstancesRead}); err != nil {
		t.Fatalf("pre-create custom: %v", err)
	}
	if _, err := repo.Create(ctx, model.Role{Name: "admin", Description: "admin", CreatedAt: now, UpdatedAt: now},
		[]string{auth.ActionRolesWrite}); err != nil {
		t.Fatalf("pre-create admin: %v", err)
	}
	config := map[string][]string{
		"finance": {auth.ActionInstancesRead},
	}
	catalog := auth.NewCatalog(config)
	authSvc := service.NewAuthService(repository.NewUserRepository(db), repo, catalog, "sys")
	logger, _ := testBootLogger()

	if err := loadRoleCatalog(ctx, repo, authSvc, config, logger); err != nil {
		t.Fatalf("loadRoleCatalog() error = %v", err)
	}

	if _, _, err := repo.Get(ctx, "finance"); err != repository.ErrRoleNotFound {
		t.Errorf("Get(finance) error = %v, want ErrRoleNotFound (config must not seed over rows)", err)
	}
	role, perms, err := repo.Get(ctx, "custom")
	if err != nil {
		t.Fatalf("Get(custom) error = %v", err)
	}
	if role.Description != "via API" || len(perms) != 1 {
		t.Errorf("custom = %+v %v, want the database row untouched", role, perms)
	}
	if !catalog.HasRole("custom") || catalog.HasRole("finance") {
		t.Error("the shared catalog does not match the database")
	}
	if got := catalog.PermissionsFor("custom"); len(got) != 1 || got[0] != auth.ActionInstancesRead {
		t.Errorf("PermissionsFor(custom) = %v, want the database grants", got)
	}
}

// A seed with no roles:write holder warns but never fails the boot: the API
// key can still grant it, and an empty first-boot seed is legitimate.
func TestWarnAboutCatalogNoRolesWriteHolder(t *testing.T) {
	logger, buf := testBootLogger()
	warnAboutCatalog(map[string][]string{
		"auditor": {auth.ActionRolesRead},
	}, logger)
	if !strings.Contains(buf.String(), "roles:write") {
		t.Errorf("log = %q, want a no-holder warning", buf.String())
	}

	logger, buf = testBootLogger()
	warnAboutCatalog(map[string][]string{
		"admin": {auth.ActionRolesWrite},
	}, logger)
	if strings.Contains(buf.String(), "roles:write") {
		t.Errorf("log = %q, want no warning while a holder exists", buf.String())
	}

	logger, buf = testBootLogger()
	warnAboutCatalog(map[string][]string{
		"root": {auth.WildcardAction},
	}, logger)
	if strings.Contains(buf.String(), "roles:write") {
		t.Errorf("log = %q, want no warning while a wildcard holder exists", buf.String())
	}
}
