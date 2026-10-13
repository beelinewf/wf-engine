package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpwf/workflow-engine/internal/workflow/auth"
	"github.com/simpwf/workflow-engine/internal/workflow/model"
	"github.com/simpwf/workflow-engine/internal/workflow/repository"
	"github.com/simpwf/workflow-engine/internal/workflow/service"
	"gorm.io/gorm"
)

// svcPublishNextVersion publishes the next version of prevID's lineage
// through the definition service, mirroring an operator publish.
func svcPublishNextVersion(t *testing.T, db *gorm.DB, prevID, start string, nodes ...string) string {
	t.Helper()
	raw := fmt.Sprintf(`{"start_node_id":%q,"nodes":[%s]}`, start, joinAll(nodes))
	def, err := svcWorkflowService(db).Create(context.Background(), service.CreateWorkflowDefinition{
		Name: "svc-flow-v2", PreviousVersionID: &prevID, Content: json.RawMessage(raw),
	})
	if err != nil {
		t.Fatalf("publish next version: %v", err)
	}
	return def.ID
}

// migrateEventData loads the data payload of an instance's latest event of
// one type.
func migrateEventData(t *testing.T, db *gorm.DB, id, typ string) map[string]any {
	t.Helper()
	events, err := repository.NewInstanceRepository(db).ListEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != typ {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			t.Fatalf("unmarshal %s event: %v", typ, err)
		}
		return data
	}
	t.Fatalf("no %q event for instance %s", typ, id)
	return nil
}

func migrateCtxMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal context: %v", err)
	}
	return m
}

func TestMigratePausedHappyPath(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111201"
	n2 := "11111111-1111-7111-8111-111111111202"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 20; return 20;", "", map[string]any{"output_property": "out2"}),
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1, Context: json.RawMessage(`{"seed":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	// Run only n1, then pause while waiting at n2.
	repo := repository.NewInstanceRepository(db)
	eng := svcTestEngine(t, db)
	claimed, err := repo.ClaimNext(ctx, "migrate-worker", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range claimed {
		if w.ID == inst.ID {
			if err := eng.Process(ctx, w); err != nil {
				t.Fatalf("Process(n1) error = %v", err)
			}
		}
	}
	if _, err := svc.Pause(ctx, service.ControlRequest{InstanceID: inst.ID}); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}

	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID:     inst.ID,
		WorkflowDefinitionID: v2,
		StartNodeID:          n2,
		ContextPatch:         json.RawMessage(`{"patched":true}`),
		StartPaused:          true,
		Reason:               "upgrade",
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if res.SourceStatus != model.WorkflowStopped {
		t.Errorf("source status = %s, want stopped", res.SourceStatus)
	}
	nxt := res.Instance
	if nxt.WorkflowDefinitionID != v2 {
		t.Errorf("def = %s, want %s", nxt.WorkflowDefinitionID, v2)
	}
	if nxt.Status != model.WorkflowPaused {
		t.Errorf("status = %s, want paused", nxt.Status)
	}
	if nxt.MigratedFrom != inst.ID {
		t.Errorf("migrated_from = %q, want %q", nxt.MigratedFrom, inst.ID)
	}
	if nxt.Debug {
		t.Error("debug = true, want false")
	}
	if nxt.Revision != 0 {
		t.Errorf("revision = %d, want 0", nxt.Revision)
	}
	frame, err := model.ParseFrame(nxt.Frame)
	if err != nil {
		t.Fatal(err)
	}
	if frame.CurrentNodeID != n2 || len(frame.GroupStack) != 0 {
		t.Errorf("frame = %+v, want cursor at %s with empty stack", frame, n2)
	}
	var counters model.Counters
	if err := json.Unmarshal(nxt.Counters, &counters); err != nil || counters.Total != 0 {
		t.Errorf("counters = %s, want fresh", nxt.Counters)
	}
	ctxMap := migrateCtxMap(t, nxt.Context)
	if ctxMap["seed"] != float64(1) || ctxMap["patched"] != true {
		t.Errorf("context = %v, want carried seed + patch", ctxMap)
	}
	if _, ok := ctxMap["out1"]; !ok {
		t.Errorf("context = %v, want carried n1 output", ctxMap)
	}

	src, err := svc.GetStatus(ctx, inst.ID, auth.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != model.WorkflowStopped || src.SupersededBy != nxt.ID {
		t.Errorf("source = (%s, %q), want (stopped, %q)", src.Status, src.SupersededBy, nxt.ID)
	}

	created := migrateEventData(t, db, nxt.ID, "instance_created")
	if created["migrated_from"] != inst.ID || created["reason"] != "upgrade" {
		t.Errorf("instance_created = %v, want migration data", created)
	}
	migrated := migrateEventData(t, db, inst.ID, "instance_migrated")
	if migrated["new_instance_id"] != nxt.ID || migrated["reason"] != "upgrade" {
		t.Errorf("instance_migrated = %v, want target ids + reason", migrated)
	}

	// Resume finishes on the v2 graph from the mid-graph node: only n2
	// runs (v2's script), the new run keeps no n1 occurrence.
	if _, err := svc.Resume(ctx, service.ControlRequest{InstanceID: nxt.ID}); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	final := driveEngine(t, db, nxt.ID)
	if final.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished (error %q)", final.Status, final.Error)
	}
	finalMap := migrateCtxMap(t, final.Context)
	if finalMap["out2"] != float64(20) {
		t.Errorf("out2 = %v, want v2 value 20", finalMap["out2"])
	}
	occs, err := repo.ListNodeInstances(ctx, nxt.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range occs {
		if o.NodeID != n2 {
			t.Errorf("occurrence node = %s, want only %s (clean run)", o.NodeID, n2)
		}
	}
}

func TestMigrateFailedSourceKeepsErrorOnOldRun(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111211"
	n2 := "11111111-1111-7111-8111-111111111212"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "throw new Error('boom');", n2, nil),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	cur := driveEngine(t, db, inst.ID)
	if cur.Status != model.WorkflowFailed {
		t.Fatalf("status = %s, want failed", cur.Status)
	}

	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID:     inst.ID,
		WorkflowDefinitionID: v2,
		StartNodeID:          n1,
		StartPaused:          true,
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if res.Instance.Error != "" {
		t.Errorf("new error = %q, want empty", res.Instance.Error)
	}
	src, _ := svc.GetStatus(ctx, inst.ID, auth.Principal{})
	if src.Status != model.WorkflowStopped || src.Error == "" {
		t.Errorf("source = (%s, %q), want stopped with kept error", src.Status, src.Error)
	}
	if _, err := svc.Resume(ctx, service.ControlRequest{InstanceID: res.Instance.ID}); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if final := driveEngine(t, db, res.Instance.ID); final.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished (error %q)", final.Status, final.Error)
	}
}

func TestMigrateInputParkedSource(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111221"
	n2 := "11111111-1111-7111-8111-111111111222"
	n3 := "11111111-1111-7111-8111-111111111223"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "return 1;", n2, map[string]any{"output_property": "a"}),
		svcNodeJSON(n2, "input", "ask", "", n3, map[string]any{"channel": "http", "output_property": "gate"}),
		svcNodeJSON(n3, "script", "b", "return 1;", "", map[string]any{"output_property": "done"}),
	)
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "return 1;", n2, map[string]any{"output_property": "a"}),
		svcNodeJSON(n2, "input", "ask", "", n3, map[string]any{"channel": "http", "output_property": "gate"}),
		svcNodeJSON(n3, "script", "b", "return 1;", "", map[string]any{"output_property": "done"}),
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	cur := driveEngine(t, db, inst.ID)
	if cur.Status != model.WorkflowWaiting || cur.WaitingReason != model.WaitingReasonInput {
		t.Fatalf("instance = %+v, want parked on input", cur)
	}
	repo := repository.NewInstanceRepository(db)
	park, err := repo.GetRunningNodeInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("parked attempt: %v", err)
	}

	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID:     inst.ID,
		WorkflowDefinitionID: v2,
		StartNodeID:          n1,
		StartPaused:          true,
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	closed, err := repo.GetNodeInstance(ctx, inst.ID, park.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != model.NodeStopped || !closed.Cancelled {
		t.Errorf("park = (%s, cancelled=%v), want stopped/cancelled", closed.Status, closed.Cancelled)
	}
	// A stale delivery to the superseded park fails its running guard.
	if _, err := svc.DeliverInput(ctx, service.DeliverInput{
		InstanceID: inst.ID, IdempotencyKey: "late-key", Payload: []byte(`{"v":1}`),
	}); err == nil {
		t.Error("DeliverInput() error = nil, want park-closed failure")
	}
	// The new run parks on its own input node on first transition.
	if _, err := svc.Resume(ctx, service.ControlRequest{InstanceID: res.Instance.ID}); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	after := driveEngine(t, db, res.Instance.ID)
	if after.Status != model.WorkflowWaiting || after.WaitingReason != model.WaitingReasonInput {
		t.Fatalf("new run = %+v, want parked on v2 input", after)
	}
	delivery, err := svc.DeliverInput(ctx, service.DeliverInput{
		InstanceID: res.Instance.ID, IdempotencyKey: "fresh-key", Payload: []byte(`{"v":1}`),
	})
	if err != nil || !delivery.Accepted {
		t.Fatalf("DeliverInput() = %+v, err %v, want accepted", delivery, err)
	}
	if final := driveEngine(t, db, res.Instance.ID); final.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished", final.Status)
	}
}

func TestMigrateStartUnpausedRunsImmediately(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111231"
	n2 := "11111111-1111-7111-8111-111111111232"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pause(ctx, service.ControlRequest{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID:     inst.ID,
		WorkflowDefinitionID: v2,
		StartNodeID:          n1,
		StartPaused:          false,
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if res.Instance.Status != model.WorkflowWaiting {
		t.Fatalf("status = %s, want waiting", res.Instance.Status)
	}
	if final := driveEngine(t, db, res.Instance.ID); final.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished", final.Status)
	}
}

func migrateTwoVersions(t *testing.T, db *gorm.DB) (svc service.InstanceService, v1, v2, n1, n2 string) {
	t.Helper()
	n1 = "11111111-1111-7111-8111-111111111241"
	n2 = "11111111-1111-7111-8111-111111111242"
	v1 = svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	v2 = svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	return svcInstanceService(db), v1, v2, n1, n2
}

func migratePaused(t *testing.T, svc service.InstanceService, defID string) string {
	t.Helper()
	inst, err := svc.Create(context.Background(), service.CreateInstance{WorkflowDefinitionID: defID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := svc.Pause(context.Background(), service.ControlRequest{InstanceID: inst.ID}); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	return inst.ID
}

func TestMigrateValidation(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc, v1, v2, n1, n2 := migrateTwoVersions(t, db)
	srcID := migratePaused(t, svc, v1)
	other := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "return 1;", "", nil),
	)
	unknownDef := svcNewID()
	unknownNode := "11111111-1111-7111-8111-111111111299"

	cases := []struct {
		name string
		req  service.MigrateRequest
		want error
	}{
		{"unknown instance", service.MigrateRequest{SourceInstanceID: svcNewID(), WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true}, model.ErrNotFound},
		{"unknown definition", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: unknownDef, StartNodeID: n1, StartPaused: true}, model.ErrNotFound},
		{"other lineage", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: other, StartNodeID: n1, StartPaused: true}, model.ErrInvalid},
		{"same version", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: v1, StartNodeID: n1, StartPaused: true}, model.ErrInvalid},
		{"unknown start node", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: unknownNode, StartPaused: true}, model.ErrInvalid},
		{"array patch", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true, ContextPatch: json.RawMessage(`[1]`)}, model.ErrInvalid},
		{"scalar patch", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true, ContextPatch: json.RawMessage(`"x"`)}, model.ErrInvalid},
		{"missing source", service.MigrateRequest{WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true}, model.ErrInvalid},
		{"missing definition", service.MigrateRequest{SourceInstanceID: srcID, StartNodeID: n1, StartPaused: true}, model.ErrInvalid},
		{"missing start node", service.MigrateRequest{SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartPaused: true}, model.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Migrate(ctx, c.req); !errors.Is(err, c.want) {
				t.Errorf("Migrate() error = %v, want %v", err, c.want)
			}
		})
	}

	// Lower version: an instance on v2 cannot move back to v1.
	v2src := migratePaused(t, svc, v2)
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: v2src, WorkflowDefinitionID: v1, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("lower version error = %v, want %v", err, model.ErrInvalid)
	}

	// Null patch is no patch.
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n2,
		StartPaused: true, ContextPatch: json.RawMessage(`null`),
	}); err != nil {
		t.Errorf("null patch Migrate() error = %v, want nil", err)
	}
}

func TestMigrateParallelStartBoundary(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111251"
	ps := "11111111-1111-7111-8111-111111111252"
	na := "11111111-1111-7111-8111-111111111253"
	nb := "11111111-1111-7111-8111-111111111254"
	pe := "11111111-1111-7111-8111-111111111255"
	nd := "11111111-1111-7111-8111-111111111256"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", "", map[string]any{"output_property": "out1"}),
	)
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", ps, map[string]any{"output_property": "out1"}),
		svcNodeJSON(ps, "parallel_start", "fork", "", "", map[string]any{
			"branches":             map[string]any{"a": na, "b": nb},
			"parallel_end_node_id": pe,
		}),
		svcNodeJSON(na, "script", "a", "context.a = 1", pe, nil),
		svcNodeJSON(nb, "script", "b", "context.b = 2", pe, nil),
		svcNodeJSON(pe, "parallel_end", "join", "", nd, map[string]any{
			"combining_script": `context.x = branch["a"].context.a + branch["b"].context.b`,
		}),
		svcNodeJSON(nd, "script", "done", "context.done = true", "", nil),
	)
	srcID := migratePaused(t, svc, v1)

	// Strictly inside a branch is rejected.
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: na, StartPaused: true,
	}); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("mid-region start error = %v, want %v", err, model.ErrInvalid)
	}

	// At the parallel_start the new run forks fresh.
	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: ps, StartPaused: true,
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if _, err := svc.Resume(ctx, service.ControlRequest{InstanceID: res.Instance.ID}); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	// The resumed run forks at the start node; drive branches, join, and
	// the tail to finish.
	parked := driveEngine(t, db, res.Instance.ID)
	if parked.Status != model.WorkflowWaiting || parked.WaitingReason != model.WaitingReasonParallel {
		t.Fatalf("instance = %s/%s, want waiting/parallel after fork", parked.Status, parked.WaitingReason)
	}
	svcDriveRbBranches(t, db, res.Instance.ID)
	joinClaimed, err := repository.NewInstanceRepository(db).ClaimNext(ctx, "migrate-worker", time.Minute, 1)
	if err != nil || len(joinClaimed) != 1 {
		t.Fatalf("ClaimNext() = %d, %v", len(joinClaimed), err)
	}
	if err := driveOne(t, db, joinClaimed[0]); err != nil {
		t.Fatalf("Process(join) error = %v", err)
	}
	if final := driveEngine(t, db, res.Instance.ID); final.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished (error %q)", final.Status, final.Error)
	}
}

func TestMigrateStateConflicts(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc, v1, v2, n1, _ := migrateTwoVersions(t, db)
	repo := repository.NewInstanceRepository(db)

	running, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimNext(ctx, "migrate-worker", time.Minute, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: running.ID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("running error = %v, want conflict", err)
	} else if !containsPauseFirst(err) {
		t.Errorf("running error = %v, want pause-first hint", err)
	}

	runnable, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: runnable.ID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("runnable error = %v, want conflict", err)
	}

	stopped, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Stop(ctx, service.ControlRequest{InstanceID: stopped.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: stopped.ID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("stopped error = %v, want conflict", err)
	}

	finished, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	if cur := driveEngine(t, db, finished.ID); cur.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished", cur.Status)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: finished.ID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("finished error = %v, want conflict", err)
	}

	// Double migrate: the migrated source is stopped, rejected by the gate.
	srcID := migratePaused(t, svc, v1)
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("double migrate error = %v, want conflict", err)
	}
}

func containsPauseFirst(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "pause first") || strings.Contains(err.Error(), "pause-first"))
}

func TestMigrateConcurrentDoubleMigrate(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc, v1, v2, n1, _ := migrateTwoVersions(t, db)
	srcID := migratePaused(t, svc, v1)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Migrate(ctx, service.MigrateRequest{
				SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
			})
		}(i)
	}
	wg.Wait()
	var conflicts, ok int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, model.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Errorf("wins = %d, conflicts = %d, want exactly one winner", ok, conflicts)
	}
}

func TestMigrateObjectAuthorization(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc, v1, v2, n1, _ := migrateTwoVersions(t, db)
	// migratePaused creates as the service default (system user).
	srcID := migratePaused(t, svc, v1)
	owner := &auth.Principal{UserID: svcSysUserID}
	other := &auth.Principal{UserID: "44444444-4444-7444-8444-444444444444"}

	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1,
		StartPaused: true, Principal: other,
	}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("non-owner error = %v, want not-found (no id oracle)", err)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: n1,
		StartPaused: true, Principal: owner,
	}); err != nil {
		t.Errorf("owner Migrate() error = %v, want nil", err)
	}
}

func TestMigrateDeliveryRace(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111261"
	n2 := "11111111-1111-7111-8111-111111111262"
	n3 := "11111111-1111-7111-8111-111111111263"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "return 1;", n2, map[string]any{"output_property": "a"}),
		svcNodeJSON(n2, "input", "ask", "", n3, map[string]any{"channel": "http", "output_property": "gate"}),
		svcNodeJSON(n3, "script", "b", "return 1;", "", map[string]any{"output_property": "done"}),
	)
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "return 1;", n2, map[string]any{"output_property": "a"}),
		svcNodeJSON(n2, "input", "ask", "", n3, map[string]any{"channel": "http", "output_property": "gate"}),
		svcNodeJSON(n3, "script", "b", "return 1;", "", map[string]any{"output_property": "done"}),
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: v1})
	if err != nil {
		t.Fatal(err)
	}
	if cur := driveEngine(t, db, inst.ID); cur.WaitingReason != model.WaitingReasonInput {
		t.Fatalf("instance = %+v, want input-parked", cur)
	}
	// The delivery wins first: the park completes, the source leaves the
	// migratable state, and migrate conflicts.
	if _, err := svc.DeliverInput(ctx, service.DeliverInput{
		InstanceID: inst.ID, IdempotencyKey: "race-key", Payload: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: inst.ID, WorkflowDefinitionID: v2, StartNodeID: n1, StartPaused: true,
	}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("post-delivery migrate error = %v, want conflict", err)
	}
}

func TestMigrateContextMergeAndSnapshotPrecedence(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc, v1, v2, n1, _ := migrateTwoVersions(t, db)

	t.Setenv("SIMPWF_MIGRATE_PROBE", "old-env")
	secrets := repository.NewSecretRepository(db)
	if _, err := secrets.Create(ctx, "MIGRATE_KEY", "old-secret"); err != nil {
		t.Fatal(err)
	}
	inst, err := svc.Create(ctx, service.CreateInstance{
		WorkflowDefinitionID: v1,
		Context:              json.RawMessage(`{"keep":1,"nested":{"x":1},"arr":[1,2]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pause(ctx, service.ControlRequest{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	// Rotate both snapshots after the source was snapshotted.
	t.Setenv("SIMPWF_MIGRATE_PROBE", "fresh-env")
	if err := secrets.Delete(ctx, "MIGRATE_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Create(ctx, "MIGRATE_KEY", "fresh-secret"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID:     inst.ID,
		WorkflowDefinitionID: v2,
		StartNodeID:          n1,
		StartPaused:          true,
		ContextPatch: json.RawMessage(`{"nested":{"y":2},"arr":[3],"b":3,` +
			`"env":{"SIMPWF_MIGRATE_PROBE":"smuggled"},"secret":{"MIGRATE_KEY":"smuggled"}}`),
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	m := migrateCtxMap(t, res.Instance.Context)
	if m["keep"] != float64(1) || m["b"] != float64(3) {
		t.Errorf("context = %v, want carried + patched scalars", m)
	}
	nested, _ := m["nested"].(map[string]any)
	if nested["x"] != float64(1) || nested["y"] != float64(2) {
		t.Errorf("nested = %v, want deep merge", nested)
	}
	if arr, _ := m["arr"].([]any); len(arr) != 1 || arr[0] != float64(3) {
		t.Errorf("arr = %v, want replaced", m["arr"])
	}
	env, _ := m["env"].(map[string]any)
	if env["SIMPWF_MIGRATE_PROBE"] != "fresh-env" {
		t.Errorf("env probe = %v, want fresh snapshot (patch must lose)", env["SIMPWF_MIGRATE_PROBE"])
	}
	secret, _ := m["secret"].(map[string]any)
	if secret["MIGRATE_KEY"] != "fresh-secret" {
		t.Errorf("secret = %v, want fresh snapshot (patch must lose)", secret["MIGRATE_KEY"])
	}
	// The source keeps its own older snapshots.
	srcMap := migrateCtxMap(t, inst.Context)
	if srcMap["secret"].(map[string]any)["MIGRATE_KEY"] != "old-secret" {
		t.Errorf("source secret rotated, want untouched old snapshot")
	}
}

func TestMigrateLeanTargetAnchorsHistory(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()

	n1 := "11111111-1111-7111-8111-111111111271"
	n2 := "11111111-1111-7111-8111-111111111272"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	)
	rawV2 := fmt.Sprintf(`{"start_node_id":%q,"context_mode":"lean","nodes":[%s]}`, n1, joinAll([]string{
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", n2, map[string]any{"output_property": "out1"}),
		svcNodeJSON(n2, "script", "b", "context.y = 2; return 2;", "", map[string]any{"output_property": "out2"}),
	}))
	v2def, err := svcWorkflowService(db).Create(ctx, service.CreateWorkflowDefinition{
		Name: "svc-flow-v2", PreviousVersionID: &v1, Content: json.RawMessage(rawV2),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := svcInstanceService(db)
	srcID := migratePaused(t, svc, v1)

	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2def.ID, StartNodeID: n2, StartPaused: true,
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if res.Instance.ContextMode != model.ContextModeLean {
		t.Fatalf("context_mode = %q, want lean", res.Instance.ContextMode)
	}
	rows, err := repository.NewInstanceRepository(db).LoadHistory(ctx, res.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].IsAnchor {
		t.Fatalf("history rows = %+v, want one anchor", rows)
	}
	if !jsonEqualCtx(t, rows[0].Snapshot, res.Instance.Context) {
		t.Errorf("anchor snapshot = %s, want new context %s", rows[0].Snapshot, res.Instance.Context)
	}
}

func TestMigrateGroupNestedStartDerivesStack(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	n1 := "11111111-1111-7111-8111-111111111281"
	grp := "11111111-1111-7111-8111-111111111282"
	inner := "11111111-1111-7111-8111-111111111283"
	after := "11111111-1111-7111-8111-111111111284"
	v1 := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", "", map[string]any{"output_property": "out1"}),
	)
	groupNode := `{"id": "` + grp + `", "type": "group", "name": "g", "start_node_id": "` + inner + `", "next_node": "` + after + `", "nodes": [` +
		svcNodeJSON(inner, "script", "inner", "context.inner = 1; return 1;", "", map[string]any{"output_property": "outInner"}) + `]}`
	v2 := svcPublishNextVersion(t, db, v1, n1,
		svcNodeJSON(n1, "script", "a", "context.x = 1; return 1;", grp, map[string]any{"output_property": "out1"}),
		groupNode,
		svcNodeJSON(after, "script", "done", "context.done = true; return 1;", "", map[string]any{"output_property": "outDone"}),
	)
	srcID := migratePaused(t, svc, v1)

	res, err := svc.Migrate(ctx, service.MigrateRequest{
		SourceInstanceID: srcID, WorkflowDefinitionID: v2, StartNodeID: inner, StartPaused: true,
	})
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	frame, err := model.ParseFrame(res.Instance.Frame)
	if err != nil {
		t.Fatal(err)
	}
	if frame.CurrentNodeID != inner || len(frame.GroupStack) != 1 || frame.GroupStack[0] != grp {
		t.Fatalf("frame = %+v, want cursor at inner with stack [group]", frame)
	}
	if _, err := svc.Resume(ctx, service.ControlRequest{InstanceID: res.Instance.ID}); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if final := driveEngine(t, db, res.Instance.ID); final.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished (error %q)", final.Status, final.Error)
	}
}
