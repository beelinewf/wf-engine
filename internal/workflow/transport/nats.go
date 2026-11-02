package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSPublisher is the narrow publish surface shared by the output node
// executor and the status publishers. Messages are Core NATS publishes (not
// stream-persisted); messageID is stamped as Nats-Msg-Id and headers ride as
// NATS headers.
type NATSPublisher interface {
	PublishJSON(ctx context.Context, subject string, payload []byte, messageID string, headers map[string]string) error
}

// Subject name helpers for the fixed per-instance subjects.
const (
	natsInputPrefix  = "beelinewf.input."
	natsOutputPrefix = "beelinewf.output."
	natsStatusPrefix = "beelinewf.status."
)

// NATSInputSubject is the subject an input node of the instance consumes.
func NATSInputSubject(instanceID string) string { return natsInputPrefix + instanceID }

// NATSInputPattern matches every instance's input subject.
func NATSInputPattern() string { return natsInputPrefix + "*" }

// NATSInstanceFromInputSubject extracts the instance id from an input
// subject. It returns the empty string when the subject is not an input
// subject.
func NATSInstanceFromInputSubject(subject string) string {
	if len(subject) <= len(natsInputPrefix) || subject[:len(natsInputPrefix)] != natsInputPrefix {
		return ""
	}
	return subject[len(natsInputPrefix):]
}

// NATSOutputSubject is the subject an output node of the instance publishes
// to.
func NATSOutputSubject(instanceID string) string { return natsOutputPrefix + instanceID }

// NATSStatusSubject is the subject status-update events of the instance are
// published to.
func NATSStatusSubject(instanceID string) string { return natsStatusPrefix + instanceID }

// NATSConsumeResult tells the consumer how to settle a delivery.
type NATSConsumeResult int

const (
	// NATSConsumeAck acknowledges the delivery as processed.
	NATSConsumeAck NATSConsumeResult = iota
	// NATSConsumeNak redelivers the message after a backoff derived from
	// its delivery count.
	NATSConsumeNak
	// NATSConsumeTerm drops the delivery without redelivery.
	NATSConsumeTerm
)

// NATSMessage is one consumed input delivery.
type NATSMessage struct {
	Subject   string
	Body      []byte
	MessageID string
	Headers   map[string]string
}

// NATSPollerMessage is one consumed poller delivery with its subject.
type NATSPollerMessage struct {
	Subject string
	Body    []byte
	Headers map[string]string
}

// NATSConsumeFunc handles one input delivery and decides its settlement. An
// error from the handler is treated as a transient failure and redelivered
// after a backoff derived from the delivery count.
type NATSConsumeFunc func(ctx context.Context, msg NATSMessage) (NATSConsumeResult, error)

// NATSClient wraps a NATS connection with a JetStream input stream and its
// durable consumer. Input is durable (at-least-once through the stream);
// output, status, and poller paths use Core publish/subscribe.
type NATSClient struct {
	nc       *nats.Conn
	consumer jetstream.Consumer
}

// NewNATSClient connects to the server, ensures the input stream capturing
// NATSInputPattern and its durable consumer exist, and returns the client. A
// dial failure, a missing JetStream, or a provisioning failure returns an
// error so startup fails fast on a configured-but-unusable broker.
func NewNATSClient(ctx context.Context, url, stream, consumer string) (*NATSClient, error) {
	nc, err := nats.Connect(url, nats.Name("beelinewf"))
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", err)
	}
	closeOnErr := func(err error) (*NATSClient, error) {
		nc.Close()
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return closeOnErr(fmt.Errorf("nats: jetstream: %w", err))
	}
	if _, err := js.AccountInfo(ctx); err != nil {
		return closeOnErr(fmt.Errorf("nats: jetstream not available: %w", err))
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      stream,
		Subjects:  []string{NATSInputPattern()},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		return closeOnErr(fmt.Errorf("nats: ensure stream %s: %w", stream, err))
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       consumer,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
	})
	if err != nil {
		return closeOnErr(fmt.Errorf("nats: ensure consumer %s: %w", consumer, err))
	}
	return &NATSClient{nc: nc, consumer: cons}, nil
}

// PublishJSON implements NATSPublisher: a Core publish carrying messageID as
// Nats-Msg-Id plus the given headers, flushed before returning so a return
// means the server accepted the bytes.
func (c *NATSClient) PublishJSON(ctx context.Context, subject string, payload []byte, messageID string, headers map[string]string) error {
	msg := nats.NewMsg(subject)
	msg.Data = payload
	msg.Header.Set("Nats-Msg-Id", messageID)
	for k, v := range headers {
		msg.Header.Set(k, v)
	}
	if err := c.nc.PublishMsg(msg); err != nil {
		return fmt.Errorf("nats: publish to %s: %w", subject, err)
	}
	if err := c.nc.FlushWithContext(ctx); err != nil {
		return fmt.Errorf("nats: flush publish to %s: %w", subject, err)
	}
	return nil
}

// Consume delivers input messages to handler until ctx is cancelled, then
// returns nil. Deliveries settle through the durable consumer: an ack result
// acknowledges, a nak result redelivers after a backoff derived from the
// delivery count, and a term result drops the message. A handler error is
// treated as transient and redelivered the same way. Connection drops heal
// through the client's automatic reconnect, so Consume only returns on
// cancellation or a terminal consume error.
func (c *NATSClient) Consume(ctx context.Context, handler NATSConsumeFunc) error {
	cc, err := c.consumer.Consume(func(msg jetstream.Msg) {
		nm := NATSMessage{
			Subject: msg.Subject(),
			Body:    msg.Data(),
			Headers: natsHeadersToMap(msg.Headers()),
		}
		nm.MessageID = nm.Headers["Nats-Msg-Id"]
		res, err := handler(ctx, nm)
		if err != nil {
			res = NATSConsumeNak
		}
		switch res {
		case NATSConsumeAck:
			_ = msg.Ack()
		case NATSConsumeNak:
			_ = msg.NakWithDelay(requeueBackoff(int(natsDeliveries(msg)) + 1))
		case NATSConsumeTerm:
			_ = msg.Term()
		}
	}, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		logf("nats: consume error", "error", err)
	}))
	if err != nil {
		return fmt.Errorf("nats: consume: %w", err)
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

// natsHeadersToMap flattens NATS headers to first-value strings.
func natsHeadersToMap(h nats.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// natsDeliveries reports how many times the server has delivered msg;
// unknown counts read as zero (first delivery).
func natsDeliveries(msg jetstream.Msg) uint64 {
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		return 0
	}
	if meta.NumDelivered == 0 {
		return 0
	}
	return meta.NumDelivered - 1
}

// Subscribe invokes handler for every Core message on subject until ctx is
// cancelled, then returns nil. Handler errors are logged and consumption
// continues rather than tearing the subscription down.
func (c *NATSClient) Subscribe(ctx context.Context, subject string, handler func(ctx context.Context, subject string, payload []byte, headers map[string]string) error) error {
	sub, err := c.nc.Subscribe(subject, func(m *nats.Msg) {
		if err := handler(ctx, m.Subject, m.Data, natsHeadersToMap(m.Header)); err != nil {
			logf("nats: handler failed", "subject", m.Subject, "error", err)
		}
	})
	if err != nil {
		return fmt.Errorf("nats: subscribe %s: %w", subject, err)
	}
	<-ctx.Done()
	_ = sub.Unsubscribe()
	return nil
}

// Close drains pending outbound messages and shuts the connection down.
func (c *NATSClient) Close() error {
	return c.nc.Drain()
}
