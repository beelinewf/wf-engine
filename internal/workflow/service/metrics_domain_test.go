package service_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Create and Stop flow through the service hooks: created counts the new
// row, terminal counts the stopped transition.
func TestMetricsCountsCreateAndStop(t *testing.T) {
	db := setupSvcDB(t)
	rec := metrics.New(metrics.Sources{})
	svc := svcInstanceService(db)
	setter, ok := svc.(interface{ SetMetrics(*metrics.Recorder) })
	if !ok {
		t.Fatal("instance service does not accept a metrics recorder")
	}
	setter.SetMetrics(rec)

	nodeID := svcNewID()
	wfID := svcCreateWorkflow(t, db, nodeID,
		svcNodeJSON(nodeID, "script", "a", "return 1;", "", nil))
	inst, err := svc.Create(context.Background(), service.CreateInstance{WorkflowDefinitionID: wfID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := testutil.ToFloat64(rec.InstancesCreatedCounter()); got != 1 {
		t.Errorf("wf_instances_created_total = %v, want 1", got)
	}

	if _, err := svc.Stop(context.Background(), service.ControlRequest{InstanceID: inst.ID}); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	w := httptest.NewRecorder()
	rec.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), `wf_instances_terminal_total{status="stopped"} 1`) {
		t.Error("exposition missing wf_instances_terminal_total{status=\"stopped\"} 1")
	}
}
