package engine_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/engine"
	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/pkg/ids"
	"gorm.io/gorm"
)

func scrape(t *testing.T, rec *metrics.Recorder) string {
	t.Helper()
	w := httptest.NewRecorder()
	rec.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(w.Result().Body)
	return string(body)
}

// Driving a chain to finish flows through the engine hooks: node executions
// per script node and one finished terminal with its duration.
func TestMetricsCountsTerminalAndNodes(t *testing.T) {
	db := setupEngineDB(t)
	wfID := createWorkflow(t, db, n1,
		nodeJSON(n1, "script", "inc", "context.x = (context.x||0) + 1; return context.x;", n2, "a", nil),
		nodeJSON(n2, "script", "done", "return 'done';", "", "b", nil),
	)
	instanceID := insertInstance(t, db, wfID, n1, map[string]any{"x": 0})
	rec := metrics.New(metrics.Sources{})
	e, _ := testEngine(t, db, model.DefaultLimits())
	e.SetMetrics(rec)

	if cur := runEngine(t, db, e, instanceID); cur.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished (error %q)", cur.Status, cur.Error)
	}
	body := scrape(t, rec)
	for _, want := range []string{
		`wf_instances_terminal_total{status="finished"} 1`,
		`wf_node_executions_total{node_type="script",result="success"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
	if !strings.Contains(body, `wf_instance_duration_seconds_count{status="finished"} 1`) {
		t.Error("exposition missing finished duration observation")
	}
}

// The dispatcher claim loop counts every claimed transition.
func TestMetricsCountsDispatcherClaims(t *testing.T) {
	db := setupEngineDB(t)
	wfID := createWorkflow(t, db, n1,
		nodeJSON(n1, "script", "done", "return 'done';", "", "b", nil),
	)
	instanceID := insertInstance(t, db, wfID, n1, map[string]any{})
	rec := metrics.New(metrics.Sources{})
	e, _ := testEngine(t, db, model.DefaultLimits())
	e.SetMetrics(rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := engine.NewDispatcher(ctx, e, repository.NewInstanceRepository(db),
		repository.NewParallelRepository(db), "metrics-test",
		engine.DispatcherOptions{PollInterval: 10 * time.Millisecond, Lease: 30 * time.Second, BatchSize: 10, PoolSize: 4, Heartbeat: time.Second})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	d.SetMetrics(rec)
	d.Run()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = d.Shutdown(shutdownCtx)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		inst, err := repository.NewInstanceRepository(db).GetByID(context.Background(), instanceID)
		if err != nil {
			t.Fatalf("GetByID() error = %v", err)
		}
		if inst.Status == model.WorkflowFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance still %s after 10s", inst.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if body := scrape(t, rec); !strings.Contains(body, "wf_dispatcher_claims_total 1") &&
		!strings.Contains(body, "wf_dispatcher_claims_total 2") {
		t.Errorf("exposition has no dispatcher claims:\n%s", body)
	}
}

// Snapshot queries serve the cached gauges: active by status, pending
// dispatch stats, and outbox depth from a seeded row.
func TestMetricsSnapshotQueries(t *testing.T) {
	db := setupEngineDB(t)
	wfID := createWorkflow(t, db, n1,
		nodeJSON(n1, "script", "done", "return 'done';", "", "b", nil),
	)
	instanceID := insertInstance(t, db, wfID, n1, map[string]any{})
	ctx := context.Background()

	instances := repository.NewInstanceRepository(db)
	active, err := instances.(interface {
		CountActiveByStatus(context.Context) (map[string]int64, error)
	}).CountActiveByStatus(ctx)
	if err != nil {
		t.Fatalf("CountActiveByStatus() error = %v", err)
	}
	if active[string(model.WorkflowWaiting)] < 1 {
		t.Errorf("active waiting = %v, want >= 1", active)
	}
	pending, _, err := instances.(interface {
		PendingDispatchStats(context.Context) (int64, float64, error)
	}).PendingDispatchStats(ctx)
	if err != nil {
		t.Fatalf("PendingDispatchStats() error = %v", err)
	}
	if pending < 1 {
		t.Errorf("pending = %d, want >= 1", pending)
	}

	seedOutboxRow(t, db, instanceID, wfID)
	outbox := repository.NewStatusUpdateRepository(db)
	depth, err := outbox.(interface {
		OutboxDepth(context.Context) (int64, error)
	}).OutboxDepth(ctx)
	if err != nil {
		t.Fatalf("OutboxDepth() error = %v", err)
	}
	if depth < 1 {
		t.Fatalf("outbox depth = %d, want >= 1", depth)
	}
	rec := metrics.New(metrics.Sources{
		StatusOutboxDepth: func() (int64, error) { return depth, nil },
	})
	if body := scrape(t, rec); !strings.Contains(body, "wf_status_outbox_backlog 1") {
		t.Errorf("exposition has no outbox backlog:\n%s", body)
	}
}

func seedOutboxRow(t *testing.T, db *gorm.DB, instanceID, wfID string) {
	t.Helper()
	id, err := ids.NewString()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	row := repository.StatusUpdateOutboxModel{
		ID: id, WorkflowInstanceID: instanceID, WorkflowDefinitionID: wfID,
		Revision: 1, EventIndex: 0, Transport: "http",
		Payload: []byte(`{"id":"seeded"}`), NextAttemptAt: now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed outbox row: %v", err)
	}
}
