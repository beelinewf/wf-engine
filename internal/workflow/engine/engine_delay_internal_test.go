package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
)

const (
	delayInstanceID = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	delayNodeA      = "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb"
	delayNodeB      = "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
)

var delayNodeLimits = model.NodeLimits{
	DefaultTimeout:   30 * time.Second,
	MaxTimeout:       5 * time.Minute,
	ConditionTimeout: 5 * time.Second,
}

// delayFakeInstances is a scriptable InstanceRepository for start_delay gate
// tests. Checkpoints apply to the canned row so a test can walk an instance
// through park -> claim -> execute without a database.
type delayFakeInstances struct {
	repository.InstanceRepository
	inst        model.WorkflowInstance
	checkpoints []repository.Checkpoint
	events      []model.WorkflowInstanceEvent
	inserted    []model.NodeInstance
}

func (f *delayFakeInstances) GetByID(_ context.Context, _ string) (*model.WorkflowInstance, error) {
	w := f.inst
	return &w, nil
}

func (f *delayFakeInstances) GetRunningNodeInstance(_ context.Context, _ string) (*model.NodeInstance, error) {
	return nil, repository.ErrNodeInstanceNotFound
}

func (f *delayFakeInstances) GetNodeInstanceByNode(_ context.Context, _, _ string) (*model.NodeInstance, error) {
	return nil, repository.ErrNodeInstanceNotFound
}

func (f *delayFakeInstances) Checkpoint(_ context.Context, c repository.Checkpoint) error {
	f.checkpoints = append(f.checkpoints, c)
	f.inst.Status = c.Status
	f.inst.WaitingReason = c.WaitingReason
	f.inst.RunAt = c.RunAt
	frameRaw, _ := c.Frame.JSON()
	f.inst.Frame = frameRaw
	f.inst.Revision++
	return nil
}

func (f *delayFakeInstances) AppendEvent(_ context.Context, e model.WorkflowInstanceEvent) error {
	f.events = append(f.events, e)
	return nil
}

func (f *delayFakeInstances) InsertNodeInstance(_ context.Context, n model.NodeInstance) error {
	f.inserted = append(f.inserted, n)
	return nil
}

func (f *delayFakeInstances) UpdateNodeInstance(_ context.Context, _ model.NodeInstance) error {
	return nil
}

func (f *delayFakeInstances) eventTypes() map[string]int {
	out := map[string]int{}
	for _, e := range f.events {
		out[e.Type]++
	}
	return out
}

// delayLoader serves a fixed parsed workflow content.
func delayLoader(t *testing.T, content string) WorkflowLoader {
	t.Helper()
	wc, err := model.ParseWorkflowContent(json.RawMessage(content), delayNodeLimits)
	if err != nil {
		t.Fatalf("ParseWorkflowContent() error = %v", err)
	}
	return func(_ context.Context, _ string) (*model.WorkflowContent, error) { return wc, nil }
}

// delayEngine builds an engine over fakes with a controllable clock.
func delayEngine(fake *delayFakeInstances, loader WorkflowLoader, now time.Time) *Engine {
	e := NewEngine(fake, nil,
		executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{}),
		executor.NewHookRunner(nil),
		model.DefaultLimits(), loader, "test", model.LeanOptions{})
	e.now = func() time.Time { return now }
	return e
}

func delayInstanceAt(nodeID string) model.WorkflowInstance {
	frameRaw, _ := model.NewFrame(nodeID).JSON()
	return model.WorkflowInstance{
		ID:            delayInstanceID,
		Status:        model.WorkflowRunning,
		WaitingReason: model.WaitingReasonRunnable,
		Frame:         frameRaw,
		Context:       json.RawMessage(`{}`),
		Counters:      json.RawMessage(`{}`),
		Revision:      7,
		LeasedBy:      "worker-1",
	}
}

// claimAsDue simulates ClaimNext picking the canned row back up: the lease
// is fenced to the worker and the status returns to running while the
// waiting reason and run_at survive, exactly as the repository leaves them.
func claimAsDue(fake *delayFakeInstances) {
	fake.inst.Status = model.WorkflowRunning
	fake.inst.LeasedBy = "worker-1"
	fake.inst.Revision++
}

func TestDelayGateParksBeforeExecution(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","start_delay":"15s"}
	]}`
	fake := &delayFakeInstances{inst: delayInstanceAt(delayNodeA)}
	e := delayEngine(fake, delayLoader(t, content), t0)

	if err := e.Process(context.Background(), fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1 park", len(fake.checkpoints))
	}
	cp := fake.checkpoints[0]
	if cp.Status != model.WorkflowWaiting || cp.WaitingReason != model.WaitingReasonDelay {
		t.Errorf("park = %s/%q, want waiting/delay", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt == nil || !cp.RunAt.Equal(t0.Add(15*time.Second)) {
		t.Errorf("run_at = %v, want t0+15s", cp.RunAt)
	}
	if len(fake.inserted) != 0 {
		t.Errorf("attempts inserted = %d, want none before the deadline", len(fake.inserted))
	}
	types := fake.eventTypes()
	if types["node_delayed"] != 1 {
		t.Errorf("node_delayed events = %d, want 1", types["node_delayed"])
	}
	if types["node_started"] != 0 {
		t.Error("node_started fired during park, want it only on real execution")
	}
	var data map[string]any
	if err := json.Unmarshal(fake.events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["node_id"] != delayNodeA {
		t.Errorf("node_delayed node_id = %v, want %s", data["node_id"], delayNodeA)
	}
}

func TestDelayGateStaleClaimReturns(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","start_delay":"15s"}
	]}`
	inst := delayInstanceAt(delayNodeA)
	inst.WaitingReason = model.WaitingReasonDelay
	future := t0.Add(time.Minute)
	inst.RunAt = &future
	fake := &delayFakeInstances{inst: inst}
	e := delayEngine(fake, delayLoader(t, content), t0)

	if err := e.Process(context.Background(), fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 0 {
		t.Errorf("checkpoints = %d, want none for a pre-deadline claim", len(fake.checkpoints))
	}
	if len(fake.inserted) != 0 {
		t.Errorf("attempts inserted = %d, want none for a pre-deadline claim", len(fake.inserted))
	}
}

func TestDelayGateExecutesAfterDeadline(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","start_delay":"15s"}
	]}`
	inst := delayInstanceAt(delayNodeA)
	inst.WaitingReason = model.WaitingReasonDelay
	past := t0.Add(-time.Second)
	inst.RunAt = &past
	fake := &delayFakeInstances{inst: inst}
	e := delayEngine(fake, delayLoader(t, content), t0)

	if err := e.Process(context.Background(), fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.inserted) != 1 {
		t.Fatalf("attempts inserted = %d, want 1 execution", len(fake.inserted))
	}
	types := fake.eventTypes()
	if types["node_started"] != 1 {
		t.Errorf("node_started events = %d, want 1", types["node_started"])
	}
	if len(fake.checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(fake.checkpoints))
	}
	cp := fake.checkpoints[0]
	if cp.Status != model.WorkflowFinished {
		t.Errorf("status = %s, want finished", cp.Status)
	}
	if cp.RunAt != nil {
		t.Errorf("run_at = %v, want cleared after execution", cp.RunAt)
	}
}

func TestDelayGateHonorsPauseRequested(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","start_delay":"15s"}
	]}`
	inst := delayInstanceAt(delayNodeA)
	inst.PauseRequested = true
	fake := &delayFakeInstances{inst: inst}
	e := delayEngine(fake, delayLoader(t, content), t0)

	if err := e.Process(context.Background(), fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1 park", len(fake.checkpoints))
	}
	cp := fake.checkpoints[0]
	if cp.Status != model.WorkflowPaused || cp.WaitingReason != model.WaitingReasonDelay {
		t.Errorf("park = %s/%q, want paused/delay", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt == nil || !cp.RunAt.Equal(t0.Add(15*time.Second)) {
		t.Errorf("run_at = %v, want t0+15s preserved across the pause", cp.RunAt)
	}
}

func TestDelayGateReparksLoop(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	// B carries the delay and loops back through A.
	content := `{"start_node_id":"` + delayNodeB + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","next_node":"` + delayNodeB + `"},
		{"id":"` + delayNodeB + `","type":"script","script":"return 2;","next_node":"` + delayNodeA + `","start_delay":"15s"}
	]}`
	fake := &delayFakeInstances{inst: delayInstanceAt(delayNodeB)}
	e := delayEngine(fake, delayLoader(t, content), t0)
	ctx := context.Background()

	// First arrival at B parks.
	if err := e.Process(ctx, fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 1 || fake.checkpoints[0].WaitingReason != model.WaitingReasonDelay {
		t.Fatalf("first arrival checkpoints = %+v, want one delay park", fake.checkpoints)
	}

	// Due claim executes B and advances to A, clearing run_at.
	past := t0.Add(-time.Second)
	fake.inst.RunAt = &past
	claimAsDue(fake)
	if err := e.Process(ctx, fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 2 || fake.checkpoints[1].RunAt != nil {
		t.Fatalf("execute checkpoints = %+v, want run_at cleared", fake.checkpoints)
	}

	// A has no delay and executes immediately without parking.
	claimAsDue(fake)
	if err := e.Process(ctx, fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 3 || fake.checkpoints[2].WaitingReason == model.WaitingReasonDelay {
		t.Fatalf("checkpoints = %+v, want A to execute without a delay park", fake.checkpoints)
	}

	// The loop returns to B, which parks again.
	claimAsDue(fake)
	if err := e.Process(ctx, fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 4 {
		t.Fatalf("checkpoints = %d, want 4", len(fake.checkpoints))
	}
	cp := fake.checkpoints[3]
	if cp.Status != model.WorkflowWaiting || cp.WaitingReason != model.WaitingReasonDelay {
		t.Errorf("loop re-arrival = %s/%q, want waiting/delay", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt == nil {
		t.Error("loop re-arrival run_at = nil, want a fresh deadline")
	}
}

const (
	delayBranchID    = "dddddddd-dddd-7ddd-8ddd-dddddddddddd"
	delayExecutionID = "eeeeeeee-eeee-7eee-8eee-eeeeeeeeeee1"
	delayNodeEnd     = "eeeeeeee-eeee-7eee-8eee-eeeeeeeeeee2"
)

// delayFakeParallel is a scriptable ParallelRepository for branch delay-gate
// tests. Branch checkpoints apply to the canned row.
type delayFakeParallel struct {
	repository.ParallelRepository
	branch      model.ParallelBranch
	execution   model.ParallelExecution
	checkpoints []repository.BranchCheckpoint
}

func (f *delayFakeParallel) GetBranch(_ context.Context, _ string) (*model.ParallelBranch, error) {
	b := f.branch
	return &b, nil
}

func (f *delayFakeParallel) GetExecution(_ context.Context, _ string) (*model.ParallelExecution, error) {
	ex := f.execution
	return &ex, nil
}

func (f *delayFakeParallel) CheckpointBranch(_ context.Context, c repository.BranchCheckpoint) error {
	f.checkpoints = append(f.checkpoints, c)
	f.branch.Status = c.Status
	f.branch.WaitingReason = c.WaitingReason
	f.branch.RunAt = c.RunAt
	frameRaw, _ := c.Frame.JSON()
	f.branch.Frame = frameRaw
	f.branch.Revision++
	return nil
}

func (f *delayFakeInstances) GetRunningBranchNodeInstance(_ context.Context, _ string) (*model.NodeInstance, error) {
	return nil, repository.ErrNodeInstanceNotFound
}

func (f *delayFakeInstances) GetBranchNodeInstanceByNode(_ context.Context, _, _ string) (*model.NodeInstance, error) {
	return nil, repository.ErrNodeInstanceNotFound
}

// delayBranchEngine builds an engine over branch fakes with a controllable
// clock.
func delayBranchEngine(inst *delayFakeInstances, par *delayFakeParallel, loader WorkflowLoader, now time.Time) *Engine {
	e := NewEngine(inst, par,
		executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{}),
		executor.NewHookRunner(nil),
		model.DefaultLimits(), loader, "test", model.LeanOptions{})
	e.now = func() time.Time { return now }
	return e
}

func delayBranchAt(nodeID string) model.ParallelBranch {
	frameRaw, _ := model.NewFrame(nodeID).JSON()
	return model.ParallelBranch{
		ID:                  delayBranchID,
		ParallelExecutionID: delayExecutionID,
		InstanceID:          delayInstanceID,
		Name:                "a",
		Status:              model.ParallelBranchRunning,
		WaitingReason:       model.WaitingReasonRunnable,
		Frame:               frameRaw,
		Context:             json.RawMessage(`{}`),
		Counters:            json.RawMessage(`{}`),
		Revision:            3,
		LeasedBy:            "worker-1",
	}
}

func delayBranchExecution() model.ParallelExecution {
	return model.ParallelExecution{
		ID:         delayExecutionID,
		InstanceID: delayInstanceID,
		Depth:      1,
		EndNodeID:  delayNodeEnd,
		Status:     model.ParallelWaitingForBranches,
	}
}

func TestDelayGateBranchParksBeforeExecution(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","next_node":"` + delayNodeB + `","start_delay":"15s"},
		{"id":"` + delayNodeB + `","type":"script","script":"return 2;"}
	]}`
	inst := &delayFakeInstances{inst: delayInstanceAt(delayNodeA)}
	par := &delayFakeParallel{branch: delayBranchAt(delayNodeA), execution: delayBranchExecution()}
	e := delayBranchEngine(inst, par, delayLoader(t, content), t0)

	if err := e.ProcessBranch(context.Background(), par.branch); err != nil {
		t.Fatalf("ProcessBranch() error = %v", err)
	}
	if len(par.checkpoints) != 1 {
		t.Fatalf("branch checkpoints = %d, want 1 park", len(par.checkpoints))
	}
	cp := par.checkpoints[0]
	if cp.Status != model.ParallelBranchWaiting || cp.WaitingReason != model.WaitingReasonDelay {
		t.Errorf("park = %s/%q, want waiting/delay", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt == nil || !cp.RunAt.Equal(t0.Add(15*time.Second)) {
		t.Errorf("run_at = %v, want t0+15s", cp.RunAt)
	}
	if len(inst.inserted) != 0 {
		t.Errorf("attempts inserted = %d, want none before the deadline", len(inst.inserted))
	}
	types := inst.eventTypes()
	if types["node_delayed"] != 1 {
		t.Errorf("node_delayed events = %d, want 1", types["node_delayed"])
	}
	if types["node_started"] != 0 {
		t.Error("node_started fired during park, want it only on real execution")
	}
}

func TestDelayNodeParksBeforeExecution(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"delay","delay":"15s"}
	]}`
	fake := &delayFakeInstances{inst: delayInstanceAt(delayNodeA)}
	e := delayEngine(fake, delayLoader(t, content), t0)

	if err := e.Process(context.Background(), fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1 park", len(fake.checkpoints))
	}
	cp := fake.checkpoints[0]
	if cp.Status != model.WorkflowWaiting || cp.WaitingReason != model.WaitingReasonDelay {
		t.Errorf("park = %s/%q, want waiting/delay", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt == nil || !cp.RunAt.Equal(t0.Add(15*time.Second)) {
		t.Errorf("run_at = %v, want t0+15s", cp.RunAt)
	}
	if len(fake.inserted) != 0 {
		t.Errorf("attempts inserted = %d, want none before the deadline", len(fake.inserted))
	}
	types := fake.eventTypes()
	if types["node_delayed"] != 1 {
		t.Errorf("node_delayed events = %d, want 1", types["node_delayed"])
	}
	if types["node_started"] != 0 {
		t.Error("node_started fired during park, want it only on real execution")
	}
}

func TestDelayNodeExecutesAfterDeadline(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"delay","delay":"15s"}
	]}`
	inst := delayInstanceAt(delayNodeA)
	inst.WaitingReason = model.WaitingReasonDelay
	past := t0.Add(-time.Second)
	inst.RunAt = &past
	fake := &delayFakeInstances{inst: inst}
	e := delayEngine(fake, delayLoader(t, content), t0)

	if err := e.Process(context.Background(), fake.inst); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fake.inserted) != 1 {
		t.Fatalf("attempts inserted = %d, want 1 execution", len(fake.inserted))
	}
	if got := string(fake.inserted[0].Output); got != "null" {
		t.Errorf("attempt output = %s, want null", got)
	}
	types := fake.eventTypes()
	if types["node_started"] != 1 {
		t.Errorf("node_started events = %d, want 1", types["node_started"])
	}
	if len(fake.checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(fake.checkpoints))
	}
	cp := fake.checkpoints[0]
	if cp.Status != model.WorkflowFinished {
		t.Errorf("status = %s, want finished", cp.Status)
	}
	if cp.RunAt != nil {
		t.Errorf("run_at = %v, want cleared after execution", cp.RunAt)
	}
}

func TestDelayNodeBranchParksBeforeExecution(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"delay","delay":"15s","next_node":"` + delayNodeB + `"},
		{"id":"` + delayNodeB + `","type":"script","script":"return 2;"}
	]}`
	inst := &delayFakeInstances{inst: delayInstanceAt(delayNodeA)}
	par := &delayFakeParallel{branch: delayBranchAt(delayNodeA), execution: delayBranchExecution()}
	e := delayBranchEngine(inst, par, delayLoader(t, content), t0)

	if err := e.ProcessBranch(context.Background(), par.branch); err != nil {
		t.Fatalf("ProcessBranch() error = %v", err)
	}
	if len(par.checkpoints) != 1 {
		t.Fatalf("branch checkpoints = %d, want 1 park", len(par.checkpoints))
	}
	cp := par.checkpoints[0]
	if cp.Status != model.ParallelBranchWaiting || cp.WaitingReason != model.WaitingReasonDelay {
		t.Errorf("park = %s/%q, want waiting/delay", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt == nil || !cp.RunAt.Equal(t0.Add(15*time.Second)) {
		t.Errorf("run_at = %v, want t0+15s", cp.RunAt)
	}
	if len(inst.inserted) != 0 {
		t.Errorf("attempts inserted = %d, want none before the deadline", len(inst.inserted))
	}
}

func TestDelayGateBranchExecutesAfterDeadline(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	content := `{"start_node_id":"` + delayNodeA + `","nodes":[
		{"id":"` + delayNodeA + `","type":"script","script":"return 1;","next_node":"` + delayNodeB + `","start_delay":"15s"},
		{"id":"` + delayNodeB + `","type":"script","script":"return 2;"}
	]}`
	branch := delayBranchAt(delayNodeA)
	branch.WaitingReason = model.WaitingReasonDelay
	past := t0.Add(-time.Second)
	branch.RunAt = &past
	inst := &delayFakeInstances{inst: delayInstanceAt(delayNodeA)}
	par := &delayFakeParallel{branch: branch, execution: delayBranchExecution()}
	e := delayBranchEngine(inst, par, delayLoader(t, content), t0)

	if err := e.ProcessBranch(context.Background(), par.branch); err != nil {
		t.Fatalf("ProcessBranch() error = %v", err)
	}
	if len(inst.inserted) != 1 {
		t.Fatalf("attempts inserted = %d, want 1 execution", len(inst.inserted))
	}
	if inst.eventTypes()["node_started"] != 1 {
		t.Errorf("node_started events = %d, want 1", inst.eventTypes()["node_started"])
	}
	if len(par.checkpoints) != 1 {
		t.Fatalf("branch checkpoints = %d, want 1", len(par.checkpoints))
	}
	cp := par.checkpoints[0]
	if cp.Status != model.ParallelBranchWaiting || cp.WaitingReason != model.WaitingReasonRunnable {
		t.Errorf("checkpoint = %s/%q, want waiting/runnable", cp.Status, cp.WaitingReason)
	}
	if cp.RunAt != nil {
		t.Errorf("run_at = %v, want cleared after execution", cp.RunAt)
	}
	if cp.Frame.CurrentNodeID != delayNodeB {
		t.Errorf("cursor = %s, want advanced to %s", cp.Frame.CurrentNodeID, delayNodeB)
	}
}
