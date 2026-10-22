package repository_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestManyIdentityLessUsersCoexist is the regression test for the partial
// unique index on (subject, issuer). Every user without a provider identity
// carries the key (”, ”), so a plain composite unique index would permit
// exactly one such row in the entire table: the system user plus any locally
// created user would collide, and definition creation would start failing
// with a duplicate-key error.
func TestManyIdentityLessUsersCoexist(t *testing.T) {
	db := setupTestDB(t)

	// The fixture already seeded the system user, so insert several more
	// identity-less users on top of it.
	ids := []string{
		"22222222-2222-7222-8222-222222222222",
		"33333333-3333-7333-8333-333333333333",
		"44444444-4444-7444-8444-444444444444",
	}
	for i, id := range ids {
		u := repository.UserToModel(model.User{
			ID: id, Name: "local", Email: "local@localhost",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("insert identity-less user %d: %v", i, err)
		}
	}

	var count int64
	if err := db.Model(&repository.UserModel{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	// One seeded system user plus the three above.
	if count != 4 {
		t.Fatalf("users = %d, want 4", count)
	}
	var identityless int64
	if err := db.Model(&repository.UserModel{}).Where("subject = ''").Count(&identityless).Error; err != nil {
		t.Fatal(err)
	}
	if identityless != 4 {
		t.Fatalf("identity-less users = %d, want 4", identityless)
	}
}

// TestUpsertBySubjectRejectsADuplicateIdentity proves the index is still
// doing its job for the rows it covers: a second human claiming the same
// (subject, issuer) is refused, which is what stops a just-in-time upsert
// from forking one identity into two rows.
func TestUpsertBySubjectRejectsADuplicateIdentity(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	first, err := repo.UpsertBySubject(ctx, model.User{
		ID: "22222222-2222-7222-8222-222222222222", Subject: "ada-1", Issuer: "https://idp.example.test",
		Name: "Ada", Email: "ada@example.test", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// A raw duplicate insert is what the unique index is protecting; the
	// repository path never issues one, because it looks the row up first.
	dup := repository.UserToModel(model.User{
		ID: "33333333-3333-7333-8333-333333333333", Subject: "ada-1", Issuer: "https://idp.example.test",
		Name: "Impostor", Email: "x@example.test", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("duplicate (subject, issuer) was accepted")
	}

	// The same subject at a different issuer is a different human.
	other, err := repo.UpsertBySubject(ctx, model.User{
		ID: "44444444-4444-7444-8444-444444444444", Subject: "ada-1", Issuer: "https://other.example.test",
		Name: "Ada", Email: "ada@other.test", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert at a different issuer: %v", err)
	}
	if other == first {
		t.Fatalf("both issuers resolved to %s", first)
	}
}

// TestUpsertBySubjectRequiresBothParts: an identity is only meaningful as a
// pair, so a half-filled one is rejected rather than silently colliding
// with every other half-filled row.
func TestUpsertBySubjectRequiresBothParts(t *testing.T) {
	db := setupTestDB(t)
	repo := repository.NewUserRepository(db)
	ctx := context.Background()

	for _, u := range []model.User{
		{ID: "22222222-2222-7222-8222-222222222222", Issuer: "https://idp.example.test"},
		{ID: "22222222-2222-7222-8222-222222222222", Subject: "ada-1"},
		{ID: "not-a-uuid", Subject: "ada-1", Issuer: "https://idp.example.test"},
	} {
		if _, err := repo.UpsertBySubject(ctx, u); err == nil {
			t.Errorf("UpsertBySubject(%+v) error = nil, want a rejection", u)
		}
	}
}

// TestUserUpsertConcurrent: a just-in-time upsert runs on every authenticated
// request, so the same human arriving on two requests at once is routine, not
// an edge case. Both calls have to resolve to one row and one id: an id that
// matched no user would leave created_by/updated_by dangling, and a 500 on a
// valid token would push the caller to retry into the same race.
func TestUserUpsertConcurrent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)
	now := time.Now().UTC()

	const goroutines = 10
	ids := make([]string, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = repo.UpsertBySubject(ctx, model.User{
				ID: fmt.Sprintf("2222222%d-2222-7222-8222-222222222222", i), Subject: "grace-1", Issuer: "https://idp.example.test",
				Name: "Grace", Email: "grace@example.test", CreatedAt: now, UpdatedAt: now,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("UpsertBySubject #%d error = %v", i, err)
		}
	}
	for i := 1; i < goroutines; i++ {
		if ids[i] != ids[0] {
			t.Errorf("UpsertBySubject #%d resolved to %s, want %s", i, ids[i], ids[0])
		}
	}
	var count int64
	if err := db.Model(&repository.UserModel{}).
		Where("subject = ? AND issuer = ?", "grace-1", "https://idp.example.test").
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("users rows for the identity = %d, want 1", count)
	}
}

// TestRoleSeedIsIdempotentAndNonDestructive covers the startup catalog
// write: seeding twice must not duplicate. A role dropped from configuration
// keeps its row, so an in-flight token still resolves the name, but the
// permissions it used to grant are pruned: the table is a copy of
// configuration, so a stale grant there is drift the audit copy must not
// keep.
func TestRoleSeedIsIdempotentAndNonDestructive(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	// seed writes the given roles, each granted the given actions.
	seed := func(perms map[string][]string) error {
		defs := make([]model.Role, 0, len(perms))
		grants := make([]model.RolePermission, 0, len(perms))
		for r, actions := range perms {
			defs = append(defs, model.Role{Name: r, CreatedAt: now, UpdatedAt: now})
			for _, a := range actions {
				grants = append(grants, model.RolePermission{
					Role: r, Action: a, CreatedAt: now, UpdatedAt: now,
				})
			}
		}
		return repo.Seed(ctx, defs, grants)
	}
	grants := map[string][]string{
		"admin":   {"*"},
		"finance": {"input:deliver"},
	}

	if err := seed(grants); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if err := seed(grants); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	roles, perms, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("roles = %d, want 2 (reseed must not duplicate)", len(roles))
	}
	var permCount int64
	if err := db.Model(&repository.RolePermissionModel{}).Count(&permCount).Error; err != nil {
		t.Fatal(err)
	}
	if permCount != 2 {
		t.Fatalf("role_permissions = %d, want 2", permCount)
	}
	if got := perms["finance"]; len(got) != 1 || got[0] != "input:deliver" {
		t.Errorf("finance permissions = %v", got)
	}

	// Narrowing a role prunes the grants it no longer has. A role dropped
	// from the seed keeps both its row and its permission rows: the prune
	// is scoped to the roles this seed names, so history stays readable and
	// the read APIs, which serve the live catalog, no longer advertise it.
	if err := seed(map[string][]string{"admin": {"*", "instances:read"}}); err != nil {
		t.Fatalf("seed admin with an extra action: %v", err)
	}
	if err := seed(map[string][]string{"admin": {"*"}}); err != nil {
		t.Fatalf("narrow admin back: %v", err)
	}
	if err := db.Model(&repository.RolePermissionModel{}).Count(&permCount).Error; err != nil {
		t.Fatal(err)
	}
	if permCount != 2 {
		t.Errorf("role_permissions = %d, want 2 after pruning the narrowed action", permCount)
	}
	_, adminPerms, err := repo.Get(ctx, "admin")
	if err != nil {
		t.Fatalf("Get(admin) error = %v", err)
	}
	if len(adminPerms) != 1 || adminPerms[0] != "*" {
		t.Errorf("admin permissions = %v, want the pruned set [*]", adminPerms)
	}

	if err := seed(map[string][]string{"admin": {"*"}}); err != nil {
		t.Fatalf("seed after removal: %v", err)
	}
	if _, _, err := repo.Get(ctx, "finance"); err != nil {
		t.Fatalf("dropped role row was deleted: %v", err)
	}
	if _, _, err := repo.Get(ctx, "wizard"); err != repository.ErrRoleNotFound {
		t.Errorf("Get(wizard) error = %v, want ErrRoleNotFound", err)
	}
}

// TestRolePermissionsForeignKeyHolds proves a permission cannot be written
// for a role that does not exist, so a typo in configuration fails loudly at
// startup instead of silently granting nothing.
func TestRolePermissionsForeignKeyHolds(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	err := repo.Seed(ctx, nil, []model.RolePermission{{
		Role: "never-defined", Action: "input:deliver", CreatedAt: now, UpdatedAt: now,
	}})
	if err == nil {
		t.Fatal("Seed() accepted a permission for an undefined role")
	}
}

// TestUpsertBySubjectSurvivesGenericPlan: the conflict target repeats the
// partial index predicate, and PostgreSQL proves that match at plan time.
// After enough executions on one connection the planner switches to a
// generic plan, which cannot prove a parameterised predicate matches the
// index — the statement then fails with 42P10 instead of upserting. The
// predicate must therefore render as a literal. Forcing generic plans on a
// pinned connection reproduces the production failure deterministically:
// every authenticated request upserts, so any warm connection trips it.
func TestUpsertBySubjectSurvivesGenericPlan(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plans: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "RESET plan_cache_mode") }()

	pinned, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.NewUserRepository(pinned)

	now := time.Now().UTC()
	first, err := repo.UpsertBySubject(ctx, model.User{
		ID: "55555555-5555-7555-8555-555555555555", Subject: "generic-1", Issuer: "https://idp.example.test",
		Name: "Generic", Email: "generic@example.test", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("upsert under a generic plan: %v", err)
	}

	// The conflict branch takes the same plan path: a repeat sighting must
	// land on the existing row, not fail inference.
	again, err := repo.UpsertBySubject(ctx, model.User{
		ID: "66666666-6666-7666-8666-666666666666", Subject: "generic-1", Issuer: "https://idp.example.test",
		Name: "Generic Renamed", Email: "generic+2@example.test", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("repeat upsert under a generic plan: %v", err)
	}
	if again != first {
		t.Fatalf("repeat upsert resolved to %s, want %s", again, first)
	}
}

// TestUserListPaginationAndOrder: the users list serves newest first with a
// stable id tie-break, and reports the total across pages. Every timestamp
// is pinned so the expected order is fully deterministic.
func TestUserListPaginationAndOrder(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// The fixture seeds the system user with a fresh timestamp; pin it to
	// the oldest slot.
	if err := db.Model(&repository.UserModel{}).
		Where("id = ?", "11111111-1111-7111-8111-111111111111").
		Updates(map[string]any{"created_at": base, "updated_at": base}).Error; err != nil {
		t.Fatalf("pin system user timestamp: %v", err)
	}
	seed := func(id, name string, at time.Time) {
		t.Helper()
		u := repository.UserToModel(model.User{
			ID: id, Name: name, Email: name + "@example.test",
			CreatedAt: at, UpdatedAt: at,
		})
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	ordered := []string{
		"22222222-2222-7222-8222-222222222222",
		"33333333-3333-7333-8333-333333333333",
		"44444444-4444-7444-8444-444444444444",
		"55555555-5555-7555-8555-555555555555",
	}
	for i, id := range ordered {
		seed(id, fmt.Sprintf("user-%d", i), base.Add(time.Duration(i+1)*time.Minute))
	}
	// Two rows sharing one timestamp prove the id tie-break: the lower id
	// sorts first regardless of insertion order.
	tieLow := "0aaaaaaa-0000-7000-8000-000000000000"
	tieHigh := "0bbbbbbb-0000-7000-8000-000000000000"
	seed(tieHigh, "tie-high", base.Add(5*time.Minute))
	seed(tieLow, "tie-low", base.Add(5*time.Minute))

	byIDs := func(users []model.User) []string {
		out := make([]string, 0, len(users))
		for _, u := range users {
			out = append(out, u.ID)
		}
		return out
	}
	assertPage := func(page, perPage int, want []string) {
		t.Helper()
		users, total, err := repo.List(ctx, page, perPage)
		if err != nil {
			t.Fatalf("List(%d, %d) error = %v", page, perPage, err)
		}
		if total != 7 {
			t.Errorf("List(%d, %d) total = %d, want 7", page, perPage, total)
		}
		got := byIDs(users)
		if len(got) != len(want) {
			t.Fatalf("List(%d, %d) ids = %v, want %v", page, perPage, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("List(%d, %d) ids = %v, want %v", page, perPage, got, want)
			}
		}
	}

	assertPage(1, 3, []string{tieLow, tieHigh, ordered[3]})
	assertPage(2, 3, []string{ordered[2], ordered[1], ordered[0]})
	assertPage(3, 3, []string{"11111111-1111-7111-8111-111111111111"})
	assertPage(4, 3, []string{})
}
