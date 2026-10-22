package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

const testCORSOrigin = "https://app.example.com"

func testCORSCfg() configuration.CORS {
	return configuration.CORS{
		AllowedOrigins:   []string{testCORSOrigin},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Api-Token"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}
}

// corsEngine wires the middleware exactly as the router does: first, before
// any route, so preflights short-circuit and errors keep the headers.
func corsEngine(cfg configuration.CORS) *gin.Engine {
	r := gin.New()
	r.Use(CORS(cfg))
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	r.OPTIONS("/ping", func(c *gin.Context) { c.String(http.StatusOK, "options-pong") })
	return r
}

func serveCORS(r *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(w, req)
	return w
}

// With no origins configured the middleware is a pass-through: no CORS
// headers on any response, exactly the pre-CORS behavior.
func TestCORSDisabledEmitsNoHeaders(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{})})

	w := serveCORS(r, http.MethodGet, "/health/live", map[string]string{"Origin": testCORSOrigin})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Vary"} {
		if got := w.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want empty when CORS is disabled", h, got)
		}
	}
}

func TestCORSActualAllowedOrigin(t *testing.T) {
	w := serveCORS(corsEngine(testCORSCfg()), http.MethodGet, "/ping", map[string]string{"Origin": testCORSOrigin})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request origin echoed", got)
	}
	if vary := w.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
		t.Errorf("Vary = %q, want it to include Origin", vary)
	}
}

// A disallowed origin gets no CORS headers, but the request is still
// processed: sharing is opt-in, and the browser enforces the denial.
func TestCORSActualDisallowedOrigin(t *testing.T) {
	w := serveCORS(corsEngine(testCORSCfg()), http.MethodGet, "/ping", map[string]string{"Origin": "https://evil.example.com"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the request still runs)", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for a disallowed origin", got)
	}
}

// Server-to-server callers send no Origin and must see no CORS headers.
func TestCORSActualNoOrigin(t *testing.T) {
	w := serveCORS(corsEngine(testCORSCfg()), http.MethodGet, "/ping", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty without an Origin", got)
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	w := serveCORS(corsEngine(testCORSCfg()), http.MethodOptions, "/ping", map[string]string{
		"Origin":                         testCORSOrigin,
		"Access-Control-Request-Method":  "PUT",
		"Access-Control-Request-Headers": "Authorization, Content-Type",
	})

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request origin echoed", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PUT") {
		t.Errorf("Access-Control-Allow-Methods = %q, want it to include PUT", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to include Authorization", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "43200" {
		t.Errorf("Access-Control-Max-Age = %q, want 43200 (12h in seconds)", got)
	}
}

func TestCORSPreflightRejections(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"disallowed origin", map[string]string{
			"Origin":                        "https://evil.example.com",
			"Access-Control-Request-Method": "PUT",
		}},
		{"disallowed method", map[string]string{
			"Origin":                        testCORSOrigin,
			"Access-Control-Request-Method": "PATCH",
		}},
		{"disallowed header", map[string]string{
			"Origin":                         testCORSOrigin,
			"Access-Control-Request-Method":  "PUT",
			"Access-Control-Request-Headers": "X-Evil",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveCORS(corsEngine(testCORSCfg()), http.MethodOptions, "/ping", tc.headers)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want empty on a rejected preflight", got)
			}
		})
	}
}

// An OPTIONS request without Access-Control-Request-Method is not a
// preflight and passes through to the route.
func TestCORSPlainOPTIONSPassesThrough(t *testing.T) {
	w := serveCORS(corsEngine(testCORSCfg()), http.MethodOptions, "/ping", map[string]string{"Origin": testCORSOrigin})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the route", w.Code)
	}
	if body := w.Body.String(); body != "options-pong" {
		t.Errorf("body = %q, want the route to have run", body)
	}
}

// The preflight must reach the CORS decision before authentication: a
// browser sends no credentials on OPTIONS, so a 401 here would block the
// real request before it is ever sent.
func TestCORSPreflightBypassesAuth(t *testing.T) {
	r := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Statistics:   &fakeStatisticsSvc{},
		AuthSettings: authConfigForTest(),
		CORS:         testCORSCfg(),
	})

	w := serveCORS(r, http.MethodOptions, "/v1/statistics", map[string]string{
		"Origin":                        testCORSOrigin,
		"Access-Control-Request-Method": "GET",
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204 without a token", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request origin echoed", got)
	}
}

// The CORS headers are set before the auth middleware runs, so a rejected
// request still shares its (rejected) response with the browser.
func TestCORSUnauthorizedCarriesAllowOrigin(t *testing.T) {
	r := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Statistics:   &fakeStatisticsSvc{},
		AuthSettings: authConfigForTest(),
		CORS:         testCORSCfg(),
	})

	w := serveCORS(r, http.MethodGet, "/v1/statistics", map[string]string{"Origin": testCORSOrigin})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request origin echoed on a 401", got)
	}
}

// A credentialed response names the origin explicitly, never "*".
func TestCORSCredentialsEchoOrigin(t *testing.T) {
	cfg := testCORSCfg()
	cfg.AllowCredentials = true

	w := serveCORS(corsEngine(cfg), http.MethodGet, "/ping", map[string]string{"Origin": testCORSOrigin})

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want the explicit origin", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestCORSWildcardAllowsAnyOrigin(t *testing.T) {
	cfg := testCORSCfg()
	cfg.AllowedOrigins = []string{"*"}

	w := serveCORS(corsEngine(cfg), http.MethodGet, "/ping", map[string]string{"Origin": "https://anything.example.com"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want empty (config forbids it with *)", got)
	}
}

func TestCORSExposesConfiguredHeaders(t *testing.T) {
	cfg := testCORSCfg()
	cfg.ExposedHeaders = []string{"X-Total"}

	w := serveCORS(corsEngine(cfg), http.MethodGet, "/ping", map[string]string{"Origin": testCORSOrigin})

	if got := w.Header().Get("Access-Control-Expose-Headers"); got != "X-Total" {
		t.Errorf("Access-Control-Expose-Headers = %q, want X-Total", got)
	}
}
