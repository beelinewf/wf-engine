package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

// CORS returns Gin middleware enforcing the configured cross-origin policy.
// With no allowed origins it passes every request through, which is the
// disabled state: the engine emits no CORS headers, exactly as before.
//
// Register it first, before any route group, so a preflight (OPTIONS with an
// Origin and an Access-Control-Request-Method) is answered directly instead
// of reaching the authentication middleware, which would 401 a request the
// browser deliberately sends without credentials. For the same reason the
// actual-response headers are set before the remaining handlers run, so an
// authentication or permission refusal still shares its response.
//
// A request from a disallowed origin runs normally but gets no CORS headers:
// sharing is opt-in, and the browser enforces the denial. A preflight that
// fails the origin, method, or header check is rejected with 403.
func CORS(cfg configuration.CORS) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	wildcard := false
	for _, origin := range cfg.AllowedOrigins {
		if strings.TrimSpace(origin) == "*" {
			wildcard = true
			continue
		}
		allowed[normalizeOrigin(origin)] = struct{}{}
	}
	methods := upperSet(cfg.AllowedMethods)
	headers := lowerSet(cfg.AllowedHeaders)
	allowMethods := strings.Join(cfg.AllowedMethods, ", ")
	allowHeaders := strings.Join(cfg.AllowedHeaders, ", ")
	exposeHeaders := strings.Join(cfg.ExposedHeaders, ", ")
	maxAge := strconv.FormatInt(int64(cfg.MaxAge/time.Second), 10)

	return func(c *gin.Context) {
		if len(allowed) == 0 && !wildcard {
			c.Next()
			return
		}
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}
		_, ok := allowed[normalizeOrigin(origin)]
		if !wildcard && !ok {
			if isPreflight(c) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.Next()
			return
		}
		if isPreflight(c) {
			if !preflightAllowed(c, methods, headers) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			writeCORSHeaders(c, origin, wildcard, cfg.AllowCredentials, exposeHeaders)
			c.Header("Access-Control-Allow-Methods", allowMethods)
			c.Header("Access-Control-Allow-Headers", allowHeaders)
			c.Header("Access-Control-Max-Age", maxAge)
			addVary(c, "Access-Control-Request-Method", "Access-Control-Request-Headers")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		writeCORSHeaders(c, origin, wildcard, cfg.AllowCredentials, exposeHeaders)
		c.Next()
	}
}

// isPreflight reports whether the request is a CORS preflight: an OPTIONS
// request whose Access-Control-Request-Method names the method the actual
// request will use. A plain OPTIONS without that header is not a preflight
// and passes through to the route.
func isPreflight(c *gin.Context) bool {
	return c.Request.Method == http.MethodOptions &&
		c.GetHeader("Origin") != "" &&
		strings.TrimSpace(c.GetHeader("Access-Control-Request-Method")) != ""
}

// preflightAllowed checks the requested method and headers against the
// configured allowlists. Methods compare case-insensitively after upper
// casing; headers compare case-insensitively per HTTP semantics.
func preflightAllowed(c *gin.Context, methods, headers map[string]struct{}) bool {
	method := strings.ToUpper(strings.TrimSpace(c.GetHeader("Access-Control-Request-Method")))
	if _, ok := methods[method]; !ok {
		return false
	}
	for _, h := range strings.Split(c.GetHeader("Access-Control-Request-Headers"), ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, ok := headers[h]; !ok {
			return false
		}
	}
	return true
}

// writeCORSHeaders sets the origin, credentials, and exposed-headers
// response headers. A credentialed response always names the origin: the
// configuration already forbids credentials with "*", and the wildcard
// branch keeps that invariant even for a hand-built config.
func writeCORSHeaders(c *gin.Context, origin string, wildcard, credentials bool, expose string) {
	if wildcard {
		c.Header("Access-Control-Allow-Origin", "*")
	} else {
		c.Header("Access-Control-Allow-Origin", origin)
	}
	if credentials && !wildcard {
		c.Header("Access-Control-Allow-Credentials", "true")
	}
	if expose != "" {
		c.Header("Access-Control-Expose-Headers", expose)
	}
	addVary(c, "Origin")
}

// addVary appends names to the Vary response header, skipping entries that
// are already present.
func addVary(c *gin.Context, names ...string) {
	h := c.Writer.Header()
	current := h.Get("Vary")
	for _, name := range names {
		if current == "" {
			current = name
			continue
		}
		duplicate := false
		for _, part := range strings.Split(current, ",") {
			if strings.EqualFold(strings.TrimSpace(part), name) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			current += ", " + name
		}
	}
	h.Set("Vary", current)
}

// normalizeOrigin trims the whitespace and trailing slashes an operator may
// leave on a configured entry, so "https://app.example.com/" matches the
// bare origin the browser sends.
func normalizeOrigin(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

func upperSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[strings.ToUpper(strings.TrimSpace(v))] = struct{}{}
	}
	return set
}

func lowerSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[strings.ToLower(strings.TrimSpace(v))] = struct{}{}
	}
	return set
}
