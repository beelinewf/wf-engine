package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/pump"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/panjf2000/ants/v2"
)

// DispatcherOptions tunes the claim loop. Zero values get defaults.
type DispatcherOptions struct {
	PollInterval time.Duration
	Lease        time.Duration
	BatchSize    int
	PoolSize     int
	Heartbeat    time.Duration
}

// Dispatcher leases runnable instances with FOR UPDATE SKIP LOCKED claims
// and executes one node transition per claim through an ants pool, renewing
// its leases on a heartbeat while it lives.
type Dispatcher struct {
	engine    *Engine
	instances repository.InstanceRepository
	parallel  repository.ParallelRepository
	workerID  string
	pool      *ants.Pool
	metrics   *metrics.Recorder

	pollInterval time.Duration
	lease        time.Duration
	batchSize    int
	heartbeat    time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// SetMetrics installs the optional metrics recorder. Nil disables recording.
func (d *Dispatcher) SetMetrics(rec *metrics.Recorder) {
	d.metrics = rec
}

// NewDispatcher builds a dispatcher; it is inert until Run is called.
func NewDispatcher(ctx context.Context, e *Engine, instances repository.InstanceRepository, parallel repository.ParallelRepository, workerID string, opts DispatcherOptions) (*Dispatcher, error) {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 200 * time.Millisecond
	}
	if opts.Lease <= 0 {
		opts.Lease = 30 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = 20
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 5 * time.Second
	}
	pool, err := ants.NewPool(opts.PoolSize, ants.WithNonblocking(true))
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	return &Dispatcher{
		engine:       e,
		instances:    instances,
		parallel:     parallel,
		workerID:     workerID,
		pool:         pool,
		pollInterval: opts.PollInterval,
		lease:        opts.Lease,
		batchSize:    opts.BatchSize,
		heartbeat:    opts.Heartbeat,
		ctx:          runCtx,
		cancel:       cancel,
	}, nil
}

// Run starts the claim and heartbeat loops. It returns immediately.
func (d *Dispatcher) Run() {
	d.wg.Add(2)
	go d.claimLoop()
	go d.heartbeatLoop()
}

func (d *Dispatcher) claimLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
		pump.Batch(d.ctx, &d.wg, d.pool, d.workerID, "dispatcher",
			func(ctx context.Context) ([]model.WorkflowInstance, error) {
				return d.instances.ClaimNext(ctx, d.workerID, d.lease, d.batchSize)
			},
			func(ctx context.Context, w model.WorkflowInstance) {
				if err := d.engine.Process(ctx, w); err != nil && ctx.Err() == nil {
					slog.Warn("dispatcher process failed", "worker", d.workerID, "instance", w.ID, "error", err)
				}
			},
			d.metrics.DispatcherClaimsAdd)
		d.claimBranches()
	}
}

func (d *Dispatcher) claimBranches() {
	if d.parallel == nil {
		return
	}
	pump.Batch(d.ctx, &d.wg, d.pool, d.workerID, "dispatcher branch",
		func(ctx context.Context) ([]model.ParallelBranch, error) {
			return d.parallel.ClaimNextBranches(ctx, d.workerID, d.lease, d.batchSize)
		},
		func(ctx context.Context, b model.ParallelBranch) {
			if err := d.engine.ProcessBranch(ctx, b); err != nil && ctx.Err() == nil {
				slog.Warn("dispatcher branch process failed", "worker", d.workerID, "branch", b.ID, "error", err)
			}
		},
		func(n int) {
			d.metrics.DispatcherClaimsAdd(n)
			if n > 0 {
				slog.Debug("dispatcher claimed branches", "worker", d.workerID, "count", n)
			}
		})
}

func (d *Dispatcher) heartbeatLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(d.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
		if err := d.instances.RenewLeases(d.ctx, d.workerID, d.lease); err != nil && d.ctx.Err() == nil {
			slog.Warn("dispatcher heartbeat failed", "worker", d.workerID, "error", err)
		}
		if d.parallel != nil {
			if err := d.parallel.RenewBranchLeases(d.ctx, d.workerID, d.lease); err != nil && d.ctx.Err() == nil {
				slog.Warn("dispatcher branch heartbeat failed", "worker", d.workerID, "error", err)
			}
		}
		d.pollTermination()
	}
}

// pollTermination propagates stops across replicas: any instance marked
// termination-pending is cancelled locally (no-op when it runs elsewhere),
// and pending flags whose node attempts already finished are swept.
func (d *Dispatcher) pollTermination() {
	pending, err := d.instances.ListTerminationPending(d.ctx)
	if err != nil {
		if d.ctx.Err() == nil {
			slog.Warn("dispatcher termination poll failed", "worker", d.workerID, "error", err)
		}
		return
	}
	for _, id := range pending {
		d.engine.Cancel(id)
		d.cancelBranches(id)
	}
	if err := d.instances.SweepTermination(d.ctx); err != nil && d.ctx.Err() == nil {
		slog.Warn("dispatcher termination sweep failed", "worker", d.workerID, "error", err)
	}
}

// cancelBranches fans a termination cancel out to an instance's in-flight
// branch workers, which register under their branch ids.
func (d *Dispatcher) cancelBranches(instanceID string) {
	if d.parallel == nil {
		return
	}
	ids, err := d.parallel.ListBranchIDs(d.ctx, instanceID)
	if err != nil {
		if d.ctx.Err() == nil {
			slog.Warn("dispatcher branch cancel list failed", "worker", d.workerID, "instance", instanceID, "error", err)
		}
		return
	}
	for _, id := range ids {
		d.engine.Cancel(id)
	}
}

// Shutdown stops claiming and heartbeat, then waits for in-flight
// transitions to finish, honoring the caller's deadline.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.cancel()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		d.pool.Release()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
