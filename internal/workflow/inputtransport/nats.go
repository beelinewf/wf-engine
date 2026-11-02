package inputtransport

import (
	"context"
	"fmt"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/internal/workflow/transport"
)

// NATSConsumer is the consume surface the NATS input consumer needs.
// *transport.NATSClient implements it.
type NATSConsumer interface {
	Consume(ctx context.Context, handler transport.NATSConsumeFunc) error
}

// NATSInput consumes the durable JetStream consumer over the input stream
// and delivers each message to its instance. Deliveries settle manually:
// permanent outcomes are acknowledged or terminated without redelivery, and
// transient failures are redelivered after a backoff derived from the
// delivery count.
type NATSInput struct {
	cons NATSConsumer
	svc  service.InstanceService
}

// NewNATSInput builds the consumer.
func NewNATSInput(cons NATSConsumer, svc service.InstanceService) *NATSInput {
	return &NATSInput{cons: cons, svc: svc}
}

// Run consumes until ctx is cancelled.
func (r *NATSInput) Run(ctx context.Context) error {
	return r.cons.Consume(ctx, r.handle)
}

func (r *NATSInput) handle(ctx context.Context, msg transport.NATSMessage) (transport.NATSConsumeResult, error) {
	instanceID := transport.NATSInstanceFromInputSubject(msg.Subject)
	if instanceID == "" {
		instanceID = msg.Headers["NodeInstanceId"]
	}
	idempotencyKey := msg.Headers["IdempotencyKey"]
	if idempotencyKey == "" {
		// Nats-Msg-Id is the idempotency fallback.
		idempotencyKey = msg.MessageID
	}
	if instanceID == "" || idempotencyKey == "" {
		// Missing routing metadata can never succeed: terminate without
		// redelivery so the malformed message does not loop.
		return transport.NATSConsumeTerm, nil
	}
	_, err := r.svc.DeliverInput(ctx, service.DeliverInput{
		InstanceID:     instanceID,
		IdempotencyKey: idempotencyKey,
		Payload:        msg.Body,
		Source:         model.InputChannelNATS,
	})
	if err != nil {
		if isPermanentInputError(err) {
			return transport.NATSConsumeTerm, nil
		}
		return transport.NATSConsumeNak, fmt.Errorf("inputtransport: deliver nats input: %w", err)
	}
	return transport.NATSConsumeAck, nil
}
