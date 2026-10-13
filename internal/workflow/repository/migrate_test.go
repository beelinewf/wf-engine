package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/simpwf/workflow-engine/internal/workflow/model"
	"github.com/simpwf/workflow-engine/internal/workflow/repository"
)

func TestMigratedFieldsRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	srcID := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	newID := "cccccccc-cccc-7ccc-8ccc-cccccccccccc"

	src := newTestInstance(srcID, model.WorkflowPaused, model.WaitingReasonRunnable)
	src.SupersededBy = newID
	insertInstance(t, db, src)

	nxt := newTestInstance(newID, model.WorkflowPaused, model.WaitingReasonRunnable)
	nxt.MigratedFrom = srcID
	insertInstance(t, db, nxt)

	gotSrc, err := repo.GetByID(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSrc.SupersededBy != newID {
		t.Errorf("SupersededBy = %q, want %q", gotSrc.SupersededBy, newID)
	}
	if gotSrc.MigratedFrom != "" {
		t.Errorf("MigratedFrom = %q, want empty", gotSrc.MigratedFrom)
	}

	gotNew, err := repo.GetByID(ctx, newID)
	if err != nil {
		t.Fatal(err)
	}
	if gotNew.MigratedFrom != srcID {
		t.Errorf("MigratedFrom = %q, want %q", gotNew.MigratedFrom, srcID)
	}
	if gotNew.SupersededBy != "" {
		t.Errorf("SupersededBy = %q, want empty", gotNew.SupersededBy)
	}

	plain := newTestInstance("dddddddd-dddd-7ddd-8ddd-dddddddddddd", model.WorkflowWaiting, model.WaitingReasonRunnable)
	insertInstance(t, db, plain)
	gotPlain, err := repo.GetByID(ctx, plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPlain.MigratedFrom != "" || gotPlain.SupersededBy != "" {
		t.Errorf("links = (%q, %q), want empty", gotPlain.MigratedFrom, gotPlain.SupersededBy)
	}
}

func migrateTestUpdate(srcID, newID string, srcRevision int64) repository.MigrateUpdate {
	nxt := newTestInstance(newID, model.WorkflowPaused, model.WaitingReasonRunnable)
	nxt.MigratedFrom = srcID
	nxt.Context = json.RawMessage(`{"a":1}`)
	return repository.MigrateUpdate{
		New:             nxt,
		SourceID:        srcID,
		SourceRevision:  srcRevision,
		Actor:           fixtureUserID,
		NewEventData:    json.RawMessage(`{"migrated_from":"` + srcID + `"}`),
		SourceEventData: json.RawMessage(`{"new_instance_id":"` + newID + `"}`),
	}
}

func TestMigrateInstanceHappyPath(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	srcID := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	newID := "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
	insertInstance(t, db, newTestInstance(srcID, model.WorkflowPaused, model.WaitingReasonRunnable))

	if err := repo.MigrateInstance(ctx, migrateTestUpdate(srcID, newID, 0)); err != nil {
		t.Fatalf("MigrateInstance() error = %v", err)
	}

	src, err := repo.GetByID(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != model.WorkflowStopped {
		t.Errorf("source status = %s, want stopped", src.Status)
	}
	if src.SupersededBy != newID {
		t.Errorf("source superseded_by = %q, want %q", src.SupersededBy, newID)
	}
	if src.Revision != 1 {
		t.Errorf("source revision = %d, want 1", src.Revision)
	}
	if src.FinishedAt == nil {
		t.Error("source finished_at is nil, want set")
	}
	if src.LeasedBy != "" {
		t.Errorf("source leased_by = %q, want cleared", src.LeasedBy)
	}

	nxt, err := repo.GetByID(ctx, newID)
	if err != nil {
		t.Fatal(err)
	}
	if nxt.MigratedFrom != srcID {
		t.Errorf("new migrated_from = %q, want %q", nxt.MigratedFrom, srcID)
	}
	assertJSON(t, nxt.Context, `{"a":1}`)

	created := instanceEventsOfType(t, db, newID, "instance_created")
	if len(created) != 1 || created[0]["migrated_from"] != srcID {
		t.Errorf("instance_created events = %v, want one with migrated_from", created)
	}
	migrated := instanceEventsOfType(t, db, srcID, "instance_migrated")
	if len(migrated) != 1 || migrated[0]["new_instance_id"] != newID {
		t.Errorf("instance_migrated events = %v, want one with new_instance_id", migrated)
	}
}

func TestMigrateInstanceSupersedesLivePark(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	srcID := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	newID := "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
	insertInstance(t, db, newTestInstance(srcID, model.WorkflowWaiting, model.WaitingReasonInput))
	now := time.Now().UTC()
	attemptID := "eeeeeeee-eeee-7eee-8eee-eeeeeeeeeeee"
	if err := repo.InsertNodeInstance(ctx, model.NodeInstance{
		ID: attemptID, WorkflowInstanceID: srcID,
		NodeID: "ffffffff-ffff-7fff-8fff-ffffffffffff", NodeDefinitionID: fixtureNodeDefID, Name: "in",
		Type: string(model.NodeTypeInput), Attempt: 1, Status: model.NodeRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertNodeInstance() error = %v", err)
	}

	if err := repo.MigrateInstance(ctx, migrateTestUpdate(srcID, newID, 0)); err != nil {
		t.Fatalf("MigrateInstance() error = %v", err)
	}

	occ, err := repo.GetNodeInstance(ctx, srcID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if occ.Status != model.NodeStopped || !occ.Cancelled || occ.Error != "superseded by migrate" {
		t.Errorf("park = (%s, cancelled=%v, %q), want stopped/cancelled/superseded", occ.Status, occ.Cancelled, occ.Error)
	}
}

func TestMigrateInstanceKeepsSourceError(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	srcID := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	newID := "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
	failed := newTestInstance(srcID, model.WorkflowFailed, model.WaitingReasonRunnable)
	failed.Error = "boom"
	insertInstance(t, db, failed)

	if err := repo.MigrateInstance(ctx, migrateTestUpdate(srcID, newID, 0)); err != nil {
		t.Fatalf("MigrateInstance() error = %v", err)
	}

	src, _ := repo.GetByID(ctx, srcID)
	if src.Status != model.WorkflowStopped || src.Error != "boom" {
		t.Errorf("source = (%s, %q), want (stopped, boom)", src.Status, src.Error)
	}
}

func TestMigrateInstanceConflicts(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	runningID := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	insertInstance(t, db, newTestInstance(runningID, model.WorkflowRunning, model.WaitingReasonRunnable))
	if err := repo.MigrateInstance(ctx, migrateTestUpdate(runningID, "cccccccc-cccc-7ccc-8ccc-cccccccccccc", 0)); !errors.Is(err, repository.ErrStatusConflict) {
		t.Errorf("running source err = %v, want ErrStatusConflict", err)
	}

	runnableID := "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbc"
	insertInstance(t, db, newTestInstance(runnableID, model.WorkflowWaiting, model.WaitingReasonRunnable))
	if err := repo.MigrateInstance(ctx, migrateTestUpdate(runnableID, "cccccccc-cccc-7ccc-8ccc-cccccccccccd", 0)); !errors.Is(err, repository.ErrStatusConflict) {
		t.Errorf("runnable source err = %v, want ErrStatusConflict", err)
	}

	if err := repo.MigrateInstance(ctx, migrateTestUpdate("dddddddd-dddd-7ddd-8ddd-ddddddddddde", "cccccccc-cccc-7ccc-8ccc-ccccccccccce", 0)); !errors.Is(err, repository.ErrInstanceNotFound) {
		t.Errorf("unknown source err = %v, want ErrInstanceNotFound", err)
	}

	staleID := "dddddddd-dddd-7ddd-8ddd-dddddddddddf"
	insertInstance(t, db, newTestInstance(staleID, model.WorkflowPaused, model.WaitingReasonRunnable))
	if err := repo.MigrateInstance(ctx, migrateTestUpdate(staleID, "cccccccc-cccc-7ccc-8ccc-cccccccccccf", 99)); !errors.Is(err, repository.ErrStatusConflict) {
		t.Errorf("stale revision err = %v, want ErrStatusConflict", err)
	}

	doubleID := "dddddddd-dddd-7ddd-8ddd-ddddddddddaa"
	newID := "cccccccc-cccc-7ccc-8ccc-ccccccccccaa"
	insertInstance(t, db, newTestInstance(doubleID, model.WorkflowPaused, model.WaitingReasonRunnable))
	if err := repo.MigrateInstance(ctx, migrateTestUpdate(doubleID, newID, 0)); err != nil {
		t.Fatalf("first MigrateInstance() error = %v", err)
	}
	if err := repo.MigrateInstance(ctx, migrateTestUpdate(doubleID, "cccccccc-cccc-7ccc-8ccc-ccccccccccab", 1)); !errors.Is(err, repository.ErrStatusConflict) {
		t.Errorf("double migrate err = %v, want ErrStatusConflict", err)
	}
}

func TestMigrateInstanceCancelsLiveBranches(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	truncateParallel(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	id := "01950000-0000-7000-8000-0000000000b1"
	insertLeasedInstance(t, db, id)
	prepo := repository.NewParallelRepository(db)
	if _, _, err := prepo.Fork(ctx, parallelFork(id)); err != nil {
		t.Fatalf("Fork() error = %v", err)
	}
	if _, err := prepo.ClaimNextBranches(ctx, "worker-1", time.Minute, 10); err != nil {
		t.Fatalf("ClaimNextBranches() error = %v", err)
	}
	// Pause the join-waiting parent: branches stay live, like Stop sees.
	if _, err := repo.Pause(ctx, id); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	cur, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	newID := "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
	if err := repo.MigrateInstance(ctx, migrateTestUpdate(id, newID, cur.Revision)); err != nil {
		t.Fatalf("MigrateInstance() error = %v", err)
	}

	src, _ := repo.GetByID(ctx, id)
	if src.Status != model.WorkflowStopped || !src.TerminationPending {
		t.Errorf("source = (%s, pending=%v), want (stopped, pending)", src.Status, src.TerminationPending)
	}
	var branches []repository.ParallelBranchModel
	if err := db.Model(&repository.ParallelBranchModel{}).Where("instance_id = ?", id).Find(&branches).Error; err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b.Status != string(model.ParallelBranchCancelled) || b.LeasedBy != "" {
			t.Errorf("branch %s = (%s, leased %q), want cancelled/unleased", b.ID, b.Status, b.LeasedBy)
		}
	}
}

func TestMigrateInstanceAtomicRollback(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	srcID := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	insertInstance(t, db, newTestInstance(srcID, model.WorkflowWaiting, model.WaitingReasonInput))

	// The replacement id collides with the source row, so the insert fails
	// and the whole transaction must roll back.
	u := migrateTestUpdate(srcID, srcID, 0)
	if err := repo.MigrateInstance(ctx, u); err == nil {
		t.Fatal("MigrateInstance() error = nil, want insert failure")
	}

	src, err := repo.GetByID(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != model.WorkflowWaiting || src.Revision != 0 || src.SupersededBy != "" {
		t.Errorf("source = (%s, rev %d, %q), want untouched waiting row", src.Status, src.Revision, src.SupersededBy)
	}
	if events := instanceEventsOfType(t, db, srcID, "instance_migrated"); len(events) != 0 {
		t.Errorf("instance_migrated events = %v, want none", events)
	}
}
