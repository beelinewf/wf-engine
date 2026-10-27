// Package pump shares the claim-and-dispatch batch used by the engine and
// status-update dispatchers: claim a batch, submit each item to the pool,
// and run inline when the pool is closed during shutdown. Sharing the batch
// keeps shutdown and claim-error handling from diverging between loops.
package pump

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
)

// SubmitOrWait submits fn to the nonblocking pool, retrying on overload
// until ctx is cancelled. It returns nil on submit, ants.ErrPoolClosed when
// the pool is closed, or ctx.Err() when cancelled while waiting for capacity.
func SubmitOrWait(ctx context.Context, pool *ants.Pool, fn func()) error {
	for {
		err := pool.Submit(fn)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ants.ErrPoolOverload) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Batch claims items and dispatches each through the pool, tracking every
// submission on wg so shutdown joins in-flight work. Claim errors are
// logged and skip the batch; a cancelled context drops remaining items so
// shutdown never blocks on a saturated pool. onClaimed reports the batch
// size for metrics and may be nil.
func Batch[T any](ctx context.Context, wg *sync.WaitGroup, pool *ants.Pool, workerID, component string, claim func(ctx context.Context) ([]T, error), handle func(ctx context.Context, item T), onClaimed func(n int)) {
	claimed, err := claim(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn(component+" claim failed", "worker", workerID, "error", err)
		}
		return
	}
	if onClaimed != nil {
		onClaimed(len(claimed))
	}
	for _, item := range claimed {
		item := item
		wg.Add(1)
		err := SubmitOrWait(ctx, pool, func() {
			defer wg.Done()
			handle(ctx, item)
		})
		if err != nil {
			wg.Done()
			if ctx.Err() != nil {
				continue
			}
			handle(ctx, item)
		}
	}
}
