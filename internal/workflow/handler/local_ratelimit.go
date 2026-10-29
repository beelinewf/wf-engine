package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

// localAuthMaxBodyBytes bounds the request bodies the local-auth limiter
// peeks at. Register/login/resend/verify bodies are a few hundred bytes;
// anything past this is rejected before any budget is touched.
const localAuthMaxBodyBytes = 4096

// localLimitMode is how one public endpoint charges the throttle.
type localLimitMode int

const (
	// localLimitEvery charges the IP key and, when the body carries an
	// email, the email key on every request: register and resend do work
	// (a row, a mail) even on success.
	localLimitEvery localLimitMode = iota
	// localLimitLogin charges the IP and email keys on 401/403 outcomes
	// only, and clears the email key on success: logins are the one
	// endpoint where success is the common case, so counting every
	// attempt would let ten legitimate logins behind one NAT address
	// block the eleventh. A success never clears the IP key — that
	// budget may belong to an attacker sharing the address.
	localLimitLogin
	// localLimitIPOnly charges the IP key on every request. Verify-email
	// carries a token rather than an email, and looking the address up
	// before limiting would defeat the point.
	localLimitIPOnly
)

// localAuthLimiter throttles the four public local-auth endpoints per
// client IP and per email. It embeds the API-token limiter's proven
// bucket mechanics unchanged and adds only keying, email extraction, and
// the login success reset. Buckets are prefixed per endpoint, so the four
// routes never share a budget. Like the API-token throttle it is
// per-replica in-memory state with the shared trusted-proxy list.
type localAuthLimiter struct {
	*authFailureLimiter
}

// newLocalAuthLimiter builds the throttle from settings and the shared
// trusted-proxy list. A nil return disables throttling: the middleware
// then passes every request through.
func newLocalAuthLimiter(s configuration.LocalRateLimit, trustedProxies []string) *localAuthLimiter {
	if !s.Enabled || s.MaxAttempts <= 0 || s.Window <= 0 || s.BlockDuration <= 0 {
		return nil
	}
	return &localAuthLimiter{
		&authFailureLimiter{
			maxAttempts: s.MaxAttempts,
			window:      s.Window,
			block:       s.BlockDuration,
			trusted:     parseTrustedProxies(trustedProxies),
			buckets:     make(map[string]*failureBucket),
		},
	}
}

// reset clears one key's count and block. A login success resets its
// email key, so failures against an address age out the moment the owner
// proves it.
func (l *localAuthLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// limitLocalAuth returns middleware throttling one public endpoint. No
// request reaches the handler until the limiter admits it, so a refusal
// costs no password check, token lookup, row write, or mail. A malformed
// body is 400 without touching any bucket.
func limitLocalAuth(l *localAuthLimiter, endpoint string, mode localLimitMode) gin.HandlerFunc {
	if l == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		email, ok := peekLocalAuthEmail(c, mode)
		if !ok {
			return
		}
		ip := l.clientIP(c.Request)
		ipKey := "local:" + endpoint + "|ip:" + ip
		var emailKey string
		if mode != localLimitIPOnly && email != "" {
			emailKey = "local:" + endpoint + "|email:" + email
		}
		if !l.check(c, ipKey) {
			return
		}
		if emailKey != "" && !l.check(c, emailKey) {
			return
		}
		if mode != localLimitLogin {
			l.recordFailure(ipKey)
		}
		if mode == localLimitEvery && emailKey != "" {
			l.recordFailure(emailKey)
		}
		c.Next()
		if mode == localLimitLogin {
			switch c.Writer.Status() {
			case http.StatusOK, http.StatusCreated:
				if emailKey != "" {
					l.reset(emailKey)
				}
			case http.StatusUnauthorized, http.StatusForbidden:
				l.recordFailure(ipKey)
				if emailKey != "" {
					l.recordFailure(emailKey)
				}
			}
		}
	}
}

// peekLocalAuthEmail validates that the body is JSON and returns its
// normalized email for the modes that key on one. It restores the body
// for the handler. On false it has already answered 400.
func peekLocalAuthEmail(c *gin.Context, mode localLimitMode) (string, bool) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, localAuthMaxBodyBytes+1))
	if err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return "", false
	}
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) > localAuthMaxBodyBytes {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: too large")
		return "", false
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return "", false
	}
	if mode == localLimitIPOnly {
		return "", true
	}
	return strings.ToLower(strings.TrimSpace(body.Email)), true
}

// check enforces one bucket. It reports whether the request may proceed;
// on false it has already answered 429 with a Retry-After. The body is
// identical across endpoints, dimensions, and cases, so a refusal cannot
// be read as an account-existence oracle.
func (l *localAuthLimiter) check(c *gin.Context, key string) bool {
	ok, retryAfter := l.allow(key)
	if ok {
		return true
	}
	// Round up: a truncated 0s would tell the caller to retry at once.
	c.Header("Retry-After", strconv.Itoa(int(retryAfter/time.Second)+1))
	WriteProblem(c, http.StatusTooManyRequests, "too many requests")
	return false
}
