package repository

import (
	"context"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// CountActiveByStatus counts non-terminal instances by status. It serves the
// cached wf_instances_active gauge; callers refresh at most once per TTL.
func (r *instanceRepo) CountActiveByStatus(ctx context.Context) (map[string]int64, error) {
	type row struct {
		Status string
		Count  int64
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&WorkflowInstanceModel{}).
		Select("status, COUNT(*) AS count").
		Where("status IN ?", []string{
			string(model.WorkflowWaiting),
			string(model.WorkflowRunning),
			string(model.WorkflowPaused),
		}).
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Status] = row.Count
	}
	return out, nil
}

// claimableWhere mirrors ClaimNext's claimable predicate for snapshot
// reads: no locking, no limit, count only.
const claimableWhere = `(status = ? AND waiting_reason = ? AND (lease_expiry IS NULL OR lease_expiry < now()))
       OR (status = ? AND lease_expiry < now())
       OR (status = ? AND waiting_reason = ? AND (run_at IS NULL OR run_at <= now()) AND (lease_expiry IS NULL OR lease_expiry < now()))`

func claimableArgs() []any {
	return []any{
		string(model.WorkflowWaiting), string(model.WaitingReasonRunnable),
		string(model.WorkflowRunning),
		string(model.WorkflowWaiting), string(model.WaitingReasonDelay),
	}
}

// PendingDispatchStats counts claimable pending transitions and reports the
// age of the oldest one. Zero rows yields (0, 0, nil).
func (r *instanceRepo) PendingDispatchStats(ctx context.Context) (pending int64, oldestAgeSeconds float64, err error) {
	db := r.db.WithContext(ctx).Model(&WorkflowInstanceModel{}).Where(claimableWhere, claimableArgs()...)
	if err := db.Count(&pending).Error; err != nil {
		return 0, 0, err
	}
	if pending == 0 {
		return 0, 0, nil
	}
	var oldest *time.Time
	if err := r.db.WithContext(ctx).Model(&WorkflowInstanceModel{}).
		Where(claimableWhere, claimableArgs()...).
		Select("MIN(updated_at)").
		Scan(&oldest).Error; err != nil {
		return 0, 0, err
	}
	if oldest != nil {
		if age := time.Since(*oldest).Seconds(); age > 0 {
			oldestAgeSeconds = age
		}
	}
	return pending, oldestAgeSeconds, nil
}

// OutboxDepth counts unresolved status-update outbox rows. It serves the
// cached wf_status_outbox_backlog gauge.
func (r *statusUpdateRepo) OutboxDepth(ctx context.Context) (int64, error) {
	var depth int64
	err := r.db.WithContext(ctx).Model(&StatusUpdateOutboxModel{}).
		Where("delivered_at IS NULL AND dead_at IS NULL").
		Count(&depth).Error
	return depth, err
}
