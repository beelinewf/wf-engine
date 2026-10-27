package pump_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/pump"
	"github.com/panjf2000/ants/v2"
)

func TestSubmitOrWaitSubmitsWhenAvailable(t *testing.T) {
	pool, err := ants.NewPool(2, ants.WithNonblocking(true))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release()
	done := make(chan struct{})
	if err := pump.SubmitOrWait(context.Background(), pool, func() { close(done) }); err != nil {
		t.Fatalf("SubmitOrWait() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("submitted func did not run")
	}
}

func TestSubmitOrWaitReturnsOnCancelWhenSaturated(t *testing.T) {
	pool, err := ants.NewPool(1, ants.WithNonblocking(true))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release()
	release := make(chan struct{})
	if err := pool.Submit(func() { <-release }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = pump.SubmitOrWait(ctx, pool, func() {})
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("SubmitOrWait(cancelled) error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("SubmitOrWait took %v, want fast return on cancel", elapsed)
	}
}

func TestBatchDispatchesEveryClaim(t *testing.T) {
	pool, err := ants.NewPool(4, ants.WithNonblocking(true))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release()
	var wg sync.WaitGroup
	var handled atomic.Int64
	var reported atomic.Int64
	pump.Batch(context.Background(), &wg, pool, "w1", "test",
		func(context.Context) ([]int, error) { return []int{1, 2, 3}, nil },
		func(_ context.Context, item int) { handled.Add(int64(item)) },
		func(n int) { reported.Add(int64(n)) })
	wg.Wait()
	if got := handled.Load(); got != 6 {
		t.Errorf("handled sum = %d, want 6", got)
	}
	if got := reported.Load(); got != 3 {
		t.Errorf("onClaimed = %d, want 3", got)
	}
}

func TestBatchSkipsOnClaimError(t *testing.T) {
	pool, err := ants.NewPool(4, ants.WithNonblocking(true))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release()
	var wg sync.WaitGroup
	handled := false
	pump.Batch(context.Background(), &wg, pool, "w1", "test",
		func(context.Context) ([]int, error) { return nil, errors.New("boom") },
		func(_ context.Context, _ int) { handled = true },
		nil)
	wg.Wait()
	if handled {
		t.Error("handle ran after claim error, want skip")
	}
}

func TestBatchDropsOnCancelWhenSaturated(t *testing.T) {
	pool, err := ants.NewPool(1, ants.WithNonblocking(true))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release()
	release := make(chan struct{})
	defer close(release)
	if err := pool.Submit(func() { <-release }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var wg sync.WaitGroup
	start := time.Now()
	pump.Batch(ctx, &wg, pool, "w1", "test",
		func(context.Context) ([]int, error) { return []int{1, 2, 3}, nil },
		func(_ context.Context, _ int) {},
		nil)
	wg.Wait()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Batch on saturated pool with cancelled ctx took %v, want fast drop", elapsed)
	}
}
