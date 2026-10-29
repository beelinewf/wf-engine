package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"gorm.io/gorm"
)

const (
	localUserAID = "22222222-2222-7222-8222-222222222222"
	localUserBID = "33333333-3333-7333-8333-333333333333"
)

// seedLocalRoles inserts the catalog roles membership tests assign.
func seedLocalRoles(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	for _, name := range []string{"admin", "viewer"} {
		if err := db.Create(&repository.RoleModel{Name: name, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatalf("seed role %s: %v", name, err)
		}
	}
}

func localUser(id, email string) model.User {
	now := time.Now().UTC()
	return model.User{
		ID: id, Subject: id, Issuer: model.LocalIssuer,
		Name: "Local", Email: email,
		PasswordHash: "hashed", IsActive: true, EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestUserCreateRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	created, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), []string{"viewer", "admin"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID != localUserAID || !created.IsLocal() {
		t.Fatalf("created = %+v", created)
	}

	got, err := repo.GetByID(ctx, localUserAID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.PasswordHash != "hashed" || !got.IsActive || !got.EmailVerified() {
		t.Errorf("credential fields = %+v", got)
	}
	if len(got.Roles) != 2 || got.Roles[0] != "admin" || got.Roles[1] != "viewer" {
		t.Errorf("roles = %v, want [admin viewer] ordered", got.Roles)
	}

	byEmail, err := repo.GetByEmail(ctx, "ada@example.test")
	if err != nil {
		t.Fatalf("GetByEmail() error = %v", err)
	}
	if byEmail.ID != localUserAID {
		t.Errorf("GetByEmail() id = %s, want %s", byEmail.ID, localUserAID)
	}
}

func TestUserCreateDuplicateIsConflict(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	if _, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), nil); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Same email, different id: the local email index refuses.
	if _, err := repo.Create(ctx, localUser(localUserBID, "ada@example.test"), nil); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate email error = %v, want ErrConflict", err)
	}
	// Same id, different email: the primary key refuses.
	dup := localUser(localUserAID, "other@example.test")
	if _, err := repo.Create(ctx, dup, nil); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate id error = %v, want ErrConflict", err)
	}
}

func TestUserCreateUnknownRoleIsInvalid(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	_, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), []string{"wizard"})
	if !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("unknown role error = %v, want ErrInvalid", err)
	}
	// The failed membership rolls the user row back with it.
	if _, err := repo.GetByID(ctx, localUserAID); err != repository.ErrUserNotFound {
		t.Errorf("GetByID() after rollback error = %v, want ErrUserNotFound", err)
	}
}

func TestUserCreateRequiresLocalIdentity(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	oidc := localUser(localUserAID, "ada@example.test")
	oidc.Issuer = "https://idp.example.test"
	oidc.Subject = "ada-1"
	if _, err := repo.Create(ctx, oidc, nil); err == nil {
		t.Error("non-local identity: error = nil, want rejection")
	}
	bad := localUser("not-a-uuid", "ada@example.test")
	bad.Subject = "not-a-uuid"
	if _, err := repo.Create(ctx, bad, nil); err == nil {
		t.Error("bad uuid: error = nil, want rejection")
	}
}

func TestUserUpdateReplacesFieldsAndRoles(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	if _, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), []string{"viewer"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now := time.Now().UTC()
	next := localUser(localUserAID, "ada.renamed@example.test")
	next.Name = "Ada Renamed"
	next.PasswordHash = "rotated"
	next.UpdatedAt = now
	updated, err := repo.Update(ctx, next, []string{"admin"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Name != "Ada Renamed" || updated.Email != "ada.renamed@example.test" || updated.PasswordHash != "rotated" {
		t.Errorf("updated = %+v", updated)
	}
	if len(updated.Roles) != 1 || updated.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin]", updated.Roles)
	}
	// An empty role list revokes every membership.
	updated.UpdatedAt = time.Now().UTC()
	cleared, err := repo.Update(ctx, updated, nil)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(cleared.Roles) != 0 {
		t.Errorf("roles = %v, want none", cleared.Roles)
	}
}

func TestUserUpdateDuplicateEmailIsConflict(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	if _, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), nil); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	other, err := repo.Create(ctx, localUser(localUserBID, "grace@example.test"), nil)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	other.Email = "ada@example.test"
	if _, err := repo.Update(ctx, other, nil); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate email error = %v, want ErrConflict", err)
	}
}

func TestUserUpdateUnknownOrNonLocalIsNotFound(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	// Well-formed but absent.
	if _, err := repo.Update(ctx, localUser(localUserAID, "ada@example.test"), nil); err != repository.ErrUserNotFound {
		t.Errorf("unknown id error = %v, want ErrUserNotFound", err)
	}
	// Present but not local: an OIDC row must never be rewritten here.
	now := time.Now().UTC()
	oidcID, err := repo.UpsertBySubject(ctx, model.User{
		ID: localUserBID, Subject: "ada-1", Issuer: "https://idp.example.test",
		Name: "Ada", Email: "ada@example.test", IsActive: true, EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertBySubject() error = %v", err)
	}
	oidc, err := repo.GetByID(ctx, oidcID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if _, err := repo.Update(ctx, oidc, nil); err != repository.ErrUserNotFound {
		t.Errorf("non-local row error = %v, want ErrUserNotFound", err)
	}
}

// A profile edit must never disturb an outstanding verification token: the
// Update column list excludes the token pair, and this pins that.
func TestUserUpdatePreservesVerificationToken(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	pending := localUser(localUserAID, "ada@example.test")
	pending.EmailVerifiedAt = nil
	if _, err := repo.Create(ctx, pending, nil); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now := time.Now().UTC()
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-1", now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetVerificationToken() error = %v", err)
	}
	got, err := repo.GetByID(ctx, localUserAID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	got.Name = "Ada Renamed"
	if _, err := repo.Update(ctx, got, nil); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-1", now.Add(time.Minute)); err != nil {
		t.Errorf("ConsumeVerificationToken() after update error = %v, want nil", err)
	}
}

func TestUserDisable(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	pending := localUser(localUserAID, "ada@example.test")
	pending.EmailVerifiedAt = nil
	if _, err := repo.Create(ctx, pending, []string{"admin"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now := time.Now().UTC()
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-1", now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetVerificationToken() error = %v", err)
	}
	disabled, err := repo.Disable(ctx, localUserAID, now)
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if disabled.IsActive || disabled.PasswordHash != "" || disabled.EmailVerified() {
		t.Errorf("disabled = %+v, want inactive with no credential", disabled)
	}
	// Roles stay so a re-enable restores the account whole.
	if len(disabled.Roles) != 1 || disabled.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin] kept", disabled.Roles)
	}
	// The outstanding token died with the account.
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-1", now.Add(time.Minute)); err != repository.ErrUserNotFound {
		t.Errorf("consume after disable error = %v, want ErrUserNotFound", err)
	}
	// The row itself survives for the audit trail.
	if _, err := repo.GetByEmail(ctx, "ada@example.test"); err != nil {
		t.Errorf("GetByEmail() after disable error = %v, want the row", err)
	}
	if _, err := repo.Disable(ctx, localUserBID, now); err != repository.ErrUserNotFound {
		t.Errorf("unknown id error = %v, want ErrUserNotFound", err)
	}
}

// The login lookup only ever sees local rows: an OIDC row holding the same
// address (legal — the email index is local-only) never answers it.
func TestGetByEmailIgnoresNonLocal(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	now := time.Now().UTC()
	if _, err := repo.UpsertBySubject(ctx, model.User{
		ID: localUserBID, Subject: "ada-1", Issuer: "https://idp.example.test",
		Name: "Ada", Email: "ada@example.test", IsActive: true, EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertBySubject() error = %v", err)
	}
	if _, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), nil); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := repo.GetByEmail(ctx, "ada@example.test")
	if err != nil {
		t.Fatalf("GetByEmail() error = %v", err)
	}
	if got.ID != localUserAID {
		t.Errorf("GetByEmail() id = %s, want the local row %s", got.ID, localUserAID)
	}
	if _, err := repo.GetByEmail(ctx, "nobody@example.test"); err != repository.ErrUserNotFound {
		t.Errorf("unknown email error = %v, want ErrUserNotFound", err)
	}
}

func TestVerificationTokenLifecycle(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	pending := localUser(localUserAID, "ada@example.test")
	pending.EmailVerifiedAt = nil
	if _, err := repo.Create(ctx, pending, []string{"viewer"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now := time.Now().UTC()
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-1", now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetVerificationToken() error = %v", err)
	}
	// Resend replaces: only the newest link works.
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-2", now.Add(2*time.Hour), now); err != nil {
		t.Fatalf("second SetVerificationToken() error = %v", err)
	}
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-1", now.Add(time.Minute)); err != repository.ErrUserNotFound {
		t.Errorf("stale token error = %v, want ErrUserNotFound", err)
	}
	verified, err := repo.ConsumeVerificationToken(ctx, "hash-2", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ConsumeVerificationToken() error = %v", err)
	}
	if !verified.EmailVerified() {
		t.Error("EmailVerified() = false, want true")
	}
	if len(verified.Roles) != 1 || verified.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", verified.Roles)
	}
	// Single-use: the cleared token never verifies twice.
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-2", now.Add(2*time.Minute)); err != repository.ErrUserNotFound {
		t.Errorf("second consume error = %v, want ErrUserNotFound", err)
	}
}

func TestUserUpdateVerdictClearsToken(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	pending := localUser(localUserAID, "ada@example.test")
	pending.EmailVerifiedAt = nil
	if _, err := repo.Create(ctx, pending, nil); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now := time.Now().UTC()
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-1", now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetVerificationToken() error = %v", err)
	}
	// The send stamps the pacing timestamp the cooldown reads.
	stamped, err := repo.GetByID(ctx, localUserAID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	// timestamptz truncates to microseconds, so compare within a delta.
	if stamped.VerificationSentAt == nil || now.Sub(*stamped.VerificationSentAt) > time.Second {
		t.Errorf("VerificationSentAt = %v, want ~%v", stamped.VerificationSentAt, now)
	}
	// Verifying by hand retires the outstanding link in the same write.
	verified := stamped
	verified.EmailVerifiedAt = &now
	verified.UpdatedAt = now
	got, err := repo.Update(ctx, verified, []string{"viewer"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !got.EmailVerified() {
		t.Error("EmailVerified() = false, want true")
	}
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-1", now.Add(time.Minute)); err != repository.ErrUserNotFound {
		t.Errorf("consume after verify error = %v, want ErrUserNotFound", err)
	}
	// Unverifying by hand clears the stamp.
	unmarked := got
	unmarked.EmailVerifiedAt = nil
	unmarked.UpdatedAt = now
	got, err = repo.Update(ctx, unmarked, []string{"viewer"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got.EmailVerified() {
		t.Error("EmailVerified() = true after unmark, want false")
	}
	// A new address never inherits the old link: the email change retires
	// it in the same write.
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-2", now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetVerificationToken() error = %v", err)
	}
	moved := got
	moved.Email = "ada.new@example.test"
	moved.UpdatedAt = now
	if _, err := repo.Update(ctx, moved, []string{"viewer"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-2", now.Add(time.Minute)); err != repository.ErrUserNotFound {
		t.Errorf("consume after email change error = %v, want ErrUserNotFound", err)
	}
}

// TestUserListFilters seeds a mixed directory and pins every filter:
// multi-value IN, bool scoping, substring search with LIKE escaping,
// AND-combination, and both created_at directions.
func TestUserListFilters(t *testing.T) {
	const (
		sys   = "11111111-1111-7111-8111-111111111111"
		ada   = localUserAID
		bo    = localUserBID
		cal   = "44444444-4444-7444-8444-444444444444"
		dan   = "55555555-5555-7555-8555-555555555555"
		grace = "66666666-6666-7666-8666-666666666666"
		hal   = "77777777-7777-7777-8777-777777777777"
	)
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	// Production backfills every pre-existing row to verified; the
	// fixture seed does not, so align the system row with reality.
	if err := db.Exec("UPDATE users SET email_verified_at = now() WHERE id = ?", sys).Error; err != nil {
		t.Fatalf("verify system user: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Second)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }
	mkLocal := func(id, name, email string, created time.Time, roles []string) {
		t.Helper()
		u := localUser(id, email)
		u.Name, u.CreatedAt, u.UpdatedAt = name, created, created
		if _, err := repo.Create(ctx, u, roles); err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
	}
	mkLocal(ada, "Ada", "ada@example.test", at(1), []string{"admin"})
	mkLocal(bo, "Bo", "bo@example.test", at(2), []string{"viewer"})
	mkLocal(hal, "Hal", "we%ird@example.test", at(3), nil)
	pending := localUser(cal, "cal@example.test")
	pending.Name, pending.CreatedAt, pending.UpdatedAt = "Cal", at(4), at(4)
	pending.EmailVerifiedAt = nil
	if _, err := repo.Create(ctx, pending, nil); err != nil {
		t.Fatalf("Create(Cal) error = %v", err)
	}
	mkLocal(dan, "Dan", "dan@example.test", at(5), []string{"viewer"})
	// Create always inserts active; the prod path to inactive is Disable.
	if _, err := repo.Disable(ctx, dan, at(5)); err != nil {
		t.Fatalf("Disable(Dan) error = %v", err)
	}
	if _, err := repo.UpsertBySubject(ctx, model.User{
		ID: grace, Subject: "g-1", Issuer: "https://idp.example.test",
		Name: "Grace", Email: "grace@example.test", IsActive: true,
		EmailVerifiedAt: &base, CreatedAt: at(6), UpdatedAt: at(6),
	}); err != nil {
		t.Fatalf("UpsertBySubject(Grace) error = %v", err)
	}

	listIDs := func(q repository.UserListQuery) ([]string, int64) {
		t.Helper()
		if q.PerPage == 0 {
			q.PerPage = 50
		}
		users, total, err := repo.List(ctx, q)
		if err != nil {
			t.Fatalf("List(%+v) error = %v", q, err)
		}
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		return ids, total
	}
	tr, fa := true, false
	setCases := map[string]struct {
		q    repository.UserListQuery
		want map[string]bool
	}{
		"empty lists all": {repository.UserListQuery{}, map[string]bool{sys: true, ada: true, bo: true, cal: true, dan: true, grace: true, hal: true}},
		"ids":             {repository.UserListQuery{IDs: []string{ada, grace}}, map[string]bool{ada: true, grace: true}},
		"role admin":      {repository.UserListQuery{Roles: []string{"admin"}}, map[string]bool{ada: true}},
		"role viewer":     {repository.UserListQuery{Roles: []string{"viewer"}}, map[string]bool{bo: true, dan: true}},
		"roles combined":  {repository.UserListQuery{Roles: []string{"admin", "viewer"}}, map[string]bool{ada: true, bo: true, dan: true}},
		"role unknown":    {repository.UserListQuery{Roles: []string{"wizard"}}, map[string]bool{}},
		"issuer local":    {repository.UserListQuery{Issuers: []string{"local"}}, map[string]bool{ada: true, bo: true, cal: true, dan: true, hal: true}},
		"issuer idp":      {repository.UserListQuery{Issuers: []string{"https://idp.example.test"}}, map[string]bool{grace: true}},
		"is_local true":   {repository.UserListQuery{IsLocal: &tr}, map[string]bool{ada: true, bo: true, cal: true, dan: true, hal: true}},
		"is_local false":  {repository.UserListQuery{IsLocal: &fa}, map[string]bool{sys: true, grace: true}},
		"verified true":   {repository.UserListQuery{EmailVerified: &tr}, map[string]bool{sys: true, ada: true, bo: true, dan: true, grace: true, hal: true}},
		"verified false":  {repository.UserListQuery{EmailVerified: &fa}, map[string]bool{cal: true}},
		"active false":    {repository.UserListQuery{IsActive: &fa}, map[string]bool{dan: true}},
		"active true":     {repository.UserListQuery{IsActive: &tr}, map[string]bool{sys: true, ada: true, bo: true, cal: true, grace: true, hal: true}},
		"email substr":    {repository.UserListQuery{Email: "ADA@"}, map[string]bool{ada: true}},
		"email wildcard":  {repository.UserListQuery{Email: "%"}, map[string]bool{hal: true}},
		"name substr":     {repository.UserListQuery{Name: "grac"}, map[string]bool{grace: true}},
		"combined":        {repository.UserListQuery{Roles: []string{"viewer"}, IsActive: &tr}, map[string]bool{bo: true}},
	}
	for name, tc := range setCases {
		got, total := listIDs(tc.q)
		if int(total) != len(tc.want) || len(got) != len(tc.want) {
			t.Errorf("%s: total = %d ids = %v, want %d ids", name, total, got, len(tc.want))
			continue
		}
		for _, id := range got {
			if !tc.want[id] {
				t.Errorf("%s: unexpected id %s in %v", name, id, got)
			}
		}
	}

	// Directions pin the full page order; the fixture timestamps ascend
	// sys < ada < bo < hal < cal < dan < grace.
	ordered, _ := listIDs(repository.UserListQuery{Order: "created_at"})
	wantAsc := []string{sys, ada, bo, hal, cal, dan, grace}
	for i := range wantAsc {
		if ordered[i] != wantAsc[i] {
			t.Errorf("created_at order = %v, want %v", ordered, wantAsc)
			break
		}
	}
	ordered, _ = listIDs(repository.UserListQuery{Order: "-created_at"})
	for i, j := 0, len(wantAsc)-1; i < len(wantAsc); i, j = i+1, j-1 {
		if ordered[i] != wantAsc[j] {
			t.Errorf("-created_at order = %v, want reverse of %v", ordered, wantAsc)
			break
		}
	}
}

func TestConsumeVerificationTokenExpiry(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	pending := localUser(localUserAID, "ada@example.test")
	pending.EmailVerifiedAt = nil
	if _, err := repo.Create(ctx, pending, nil); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now := time.Now().UTC()
	if err := repo.SetVerificationToken(ctx, localUserAID, "hash-1", now.Add(time.Hour), now); err != nil {
		t.Fatalf("SetVerificationToken() error = %v", err)
	}
	if _, err := repo.ConsumeVerificationToken(ctx, "hash-1", now.Add(2*time.Hour)); err != repository.ErrVerificationExpired {
		t.Errorf("expired token error = %v, want ErrVerificationExpired", err)
	}
	// The expiry leaves the account pending, not verified.
	got, err := repo.GetByID(ctx, localUserAID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.EmailVerified() {
		t.Error("EmailVerified() = true after an expired consume, want false")
	}
}

// Deleting a role drops the memberships naming it, or the RESTRICT foreign
// key would turn a valid delete into a 500.
func TestRoleDeleteClearsMemberships(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	users := repository.NewUserRepository(db)
	roles := repository.NewRoleRepository(db)
	now := time.Now().UTC()

	if err := roles.Seed(ctx,
		[]model.Role{{Name: "holder", CreatedAt: now, UpdatedAt: now}},
		[]model.RolePermission{{Role: "holder", Action: "roles:write", CreatedAt: now, UpdatedAt: now}},
	); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	if _, err := users.Create(ctx, localUser(localUserAID, "ada@example.test"), []string{"admin", "viewer"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := roles.Delete(ctx, "admin"); err != nil {
		t.Fatalf("Delete(admin) error = %v", err)
	}
	got, err := users.GetByID(ctx, localUserAID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "viewer" {
		t.Errorf("roles = %v, want [viewer]", got.Roles)
	}
}

// List fills stored roles for local rows in the page and leaves OIDC rows
// untouched: their roles live on the live token.
func TestUserListIncludesLocalRoles(t *testing.T) {
	db := setupTestDB(t)
	seedLocalRoles(t, db)
	ctx := context.Background()
	repo := repository.NewUserRepository(db)

	now := time.Now().UTC()
	if _, err := repo.Create(ctx, localUser(localUserAID, "ada@example.test"), []string{"admin"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := repo.UpsertBySubject(ctx, model.User{
		ID: localUserBID, Subject: "g-1", Issuer: "https://idp.example.test",
		Name: "Grace", Email: "grace@example.test", IsActive: true, EmailVerifiedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertBySubject() error = %v", err)
	}
	users, total, err := repo.List(ctx, repository.UserListQuery{Page: 1, PerPage: 50})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (system + local + OIDC)", total)
	}
	byID := make(map[string]model.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	if roles := byID[localUserAID].Roles; len(roles) != 1 || roles[0] != "admin" {
		t.Errorf("local roles = %v, want [admin]", roles)
	}
	if roles := byID[localUserBID].Roles; len(roles) != 0 {
		t.Errorf("OIDC roles = %v, want none", roles)
	}
}
