package statusupdate_test

import (
	"context"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/statusupdate"
)

// joinOnceRepo claims each queued event exactly once, then idles.
type joinOnceRepo struct {
	repository.StatusUpdateRepository
	ch chan repository.PendingStatusUpdate
}

func (f *joinOnceRepo) ClaimNextStatusUpdates(ctx context.Context, workerID string, lease time.Duration, limit int) ([]repository.PendingStatusUpdate, error) {
	select {
	case ev := <-f.ch:
		return []repository.PendingStatusUpdate{ev}, nil
	default:
		return nil, nil
	}
}

func (f *joinOnceRepo) MarkStatusUpdateDelivered(ctx context.Context, eventID, workerID string) error {
	return nil
}

func (f *joinOnceRepo) FailStatusUpdate(ctx context.Context, eventID, workerID string, retryDelay time.Duration, maxRetry int, errMsg string) error {
	return nil
}

// joinGatePublisher blocks inside Publish until release is closed, ignoring
// context cancellation like a commit that must finish once started.
type joinGatePublisher struct {
	started chan struct{}
	release chan struct{}
}

func (p *joinGatePublisher) Publish(ctx context.Context, cfg *model.StatusUpdateConfig, ev repository.PendingStatusUpdate) error {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return nil
}

// TestDispatcherShutdownJoinsInFlightDeliveries is the CQ-H4 regression test:
// Shutdown must block until a gated pool delivery returns, not return while
// work still runs on the cancelled run context.
func TestDispatcherShutdownJoinsInFlightDeliveries(t *testing.T) {
	repo := &joinOnceRepo{ch: make(chan repository.PendingStatusUpdate, 1)}
	repo.ch <- repository.PendingStatusUpdate{
		ID: "ev-join-1", WorkflowInstanceID: "inst-join-1",
		WorkflowDefinitionID: "def-join-1", Transport: "http",
	}
	pub := &joinGatePublisher{started: make(chan struct{}, 1), release: make(chan struct{})}
	loader := func(ctx context.Context, definitionID string) (*model.StatusUpdateConfig, error) {
		return &model.StatusUpdateConfig{
			HTTP: &model.HTTPStatusUpdateConfig{URL: "http://example.test/hook", Method: "POST"},
		}, nil
	}
	d, err := statusupdate.NewDispatcher(context.Background(), repo, loader,
		map[string]statusupdate.Publisher{"http": pub}, "w-join",
		statusupdate.DispatcherOptions{
			PollInterval: 10 * time.Millisecond, Lease: time.Minute, BatchSize: 10, PoolSize: 2,
		})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	d.Run()

	select {
	case <-pub.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delivery to start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- d.Shutdown(ctx)
	}()

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned while a delivery was still in flight (err=%v); pool tasks are not joined", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: Shutdown blocks on the gated delivery.
	}

	close(pub.release)

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the in-flight delivery finished")
	}
}
