package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

func condGraph() *workflowGraph {
	return buildGraph(&model.WorkflowContent{
		StartNodeID: "cond",
		Keys:        map[string]string{"a": "target"},
		Nodes: []*model.NodeContent{
			{ID: "cond", Type: model.NodeTypeConditions},
			{ID: "target", Type: model.NodeTypeOutput},
		},
	})
}

func TestResolveNextConditions(t *testing.T) {
	g := condGraph()
	ctxMap := map[string]any{"a": 1}
	nc := &model.NodeContent{ID: "cond", Type: model.NodeTypeConditions}

	t.Run("matched key resolves target", func(t *testing.T) {
		res := &executor.Result{Output: &executor.ConditionResult{Matched: true, Index: 2, Key: "a"}}
		next, outCtx, hookOutput, err := resolveNext(g, nc, ctxMap, res)
		if err != nil {
			t.Fatalf("resolveNext() error = %v", err)
		}
		if next != "target" {
			t.Fatalf("resolveNext() next = %q, want target", next)
		}
		if len(outCtx) != len(ctxMap) || outCtx["a"] != 1 {
			t.Fatalf("resolveNext() outCtx = %v, want passthrough ctxMap", outCtx)
		}
		hook, ok := hookOutput.(map[string]any)
		if !ok || hook["matched"] != true || hook["index"] != 2 || hook["key"] != "a" {
			t.Fatalf("resolveNext() hookOutput = %v, want matched/index/key", hookOutput)
		}
	})

	t.Run("empty key ends the flow", func(t *testing.T) {
		res := &executor.Result{Output: &executor.ConditionResult{Matched: true, Key: ""}}
		next, _, _, err := resolveNext(g, nc, ctxMap, res)
		if err != nil {
			t.Fatalf("resolveNext() error = %v", err)
		}
		if next != "" {
			t.Fatalf("resolveNext() next = %q, want empty", next)
		}
	})

	t.Run("unmatched fails", func(t *testing.T) {
		res := &executor.Result{Output: &executor.ConditionResult{Matched: false}}
		_, _, _, err := resolveNext(g, nc, ctxMap, res)
		if err == nil || !strings.Contains(err.Error(), "no condition matched") {
			t.Fatalf("resolveNext() error = %v, want no-condition-matched", err)
		}
	})

	t.Run("invalid result type fails", func(t *testing.T) {
		res := &executor.Result{Output: "not-a-condition-result"}
		_, _, _, err := resolveNext(g, nc, ctxMap, res)
		if err == nil || !strings.Contains(err.Error(), "invalid result") {
			t.Fatalf("resolveNext() error = %v, want invalid-result", err)
		}
	})

	t.Run("unknown key fails", func(t *testing.T) {
		res := &executor.Result{Output: &executor.ConditionResult{Matched: true, Key: "zzz"}}
		_, _, _, err := resolveNext(g, nc, ctxMap, res)
		if err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Fatalf("resolveNext() error = %v, want key-not-defined", err)
		}
	})
}

func TestResolveNextOutputMerge(t *testing.T) {
	g := condGraph()
	for _, tc := range []struct {
		name     string
		outProp  string
		resCtx   map[string]any
		ctxMap   map[string]any
		wantKey  string
		wantInto string
	}{
		{name: "explicit property merges into ctxMap", outProp: "res", ctxMap: map[string]any{}, wantKey: "res", wantInto: "ctx"},
		{name: "empty property defaults to node id", outProp: "", ctxMap: map[string]any{}, wantKey: "n1", wantInto: "ctx"},
		{name: "blank property defaults to node id", outProp: "  ", ctxMap: map[string]any{}, wantKey: "n1", wantInto: "ctx"},
		{name: "result context wins over ctxMap", outProp: "res", resCtx: map[string]any{}, ctxMap: map[string]any{}, wantKey: "res", wantInto: "res"},
		{name: "nil maps allocate", outProp: "res", wantKey: "res", wantInto: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc := &model.NodeContent{ID: "n1", Type: model.NodeTypeScript, NextNode: "n2", OutputProperty: tc.outProp}
			res := &executor.Result{Output: "ok", Context: tc.resCtx}
			next, outCtx, hookOutput, err := resolveNext(g, nc, tc.ctxMap, res)
			if err != nil {
				t.Fatalf("resolveNext() error = %v", err)
			}
			if next != "n2" {
				t.Fatalf("resolveNext() next = %q, want n2", next)
			}
			if hookOutput != "ok" {
				t.Fatalf("resolveNext() hookOutput = %v, want ok", hookOutput)
			}
			if outCtx[tc.wantKey] != "ok" {
				t.Fatalf("resolveNext() outCtx[%q] = %v, want ok", tc.wantKey, outCtx[tc.wantKey])
			}
			switch tc.wantInto {
			case "ctx":
				if tc.ctxMap[tc.wantKey] != "ok" {
					t.Fatalf("resolveNext() did not merge into ctxMap: %v", tc.ctxMap)
				}
			case "res":
				if tc.resCtx[tc.wantKey] != "ok" {
					t.Fatalf("resolveNext() did not merge into res.Context: %v", tc.resCtx)
				}
				if _, ok := tc.ctxMap[tc.wantKey]; ok {
					t.Fatalf("resolveNext() leaked into ctxMap: %v", tc.ctxMap)
				}
			case "new":
				if outCtx == nil {
					t.Fatalf("resolveNext() outCtx is nil, want allocated map")
				}
			}
		})
	}
}

func TestFinishAttempt(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	t.Run("full mode snapshots context", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		start := map[string]any{"a": 1}
		end := map[string]any{"a": 1, "b": 2}
		if err := finishAttempt(attempt, "ok", start, end, "full", now); err != nil {
			t.Fatalf("finishAttempt() error = %v", err)
		}
		if attempt.Status != model.NodeFinished {
			t.Fatalf("finishAttempt() status = %v, want finished", attempt.Status)
		}
		if string(attempt.Output) != `"ok"` {
			t.Fatalf("finishAttempt() output = %s, want ok", attempt.Output)
		}
		var got map[string]any
		if err := json.Unmarshal(attempt.ContextAfter, &got); err != nil || got["b"] != float64(2) {
			t.Fatalf("finishAttempt() contextAfter = %s, want full snapshot", attempt.ContextAfter)
		}
		if attempt.FinishedAt == nil || !attempt.FinishedAt.Equal(now) || !attempt.UpdatedAt.Equal(now) {
			t.Fatalf("finishAttempt() timestamps = %v/%v, want %v", attempt.FinishedAt, attempt.UpdatedAt, now)
		}
	})

	t.Run("lean mode stores diff", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		start := map[string]any{"a": 1}
		end := map[string]any{"a": 1, "b": 2}
		if err := finishAttempt(attempt, "ok", start, end, "lean", now); err != nil {
			t.Fatalf("finishAttempt() error = %v", err)
		}
		if string(attempt.ContextBefore) != "null" {
			t.Fatalf("finishAttempt() contextBefore = %s, want null", attempt.ContextBefore)
		}
		if !strings.Contains(string(attempt.ContextAfter), `"b"`) {
			t.Fatalf("finishAttempt() contextAfter = %s, want diff with b", attempt.ContextAfter)
		}
	})

	t.Run("unserializable output fails", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		if err := finishAttempt(attempt, func() {}, map[string]any{}, map[string]any{}, "full", now); err == nil {
			t.Fatalf("finishAttempt() error = nil, want marshal error")
		}
	})

	t.Run("unserializable context fails in full mode", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		end := map[string]any{"bad": func() {}}
		if err := finishAttempt(attempt, "ok", map[string]any{}, end, "full", now); err == nil {
			t.Fatalf("finishAttempt() error = nil, want marshal error")
		}
	})
}

func TestPersistContextAfterPolicies(t *testing.T) {
	t.Run("fail policy returns marshal error", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		end := map[string]any{"bad": func() {}}
		if err := persistContextAfter(attempt, map[string]any{"a": 1}, end, "full", failOnUnserializable); err == nil {
			t.Fatal("persistContextAfter(fail) error = nil, want marshal error")
		}
	})
	t.Run("fallback policy uses start context", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		start := map[string]any{"a": 1}
		end := map[string]any{"bad": func() {}}
		if err := persistContextAfter(attempt, start, end, "full", fallbackToStart); err != nil {
			t.Fatalf("persistContextAfter(fallback) error = %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(attempt.ContextAfter, &got); err != nil || got["a"] != float64(1) {
			t.Errorf("ContextAfter = %s, want start context", attempt.ContextAfter)
		}
	})
	t.Run("fallback policy fails when start is unserializable", func(t *testing.T) {
		attempt := &model.NodeInstance{}
		bad := map[string]any{"bad": func() {}}
		if err := persistContextAfter(attempt, bad, bad, "full", fallbackToStart); err == nil {
			t.Fatal("persistContextAfter(fallback, bad start) error = nil, want marshal error")
		}
	})
	t.Run("lean mode stores diff under both policies", func(t *testing.T) {
		for _, p := range []contextAfterPolicy{failOnUnserializable, fallbackToStart} {
			attempt := &model.NodeInstance{}
			if err := persistContextAfter(attempt, map[string]any{"a": 1}, map[string]any{"a": 1, "b": 2}, "lean", p); err != nil {
				t.Fatalf("persistContextAfter(lean) error = %v", err)
			}
			if string(attempt.ContextBefore) != "null" {
				t.Errorf("ContextBefore = %s, want null", attempt.ContextBefore)
			}
		}
	})
}
