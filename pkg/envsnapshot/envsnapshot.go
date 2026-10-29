// Package envsnapshot captures allowlisted process env for workflow templates.
//
// A deny list guards the snapshot: credentials that must never reach a
// workflow template are dropped on the way in, and {{ env.* }} references to
// them resolve as missing. Two independent layers apply — the snapshot itself
// excludes denied names, and callers strip denied names out of a base map —
// because a snapshot is only trustworthy if the merge cannot be bypassed.
package envsnapshot

import (
	"os"
	"path"
	"strings"
	"sync/atomic"
)

// Prefix is the only env namespace snapshotted into workflow contexts.
// Workflows address values as {{ env.BEELINEWF_X }}.
const Prefix = "BEELINEWF_"

// Denied names are patterns matched against the full env var name. Globs are
// supported so an operator can add patterns without code changes; a pattern
// without a wildcard is an exact match.
var defaultDenied = []string{
	"BEELINEWF_API_TOKEN",
	"BEELINEWF_AUTH_*",
	"BEELINEWF_SYSTEM_*",
	"BEELINEWF_INFRA_*",
	"BEELINEWF_ENGINE_SMTP_PASSWORD",
	"BEELINEWF_*_DSN",
	// The at-rest master key itself: it arrives through the process
	// environment, so without this entry every instance snapshot would
	// hand it to any template that asks.
	"BEELINEWF_SECRET_ENCRYPTION_KEY",
	"BEELINEWF_SECRETS_ENCRYPTION_KEY",
}

// overrideConfig holds the operator-supplied patterns installed by
// SetOverride. It is immutable once stored.
type overrideConfig struct {
	extra []string
	allow []string
}

// override is read on every snapshot and render, concurrently with the
// startup install (and any test reinstall), so it is swapped atomically
// and readers load one consistent snapshot.
var override atomic.Pointer[overrideConfig]

// IsDeniedDefault reports whether name matches a hardcoded deny pattern.
func IsDeniedDefault(name string) bool {
	return matchAny(defaultDenied, name)
}

// IsDenied reports whether name is denied: a default or extra pattern matches
// and no allow pattern matches.
func IsDenied(name string) bool {
	var extra, allow []string
	if ov := override.Load(); ov != nil {
		extra, allow = ov.extra, ov.allow
	}
	denied := IsDeniedDefault(name) || matchAny(extra, name)
	if !denied {
		return false
	}
	return !matchAny(allow, name)
}

// SetOverride installs the operator deny additions and allow exceptions. It is
// called once at startup from configuration; the empty lists mean defaults
// only. The install is atomic: concurrent readers keep seeing one
// consistent snapshot.
func SetOverride(extra, allow []string) {
	override.Store(&overrideConfig{
		extra: append([]string(nil), extra...),
		allow: append([]string(nil), allow...),
	})
}

// matchAny reports whether any glob pattern matches name. An empty pattern
// list never matches.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// Snapshot returns {"NAME": "value"} for env vars starting with prefix and
// matching no deny pattern.
func Snapshot(prefix string) map[string]any {
	out := map[string]any{}
	for _, kv := range os.Environ() {
		if name, val, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, prefix) && !IsDenied(name) {
			out[name] = val
		}
	}
	return out
}

// StripDenied copies base without denied keys (base must be map or nil). A
// caller-supplied env map therefore cannot smuggle a denied name past the
// snapshot.
func StripDenied(base any) map[string]any {
	out := map[string]any{}
	m, ok := base.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if IsDenied(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// Merge overlays snapshot onto base (base must be map or nil); snapshot wins per key.
func Merge(base any, snapshot map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := base.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range snapshot {
		out[k] = v
	}
	return out
}
