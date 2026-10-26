// Package metrics owns the Prometheus registry, domain series, and the
// TTL-cached collector for DB-backed gauges. It imports nothing from
// engine, executor, or service: labels are plain strings, keeping the
// dependency direction clean. Every recorder method is nil-safe so
// components constructed without a registry behave exactly as before.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DBPoolStats is a sql.DB.Stats snapshot without the database/sql import.
type DBPoolStats struct {
	OpenConnections int
	InUse           int
	WaitCount       int64
	WaitDuration    time.Duration
}

// Sources feeds the TTL-cached gauges. A nil func leaves its gauge at its
// last value (zero until the first successful refresh). An error keeps the
// previous snapshot instead of zeroing it.
type Sources struct {
	InstancesActive   func() (map[string]int64, error)
	DispatcherStats   func() (pending int64, oldestAgeSeconds float64, err error)
	StatusOutboxDepth func() (int64, error)
	DBStats           func() (DBPoolStats, error)
	// TTL caps refresh frequency; zero means 15s.
	TTL time.Duration
}

func (s Sources) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return 15 * time.Second
}

// Recorder records domain metrics. Use New in production wiring; a nil
// *Recorder is a valid no-op for tests that construct components directly.
type Recorder struct {
	reg              *prometheus.Registry
	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	instancesCreated prometheus.Counter
	instancesTerm    *prometheus.CounterVec
	instanceDuration *prometheus.HistogramVec
	nodeExecutions   *prometheus.CounterVec
	nodeDuration     *prometheus.HistogramVec
	dispatcherClaims prometheus.Counter
	statusDeliveries *prometheus.CounterVec
	schedulerFires   *prometheus.CounterVec
	consumerHealthy  *prometheus.GaugeVec
	bulkheadInflight prometheus.Gauge
}

// New builds the registry with Go/process collectors, all domain series,
// and the TTL-cached gauge collector over sources.
func New(sources Sources) *Recorder {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	r := &Recorder{reg: reg}
	r.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "API requests by method, route template, and status.",
	}, []string{"method", "route", "status"})
	r.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "API request duration by method, route template, and status.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})
	r.instancesCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wf_instances_created_total",
		Help: "Workflow instances created.",
	})
	r.instancesTerm = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_instances_terminal_total",
		Help: "Workflow instances reaching a terminal status.",
	}, []string{"status"})
	r.instanceDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wf_instance_duration_seconds",
		Help:    "End-to-end instance duration by terminal status.",
		Buckets: prometheus.DefBuckets,
	}, []string{"status"})
	r.nodeExecutions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_node_executions_total",
		Help: "Node executions by node type and result.",
	}, []string{"node_type", "result"})
	r.nodeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wf_node_execution_duration_seconds",
		Help:    "Node execution duration by node type.",
		Buckets: prometheus.DefBuckets,
	}, []string{"node_type"})
	r.dispatcherClaims = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wf_dispatcher_claims_total",
		Help: "Transitions claimed by the engine dispatcher.",
	})
	r.statusDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_status_deliveries_total",
		Help: "Status-update deliveries by transport and result.",
	}, []string{"transport", "result"})
	r.schedulerFires = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wf_scheduler_fires_total",
		Help: "Scheduler fires by result.",
	}, []string{"result"})
	r.consumerHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "wf_broker_consumer_healthy",
		Help: "Broker input consumer liveness by transport (1 healthy, 0 down).",
	}, []string{"transport"})
	r.bulkheadInflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wf_http_bulkhead_inflight",
		Help: "Global HTTP bulkhead inflight executions (global scope only in v1).",
	})
	reg.MustRegister(
		r.httpRequests, r.httpDuration,
		r.instancesCreated, r.instancesTerm, r.instanceDuration,
		r.nodeExecutions, r.nodeDuration,
		r.dispatcherClaims, r.statusDeliveries, r.schedulerFires,
		r.consumerHealthy, r.bulkheadInflight,
		newCachedCollector(sources),
	)
	// Bounded terminal statuses exist from boot so the series is greppable
	// before the first completion; node types and routes stay lazy.
	for _, s := range []string{"finished", "failed", "stopped"} {
		r.instancesTerm.WithLabelValues(s).Add(0)
		_ = r.instanceDuration.WithLabelValues(s)
	}
	return r
}

// Handler serves the registry via promhttp. A nil recorder serves 404 so a
// miswired router fails closed instead of exposing an empty registry.
func (r *Recorder) Handler() http.Handler {
	if r == nil || r.reg == nil {
		return http.HandlerFunc(http.NotFound)
	}
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// Registry exposes the underlying registry for tests.
func (r *Recorder) Registry() *prometheus.Registry {
	if r == nil {
		return nil
	}
	return r.reg
}

// InstancesCreatedCounter exposes the created counter for testutil.
func (r *Recorder) InstancesCreatedCounter() prometheus.Counter {
	if r == nil {
		return nil
	}
	return r.instancesCreated
}

// ObserveHTTP records one API request. Route must be the Gin template
// (c.FullPath()), never the raw path.
func (r *Recorder) ObserveHTTP(method, route, status string, dur time.Duration) {
	if r == nil {
		return
	}
	r.httpRequests.WithLabelValues(method, route, status).Inc()
	r.httpDuration.WithLabelValues(method, route, status).Observe(dur.Seconds())
}

// InstanceCreated counts one created instance.
func (r *Recorder) InstanceCreated() {
	if r == nil {
		return
	}
	r.instancesCreated.Inc()
}

// InstanceTerminal counts one terminal transition and its end-to-end duration.
func (r *Recorder) InstanceTerminal(status string, dur time.Duration) {
	if r == nil {
		return
	}
	r.instancesTerm.WithLabelValues(status).Inc()
	r.instanceDuration.WithLabelValues(status).Observe(dur.Seconds())
}

// NodeExecution counts one node execution and its duration.
func (r *Recorder) NodeExecution(nodeType, result string, dur time.Duration) {
	if r == nil {
		return
	}
	r.nodeExecutions.WithLabelValues(nodeType, result).Inc()
	r.nodeDuration.WithLabelValues(nodeType).Observe(dur.Seconds())
}

// DispatcherClaimed counts one claimed transition.
func (r *Recorder) DispatcherClaimed() {
	if r == nil {
		return
	}
	r.dispatcherClaims.Inc()
}

// DispatcherClaimsAdd counts n claimed transitions.
func (r *Recorder) DispatcherClaimsAdd(n int) {
	if r == nil || n <= 0 {
		return
	}
	r.dispatcherClaims.Add(float64(n))
}

// StatusDelivered counts one status-update delivery attempt.
func (r *Recorder) StatusDelivered(transport, result string) {
	if r == nil {
		return
	}
	r.statusDeliveries.WithLabelValues(transport, result).Inc()
}

// SchedulerFired counts one scheduler fire.
func (r *Recorder) SchedulerFired(result string) {
	if r == nil {
		return
	}
	r.schedulerFires.WithLabelValues(result).Inc()
}

// SetConsumerHealthy sets broker consumer liveness for a transport.
func (r *Recorder) SetConsumerHealthy(transport string, healthy bool) {
	if r == nil {
		return
	}
	if healthy {
		r.consumerHealthy.WithLabelValues(transport).Set(1)
		return
	}
	r.consumerHealthy.WithLabelValues(transport).Set(0)
}

// SetBulkheadInflight sets global bulkhead occupancy.
func (r *Recorder) SetBulkheadInflight(n float64) {
	if r == nil {
		return
	}
	r.bulkheadInflight.Set(n)
}

type snapshot struct {
	active  map[string]int64
	backlog int64
	oldest  float64
	outbox  int64
	db      DBPoolStats
}

type cachedCollector struct {
	mu      sync.Mutex
	last    time.Time
	ttl     time.Duration
	sources Sources
	snap    snapshot
	active  *prometheus.Desc
	backlog *prometheus.Desc
	oldest  *prometheus.Desc
	outbox  *prometheus.Desc
	dbOpen  *prometheus.Desc
	dbInUse *prometheus.Desc
	dbWait  *prometheus.Desc
	dbWaitD *prometheus.Desc
}

func newCachedCollector(sources Sources) *cachedCollector {
	return &cachedCollector{
		ttl:     sources.ttl(),
		sources: sources,
		active: prometheus.NewDesc("wf_instances_active", "Active (non-terminal) instances by status (cached snapshot).",
			[]string{"status"}, nil),
		backlog: prometheus.NewDesc("wf_dispatcher_backlog", "Claimable pending transitions (cached snapshot).",
			nil, nil),
		oldest: prometheus.NewDesc("wf_dispatcher_oldest_pending_age_seconds", "Age of the oldest pending transition (cached snapshot).",
			nil, nil),
		outbox: prometheus.NewDesc("wf_status_outbox_backlog", "Pending status-update outbox rows (cached snapshot).",
			nil, nil),
		dbOpen: prometheus.NewDesc("wf_db_pool_open_connections", "Open DB connections.",
			nil, nil),
		dbInUse: prometheus.NewDesc("wf_db_pool_in_use", "In-use DB connections.",
			nil, nil),
		dbWait: prometheus.NewDesc("wf_db_pool_wait_count_total", "Total DB pool waits.",
			nil, nil),
		dbWaitD: prometheus.NewDesc("wf_db_pool_wait_duration_seconds_total", "Total DB pool wait time in seconds.",
			nil, nil),
	}
}

func (c *cachedCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.active
	ch <- c.backlog
	ch <- c.oldest
	ch <- c.outbox
	ch <- c.dbOpen
	ch <- c.dbInUse
	ch <- c.dbWait
	ch <- c.dbWaitD
}

// Collect serves the cached snapshot, refreshing at most once per TTL. No
// new goroutine runs: refresh happens inline on scrape, and sources are
// called outside the mutex so a slow query does not block scrapes.
func (c *cachedCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	fresh := !c.last.IsZero() && time.Since(c.last) < c.ttl
	src := c.sources
	snap := c.snap
	c.mu.Unlock()
	if !fresh {
		if active, err := callActive(src); err == nil && active != nil {
			snap.active = active
		}
		if pending, age, err := callDispatcher(src); err == nil {
			snap.backlog, snap.oldest = pending, age
		}
		if depth, err := callOutbox(src); err == nil {
			snap.outbox = depth
		}
		if db, err := callDB(src); err == nil {
			snap.db = db
		}
		c.mu.Lock()
		c.snap, c.last = snap, time.Now()
		c.mu.Unlock()
	}
	for status, n := range snap.active {
		ch <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, float64(n), status)
	}
	ch <- prometheus.MustNewConstMetric(c.backlog, prometheus.GaugeValue, float64(snap.backlog))
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, snap.oldest)
	ch <- prometheus.MustNewConstMetric(c.outbox, prometheus.GaugeValue, float64(snap.outbox))
	ch <- prometheus.MustNewConstMetric(c.dbOpen, prometheus.GaugeValue, float64(snap.db.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.dbInUse, prometheus.GaugeValue, float64(snap.db.InUse))
	ch <- prometheus.MustNewConstMetric(c.dbWait, prometheus.CounterValue, float64(snap.db.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.dbWaitD, prometheus.CounterValue, snap.db.WaitDuration.Seconds())
}

func callActive(src Sources) (map[string]int64, error) {
	if src.InstancesActive == nil {
		return nil, errNoSource
	}
	return src.InstancesActive()
}

func callDispatcher(src Sources) (int64, float64, error) {
	if src.DispatcherStats == nil {
		return 0, 0, errNoSource
	}
	return src.DispatcherStats()
}

func callOutbox(src Sources) (int64, error) {
	if src.StatusOutboxDepth == nil {
		return 0, errNoSource
	}
	return src.StatusOutboxDepth()
}

func callDB(src Sources) (DBPoolStats, error) {
	if src.DBStats == nil {
		return DBPoolStats{}, errNoSource
	}
	return src.DBStats()
}

type noSource struct{}

func (noSource) Error() string { return "no source" }

var errNoSource = noSource{}
