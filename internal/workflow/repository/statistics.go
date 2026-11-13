package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"gorm.io/gorm"
)

// StatisticsQuery filters the aggregate summary by instance creation time
// and workflow definition. Bounds are inclusive: created_at >= From AND
// created_at <= To. Order selects the runs-per-day bucket direction
// ("date" or "-date", default "-date"); callers validate before use.
// WorkflowDefinitionID scopes the aggregate to one definition version;
// with IncludeAll it expands to every version sharing that definition's
// lineage.
type StatisticsQuery struct {
	From  *time.Time
	To    *time.Time
	Order string
	// CreatedBy scopes the aggregate to one owner. The service sets it
	// from the authenticated principal, so a caller cannot widen the
	// result set by naming another user: the field is not parsed from
	// the query string.
	CreatedBy string
	// WorkflowDefinitionID scopes the aggregate to one workflow
	// definition version. Empty leaves the aggregate unfiltered.
	WorkflowDefinitionID string
	// IncludeAll expands WorkflowDefinitionID to the whole version
	// family (every version with the same lineage_id). It is meaningless
	// without WorkflowDefinitionID; callers validate before use.
	IncludeAll bool
}

// RunsPerDayPoint is one calendar-day bucket (Date is YYYY-MM-DD).
type RunsPerDayPoint struct {
	Date     string
	Total    int64
	Finished int64
	Failed   int64
	Stopped  int64
}

// StatisticsResult is the aggregate over a creation-time window.
// SuccessRate is finished/terminal (nil when terminal is 0).
// AverageDurationMS is the mean finished_at-started_at in milliseconds over
// terminal runs with both timestamps set (nil when none qualify).
// ActiveRuns counts waiting+running+paused instances in the same window.
type StatisticsResult struct {
	TotalRuns         int64
	FinishedRuns      int64
	FailedRuns        int64
	StoppedRuns       int64
	TerminalRuns      int64
	ActiveRuns        int64
	SuccessRate       *float64
	AverageDurationMS *float64
	RunsPerDay        []RunsPerDayPoint
}

// statisticsAggRow is the single-row aggregate scan target.
type statisticsAggRow struct {
	Total    int64           `gorm:"column:total"`
	Finished int64           `gorm:"column:finished"`
	Failed   int64           `gorm:"column:failed"`
	Stopped  int64           `gorm:"column:stopped"`
	Terminal int64           `gorm:"column:terminal"`
	Active   int64           `gorm:"column:active"`
	AvgMs    sql.NullFloat64 `gorm:"column:avg_ms"`
}

// statisticsDayRow is one (day, status) count for the per-day pivot.
type statisticsDayRow struct {
	Day    time.Time `gorm:"column:day"`
	Status string    `gorm:"column:status"`
	N      int64     `gorm:"column:n"`
}

// applyStatisticsFilters scopes a workflow-instances query to the
// aggregate's creation window, owner, and definition filter. Both the
// totals query and the runs-per-day query share it so the two can never
// disagree on the row set.
func applyStatisticsFilters(db *gorm.DB, q StatisticsQuery) *gorm.DB {
	if q.From != nil {
		db = db.Where("created_at >= ?", *q.From)
	}
	if q.To != nil {
		db = db.Where("created_at <= ?", *q.To)
	}
	if q.CreatedBy != "" {
		db = db.Where("created_by = ?", q.CreatedBy)
	}
	if q.WorkflowDefinitionID != "" {
		if q.IncludeAll {
			// Every version in the named definition's lineage. An
			// unknown id matches no lineage, so the aggregate is zeros.
			db = db.Where(`workflow_definition_id IN (
				SELECT id FROM workflow_definitions WHERE lineage_id = (
					SELECT lineage_id FROM workflow_definitions WHERE id = ?))`,
				q.WorkflowDefinitionID)
		} else {
			db = db.Where("workflow_definition_id = ?", q.WorkflowDefinitionID)
		}
	}
	return db
}

// StatisticsSummary aggregates instance counts, success rate, average
// terminal duration, and per-day buckets over the creation-time window.
func (r *instanceRepo) StatisticsSummary(ctx context.Context, q StatisticsQuery) (StatisticsResult, error) {
	var out StatisticsResult

	base := applyStatisticsFilters(r.db.WithContext(ctx).Model(&WorkflowInstanceModel{}), q)

	var agg statisticsAggRow
	if err := base.Select(`COUNT(*) AS total,` +
		` COUNT(*) FILTER (WHERE status = 'finished') AS finished,` +
		` COUNT(*) FILTER (WHERE status = 'failed') AS failed,` +
		` COUNT(*) FILTER (WHERE status = 'stopped') AS stopped,` +
		` COUNT(*) FILTER (WHERE status IN ('finished','failed','stopped')) AS terminal,` +
		` COUNT(*) FILTER (WHERE status IN ('waiting','running','paused')) AS active,` +
		` AVG(EXTRACT(EPOCH FROM (finished_at - started_at)) * 1000)` +
		` FILTER (WHERE status IN ('finished','failed','stopped')` +
		` AND started_at IS NOT NULL AND finished_at IS NOT NULL) AS avg_ms`,
	).Scan(&agg).Error; err != nil {
		return StatisticsResult{}, fmt.Errorf("statistics summary: %w", err)
	}
	out.TotalRuns = agg.Total
	out.FinishedRuns = agg.Finished
	out.FailedRuns = agg.Failed
	out.StoppedRuns = agg.Stopped
	out.TerminalRuns = agg.Terminal
	out.ActiveRuns = agg.Active
	if agg.Terminal > 0 {
		rate := float64(agg.Finished) / float64(agg.Terminal)
		out.SuccessRate = &rate
	}
	if agg.AvgMs.Valid {
		avg := agg.AvgMs.Float64
		out.AverageDurationMS = &avg
	}

	dayDir := "DESC"
	if q.Order == "date" {
		dayDir = "ASC"
	}
	dayQuery := applyStatisticsFilters(r.db.WithContext(ctx).Model(&WorkflowInstanceModel{}), q)
	var dayRows []statisticsDayRow
	if err := dayQuery.Select(`DATE(created_at) AS day, status, COUNT(*) AS n`).
		Group("1, 2").
		Order("1 " + dayDir).
		Scan(&dayRows).Error; err != nil {
		return StatisticsResult{}, fmt.Errorf("statistics runs per day: %w", err)
	}
	byDay := make(map[string]*RunsPerDayPoint, len(dayRows))
	var order []string
	for _, row := range dayRows {
		d := row.Day.Format("2006-01-02")
		p, ok := byDay[d]
		if !ok {
			p = &RunsPerDayPoint{Date: d}
			byDay[d] = p
			order = append(order, d)
		}
		p.Total += row.N
		switch row.Status {
		case string(model.WorkflowFinished):
			p.Finished += row.N
		case string(model.WorkflowFailed):
			p.Failed += row.N
		case string(model.WorkflowStopped):
			p.Stopped += row.N
		}
	}
	out.RunsPerDay = make([]RunsPerDayPoint, 0, len(order))
	for _, d := range order {
		out.RunsPerDay = append(out.RunsPerDay, *byDay[d])
	}
	if out.RunsPerDay == nil {
		out.RunsPerDay = []RunsPerDayPoint{}
	}
	return out, nil
}
