package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecorderCountsInstances(t *testing.T) {
	r := metrics.New(metrics.Sources{})
	r.InstanceCreated()
	r.InstanceCreated()
	r.InstanceTerminal("finished", time.Second)

	if got := testutil.ToFloat64(r.InstancesCreatedCounter()); got != 2 {
		t.Errorf("created = %v, want 2", got)
	}
}

func TestHandlerServesExposition(t *testing.T) {
	r := metrics.New(metrics.Sources{})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "wf_instances_created_total") {
		t.Error("exposition missing wf_instances_created_total")
	}
	if !strings.Contains(string(body), "go_goroutines") {
		t.Error("exposition missing go_goroutines")
	}
}

func TestCachedCollectorQueriesOnceWithinTTL(t *testing.T) {
	calls := 0
	src := metrics.Sources{
		StatusOutboxDepth: func() (int64, error) { calls++; return 3, nil },
		TTL:               time.Minute,
	}
	r := metrics.New(src)
	h := r.Handler()
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	if calls != 1 {
		t.Errorf("source calls = %d, want 1 within TTL", calls)
	}
}
