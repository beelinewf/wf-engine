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

func testLocalLimiter() *localAuthLimiter {
	return newLocalAuthLimiter(configuration.LocalRateLimit{
		Enabled: true, MaxAttempts: 3, Window: time.Minute, BlockDuration: time.Minute,
	}, nil)
}

// localTestRouter wires the limiter in front of a stub handler answering a
// fixed status, so tests drive limiting without a service.
func localTestRouter(l *localAuthLimiter, endpoint string, mode localLimitMode, status *int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/t", limitLocalAuth(l, endpoint, mode), func(c *gin.Context) {
		c.Status(*status)
	})
	return r
}

func postJSON(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSONFrom(t, r, body, "203.0.113.7:1234")
}

func postJSONFrom(t *testing.T, r *gin.Engine, body, remote string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// Each endpoint admits MaxAttempts requests, then answers 429 with a
// Retry-After. Register and resend count every request — including the
// ones that succeed — because each does work.
func TestLocalLimiterEveryCountsAll(t *testing.T) {
	for _, endpoint := range []string{"register", "resend-verification"} {
		status := http.StatusOK
		r := localTestRouter(testLocalLimiter(), endpoint, localLimitEvery, &status)
		body := `{"email":"ada@example.test"}`
		for i := 0; i < 3; i++ {
			if w := postJSON(t, r, body); w.Code != http.StatusOK {
				t.Fatalf("%s attempt %d: status = %d, want 200", endpoint, i+1, w.Code)
			}
		}
		w := postJSON(t, r, body)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status = %d, want 429", endpoint, w.Code)
		}
		if w.Header().Get("Retry-After") == "" {
			t.Errorf("%s: missing Retry-After", endpoint)
		}
	}
}

// Verify-email paces by IP alone: tokens are unguessable, but each guess
// is cheap, so successes count too.
func TestLocalLimiterVerifyCountsEveryAttempt(t *testing.T) {
	status := http.StatusOK
	r := localTestRouter(testLocalLimiter(), "verify-email", localLimitIPOnly, &status)
	body := `{"token":"abc"}`
	for i := 0; i < 3; i++ {
		if w := postJSON(t, r, body); w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, w.Code)
		}
	}
	if w := postJSON(t, r, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

// Login charges the IP on failures only: three failing logins from one
// IP block it, whatever the emails, while successes cost nothing — ten
// legitimate logins behind one NAT address must not block the eleventh.
func TestLocalLimiterLoginIPFailuresOnly(t *testing.T) {
	status := http.StatusUnauthorized
	r := localTestRouter(testLocalLimiter(), "login", localLimitLogin, &status)
	for i := 0; i < 3; i++ {
		body := `{"email":"user` + string(rune('a'+i)) + `@example.test"}`
		if w := postJSON(t, r, body); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, w.Code)
		}
	}
	if w := postJSON(t, r, `{"email":"other@example.test"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 on the IP", w.Code)
	}

	status = http.StatusOK
	r = localTestRouter(testLocalLimiter(), "login", localLimitLogin, &status)
	for i := 0; i < 5; i++ {
		if w := postJSON(t, r, `{"email":"ada@example.test"}`); w.Code != http.StatusOK {
			t.Fatalf("success %d: status = %d, want 200", i+1, w.Code)
		}
	}
}

// A success neither burns the IP budget nor clears it: the failures on
// a shared address may belong to someone else.
func TestLocalLimiterLoginSuccessLeavesIPBudget(t *testing.T) {
	status := http.StatusUnauthorized
	r := localTestRouter(testLocalLimiter(), "login", localLimitLogin, &status)
	postJSON(t, r, `{"email":"ada@example.test"}`) // fail 1
	status = http.StatusOK
	postJSON(t, r, `{"email":"ada@example.test"}`) // success: no charge, no reset
	status = http.StatusUnauthorized
	postJSON(t, r, `{"email":"ada@example.test"}`) // fail 2
	postJSON(t, r, `{"email":"ada@example.test"}`) // fail 3 trips the IP budget
	if w := postJSON(t, r, `{"email":"ada@example.test"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 on the IP", w.Code)
	}
}

// The email dimension counts failures only and clears on success: an
// owner alternating a failure with a success is never blocked, while
// three straight failures block the address from anywhere.
func TestLocalLimiterLoginEmailFailuresOnly(t *testing.T) {
	status := http.StatusUnauthorized
	r := localTestRouter(testLocalLimiter(), "login", localLimitLogin, &status)
	body := `{"email":"ada@example.test"}`

	postJSON(t, r, body) // fail 1
	status = http.StatusOK
	postJSONFrom(t, r, body, "198.51.100.9:1234") // success clears the email key
	status = http.StatusUnauthorized
	postJSONFrom(t, r, body, "192.0.2.9:1234")  // fail 1 again, fresh IP
	postJSONFrom(t, r, body, "192.0.2.10:1234") // fail 2
	postJSONFrom(t, r, body, "192.0.2.11:1234") // fail 3 trips the email budget
	if w := postJSONFrom(t, r, body, "192.0.2.12:1234"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 on the email", w.Code)
	}
	// A different address from a fresh IP is unaffected by the block.
	if w := postJSONFrom(t, r, `{"email":"grace@example.test"}`, "192.0.2.13:1234"); w.Code != http.StatusUnauthorized {
		t.Fatalf("other email status = %d, want 401", w.Code)
	}
}

// Budgets never leak across endpoints: an exhausted register budget leaves
// login, verify, and resend admitted.
func TestLocalLimiterEndpointsAreIndependent(t *testing.T) {
	l := testLocalLimiter()
	gin.SetMode(gin.TestMode)
	newRoute := func(endpoint string, mode localLimitMode, status int) *gin.Engine {
		r := gin.New()
		r.POST("/t", limitLocalAuth(l, endpoint, mode), func(c *gin.Context) { c.Status(status) })
		return r
	}
	register := newRoute("register", localLimitEvery, http.StatusOK)
	for i := 0; i < 4; i++ {
		postJSON(t, register, `{"email":"ada@example.test"}`)
	}
	if w := postJSON(t, register, `{"email":"ada@example.test"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("register status = %d, want 429", w.Code)
	}
	for _, tc := range []struct {
		endpoint string
		mode     localLimitMode
		body     string
		status   int
	}{
		{"login", localLimitLogin, `{"email":"ada@example.test"}`, http.StatusUnauthorized},
		{"verify-email", localLimitIPOnly, `{"token":"x"}`, http.StatusOK},
		{"resend-verification", localLimitEvery, `{"email":"ada@example.test"}`, http.StatusOK},
	} {
		if w := postJSON(t, newRoute(tc.endpoint, tc.mode, tc.status), tc.body); w.Code != tc.status {
			t.Errorf("%s status = %d, want %d", tc.endpoint, w.Code, tc.status)
		}
	}
}

// A malformed body is 400 without touching any bucket: three garbage
// requests leave the full budget intact.
func TestLocalLimiterMalformedBodyCostsNothing(t *testing.T) {
	status := http.StatusOK
	r := localTestRouter(testLocalLimiter(), "login", localLimitLogin, &status)
	for i := 0; i < 3; i++ {
		if w := postJSON(t, r, `{oops`); w.Code != http.StatusBadRequest {
			t.Fatalf("malformed %d: status = %d, want 400", i+1, w.Code)
		}
	}
	for i := 0; i < 3; i++ {
		if w := postJSON(t, r, `{"email":"ada@example.test"}`); w.Code != http.StatusOK {
			t.Fatalf("valid %d: status = %d, want 200", i+1, w.Code)
		}
	}
}

// The handler still sees the body the limiter peeked at.
func TestLocalLimiterRestoresBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var seen string
	r.POST("/t", limitLocalAuth(testLocalLimiter(), "login", localLimitLogin), func(c *gin.Context) {
		var body struct {
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		seen = body.Email
		c.Status(http.StatusOK)
	})
	if w := postJSON(t, r, `{"email":"ada@example.test"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if seen != "ada@example.test" {
		t.Errorf("handler saw email %q", seen)
	}
}

// Email keys are normalized: case and padding never fork a budget.
func TestLocalLimiterEmailNormalized(t *testing.T) {
	status := http.StatusUnauthorized
	r := localTestRouter(testLocalLimiter(), "resend-verification", localLimitEvery, &status)
	postJSON(t, r, `{"email":"ada@example.test"}`)
	postJSON(t, r, `{"email":" ADA@example.test "}`)
	postJSON(t, r, `{"email":"Ada@Example.Test"}`)
	if w := postJSON(t, r, `{"email":"ada@example.test"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

// A forged X-Forwarded-For from an untrusted peer cannot rotate the key:
// the limiter keys by the direct peer, so two spoofed identities share
// one budget.
func TestLocalLimiterIgnoresSpoofedForwardedFor(t *testing.T) {
	status := http.StatusOK
	r := localTestRouter(testLocalLimiter(), "verify-email", localLimitIPOnly, &status)
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(`{"token":"x"}`))
		req.RemoteAddr = "203.0.113.7:1234"
		req.Header.Set("X-Forwarded-For", "10.9.9.9")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, w.Code)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(`{"token":"x"}`))
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("X-Forwarded-For", "10.9.9.10")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 on the shared peer budget", w.Code)
	}
}

// A nil limiter passes everything through: throttling off leaves the
// routes exactly as before.
func TestLocalLimiterNilPassesThrough(t *testing.T) {
	status := http.StatusOK
	r := localTestRouter(nil, "login", localLimitLogin, &status)
	for i := 0; i < 5; i++ {
		if w := postJSON(t, r, `{"email":"ada@example.test"}`); w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, w.Code)
		}
	}
}

func TestNewLocalAuthLimiterDisabledIsNil(t *testing.T) {
	if newLocalAuthLimiter(configuration.LocalRateLimit{Enabled: false}, nil) != nil {
		t.Error("disabled limiter != nil")
	}
	if newLocalAuthLimiter(configuration.LocalRateLimit{Enabled: true}, nil) != nil {
		t.Error("zero-budget limiter != nil")
	}
}
