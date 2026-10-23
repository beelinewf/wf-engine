package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
)

var (
	formTestSchema = []byte(`{"type":"object","properties":{"email":{"type":"string"}}}`)
	formTestUI     = []byte(`{"order":["email"]}`)
)

type fakeFormRepo struct {
	forms    map[string]repository.Form
	wfRefs   map[string][]string
	nodeRefs map[string][]string
	seq      int

	usageDefs      []repository.FormUsageWorkflowDefinition
	usageNodeDefs  []repository.FormUsageNodeDefinition
	usageInstances []repository.FormUsageInstance
	usageTotal     int64
	usageErr       error
}

func newFakeFormRepo() *fakeFormRepo {
	return &fakeFormRepo{
		forms:    map[string]repository.Form{},
		wfRefs:   map[string][]string{},
		nodeRefs: map[string][]string{},
	}
}

// fakeResolver is a scriptable WorkflowDefinitionResolver for usage tests.
type fakeResolver struct {
	trees map[string]*model.WorkflowContent
	err   error
	calls map[string]int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{trees: map[string]*model.WorkflowContent{}, calls: map[string]int{}}
}

func (f *fakeResolver) Resolve(_ context.Context, defID string) (*model.WorkflowContent, error) {
	f.calls[defID]++
	if f.err != nil {
		return nil, f.err
	}
	tree, ok := f.trees[defID]
	if !ok {
		return nil, model.ErrNotFound
	}
	return tree, nil
}

func (f *fakeFormRepo) Create(_ context.Context, name, contentType string, schema, ui []byte) (repository.Form, error) {
	f.seq++
	form := repository.Form{
		ID:          fmt.Sprintf("aaaaaaaa-aaaa-7aaa-8aaa-%012d", f.seq),
		Name:        name,
		ContentType: contentType,
		Schema:      schema,
		UI:          ui,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	f.forms[form.ID] = form
	return form, nil
}

func (f *fakeFormRepo) GetByID(_ context.Context, id string) (repository.Form, error) {
	form, ok := f.forms[id]
	if !ok {
		return repository.Form{}, model.ErrNotFound
	}
	return form, nil
}

func (f *fakeFormRepo) List(_ context.Context, _, _ int) ([]repository.Form, int64, error) {
	items := make([]repository.Form, 0, len(f.forms))
	for _, form := range f.forms {
		items = append(items, form)
	}
	return items, int64(len(items)), nil
}

func (f *fakeFormRepo) Update(_ context.Context, id, name, contentType string, schema, ui []byte) (repository.Form, error) {
	form, ok := f.forms[id]
	if !ok {
		return repository.Form{}, model.ErrNotFound
	}
	form.Name = name
	form.ContentType = contentType
	form.Schema = schema
	form.UI = ui
	form.UpdatedAt = time.Now().UTC()
	f.forms[id] = form
	return form, nil
}

func (f *fakeFormRepo) Delete(_ context.Context, id string) error {
	if f.referenced(id) {
		return model.ErrConflict
	}
	if _, ok := f.forms[id]; !ok {
		return model.ErrNotFound
	}
	delete(f.forms, id)
	return nil
}

func (f *fakeFormRepo) referenced(id string) bool {
	for _, refs := range f.wfRefs {
		for _, ref := range refs {
			if ref == id {
				return true
			}
		}
	}
	for _, refs := range f.nodeRefs {
		for _, ref := range refs {
			if ref == id {
				return true
			}
		}
	}
	return false
}

func (f *fakeFormRepo) SaveWorkflowFormRefs(_ context.Context, owner string, formIDs []string) error {
	f.wfRefs[owner] = formIDs
	return nil
}

func (f *fakeFormRepo) SaveNodeFormRefs(_ context.Context, owner string, formIDs []string) error {
	f.nodeRefs[owner] = formIDs
	return nil
}

func (f *fakeFormRepo) WorkflowFormRefs(_ context.Context, owner string) ([]string, error) {
	return f.wfRefs[owner], nil
}

func (f *fakeFormRepo) NodeFormRefs(_ context.Context, owner string) ([]string, error) {
	return f.nodeRefs[owner], nil
}

func (f *fakeFormRepo) Usage(_ context.Context, _ string, _, _ int) ([]repository.FormUsageWorkflowDefinition, []repository.FormUsageNodeDefinition, []repository.FormUsageInstance, int64, error) {
	if f.usageErr != nil {
		return nil, nil, nil, 0, f.usageErr
	}
	return f.usageDefs, f.usageNodeDefs, f.usageInstances, f.usageTotal, nil
}

func newFormService() (service.FormService, *fakeFormRepo) {
	repo := newFakeFormRepo()
	return service.NewFormService(repo, newFakeResolver()), repo
}

func newFormServiceWithUsage() (service.FormService, *fakeFormRepo, *fakeResolver) {
	repo := newFakeFormRepo()
	resolver := newFakeResolver()
	return service.NewFormService(repo, resolver), repo, resolver
}

func TestFormServiceCreateRoundTrip(t *testing.T) {
	svc, _ := newFormService()
	created, err := svc.Create(context.Background(), "contact", "", formTestSchema, formTestUI)
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "contact" || string(created.Schema) != string(formTestSchema) || string(created.UI) != string(formTestUI) {
		t.Fatalf("created = %+v", created)
	}
	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Fatalf("Get() = %+v, want %+v", got, created)
	}
	items, total, err := svc.List(context.Background(), 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("List() = %+v, total %d", items, total)
	}
}

func TestFormServiceContentTypeDefaultAndValidate(t *testing.T) {
	svc, repo := newFormService()

	omitted, err := svc.Create(context.Background(), "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatalf("Create() with omitted content type error = %v", err)
	}
	if omitted.ContentType != model.MediaTypeJSON {
		t.Errorf("Create() content_type = %q, want %q", omitted.ContentType, model.MediaTypeJSON)
	}

	multi, err := svc.Create(context.Background(), "upload", model.MediaTypeMultipart, formTestSchema, nil)
	if err != nil {
		t.Fatalf("Create() multipart error = %v", err)
	}
	if multi.ContentType != model.MediaTypeMultipart {
		t.Errorf("Create() content_type = %q, want %q", multi.ContentType, model.MediaTypeMultipart)
	}

	if _, err := svc.Create(context.Background(), "bogus", "text/plain", formTestSchema, nil); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("Create(text/plain) error = %v, want ErrInvalid", err)
	}
	if len(repo.forms) != 2 {
		t.Errorf("stored forms = %d, want 2 (bogus create stored nothing)", len(repo.forms))
	}

	flipped, err := svc.Update(context.Background(), omitted.ID, "contact", model.MediaTypeMultipart, formTestSchema, nil)
	if err != nil {
		t.Fatalf("Update() to multipart error = %v", err)
	}
	if flipped.ContentType != model.MediaTypeMultipart {
		t.Errorf("Update() content_type = %q, want %q", flipped.ContentType, model.MediaTypeMultipart)
	}
	back, err := svc.Update(context.Background(), omitted.ID, "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatalf("Update() with omitted content type error = %v", err)
	}
	if back.ContentType != model.MediaTypeJSON {
		t.Errorf("Update() content_type = %q, want %q", back.ContentType, model.MediaTypeJSON)
	}
	if _, err := svc.Update(context.Background(), omitted.ID, "contact", "text/plain", formTestSchema, nil); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("Update(text/plain) error = %v, want ErrInvalid", err)
	}
	if stored := repo.forms[omitted.ID]; stored.ContentType != model.MediaTypeJSON {
		t.Errorf("stored content_type after bogus update = %q, want %q", stored.ContentType, model.MediaTypeJSON)
	}
}

func TestFormServiceRejectsBlankName(t *testing.T) {
	svc, _ := newFormService()
	for _, name := range []string{"", "   "} {
		if _, err := svc.Create(context.Background(), name, "", formTestSchema, nil); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("Create(%q) error = %v, want ErrInvalid", name, err)
		}
		if _, err := svc.Update(context.Background(), "id", name, "", formTestSchema, nil); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("Update(%q) error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestFormServiceRejectsInvalidSchema(t *testing.T) {
	svc, _ := newFormService()
	cases := map[string][]byte{
		"missing":      nil,
		"empty":        []byte{},
		"null":         []byte(`null`),
		"non-object":   []byte(`[]`),
		"empty object": []byte(`{}`),
		"uncompilable": []byte(`{"type":"object","properties":{"name":{"type":"string","pattern":"(["}}}`),
	}
	for name, schema := range cases {
		if _, err := svc.Create(context.Background(), "contact", "", schema, nil); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("Create(%s) error = %v, want ErrInvalid", name, err)
		}
		if _, err := svc.Update(context.Background(), "id", "contact", "", schema, nil); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("Update(%s) error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestFormServiceRejectsNonObjectUI(t *testing.T) {
	svc, _ := newFormService()
	for _, ui := range []string{`null`, `[1,2]`, `"x"`, `42`} {
		if _, err := svc.Create(context.Background(), "contact", "", formTestSchema, []byte(ui)); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("Create(ui=%s) error = %v, want ErrInvalid", ui, err)
		}
		if _, err := svc.Update(context.Background(), "id", "contact", "", formTestSchema, []byte(ui)); !errors.Is(err, model.ErrInvalid) {
			t.Errorf("Update(ui=%s) error = %v, want ErrInvalid", ui, err)
		}
	}
	// An absent ui is fine on create and clears on update.
	created, err := svc.Create(context.Background(), "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.UI) != 0 {
		t.Fatalf("created UI = %s, want empty", created.UI)
	}
	updated, err := svc.Update(context.Background(), created.ID, "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.UI) != 0 {
		t.Fatalf("updated UI = %s, want empty", updated.UI)
	}
}

func TestFormServiceUpdateUnknownNotFound(t *testing.T) {
	svc, _ := newFormService()
	if _, err := svc.Update(context.Background(), "missing", "contact", "", formTestSchema, nil); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Update() error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(context.Background(), "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestFormServiceUpdateReferencedSucceeds(t *testing.T) {
	svc, repo := newFormService()
	created, err := svc.Create(context.Background(), "contact", "", formTestSchema, formTestUI)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWorkflowFormRefs(context.Background(), "wf", []string{created.ID}); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(context.Background(), created.ID, "renamed", "", formTestSchema, nil)
	if err != nil {
		t.Fatalf("Update() of referenced form error = %v, want nil", err)
	}
	if updated.Name != "renamed" || len(updated.UI) != 0 {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestFormServiceDeleteReferencedConflict(t *testing.T) {
	svc, repo := newFormService()
	created, err := svc.Create(context.Background(), "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveNodeFormRefs(context.Background(), "nd", []string{created.ID}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), created.ID); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("Delete() of referenced form error = %v, want ErrConflict", err)
	}
}

func TestFormUsageUnknownNotFound(t *testing.T) {
	svc, _, _ := newFormServiceWithUsage()
	if _, err := svc.Usage(context.Background(), "missing", 1, 50); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Usage() error = %v, want ErrNotFound", err)
	}
}

func TestFormUsagePinpointsDirectAndIndirect(t *testing.T) {
	svc, repo, resolver := newFormServiceWithUsage()
	ctx := context.Background()
	form, err := svc.Create(ctx, "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatal(err)
	}

	const (
		directID   = "33333333-3333-7333-8333-333333333333"
		indirectID = "44444444-4444-7444-8444-444444444444"
		nodeDefID  = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	)
	repo.usageDefs = []repository.FormUsageWorkflowDefinition{
		{ID: directID, Name: "direct", Version: 2, LineageID: "lin-direct"},
		{ID: indirectID, Name: "indirect", Version: 1, LineageID: "lin-indirect"},
	}
	repo.usageNodeDefs = []repository.FormUsageNodeDefinition{
		{ID: nodeDefID, Name: "ask", Version: 1, LineageID: "lin-ask", Type: "input"},
	}
	resolver.trees[directID] = &model.WorkflowContent{Nodes: []*model.NodeContent{
		{ID: "11111111-1111-7111-8111-111111111111", Name: "ask email", Type: model.NodeTypeInput, FormID: form.ID},
		{ID: "22222222-2222-7222-8222-222222222222", Name: "group", Type: model.NodeTypeGroup, Group: &model.GroupContent{Nodes: []*model.NodeContent{
			{ID: "33333333-3333-7333-8333-333333333333", Name: "nested ask", Type: model.NodeTypeInput, FormID: form.ID},
		}}},
		{ID: "44444444-4444-7444-8444-444444444444", Name: "run", Type: model.NodeTypeScript},
	}}
	// The resolved tree carries the materialized occurrence: the form id
	// inherited from the node definition plus the reference itself.
	resolver.trees[indirectID] = &model.WorkflowContent{Nodes: []*model.NodeContent{
		{ID: "55555555-5555-7555-8555-555555555555", Name: "shared ask", Type: model.NodeTypeInput, NodeDefinitionID: nodeDefID, FormID: form.ID},
	}}

	usage, err := svc.Usage(ctx, form.ID, 1, 50)
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.FormID != form.ID || usage.Page != 1 || usage.PerPage != 50 {
		t.Fatalf("Usage() envelope = %+v, want form id and pagination echo", usage)
	}
	if len(usage.WorkflowDefinitions) != 2 {
		t.Fatalf("Usage() workflow definitions = %+v, want 2", usage.WorkflowDefinitions)
	}
	direct := usage.WorkflowDefinitions[0]
	if direct.ID != directID || direct.Name != "direct" || direct.Version != 2 || direct.LineageID != "lin-direct" {
		t.Fatalf("Usage() direct definition = %+v", direct)
	}
	if len(direct.Nodes) != 2 ||
		direct.Nodes[0].NodeID != "11111111-1111-7111-8111-111111111111" || direct.Nodes[0].NodeName != "ask email" || direct.Nodes[0].NodeDefinitionID != "" ||
		direct.Nodes[1].NodeID != "33333333-3333-7333-8333-333333333333" || direct.Nodes[1].NodeName != "nested ask" {
		t.Fatalf("Usage() direct nodes = %+v, want the inline node and the group child", direct.Nodes)
	}
	indirect := usage.WorkflowDefinitions[1]
	if len(indirect.Nodes) != 1 || indirect.Nodes[0].NodeID != "55555555-5555-7555-8555-555555555555" ||
		indirect.Nodes[0].NodeDefinitionID != nodeDefID {
		t.Fatalf("Usage() indirect nodes = %+v, want the occurrence with its node_definition_id", indirect.Nodes)
	}
	if len(usage.NodeDefinitions) != 1 || usage.NodeDefinitions[0].ID != nodeDefID || usage.NodeDefinitions[0].Type != "input" {
		t.Fatalf("Usage() node definitions = %+v, want the using node definition", usage.NodeDefinitions)
	}
}

func TestFormUsageWaitingFlag(t *testing.T) {
	svc, repo, resolver := newFormServiceWithUsage()
	ctx := context.Background()
	form, err := svc.Create(ctx, "contact", "", formTestSchema, nil)
	if err != nil {
		t.Fatal(err)
	}

	const (
		wfID          = "33333333-3333-7333-8333-333333333333"
		nodeOnForm    = "11111111-1111-7111-8111-111111111111"
		nodeElsewhere = "22222222-2222-7222-8222-222222222222"
	)
	repo.usageDefs = []repository.FormUsageWorkflowDefinition{{ID: wfID, Name: "flow", Version: 1, LineageID: "lin"}}
	resolver.trees[wfID] = &model.WorkflowContent{Nodes: []*model.NodeContent{
		{ID: nodeOnForm, Name: "ask", Type: model.NodeTypeInput, FormID: form.ID},
		{ID: nodeElsewhere, Name: "other", Type: model.NodeTypeInput},
	}}
	repo.usageInstances = []repository.FormUsageInstance{
		{ID: "66666666-6666-7666-8666-666666666666", WorkflowDefinitionID: wfID, Status: string(model.WorkflowWaiting), CurrentNodeID: strptr(nodeOnForm)},
		{ID: "77777777-7777-7777-8777-777777777777", WorkflowDefinitionID: wfID, Status: string(model.WorkflowWaiting), CurrentNodeID: strptr(nodeElsewhere)},
		{ID: "88888888-8888-7888-8888-888888888888", WorkflowDefinitionID: wfID, Status: string(model.WorkflowRunning), CurrentNodeID: strptr(nodeOnForm)},
		{ID: "99999999-9999-7999-8999-999999999999", WorkflowDefinitionID: wfID, Status: string(model.WorkflowWaiting)},
	}
	repo.usageTotal = 4

	usage, err := svc.Usage(ctx, form.ID, 1, 50)
	if err != nil {
		t.Fatalf("Usage() error = %v", err)
	}
	if usage.InstanceTotal != 4 || len(usage.WorkflowInstances) != 4 {
		t.Fatalf("Usage() instances = %+v, total %d, want 4", usage.WorkflowInstances, usage.InstanceTotal)
	}
	wantWaiting := []bool{true, false, false, false}
	for i, inst := range usage.WorkflowInstances {
		if inst.IsWaitingOnForm != wantWaiting[i] {
			t.Errorf("Usage() instance %s IsWaitingOnForm = %v, want %v", inst.ID, inst.IsWaitingOnForm, wantWaiting[i])
		}
	}
	if usage.WorkflowInstances[0].CurrentNodeID == nil || *usage.WorkflowInstances[0].CurrentNodeID != nodeOnForm {
		t.Errorf("Usage() instance current_node_id = %v, want %s", usage.WorkflowInstances[0].CurrentNodeID, nodeOnForm)
	}
	if resolver.calls[wfID] != 1 {
		t.Errorf("Resolve(%s) calls = %d, want 1 (each definition resolved once)", wfID, resolver.calls[wfID])
	}
}

func strptr(s string) *string { return &s }
