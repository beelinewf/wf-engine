package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

const outInstanceID = "11111111-1111-7111-8111-111111111111"

// fakeRedisPublisher records redis publishes for assertions.
type fakeRedisPublisher struct {
	calls   int
	channel string
	payload []byte
	err     error
}

func (f *fakeRedisPublisher) PublishJSON(_ context.Context, channel string, payload []byte) error {
	f.calls++
	f.channel = channel
	f.payload = payload
	return f.err
}

// fakeRabbitPublisher records rabbit publishes for assertions.
type fakeRabbitPublisher struct {
	calls     int
	queue     string
	payload   []byte
	messageID string
	headers   map[string]string
	err       error
}

func (f *fakeRabbitPublisher) PublishJSON(_ context.Context, queue string, payload []byte, messageID string, headers map[string]string) error {
	f.calls++
	f.queue = queue
	f.payload = payload
	f.messageID = messageID
	f.headers = headers
	return f.err
}

// fakeNATSPublisher records nats publishes for assertions.
type fakeNATSPublisher struct {
	calls     int
	subject   string
	payload   []byte
	messageID string
	headers   map[string]string
	err       error
}

func (f *fakeNATSPublisher) PublishJSON(_ context.Context, subject string, payload []byte, messageID string, headers map[string]string) error {
	f.calls++
	f.subject = subject
	f.payload = payload
	f.messageID = messageID
	f.headers = headers
	return f.err
}

func outNode(channel, contextPath string) *model.NodeContent {
	return &model.NodeContent{
		Type:           model.NodeTypeOutput,
		ID:             "11111111-1111-7111-8111-111111111111",
		Channel:        channel,
		ContextPath:    contextPath,
		OutputProperty: "out",
	}
}

func TestOutputExecutorPublishesToRedis(t *testing.T) {
	redisPub := &fakeRedisPublisher{}
	rabbitPub := &fakeRabbitPublisher{}
	ex := executor.NewBrokerOutputPublisher(redisPub, rabbitPub, "beelinewf.output", &fakeNATSPublisher{})
	// The registry wires the publisher into the executor.
	reg := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{Output: ex})
	got := reg[model.NodeTypeOutput]
	ctx := context.Background()
	res, err := got.Execute(ctx, executor.Request{
		Node:           outNode("redis", "result.payload"),
		Context:        map[string]any{"result": map[string]any{"payload": map[string]any{"ok": true, "n": 1}}},
		IdempotencyKey: outInstanceID + ":occ-1",
		InstanceID:     outInstanceID,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if redisPub.calls != 1 || redisPub.channel != "workflow:output:"+outInstanceID {
		t.Errorf("redis publish = %+v, want channel workflow:output:%s", redisPub, outInstanceID)
	}
	if rabbitPub.calls != 0 {
		t.Errorf("rabbit publish calls = %d, want 0", rabbitPub.calls)
	}
	var sent map[string]any
	if err := json.Unmarshal(redisPub.payload, &sent); err != nil || sent["ok"] != true || sent["n"] != float64(1) {
		t.Errorf("payload = %s, want exact JSON of resolved context value", redisPub.payload)
	}
	receipt, ok := res.Output.(*executor.PublishReceipt)
	if !ok {
		t.Fatalf("output = %T, want *PublishReceipt", res.Output)
	}
	if receipt.Channel != "redis" || receipt.Destination != "workflow:output:"+outInstanceID || receipt.MessageID != outInstanceID+":occ-1" {
		t.Errorf("receipt = %+v", receipt)
	}
}

func TestOutputExecutorPublishesToRabbit(t *testing.T) {
	redisPub := &fakeRedisPublisher{}
	rabbitPub := &fakeRabbitPublisher{}
	ex := executor.NewBrokerOutputPublisher(redisPub, rabbitPub, "beelinewf.output", &fakeNATSPublisher{})
	reg := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{Output: ex})
	got := reg[model.NodeTypeOutput]
	_, err := got.Execute(context.Background(), executor.Request{
		Node:           outNode("rabbitmq", "payload"),
		Context:        map[string]any{"payload": map[string]any{"k": "v"}},
		IdempotencyKey: outInstanceID + ":occ-1",
		InstanceID:     outInstanceID,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if rabbitPub.calls != 1 || rabbitPub.queue != "beelinewf.output" {
		t.Errorf("rabbit publish = %+v, want queue beelinewf.output", rabbitPub)
	}
	if rabbitPub.messageID != outInstanceID+":occ-1" {
		t.Errorf("message_id = %q, want stable execution id", rabbitPub.messageID)
	}
	if rabbitPub.headers["NodeInstanceId"] != outInstanceID || rabbitPub.headers["IdempotencyKey"] != outInstanceID+":occ-1" {
		t.Errorf("headers = %+v", rabbitPub.headers)
	}
	if redisPub.calls != 0 {
		t.Errorf("redis publish calls = %d, want 0", redisPub.calls)
	}
}

func TestOutputExecutorFailsOnMissingContextPath(t *testing.T) {
	reg := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{
		Output: executor.NewBrokerOutputPublisher(&fakeRedisPublisher{}, &fakeRabbitPublisher{}, "q", &fakeNATSPublisher{}),
	})
	exe := reg[model.NodeTypeOutput]
	_, err := exe.Execute(context.Background(), executor.Request{
		Node:       outNode("redis", "missing.path"),
		Context:    map[string]any{},
		InstanceID: outInstanceID,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want error for missing context path")
	}
}

func TestOutputExecutorFailsOnPublishError(t *testing.T) {
	redisPub := &fakeRedisPublisher{err: errors.New("broker down")}
	reg := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{
		Output: executor.NewBrokerOutputPublisher(redisPub, &fakeRabbitPublisher{}, "q", &fakeNATSPublisher{}),
	})
	exe := reg[model.NodeTypeOutput]
	_, err := exe.Execute(context.Background(), executor.Request{
		Node:       outNode("redis", "x"),
		Context:    map[string]any{"x": 1},
		InstanceID: outInstanceID,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want publish error")
	}
}

func TestOutputExecutorFailsWhenBrokerDisabled(t *testing.T) {
	// No publisher wired means the broker is not configured; the node fails
	// like any other broker-disabled operation.
	reg := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{})
	exe := reg[model.NodeTypeOutput]
	_, err := exe.Execute(context.Background(), executor.Request{
		Node:       outNode("redis", "x"),
		Context:    map[string]any{"x": 1},
		InstanceID: outInstanceID,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want error for unconfigured publisher")
	}
}

func TestOutputExecutorPublishesToNATS(t *testing.T) {
	natsPub := &fakeNATSPublisher{}
	ex := executor.NewBrokerOutputPublisher(&fakeRedisPublisher{}, &fakeRabbitPublisher{}, "beelinewf.output", natsPub)
	reg := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{Output: ex})
	got := reg[model.NodeTypeOutput]
	res, err := got.Execute(context.Background(), executor.Request{
		Node:           outNode("nats", "payload"),
		Context:        map[string]any{"payload": map[string]any{"k": "v"}},
		IdempotencyKey: outInstanceID + ":occ-1",
		InstanceID:     outInstanceID,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if natsPub.calls != 1 || natsPub.subject != "beelinewf.output."+outInstanceID {
		t.Errorf("nats publish = %+v, want subject beelinewf.output.%s", natsPub, outInstanceID)
	}
	if natsPub.messageID != outInstanceID+":occ-1" {
		t.Errorf("message_id = %q, want stable execution id", natsPub.messageID)
	}
	if natsPub.headers["NodeInstanceId"] != outInstanceID || natsPub.headers["IdempotencyKey"] != outInstanceID+":occ-1" {
		t.Errorf("headers = %+v", natsPub.headers)
	}
	receipt, ok := res.Output.(*executor.PublishReceipt)
	if !ok {
		t.Fatalf("output = %T, want *PublishReceipt", res.Output)
	}
	if receipt.Channel != "nats" || receipt.Destination != "beelinewf.output."+outInstanceID {
		t.Errorf("receipt = %+v", receipt)
	}
}

func TestBrokerOutputPublisherFailsWhenNATSDisabled(t *testing.T) {
	pub := executor.NewBrokerOutputPublisher(&fakeRedisPublisher{}, &fakeRabbitPublisher{}, "q", nil)
	_, err := pub.Publish(context.Background(), "nats", outInstanceID, []byte(`{}`), "m")
	if err == nil {
		t.Fatal("Publish() error = nil, want error for unconfigured nats publisher")
	}
}

func TestBrokerOutputPublisherRejectsUnknownChannel(t *testing.T) {
	pub := executor.NewBrokerOutputPublisher(&fakeRedisPublisher{}, &fakeRabbitPublisher{}, "q", &fakeNATSPublisher{})
	_, err := pub.Publish(context.Background(), "kafka", outInstanceID, []byte(`{}`), "m")
	if err == nil {
		t.Fatal("Publish() error = nil, want error for unknown channel")
	}
}
