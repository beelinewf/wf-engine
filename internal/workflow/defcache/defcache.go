// Package defcache is a tiny generic cache core for immutable workflow
// artifacts: materialized definitions, status-update configs, and compiled
// JSON schemas. All three are keyed by values that never change meaning
// (a definition id always resolves to the same bytes; a schema hash always
// denotes the same schema), so entries need no invalidation or TTL — an
// ID-keyed entry is stable forever.
//
// Eviction is FIFO, not LRU: no new dependency for boundedness, and
// immutable keys with a small stable working set need no recency precision.
// Callers must treat shared cached pointers as READ-ONLY; a consumer that
// mutates a cached value corrupts every later reader of that key.
package defcache

import (
	"sync"
	"sync/atomic"
)

// Cache is a bounded mutex-guarded map with FIFO eviction. The zero value
// is not usable; build one with New. It is safe for concurrent use:
// steady-state hits take a read lock while a miss computes outside the
// lock under per-key singleflight, so a slow compute never stalls readers
// and a cold key computes exactly once per process.
type Cache[K comparable, V any] struct {
	mu     sync.RWMutex
	max    int
	items  map[K]V
	order  []K // insertion order; order[0] is the oldest entry
	calls  map[K]*call[V]
	hits   atomic.Uint64
	misses atomic.Uint64
}

// call is one in-flight compute shared by concurrent misses on a key.
type call[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// New builds a cache holding at most maxEntries entries. A zero or
// negative cap collapses to a single entry rather than panicking or
// growing without bound.
func New[K comparable, V any](maxEntries int) *Cache[K, V] {
	if maxEntries <= 0 {
		maxEntries = 1
	}
	return &Cache[K, V]{max: maxEntries, items: make(map[K]V, maxEntries), calls: make(map[K]*call[V])}
}

// GetOrCompute returns the cached value for key, computing and storing it
// on a miss. Successful results are cached; compute errors are returned
// without storing anything, so a transient failure never poisons the cache
// and the next call retries. Concurrent misses on one cold key share a
// single compute and the losers observe the winner's entry.
func (c *Cache[K, V]) GetOrCompute(key K, compute func() (V, error)) (V, error) {
	c.mu.RLock()
	v, ok := c.items[key]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
		return v, nil
	}
	c.mu.Lock()
	if v, ok := c.items[key]; ok {
		c.mu.Unlock()
		c.hits.Add(1)
		return v, nil
	}
	if cl, ok := c.calls[key]; ok {
		c.mu.Unlock()
		<-cl.done
		if cl.err != nil {
			var zero V
			return zero, cl.err
		}
		c.hits.Add(1)
		return cl.val, nil
	}
	cl := &call[V]{done: make(chan struct{})}
	c.calls[key] = cl
	c.mu.Unlock()

	v, err := compute()

	c.mu.Lock()
	delete(c.calls, key)
	if err == nil {
		if len(c.items) >= c.max {
			var zero K
			oldest := c.order[0]
			c.order[0] = zero
			c.order = c.order[1:]
			if len(c.order) == 0 {
				c.order = nil
			}
			delete(c.items, oldest)
		}
		c.items[key] = v
		c.order = append(c.order, key)
		cl.val = v
	} else {
		cl.err = err
	}
	close(cl.done)
	c.mu.Unlock()

	c.misses.Add(1)
	if err != nil {
		var zero V
		return zero, err
	}
	return v, nil
}

// Hits counts cache hits; Misses counts miss computations (including
// computations that failed and were not stored). Tests assert on both.
func (c *Cache[K, V]) Hits() uint64   { return c.hits.Load() }
func (c *Cache[K, V]) Misses() uint64 { return c.misses.Load() }

// Len reports the number of stored entries.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
