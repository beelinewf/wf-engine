package executor_test

import (
	"context"
	"testing"

	"github.com/simpwf/workflow-engine/internal/workflow/executor"
	"github.com/simpwf/workflow-engine/internal/workflow/model"
)

func TestDelayExecutorReturnsNull(t *testing.T) {
	ex, ok := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{})[model.NodeTypeDelay]
	if !ok {
		t.Fatal("NewExecutors() has no delay executor")
	}
	res, err := ex.Execute(context.Background(), executor.Request{
		Node:    &model.NodeContent{Type: model.NodeTypeDelay},
		Context: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Output != nil {
		t.Errorf("output = %v, want nil", res.Output)
	}
}
