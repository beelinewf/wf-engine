package repository_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
)

func TestStatisticsSummaryCounts(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	now := time.Now().UTC().Truncate(time.Second)
	day1 := now.Add(-48 * time.Hour)
	day2 := now.Add(-24 * time.Hour)

	mk := func(id string, status model.WorkflowStatus, created time.Time, withTimes bool) model.WorkflowInstance {
		w := newTestInstance(id, status, "")
		w.CreatedAt = created
		w.UpdatedAt = created
		if withTimes {
			start := created
			fin := created.Add(90 * time.Second)
			w.StartedAt = &start
			w.FinishedAt = &fin
		}
		return w
	}

	// Day 1: 1 finished (with timestamps), 1 failed (with timestamps), 1 running.
	insertInstance(t, db, mk("11111111-1111-7111-8111-111111111111", model.WorkflowFinished, day1, true))
	failed := mk("22222222-2222-7222-8222-222222222222", model.WorkflowFailed, day1, true)
	failed.Error = "boom"
	insertInstance(t, db, failed)
	insertInstance(t, db, mk("33333333-3333-7333-8333-333333333333", model.WorkflowRunning, day1, false))
	// Day 2: 1 stopped (with timestamps), 1 waiting.
	insertInstance(t, db, mk("44444444-4444-7444-8444-444444444444", model.WorkflowStopped, day2, true))
	insertInstance(t, db, mk("55555555-5555-7555-8555-555555555555", model.WorkflowWaiting, day2, false))

	from := day1.Add(-time.Hour)
	to := now.Add(time.Hour)
	got, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{From: &from, To: &to})
	if err != nil {
		t.Fatalf("StatisticsSummary() error = %v", err)
	}
	if got.TotalRuns != 5 {
		t.Errorf("TotalRuns = %d, want 5", got.TotalRuns)
	}
	if got.FinishedRuns != 1 || got.FailedRuns != 1 || got.StoppedRuns != 1 {
		t.Errorf("finished/failed/stopped = %d/%d/%d, want 1/1/1", got.FinishedRuns, got.FailedRuns, got.StoppedRuns)
	}
	if got.TerminalRuns != 3 {
		t.Errorf("TerminalRuns = %d, want 3", got.TerminalRuns)
	}
	// Active = waiting + running + paused: 1 running (day1) + 1 waiting (day2).
	if got.ActiveRuns != 2 {
		t.Errorf("ActiveRuns = %d, want 2", got.ActiveRuns)
	}
	if got.SuccessRate == nil || *got.SuccessRate < 0.333 || *got.SuccessRate > 0.334 {
		t.Errorf("SuccessRate = %v, want ~1/3", got.SuccessRate)
	}
	// Three terminal runs each lasting 90s → avg 90000ms.
	if got.AverageDurationMS == nil || *got.AverageDurationMS != 90000 {
		t.Errorf("AverageDurationMS = %v, want 90000", got.AverageDurationMS)
	}
	if len(got.RunsPerDay) != 2 {
		t.Fatalf("RunsPerDay len = %d, want 2", len(got.RunsPerDay))
	}
	// Default order is descending by date: day2 bucket first.
	if got.RunsPerDay[0].Total != 2 || got.RunsPerDay[1].Total != 3 {
		t.Errorf("RunsPerDay totals = %+v, want day2=2 then day1=3", got.RunsPerDay)
	}
	if got.RunsPerDay[0].Date == "" || got.RunsPerDay[1].Date == "" {
		t.Errorf("RunsPerDay dates must be YYYY-MM-DD, got %+v", got.RunsPerDay)
	}

	// Narrow window to day 2 only (inclusive bounds).
	from2 := day2.Add(-time.Hour)
	to2 := day2.Add(time.Hour)
	day2only, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{From: &from2, To: &to2})
	if err != nil {
		t.Fatalf("StatisticsSummary(day2) error = %v", err)
	}
	if day2only.TotalRuns != 2 {
		t.Errorf("day2 TotalRuns = %d, want 2", day2only.TotalRuns)
	}
	if day2only.ActiveRuns != 1 {
		t.Errorf("day2 ActiveRuns = %d, want 1", day2only.ActiveRuns)
	}

	// Empty window: zero counts, nil rate/avg.
	from3 := now.Add(time.Hour)
	to3 := now.Add(2 * time.Hour)
	empty, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{From: &from3, To: &to3})
	if err != nil {
		t.Fatalf("StatisticsSummary(empty) error = %v", err)
	}
	if empty.TotalRuns != 0 || empty.TerminalRuns != 0 || empty.ActiveRuns != 0 {
		t.Errorf("empty window = %+v, want zeros", empty)
	}
	if empty.SuccessRate != nil || empty.AverageDurationMS != nil {
		t.Errorf("empty window rate/avg = %v/%v, want nil/nil", empty.SuccessRate, empty.AverageDurationMS)
	}
	if len(empty.RunsPerDay) != 0 {
		t.Errorf("empty window RunsPerDay = %+v, want empty", empty.RunsPerDay)
	}
}

func TestStatisticsSummaryOwnerScoping(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	const otherUserID = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	now := time.Now().UTC().Truncate(time.Second)
	other := repository.UserToModel(model.User{ID: otherUserID, Name: "other", Email: "other@localhost", CreatedAt: now, UpdatedAt: now})
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("seed other user: %v", err)
	}

	day1 := now.Add(-48 * time.Hour)
	day2 := now.Add(-24 * time.Hour)
	mk := func(id, owner string, status model.WorkflowStatus, created time.Time) model.WorkflowInstance {
		w := newTestInstance(id, status, "")
		w.CreatedBy = owner
		w.CreatedAt = created
		w.UpdatedAt = created
		return w
	}

	// Owner fixtureUserID: 2 finished on day1. Owner otherUserID: 1 failed
	// on day1, 1 waiting on day2.
	insertInstance(t, db, mk("66666666-6666-7666-8666-666666666666", fixtureUserID, model.WorkflowFinished, day1))
	insertInstance(t, db, mk("67676767-6767-7676-8676-676767676767", fixtureUserID, model.WorkflowFinished, day1))
	insertInstance(t, db, mk("68686868-6868-7686-8686-686868686868", otherUserID, model.WorkflowFailed, day1))
	insertInstance(t, db, mk("69696969-6969-7696-8696-696969696969", otherUserID, model.WorkflowWaiting, day2))

	from := day1.Add(-time.Hour)
	to := now.Add(time.Hour)

	mine, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{From: &from, To: &to, CreatedBy: fixtureUserID})
	if err != nil {
		t.Fatalf("StatisticsSummary(mine) error = %v", err)
	}
	if mine.TotalRuns != 2 || mine.FinishedRuns != 2 || mine.FailedRuns != 0 {
		t.Errorf("mine finished/failed/total = %d/%d/%d, want 2/0/2",
			mine.FinishedRuns, mine.FailedRuns, mine.TotalRuns)
	}
	if len(mine.RunsPerDay) != 1 || mine.RunsPerDay[0].Total != 2 || mine.RunsPerDay[0].Finished != 2 {
		t.Errorf("mine RunsPerDay = %+v, want one day1 bucket with total/finished 2/2", mine.RunsPerDay)
	}

	theirs, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{From: &from, To: &to, CreatedBy: otherUserID})
	if err != nil {
		t.Fatalf("StatisticsSummary(theirs) error = %v", err)
	}
	if theirs.TotalRuns != 2 || theirs.FailedRuns != 1 || theirs.ActiveRuns != 1 {
		t.Errorf("theirs failed/active/total = %d/%d/%d, want 1/1/2",
			theirs.FailedRuns, theirs.ActiveRuns, theirs.TotalRuns)
	}
	if len(theirs.RunsPerDay) != 2 || theirs.RunsPerDay[0].Total != 1 || theirs.RunsPerDay[1].Total != 1 {
		t.Errorf("theirs RunsPerDay = %+v, want two single-run buckets", theirs.RunsPerDay)
	}

	// No owner set: the pre-scoping unscoped aggregate over all rows.
	all, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{From: &from, To: &to})
	if err != nil {
		t.Fatalf("StatisticsSummary(all) error = %v", err)
	}
	if all.TotalRuns != 4 {
		t.Errorf("unscoped TotalRuns = %d, want 4", all.TotalRuns)
	}
}

func TestStatisticsSummaryDefinitionFilter(t *testing.T) {
	db := setupTestDB(t)
	seedInstanceFixture(t, db)
	ctx := context.Background()
	repo := repository.NewInstanceRepository(db)

	now := time.Now().UTC().Truncate(time.Second)
	lineage := "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	v1 := "a1111111-1111-7111-8111-111111111111"
	v2 := "a2222222-2222-7222-8222-222222222222"
	v3 := "a3333333-3333-7333-8333-333333333333"
	unrelated := "b4444444-4444-7444-8444-444444444444"
	mkDef := func(id string, version int, lineageID string, prev *string) {
		t.Helper()
		wd := repository.WorkflowDefinitionToModel(model.WorkflowDefinition{
			ID: id, Name: "versioned-flow", Version: version,
			PreviousVersionID: prev, LineageID: lineageID, Content: json.RawMessage(`{}`),
			CreatedBy: fixtureUserID, UpdatedBy: fixtureUserID, CreatedAt: now, UpdatedAt: now,
		})
		if err := db.Create(&wd).Error; err != nil {
			t.Fatalf("seed workflow definition %s: %v", id, err)
		}
	}
	mkDef(v1, 1, lineage, nil)
	mkDef(v2, 2, lineage, &v1)
	mkDef(v3, 3, lineage, &v2)
	mkDef(unrelated, 1, unrelated, nil)

	day1 := now.Add(-48 * time.Hour)
	day2 := now.Add(-24 * time.Hour)
	mk := func(id, defID string, status model.WorkflowStatus, created time.Time) model.WorkflowInstance {
		w := newTestInstance(id, status, "")
		w.WorkflowDefinitionID = defID
		w.CreatedAt = created
		w.UpdatedAt = created
		return w
	}
	insertInstance(t, db, mk("c1111111-1111-7111-8111-111111111111", v1, model.WorkflowFinished, day1))
	failed := mk("c2222222-2222-7222-8222-222222222222", v2, model.WorkflowFailed, day1)
	failed.Error = "boom"
	insertInstance(t, db, failed)
	insertInstance(t, db, mk("c3333333-3333-7333-8333-333333333333", v2, model.WorkflowRunning, day2))
	insertInstance(t, db, mk("c4444444-4444-7444-8444-444444444444", v3, model.WorkflowWaiting, day2))
	insertInstance(t, db, mk("c5555555-5555-7555-8555-555555555555", unrelated, model.WorkflowFinished, day1))

	// Exact: only v2's own runs.
	exact, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{WorkflowDefinitionID: v2})
	if err != nil {
		t.Fatalf("StatisticsSummary(exact) error = %v", err)
	}
	if exact.TotalRuns != 2 || exact.FailedRuns != 1 || exact.ActiveRuns != 1 {
		t.Errorf("exact failed/active/total = %d/%d/%d, want 1/1/2",
			exact.FailedRuns, exact.ActiveRuns, exact.TotalRuns)
	}
	if len(exact.RunsPerDay) != 2 {
		t.Errorf("exact RunsPerDay len = %d, want 2 (one run on each day)", len(exact.RunsPerDay))
	}

	// Expanded: the whole lineage, excluding the unrelated definition.
	expanded, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{WorkflowDefinitionID: v2, IncludeAll: true})
	if err != nil {
		t.Fatalf("StatisticsSummary(include_all) error = %v", err)
	}
	if expanded.TotalRuns != 4 || expanded.FinishedRuns != 1 || expanded.FailedRuns != 1 {
		t.Errorf("include_all finished/failed/total = %d/%d/%d, want 1/1/4",
			expanded.FinishedRuns, expanded.FailedRuns, expanded.TotalRuns)
	}
	if expanded.ActiveRuns != 2 {
		t.Errorf("include_all ActiveRuns = %d, want 2", expanded.ActiveRuns)
	}

	// The filter composes with the window and the owner scope.
	from := day2.Add(-time.Hour)
	to := now.Add(time.Hour)
	composed, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{
		From: &from, To: &to, CreatedBy: fixtureUserID, WorkflowDefinitionID: v2, IncludeAll: true,
	})
	if err != nil {
		t.Fatalf("StatisticsSummary(composed) error = %v", err)
	}
	if composed.TotalRuns != 2 {
		t.Errorf("composed TotalRuns = %d, want 2 (v2 running + v3 waiting on day2)", composed.TotalRuns)
	}
	if len(composed.RunsPerDay) != 1 || composed.RunsPerDay[0].Total != 2 {
		t.Errorf("composed RunsPerDay = %+v, want one day2 bucket with total 2", composed.RunsPerDay)
	}

	// Unknown definition: zero counts, nil rate/avg, empty buckets.
	unknown, err := repo.StatisticsSummary(ctx, repository.StatisticsQuery{
		WorkflowDefinitionID: "d6666666-6666-7666-8666-666666666666", IncludeAll: true,
	})
	if err != nil {
		t.Fatalf("StatisticsSummary(unknown) error = %v", err)
	}
	if unknown.TotalRuns != 0 || unknown.TerminalRuns != 0 || unknown.ActiveRuns != 0 {
		t.Errorf("unknown definition = %+v, want zeros", unknown)
	}
	if unknown.SuccessRate != nil || unknown.AverageDurationMS != nil {
		t.Errorf("unknown definition rate/avg = %v/%v, want nil/nil", unknown.SuccessRate, unknown.AverageDurationMS)
	}
	if len(unknown.RunsPerDay) != 0 {
		t.Errorf("unknown definition RunsPerDay = %+v, want empty", unknown.RunsPerDay)
	}
}
