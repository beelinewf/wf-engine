package statusupdate

import (
	"context"
	"errors"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/transport"
)

// NATSPublisher publishes status events to the instance's fixed
// beelinewf.status.<instance> subject as Core messages carrying the logical
// event id as Nats-Msg-Id and IdempotencyKey header for cross-transport
// deduplication.
type NATSPublisher struct {
	client transport.NATSPublisher
}

// NewNATSPublisher builds the publisher.
func NewNATSPublisher(client transport.NATSPublisher) *NATSPublisher {
	return &NATSPublisher{client: client}
}

// Publish implements Publisher.
func (p *NATSPublisher) Publish(ctx context.Context, cfg *model.StatusUpdateConfig, ev repository.PendingStatusUpdate) error {
	if cfg.NATS == nil {
		return errors.New("status_update nats transport not configured")
	}
	headers := map[string]string{
		"NodeInstanceId": ev.WorkflowInstanceID,
		"IdempotencyKey": ev.LogicalID,
	}
	return p.client.PublishJSON(ctx, transport.NATSStatusSubject(ev.WorkflowInstanceID), ev.Payload, ev.LogicalID, headers)
}
