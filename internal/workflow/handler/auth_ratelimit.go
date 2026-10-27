package handler

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

// maxTrackedClientIPs bounds the limiter's memory: past this many distinct
// source IPs the next new key sweeps expired buckets first. A distributed
// spray can still fill the map, but only with live windows, and each entry
// is a counter and two timestamps.
const maxTrackedClientIPs = 10000

// authFailureLimiter throttles failed API-token presentations per client IP
// (F7): after maxAttempts failures within window, the IP answers 429 for
// block. Successes never count, so legitimate clients are unaffected no
// matter their request rate. Build one with newAuthFailureLimiter, which
// returns nil when the throttle is disabled.
type authFailureLimiter struct {
	maxAttempts int
	window      time.Duration
	block       time.Duration
	trusted     []*net.IPNet

	mu      sync.Mutex
	buckets map[string]*failureBucket
}

// failureBucket is one source IP's failure count within its window, plus
// the block deadline once the count trips the throttle.
type failureBucket struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

// newAuthFailureLimiter builds the throttle from settings. A nil return
// disables throttling: the middleware then behaves exactly as before.
func newAuthFailureLimiter(s configuration.RateLimit) *authFailureLimiter {
	if !s.Enabled || s.MaxAttempts <= 0 || s.Window <= 0 || s.BlockDuration <= 0 {
		return nil
	}
	return &authFailureLimiter{
		maxAttempts: s.MaxAttempts,
		window:      s.Window,
		block:       s.BlockDuration,
		trusted:     parseTrustedProxies(s.TrustedProxies),
		buckets:     make(map[string]*failureBucket),
	}
}

// parseTrustedProxies compiles the configured proxy IPs and CIDRs. A bad
// entry is skipped: configuration validation already rejects it at boot, so
// reaching here with one means a hand-built setting, and trusting nothing
// is the safe reading.
func parseTrustedProxies(entries []string) []*net.IPNet {
	var out []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			out = append(out, singleIPNet(ip))
			continue
		}
		if _, cidr, err := net.ParseCIDR(e); err == nil {
			out = append(out, cidr)
		}
	}
	return out
}

// singleIPNet widens a bare IP into its host network.
func singleIPNet(ip net.IP) *net.IPNet {
	if ip.To4() != nil {
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

// clientIP returns the limiter key for a request: the direct peer, unless
// the peer itself is a trusted proxy, in which case the leftmost
// X-Forwarded-For entry (or X-Real-Ip) the proxy vouches for. Headers from
// an untrusted peer are attacker-controlled and ignored, so a spoofed
// header can never rotate the key.
//
// This deliberately does not use gin's ClientIP: gin trusts every proxy by
// default, which would honor a forged X-Forwarded-For from anyone.
func (l *authFailureLimiter) clientIP(r *http.Request) string {
	peer := stripPort(r.RemoteAddr)
	peerIP := net.ParseIP(peer)
	if peerIP == nil || !l.trustedPeer(peerIP) {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if first = strings.TrimSpace(stripPort(first)); first != "" {
			return first
		}
	}
	if xri := strings.TrimSpace(stripPort(r.Header.Get("X-Real-Ip"))); xri != "" {
		return xri
	}
	return peer
}

// trustedPeer reports whether the direct peer is a configured proxy.
func (l *authFailureLimiter) trustedPeer(ip net.IP) bool {
	for _, cidr := range l.trusted {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// stripPort drops the :port suffix from a host:port peer address. A value
// without a port, or an unparseable one, is returned as-is, trimmed.
func stripPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// allow reports whether a request from ip may attempt authentication. A
// denial carries the remaining block as the Retry-After.
func (l *authFailureLimiter) allow(ip string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucketLocked(ip)
	if now.Before(b.blockedUntil) {
		return false, b.blockedUntil.Sub(now)
	}
	if now.Sub(b.windowStart) >= l.window {
		b.count = 0
		b.windowStart = now
	}
	if b.count >= l.maxAttempts {
		b.blockedUntil = now.Add(l.block)
		return false, l.block
	}
	return true, 0
}

// recordFailure charges one failed attempt to ip. The failure that trips
// the threshold still answers 401; the block bites from the next attempt
// on, which is what keeps the count exact: MaxAttempts failures, then 429s.
func (l *authFailureLimiter) recordFailure(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucketLocked(ip)
	if now.Before(b.blockedUntil) {
		return
	}
	if now.Sub(b.windowStart) >= l.window {
		b.count = 0
		b.windowStart = now
	}
	b.count++
	if b.count >= l.maxAttempts {
		b.blockedUntil = now.Add(l.block)
	}
}

// bucketLocked returns ip's bucket, sweeping expired buckets first when
// the map has grown past its bound.
func (l *authFailureLimiter) bucketLocked(ip string) *failureBucket {
	if b, ok := l.buckets[ip]; ok {
		return b
	}
	if len(l.buckets) >= maxTrackedClientIPs {
		l.sweepLocked(time.Now())
	}
	b := &failureBucket{}
	l.buckets[ip] = b
	return b
}

// sweepLocked drops buckets whose window and block have both expired.
func (l *authFailureLimiter) sweepLocked(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.windowStart) >= l.window && !now.Before(b.blockedUntil) {
			delete(l.buckets, ip)
		}
	}
}

// limitApplies reports whether a request is an API-token attempt the
// throttle covers: token auth enabled with a token configured, and the
// header presented. OIDC-only requests never touch the limiter: Bearer
// tokens are signed, not guessable, and expiry churn must not lock an IP
// out. A nil limiter disables throttling.
func (cfg AuthConfig) limitApplies(c *gin.Context) bool {
	return cfg.FailureLimiter != nil && cfg.Enabled && cfg.APIToken != "" && c.GetHeader(APIKeyHeader) != ""
}

// checkAuthLimit enforces the throttle before a credential check. It
// reports whether the request may proceed; on false it has already
// answered 429 with a Retry-After.
func checkAuthLimit(c *gin.Context, cfg AuthConfig) bool {
	ip := cfg.FailureLimiter.clientIP(c.Request)
	ok, retryAfter := cfg.FailureLimiter.allow(ip)
	if ok {
		return true
	}
	// Round up: a truncated 0s would tell the caller to retry at once.
	c.Header("Retry-After", strconv.Itoa(int(retryAfter/time.Second)+1))
	WriteProblem(c, http.StatusTooManyRequests, "too many failed authentication attempts")
	return false
}

// noteAuthFailure charges a failed API-token attempt to the throttle. The
// caller must have checked limitApplies: with a presented token any
// authentication failure means the token was wrong (a match wins before
// OIDC is even consulted), so the charge is exact.
func noteAuthFailure(c *gin.Context, cfg AuthConfig) {
	cfg.FailureLimiter.recordFailure(cfg.FailureLimiter.clientIP(c.Request))
}
