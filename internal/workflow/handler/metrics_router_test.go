package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

func metricsTestRouter(t *testing.T, deps Deps) *gin.Engine {
	t.Helper()
	if deps.Health == nil {
		deps.Health = NewHealth(fakePinger{})
	}
	if deps.Metrics == nil && deps.MetricsEnabled {
		deps.Metrics = metrics.New(metrics.Sources{})
	}
	return NewRouter(deps)
}

func metricsGet(e *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	e.ServeHTTP(w, req)
	return w
}

func TestMetricsDisabledReturns404(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"auth off", Deps{MetricsEnabled: false}},
		{"auth on", Deps{MetricsEnabled: false, AuthSettings: authConfigForTest()}},
	} {
		r := metricsTestRouter(t, tc.deps)
		if w := metricsGet(r, "/metrics", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: GET /metrics = %d, want 404", tc.name, w.Code)
		}
		if w := metricsGet(r, "/metrics", map[string]string{APIKeyHeader: "test-token"}); w.Code != http.StatusNotFound {
			t.Errorf("%s: GET /metrics with token = %d, want 404", tc.name, w.Code)
		}
	}
}

func TestMetricsPublicWhenAuthOff(t *testing.T) {
	r := metricsTestRouter(t, Deps{MetricsEnabled: true})
	w := metricsGet(r, "/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain exposition", ct)
	}
	body, _ := io.ReadAll(w.Result().Body)
	for _, want := range []string{"# TYPE go_goroutines", "# TYPE wf_instances_terminal_total", "# TYPE wf_dispatcher_backlog"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestMetricsRequiresTokenWhenAuthOn(t *testing.T) {
	r := metricsTestRouter(t, Deps{MetricsEnabled: true, AuthSettings: authConfigForTest()})
	w := metricsGet(r, "/metrics", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no header = %d, want 401", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if strings.Contains(string(body), "# TYPE") || strings.Contains(string(body), "wf_") {
		t.Error("401 body leaks metric series")
	}
	if w := metricsGet(r, "/metrics", map[string]string{APIKeyHeader: "wrong"}); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", w.Code)
	}
	if w := metricsGet(r, "/metrics", map[string]string{"Authorization": "Bearer [REDACTED]"}); w.Code != http.StatusUnauthorized {
		t.Errorf("Bearer [REDACTED] = %d, want 401", w.Code)
	}
	if w := metricsGet(r, "/metrics", map[string]string{APIKeyHeader: "test-token"}); w.Code != http.StatusOK {
		t.Errorf("correct token = %d, want 200", w.Code)
	}
}

func TestMetricsFailClosedWithEmptyToken(t *testing.T) {
	r := metricsTestRouter(t, Deps{MetricsEnabled: true, AuthSettings: configuration.Auth{Enabled: true}})
	for _, h := range []map[string]string{nil, {APIKeyHeader: "anything"}} {
		if w := metricsGet(r, "/metrics", h); w.Code != http.StatusUnauthorized {
			t.Errorf("headers %v = %d, want 401", h, w.Code)
		}
	}
}

func TestMetricsLeavesHealthPublicAndV1Gated(t *testing.T) {
	r := metricsTestRouter(t, Deps{
		MetricsEnabled: true,
		Statistics:     &fakeStatisticsSvc{},
		AuthSettings:   authConfigForTest(),
	})
	if w := metricsGet(r, "/health/live", nil); w.Code != http.StatusOK {
		t.Errorf("GET /health/live = %d, want 200", w.Code)
	}
	if w := metricsGet(r, "/v1/statistics", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/statistics = %d, want 401", w.Code)
	}
}

func TestREDMiddlewareRecordsRouteTemplate(t *testing.T) {
	r := metricsTestRouter(t, Deps{MetricsEnabled: true})
	_ = metricsGet(r, "/health/live", nil)
	w := metricsGet(r, "/metrics", nil)
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), `http_requests_total{method="GET",route="/health/live",status="200"}`) {
		t.Error("RED missing http_requests_total for GET /health/live with route template")
	}
}
