package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

// testRateLimit is a small-budget throttle so tests trip it in a few
// requests: 3 failures within a minute, then 429s for a minute.
func testRateLimit() configuration.RateLimit {
	return configuration.RateLimit{
		Enabled:       true,
		MaxAttempts:   3,
		Window:        time.Minute,
		BlockDuration: time.Minute,
	}
}

// limitedAuth is tokenAuth with an API-token failure throttle attached.
func limitedAuth(t *testing.T, token string, s configuration.RateLimit) AuthConfig {
	t.Helper()
	cfg := tokenAuth(token)
	cfg.FailureLimiter = newAuthFailureLimiter(s)
	if cfg.FailureLimiter == nil {
		t.Fatal("newAuthFailureLimiter returned nil for an enabled throttle")
	}
	return cfg
}

// limitedRouter serves one protected route behind RequireAuth.
func limitedRouter(cfg AuthConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/protected", RequireAuth(cfg), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// authGet fires one GET from the given peer with the given API token and
// extra headers, returning the status code and Retry-After header.
func authGet(t *testing.T, e *gin.Engine, remoteAddr, token string, headers map[string]string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set(APIKeyHeader, token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	e.ServeHTTP(w, req)
	return w.Code, w.Header().Get("Retry-After")
}

// After MaxAttempts failures the same source answers 429 with a
// Retry-After; the count is exact, not off by one.
func TestRequireAuthThrottlesRepeatedWrongTokens(t *testing.T) {
	r := limitedRouter(limitedAuth(t, "secret", testRateLimit()))

	var codes []int
	var retryAfter string
	for i := 0; i < 5; i++ {
		code, ra := authGet(t, r, "198.51.100.7:1111", "wrong", nil)
		codes = append(codes, code)
		retryAfter = ra
	}
	want := []int{401, 401, 401, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("attempt %d status = %d, want %d (all: %v)", i+1, codes[i], want[i], codes)
		}
	}
	if n, err := strconv.Atoi(retryAfter); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retryAfter)
	}
}

// One IP's failures never spend another IP's budget.
func TestRequireAuthThrottleIsPerIP(t *testing.T) {
	r := limitedRouter(limitedAuth(t, "secret", testRateLimit()))

	for i := 0; i < 5; i++ {
		if code, _ := authGet(t, r, "198.51.100.7:1111", "wrong", nil); i >= 3 && code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d from throttled IP = %d, want 429", i+1, code)
		}
	}
	if code, _ := authGet(t, r, "203.0.113.9:2222", "wrong", nil); code != http.StatusUnauthorized {
		t.Errorf("fresh IP status = %d, want 401", code)
	}
}

// Successes never count: a valid token interleaved with guesses succeeds,
// and only the guesses accumulate toward the block.
func TestRequireAuthSuccessDoesNotConsumeBudget(t *testing.T) {
	r := limitedRouter(limitedAuth(t, "secret", testRateLimit()))

	steps := []struct {
		token string
		want  int
	}{
		{"wrong", http.StatusUnauthorized},
		{"secret", http.StatusOK},
		{"wrong", http.StatusUnauthorized},
		{"wrong", http.StatusUnauthorized},
		{"wrong", http.StatusTooManyRequests},
	}
	for i, s := range steps {
		if code, _ := authGet(t, r, "198.51.100.7:1111", s.token, nil); code != s.want {
			t.Errorf("step %d (token %q) = %d, want %d", i+1, s.token, code, s.want)
		}
	}
}

// Once the block expires the IP is 401 again rather than stuck at 429.
func TestRequireAuthThrottleExpires(t *testing.T) {
	r := limitedRouter(limitedAuth(t, "secret", configuration.RateLimit{
		Enabled:       true,
		MaxAttempts:   1,
		Window:        30 * time.Millisecond,
		BlockDuration: 30 * time.Millisecond,
	}))

	if code, _ := authGet(t, r, "198.51.100.7:1111", "wrong", nil); code != http.StatusUnauthorized {
		t.Fatalf("first attempt = %d, want 401", code)
	}
	if code, _ := authGet(t, r, "198.51.100.7:1111", "wrong", nil); code != http.StatusTooManyRequests {
		t.Fatalf("blocked attempt = %d, want 429", code)
	}
	time.Sleep(80 * time.Millisecond)
	if code, _ := authGet(t, r, "198.51.100.7:1111", "wrong", nil); code != http.StatusUnauthorized {
		t.Errorf("post-expiry attempt = %d, want 401", code)
	}
}

// A block that expires while the window is still fresh re-blocks on the
// next attempt instead of granting a free guess per block.
func TestRequireAuthThrottleReblocksWithinWindow(t *testing.T) {
	r := limitedRouter(limitedAuth(t, "secret", configuration.RateLimit{
		Enabled:       true,
		MaxAttempts:   1,
		Window:        500 * time.Millisecond,
		BlockDuration: 30 * time.Millisecond,
	}))

	if code, _ := authGet(t, r, "198.51.100.7:1111", "wrong", nil); code != http.StatusUnauthorized {
		t.Fatalf("first attempt = %d, want 401", code)
	}
	time.Sleep(60 * time.Millisecond)
	if code, _ := authGet(t, r, "198.51.100.7:1111", "wrong", nil); code != http.StatusTooManyRequests {
		t.Errorf("attempt after block expiry within window = %d, want 429", code)
	}
}

// With no trusted proxies a forged X-Forwarded-For is ignored: rotating
// the header does not rotate the budget, which stays keyed by the peer.
func TestRequireAuthThrottleIgnoresUntrustedForwardedHeader(t *testing.T) {
	r := limitedRouter(limitedAuth(t, "secret", testRateLimit()))

	var last int
	for i := 0; i < 5; i++ {
		headers := map[string]string{"X-Forwarded-For": "203.0.113." + strconv.Itoa(i+1)}
		last, _ = authGet(t, r, "198.51.100.7:1111", "wrong", headers)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("fifth attempt with rotating X-Forwarded-For = %d, want 429", last)
	}
}

// From a trusted proxy the leftmost X-Forwarded-For entry is the key, so
// two clients behind the same proxy hold separate budgets.
func TestRequireAuthThrottleHonorsTrustedProxy(t *testing.T) {
	settings := testRateLimit()
	settings.TrustedProxies = []string{"10.0.0.9"}
	r := limitedRouter(limitedAuth(t, "secret", settings))

	clientA := map[string]string{"X-Forwarded-For": "198.51.100.7"}
	for i := 0; i < 5; i++ {
		code, _ := authGet(t, r, "10.0.0.9:3333", "wrong", clientA)
		if i >= 3 && code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d from client A = %d, want 429", i+1, code)
		}
	}
	clientB := map[string]string{"X-Forwarded-For": "198.51.100.8"}
	if code, _ := authGet(t, r, "10.0.0.9:4444", "wrong", clientB); code != http.StatusUnauthorized {
		t.Errorf("client B behind the same proxy = %d, want 401", code)
	}
}

// OIDC failures never touch the throttle: Bearer [REDACTED] are signed, not
// guessable, and expiry churn must not lock an IP out.
func TestRequireAuthOIDCFailuresNeverThrottle(t *testing.T) {
	cfg := AuthConfig{
		Enabled:        true,
		APIToken:       "secret",
		Verifier:       stubVerifier{err: errors.New("expired")},
		FailureLimiter: newAuthFailureLimiter(testRateLimit()),
		SystemUserID:   testSystemUserID,
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/protected", RequireAuth(cfg), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for i := 0; i < 6; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.RemoteAddr = "198.51.100.7:1111"
		req.Header.Set(BearerHeader, "Bearer [REDACTED]")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 (OIDC failures must never 429)", i+1, w.Code)
		}
	}
}

// TryAuth throttles presented-but-wrong API tokens exactly like
// RequireAuth, while the credential-less anonymous path stays untouched.
func TestTryAuthThrottlesWrongToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/protected", TryAuth(limitedAuth(t, "secret", testRateLimit())), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	fire := func(token string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/protected", nil)
		req.RemoteAddr = "198.51.100.7:1111"
		if token != "" {
			req.Header.Set(APIKeyHeader, token)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < 3; i++ {
		if code := fire("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, code)
		}
	}
	if code := fire("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt = %d, want 429", code)
	}
	if code := fire(""); code != http.StatusOK {
		t.Errorf("anonymous request from throttled IP = %d, want 200", code)
	}
}

// The /metrics token gate shares the throttle: guesses there count.
func TestRequireAPITokenThrottlesWrongToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := limitedAuth(t, "secret", testRateLimit())
	r := gin.New()
	r.GET("/metrics", RequireAPIToken(cfg), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	fire := func(token string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = "198.51.100.7:1111"
		if token != "" {
			req.Header.Set(APIKeyHeader, token)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < 3; i++ {
		if code := fire("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, code)
		}
	}
	if code := fire("wrong"); code != http.StatusTooManyRequests {
		t.Errorf("throttled attempt = %d, want 429", code)
	}
	if code := fire("secret"); code != http.StatusTooManyRequests {
		t.Errorf("valid token from throttled IP = %d, want 429 until the block expires", code)
	}
}

// The router passes Deps.AuthSettings.RateLimit to the middleware: a burst
// of wrong tokens against a real route trips the shared throttle.
func TestRouterThrottlesRepeatedWrongTokens(t *testing.T) {
	authSettings := authConfigForTest()
	authSettings.RateLimit = configuration.RateLimit{
		Enabled:       true,
		MaxAttempts:   2,
		Window:        time.Minute,
		BlockDuration: time.Minute,
	}
	r := metricsTestRouter(t, Deps{
		Statistics:   &fakeStatisticsSvc{},
		AuthSettings: authSettings,
	})

	var codes []int
	for i := 0; i < 4; i++ {
		w := metricsGet(r, "/v1/statistics", map[string]string{APIKeyHeader: "wrong"})
		codes = append(codes, w.Code)
	}
	want := []int{401, 401, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("attempt %d = %d, want %d (all: %v)", i+1, codes[i], want[i], codes)
		}
	}
}

// The router shares one limiter between /metrics and /v1: guesses against
// /metrics spend the budget a /v1 burst would otherwise use.
func TestRouterMetricsSharesThrottleBudget(t *testing.T) {
	authSettings := authConfigForTest()
	authSettings.RateLimit = configuration.RateLimit{
		Enabled:       true,
		MaxAttempts:   2,
		Window:        time.Minute,
		BlockDuration: time.Minute,
	}
	r := metricsTestRouter(t, Deps{
		MetricsEnabled: true,
		Statistics:     &fakeStatisticsSvc{},
		AuthSettings:   authSettings,
	})

	if w := metricsGet(r, "/metrics", map[string]string{APIKeyHeader: "wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("/metrics wrong token = %d, want 401", w.Code)
	}
	if w := metricsGet(r, "/metrics", map[string]string{APIKeyHeader: "wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("/metrics wrong token = %d, want 401", w.Code)
	}
	// The two /metrics failures tripped the shared budget, so /v1 is 429
	// without any /v1 guess having been made.
	if w := metricsGet(r, "/v1/statistics", map[string]string{APIKeyHeader: "wrong"}); w.Code != http.StatusTooManyRequests {
		t.Errorf("/v1 after /metrics budget spent = %d, want 429", w.Code)
	}
}

func TestNewAuthFailureLimiterDisabled(t *testing.T) {
	cases := map[string]configuration.RateLimit{
		"disabled":            {Enabled: false, MaxAttempts: 3, Window: time.Minute, BlockDuration: time.Minute},
		"zero":                {},
		"no attempts":         {Enabled: true, Window: time.Minute, BlockDuration: time.Minute},
		"no window":           {Enabled: true, MaxAttempts: 3, BlockDuration: time.Minute},
		"no block":            {Enabled: true, MaxAttempts: 3, Window: time.Minute},
		"negative max":        {Enabled: true, MaxAttempts: -1, Window: time.Minute, BlockDuration: time.Minute},
		"negative window":     {Enabled: true, MaxAttempts: 3, Window: -time.Second, BlockDuration: time.Minute},
		"negative block":      {Enabled: true, MaxAttempts: 3, Window: time.Minute, BlockDuration: -time.Second},
		"unparseable trusted": {Enabled: true, MaxAttempts: 3, Window: time.Minute, BlockDuration: time.Minute, TrustedProxies: []string{"not-an-ip"}},
	}
	for name, settings := range cases {
		t.Run(name, func(t *testing.T) {
			l := newAuthFailureLimiter(settings)
			if name == "unparseable trusted" {
				// A bad entry is skipped, not fatal: the limiter
				// stands, trusting nothing.
				if l == nil {
					t.Fatal("limiter is nil, want a limiter that trusts nothing")
				}
				return
			}
			if l != nil {
				t.Error("limiter is non-nil, want nil (throttle disabled)")
			}
		})
	}
}

func TestAuthFailureLimiterClientIP(t *testing.T) {
	newLimiter := func(trusted ...string) *authFailureLimiter {
		settings := testRateLimit()
		settings.TrustedProxies = trusted
		return newAuthFailureLimiter(settings)
	}
	request := func(remoteAddr string, headers map[string]string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.RemoteAddr = remoteAddr
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req
	}
	cases := []struct {
		name    string
		limiter *authFailureLimiter
		request *http.Request
		want    string
	}{
		{"direct peer", newLimiter(), request("198.51.100.7:1111", nil), "198.51.100.7"},
		{"spoofed XFF ignored", newLimiter(), request("198.51.100.7:1111", map[string]string{"X-Forwarded-For": "203.0.113.9"}), "198.51.100.7"},
		{"spoofed X-Real-Ip ignored", newLimiter(), request("198.51.100.7:1111", map[string]string{"X-Real-Ip": "203.0.113.9"}), "198.51.100.7"},
		{"trusted proxy honors leftmost XFF", newLimiter("10.0.0.9"), request("10.0.0.9:3333", map[string]string{"X-Forwarded-For": "198.51.100.7, 10.0.0.9"}), "198.51.100.7"},
		{"trusted proxy honors X-Real-Ip", newLimiter("10.0.0.9"), request("10.0.0.9:3333", map[string]string{"X-Real-Ip": "198.51.100.7"}), "198.51.100.7"},
		{"trusted proxy without headers keys by peer", newLimiter("10.0.0.9"), request("10.0.0.9:3333", nil), "10.0.0.9"},
		{"CIDR trust", newLimiter("10.0.0.0/8"), request("10.1.2.3:3333", map[string]string{"X-Forwarded-For": "198.51.100.7"}), "198.51.100.7"},
		{"peer outside CIDR untrusted", newLimiter("10.0.0.0/8"), request("192.0.2.1:3333", map[string]string{"X-Forwarded-For": "198.51.100.7"}), "192.0.2.1"},
		{"IPv6 peer", newLimiter(), request("[2001:db8::1]:443", nil), "2001:db8::1"},
		{"unparseable peer kept as-is", newLimiter(), request("not-an-addr", nil), "not-an-addr"},
		{"blank trusted entries skipped", newLimiter("", "  "), request("198.51.100.7:1111", map[string]string{"X-Forwarded-For": "203.0.113.9"}), "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.limiter.clientIP(tc.request); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// Expired buckets are swept so a long-lived limiter does not grow with
// every distinct source it ever saw.
func TestAuthFailureLimiterSweepsExpiredBuckets(t *testing.T) {
	l := newAuthFailureLimiter(testRateLimit())
	now := time.Now()
	l.buckets["expired"] = &failureBucket{count: 3, windowStart: now.Add(-time.Hour)}
	l.buckets["blocked-expired"] = &failureBucket{count: 3, windowStart: now.Add(-time.Hour), blockedUntil: now.Add(-time.Minute)}
	l.buckets["live"] = &failureBucket{count: 1, windowStart: now}

	l.sweepLocked(now)

	if len(l.buckets) != 1 {
		t.Fatalf("buckets after sweep = %d, want 1", len(l.buckets))
	}
	if _, ok := l.buckets["live"]; !ok {
		t.Error("live bucket was swept, want it kept")
	}
}
