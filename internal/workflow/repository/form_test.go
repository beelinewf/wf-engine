package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/simpwf/workflow-engine/internal/workflow/model"
	"github.com/simpwf/workflow-engine/internal/workflow/repository"
	"github.com/simpwf/workflow-engine/pkg/ids"
	"gorm.io/gorm"
)

var (
	testFormSchema = []byte(`{"type":"object","properties":{"email":{"type":"string"}}}`)
	testFormUI     = []byte(`{"order":["email"]}`)
)

func TestFormCreateGetRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	created, err := repo.Create(ctx, "contact", testFormSchema, testFormUI)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !ids.Valid(created.ID) {
		t.Errorf("Create() id = %q, want a valid uuid", created.ID)
	}
	if created.Name != "contact" {
		t.Errorf("Create() name = %q, want contact", created.Name)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("Create() timestamps = %v/%v, want both set", created.CreatedAt, created.UpdatedAt)
	}
	got, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.ID != created.ID || got.Name != "contact" {
		t.Fatalf("GetByID() = %+v", got)
	}
	if !jsonEqual(t, testFormSchema, got.Schema) {
		t.Errorf("GetByID() schema = %s, want %s", got.Schema, testFormSchema)
	}
	if !jsonEqual(t, testFormUI, got.UI) {
		t.Errorf("GetByID() ui = %s, want %s", got.UI, testFormUI)
	}

	// A form without ui round-trips with an empty UI.
	plain, err := repo.Create(ctx, "plain", testFormSchema, nil)
	if err != nil {
		t.Fatalf("Create() without ui error = %v", err)
	}
	got, err = repo.GetByID(ctx, plain.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if len(got.UI) != 0 {
		t.Errorf("GetByID() ui = %s, want empty", got.UI)
	}
}

func TestFormGetUnknownNotFound(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	if _, err := repo.GetByID(ctx, "11111111-1111-7111-8111-111111111111"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetByID() error = %v, want ErrNotFound", err)
	}
}

func TestFormListPaginationAndOrder(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	var createdIDs []string
	for _, name := range []string{"first", "second", "third"} {
		created, err := repo.Create(ctx, name, testFormSchema, nil)
		if err != nil {
			t.Fatal(err)
		}
		createdIDs = append(createdIDs, created.ID)
		time.Sleep(2 * time.Millisecond)
	}

	items, total, err := repo.List(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 2 || items[0].ID != createdIDs[0] || items[1].ID != createdIDs[1] {
		t.Fatalf("List() page 1 = %+v, total %d, want creation order", items, total)
	}
	items, total, err = repo.List(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 1 || items[0].ID != createdIDs[2] {
		t.Fatalf("List() page 2 = %+v, total %d, want the third form", items, total)
	}
}

func TestFormUpdateOverwrites(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	created, err := repo.Create(ctx, "contact", testFormSchema, testFormUI)
	if err != nil {
		t.Fatal(err)
	}
	newSchema := []byte(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	updated, err := repo.Update(ctx, created.ID, "renamed", newSchema, nil)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.ID != created.ID || updated.Name != "renamed" {
		t.Fatalf("Update() = %+v, want renamed", updated)
	}
	if !jsonEqual(t, newSchema, updated.Schema) {
		t.Errorf("Update() schema = %s, want %s", updated.Schema, newSchema)
	}
	if len(updated.UI) != 0 {
		t.Errorf("Update() ui = %s, want cleared", updated.UI)
	}
	got, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || len(got.UI) != 0 || !jsonEqual(t, newSchema, got.Schema) {
		t.Errorf("GetByID() after update = %+v, want overwritten fields", got)
	}
}

func TestFormUpdateUnknownNotFound(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	if _, err := repo.Update(ctx, "11111111-1111-7111-8111-111111111111", "x", testFormSchema, nil); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Update() error = %v, want ErrNotFound", err)
	}
}

func TestFormUpdateReferencedSucceeds(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	created, err := repo.Create(ctx, "contact", testFormSchema, testFormUI)
	if err != nil {
		t.Fatal(err)
	}
	wfID := seedWorkflowDef(t, db, "33333333-3333-7333-8333-333333333333")
	if err := repo.SaveWorkflowFormRefs(ctx, wfID, []string{created.ID}); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Update(ctx, created.ID, "renamed", testFormSchema, nil)
	if err != nil {
		t.Fatalf("Update() of referenced form error = %v, want nil", err)
	}
	if updated.Name != "renamed" || len(updated.UI) != 0 {
		t.Errorf("Update() = %+v, want overwritten fields", updated)
	}
}

func TestFormWorkflowRefsRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	formA, err := repo.Create(ctx, "a", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	formB, err := repo.Create(ctx, "b", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	wfID := seedWorkflowDef(t, db, "33333333-3333-7333-8333-333333333333")

	if err := repo.SaveWorkflowFormRefs(ctx, wfID, []string{formA.ID, formB.ID}); err != nil {
		t.Fatalf("SaveWorkflowFormRefs() error = %v", err)
	}
	got, err := repo.WorkflowFormRefs(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("WorkflowFormRefs() = %v, want 2 refs", got)
	}

	// Save replaces the owner's whole reference set.
	if err := repo.SaveWorkflowFormRefs(ctx, wfID, []string{formB.ID}); err != nil {
		t.Fatal(err)
	}
	got, err = repo.WorkflowFormRefs(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != formB.ID {
		t.Fatalf("WorkflowFormRefs() after replace = %v, want [%s]", got, formB.ID)
	}
}

func TestFormNodeRefsRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	created, err := repo.Create(ctx, "a", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := newDef("11111111-1111-7111-8111-111111111111", "input-node", 1, "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa", "input")
	if err := repository.NewNodeDefinitionRepository(db).Create(ctx, def); err != nil {
		t.Fatal(err)
	}

	if err := repo.SaveNodeFormRefs(ctx, def.ID, []string{created.ID}); err != nil {
		t.Fatalf("SaveNodeFormRefs() error = %v", err)
	}
	got, err := repo.NodeFormRefs(ctx, def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != created.ID {
		t.Fatalf("NodeFormRefs() = %v, want [%s]", got, created.ID)
	}

	// Save replaces the owner's whole reference set.
	if err := repo.SaveNodeFormRefs(ctx, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err = repo.NodeFormRefs(ctx, def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("NodeFormRefs() after clear = %v, want empty", got)
	}
}

func TestFormDeleteReferencedConflict(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	wfForm, err := repo.Create(ctx, "wf-form", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	wfID := seedWorkflowDef(t, db, "33333333-3333-7333-8333-333333333333")
	if err := repo.SaveWorkflowFormRefs(ctx, wfID, []string{wfForm.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, wfForm.ID); !errors.Is(err, model.ErrConflict) {
		t.Errorf("Delete() of workflow-referenced form error = %v, want ErrConflict", err)
	}

	nodeForm, err := repo.Create(ctx, "node-form", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := newDef("11111111-1111-7111-8111-111111111111", "input-node", 1, "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa", "input")
	if err := repository.NewNodeDefinitionRepository(db).Create(ctx, def); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveNodeFormRefs(ctx, def.ID, []string{nodeForm.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, nodeForm.ID); !errors.Is(err, model.ErrConflict) {
		t.Errorf("Delete() of node-referenced form error = %v, want ErrConflict", err)
	}
}

func TestFormDeleteOK(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	created, err := repo.Create(ctx, "contact", testFormSchema, testFormUI)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := repo.GetByID(ctx, created.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetByID() after delete error = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, created.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("second Delete() error = %v, want ErrNotFound", err)
	}
}

func TestFormUsage(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := repository.NewFormRepository(db)

	form, err := repo.Create(ctx, "contact", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}

	nodeDef := newDef("11111111-1111-7111-8111-111111111111", "ask", 1, "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa", "input")
	if err := repository.NewNodeDefinitionRepository(db).Create(ctx, nodeDef); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveNodeFormRefs(ctx, nodeDef.ID, []string{form.ID}); err != nil {
		t.Fatal(err)
	}

	// Indirect user: references the node definition, which references the form.
	indirectID := seedWorkflowDef(t, db, "33333333-3333-7333-8333-333333333333")
	if err := repository.AddWorkflowDefinitionNodeRefs(ctx, db, indirectID, []string{nodeDef.ID}); err != nil {
		t.Fatal(err)
	}
	// Direct user: references the form inline.
	directID := seedWorkflowDef(t, db, "44444444-4444-7444-8444-444444444444")
	if err := repo.SaveWorkflowFormRefs(ctx, directID, []string{form.ID}); err != nil {
		t.Fatal(err)
	}
	unrelatedID := seedWorkflowDef(t, db, "55555555-5555-7555-8555-555555555555")

	instances := repository.NewInstanceRepository(db)
	seedUsageInstance := func(id, wfID, status string) {
		t.Helper()
		frame, _ := model.NewFrame("node-1").JSON()
		w := model.WorkflowInstance{
			ID: id, WorkflowDefinitionID: wfID,
			Status: model.WorkflowStatus(status),
			Frame:  frame, Context: json.RawMessage(`{}`), Counters: json.RawMessage(`{}`),
			CreatedBy: "11111111-1111-7111-8111-111111111111",
			UpdatedBy: "11111111-1111-7111-8111-111111111111",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := instances.Insert(ctx, w); err != nil {
			t.Fatalf("insert instance %s: %v", id, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	seedUsageInstance("66666666-6666-7666-8666-666666666666", directID, string(model.WorkflowWaiting))
	seedUsageInstance("77777777-7777-7777-8777-777777777777", indirectID, string(model.WorkflowRunning))
	seedUsageInstance("88888888-8888-7888-8888-888888888888", unrelatedID, string(model.WorkflowWaiting))

	defs, nodeDefs, got, total, err := repo.Usage(ctx, form.ID, 1, 50)
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("Usage() workflow definitions = %v, want the direct and indirect users", defs)
	}
	seenDefs := map[string]bool{}
	for _, d := range defs {
		seenDefs[d.ID] = true
		if len(d.Content) == 0 {
			t.Errorf("Usage() definition %s content is empty, want the stored JSON", d.ID)
		}
	}
	if !seenDefs[directID] || !seenDefs[indirectID] {
		t.Fatalf("Usage() workflow definitions = %v, want %s and %s", defs, directID, indirectID)
	}
	if len(nodeDefs) != 1 || nodeDefs[0].ID != nodeDef.ID || nodeDefs[0].Type != "input" {
		t.Fatalf("Usage() node definitions = %+v, want [%s]", nodeDefs, nodeDef.ID)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("Usage() instances = %+v, total %d, want the 2 instances of using definitions", got, total)
	}
	if got[0].ID != "66666666-6666-7666-8666-666666666666" || got[1].ID != "77777777-7777-7777-8777-777777777777" {
		t.Errorf("Usage() instances order = [%s %s], want creation order", got[0].ID, got[1].ID)
	}

	// Instance pagination.
	_, _, page1, total, err := repo.Usage(ctx, form.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(page1) != 1 || page1[0].ID != "66666666-6666-7666-8666-666666666666" {
		t.Fatalf("Usage() page 1 = %+v, total %d, want the first instance", page1, total)
	}
	_, _, page2, _, err := repo.Usage(ctx, form.ID, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].ID != "77777777-7777-7777-8777-777777777777" {
		t.Fatalf("Usage() page 2 = %+v, want the second instance", page2)
	}

	// An unused form reports empty non-nil sets.
	unused, err := repo.Create(ctx, "unused", testFormSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defs, nodeDefs, got, total, err = repo.Usage(ctx, unused.ID, 1, 50)
	if err != nil {
		t.Fatalf("Usage() of unused form error = %v", err)
	}
	if defs == nil || nodeDefs == nil || got == nil {
		t.Fatalf("Usage() of unused form = %#v, %#v, %#v, want non-nil slices", defs, nodeDefs, got)
	}
	if len(defs) != 0 || len(nodeDefs) != 0 || len(got) != 0 || total != 0 {
		t.Fatalf("Usage() of unused form = %+v, %+v, %+v, total %d, want all empty", defs, nodeDefs, got, total)
	}
}

// seedWorkflowDef inserts a workflow definition row for reference tests,
// mirroring the node-refs round-trip fixture.
func seedWorkflowDef(t *testing.T, db *gorm.DB, wfID string) string {
	t.Helper()
	wf := model.WorkflowDefinition{
		ID: wfID, Name: "flow", Version: 1,
		LineageID: wfID,
		Content:   json.RawMessage(`{"start_node_id":"11111111-1111-7111-8111-111111111111"}`),
		CreatedBy: "11111111-1111-7111-8111-111111111111",
		UpdatedBy: "11111111-1111-7111-8111-111111111111",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	m := repository.WorkflowDefinitionToModel(wf)
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("seed workflow definition: %v", err)
	}
	return wfID
}
