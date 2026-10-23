package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/gin-gonic/gin"
)

const testFormID = "11111111-1111-7111-8111-111111111111"

type fakeFormSvc struct {
	created   repository.Form
	createErr error
	get       repository.Form
	getErr    error
	items     []repository.Form
	total     int64
	listErr   error
	updated   repository.Form
	updateErr error
	deleteErr error
	lastGetID string
	lastDelID string

	usage         service.FormUsage
	usageErr      error
	lastUsageID   string
	lastUsagePage int
	lastUsagePP   int
}

func (f *fakeFormSvc) Create(_ context.Context, name, contentType string, schema, ui []byte) (repository.Form, error) {
	if f.createErr != nil {
		return repository.Form{}, f.createErr
	}
	// Mirror the service contract: an omitted content type reads as JSON.
	if contentType == "" {
		contentType = model.MediaTypeJSON
	}
	f.created = repository.Form{
		ID: testFormID, Name: name, ContentType: contentType, Schema: schema, UI: ui,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	return f.created, nil
}

func (f *fakeFormSvc) Get(_ context.Context, id string) (repository.Form, error) {
	f.lastGetID = id
	return f.get, f.getErr
}

func (f *fakeFormSvc) List(_ context.Context, _, _ int) ([]repository.Form, int64, error) {
	return f.items, f.total, f.listErr
}

func (f *fakeFormSvc) Update(_ context.Context, id, name, contentType string, schema, ui []byte) (repository.Form, error) {
	if f.updateErr != nil {
		return repository.Form{}, f.updateErr
	}
	if contentType == "" {
		contentType = model.MediaTypeJSON
	}
	f.updated = repository.Form{
		ID: id, Name: name, ContentType: contentType, Schema: schema, UI: ui,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	return f.updated, nil
}

func (f *fakeFormSvc) Delete(_ context.Context, id string) error {
	f.lastDelID = id
	return f.deleteErr
}

func (f *fakeFormSvc) Usage(_ context.Context, id string, page, perPage int) (service.FormUsage, error) {
	f.lastUsageID = id
	f.lastUsagePage = page
	f.lastUsagePP = perPage
	if f.usageErr != nil {
		return service.FormUsage{}, f.usageErr
	}
	return f.usage, nil
}

func formRouter(f *fakeFormSvc) *gin.Engine {
	return NewRouter(Deps{Health: NewHealth(fakePinger{}), Forms: f})
}

func TestFormCreateEchoesSchemaAndUI(t *testing.T) {
	f := &fakeFormSvc{}
	w := performJSON(formRouter(f), http.MethodPost, "/v1/forms",
		`{"name":"contact","schema":{"type":"object"},"ui":{"order":["email"]}}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", w.Code, w.Body.String())
	}
	var resp FormResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != testFormID || resp.Name != "contact" {
		t.Fatalf("response = %+v", resp)
	}
	if string(resp.Schema) != `{"type":"object"}` {
		t.Errorf("schema = %s, want the created object", resp.Schema)
	}
	if string(resp.UI) != `{"order":["email"]}` {
		t.Errorf("ui = %s, want the created object", resp.UI)
	}
}

func TestFormCreateContentTypeEcho(t *testing.T) {
	f := &fakeFormSvc{}
	w := performJSON(formRouter(f), http.MethodPost, "/v1/forms",
		`{"name":"upload","content_type":"multipart/form-data","schema":{"type":"object"}}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", w.Code, w.Body.String())
	}
	var resp FormResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ContentType != model.MediaTypeMultipart {
		t.Errorf("response content_type = %q, want multipart", resp.ContentType)
	}
	if f.created.ContentType != model.MediaTypeMultipart {
		t.Errorf("service saw content_type = %q, want multipart", f.created.ContentType)
	}
}

func TestFormCreateOmittedContentTypeDefaultsJSON(t *testing.T) {
	f := &fakeFormSvc{}
	w := performJSON(formRouter(f), http.MethodPost, "/v1/forms",
		`{"name":"contact","schema":{"type":"object"}}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", w.Code, w.Body.String())
	}
	var resp FormResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ContentType != model.MediaTypeJSON {
		t.Errorf("response content_type = %q, want %q", resp.ContentType, model.MediaTypeJSON)
	}
}

func TestFormCreateBogusContentType(t *testing.T) {
	f := &fakeFormSvc{createErr: model.ErrInvalid}
	w := performJSON(formRouter(f), http.MethodPost, "/v1/forms",
		`{"name":"contact","content_type":"text/plain","schema":{"type":"object"}}`, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestFormUpdateContentTypeFlips(t *testing.T) {
	f := &fakeFormSvc{}
	for _, ct := range []string{model.MediaTypeMultipart, model.MediaTypeJSON} {
		w := performJSON(formRouter(f), http.MethodPut, "/v1/forms/"+testFormID,
			`{"name":"contact","content_type":"`+ct+`","schema":{"type":"object"}}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s status = %d, want 200; body %s", ct, w.Code, w.Body.String())
		}
		var resp FormResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.ContentType != ct {
			t.Errorf("response content_type = %q, want %q", resp.ContentType, ct)
		}
		if f.updated.ContentType != ct {
			t.Errorf("service saw content_type = %q, want %q", f.updated.ContentType, ct)
		}
	}
}

func TestFormCreateRejectsBadBody(t *testing.T) {
	r := formRouter(&fakeFormSvc{})
	w := performJSON(r, http.MethodPost, "/v1/forms", `{"name":`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d, want 400", w.Code)
	}
	for name, body := range map[string]string{
		"missing name":   `{"schema":{"type":"object"}}`,
		"missing schema": `{"name":"contact"}`,
	} {
		w := performJSON(r, http.MethodPost, "/v1/forms", body, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s status = %d, want 422", name, w.Code)
		}
	}
}

func TestFormCreateInvalidSchema(t *testing.T) {
	f := &fakeFormSvc{createErr: model.ErrInvalid}
	w := performJSON(formRouter(f), http.MethodPost, "/v1/forms", `{"name":"contact","schema":{}}`, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestFormListShape(t *testing.T) {
	f := &fakeFormSvc{
		items: []repository.Form{{ID: testFormID, Name: "contact", Schema: json.RawMessage(`{"type":"object"}`)}},
		total: 1,
	}
	w := performJSON(formRouter(f), http.MethodGet, "/v1/forms?page=1&per_page=10", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp ListResponse[FormResponse]
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != testFormID ||
		resp.Page != 1 || resp.PerPage != 10 || resp.Total != 1 || resp.TotalPages != 1 {
		t.Fatalf("list response = %+v", resp)
	}
}

func TestFormListRejectsBadPagination(t *testing.T) {
	for _, path := range []string{"/v1/forms?page=0", "/v1/forms?per_page=999", "/v1/forms?per_page=nope"} {
		w := performJSON(formRouter(&fakeFormSvc{}), http.MethodGet, path, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, w.Code)
		}
	}
}

func TestFormGetUnknownNotFound(t *testing.T) {
	f := &fakeFormSvc{getErr: model.ErrNotFound}
	w := performJSON(formRouter(f), http.MethodGet, "/v1/forms/"+testFormID, "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if f.lastGetID != testFormID {
		t.Fatalf("service id = %q, want %q", f.lastGetID, testFormID)
	}
}

func TestFormPathsRejectBadID(t *testing.T) {
	r := formRouter(&fakeFormSvc{})
	cases := []struct {
		method string
		body   string
	}{
		{http.MethodGet, ""},
		{http.MethodPut, `{"name":"x","schema":{"type":"object"}}`},
		{http.MethodDelete, ""},
	}
	for _, tc := range cases {
		w := performJSON(r, tc.method, "/v1/forms/not-a-uuid", tc.body, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", tc.method, w.Code)
		}
	}
}

func TestFormUpdateEchoesReplacement(t *testing.T) {
	f := &fakeFormSvc{}
	w := performJSON(formRouter(f), http.MethodPut, "/v1/forms/"+testFormID,
		`{"name":"renamed","schema":{"type":"object"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var resp FormResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != testFormID || resp.Name != "renamed" {
		t.Fatalf("response = %+v", resp)
	}
	if f.updated.Name != "renamed" || len(f.updated.UI) != 0 {
		t.Fatalf("service saw = %+v, want the replacement with cleared ui", f.updated)
	}
}

func TestFormUpdateErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		updateErr error
		body      string
		want      int
	}{
		"unknown id":   {model.ErrNotFound, `{"name":"x","schema":{"type":"object"}}`, http.StatusNotFound},
		"invalid":      {model.ErrInvalid, `{"name":"x","schema":{"type":"object"}}`, http.StatusUnprocessableEntity},
		"malformed":    {nil, `{"name":`, http.StatusBadRequest},
		"missing name": {nil, `{"schema":{"type":"object"}}`, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeFormSvc{updateErr: tc.updateErr}
			w := performJSON(formRouter(f), http.MethodPut, "/v1/forms/"+testFormID, tc.body, nil)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestFormDeleteNoContent(t *testing.T) {
	f := &fakeFormSvc{}
	w := performJSON(formRouter(f), http.MethodDelete, "/v1/forms/"+testFormID, "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if f.lastDelID != testFormID {
		t.Fatalf("service id = %q, want %q", f.lastDelID, testFormID)
	}
}

func TestFormDeleteErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		deleteErr error
		want      int
	}{
		"unknown id": {model.ErrNotFound, http.StatusNotFound},
		"referenced": {model.ErrConflict, http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeFormSvc{deleteErr: tc.deleteErr}
			w := performJSON(formRouter(f), http.MethodDelete, "/v1/forms/"+testFormID, "", nil)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestFormHandlerImplementsService(t *testing.T) {
	var _ service.FormService = (*fakeFormSvc)(nil)
}

func TestFormUsageShape(t *testing.T) {
	nodeID := "22222222-2222-7222-8222-222222222222"
	f := &fakeFormSvc{usage: service.FormUsage{
		FormID: testFormID,
		WorkflowDefinitions: []service.FormUsageWorkflowDefinition{{
			ID: "33333333-3333-7333-8333-333333333333", Name: "flow", Version: 2, LineageID: "44444444-4444-7444-8444-444444444444",
			Nodes: []service.FormUsageNode{{NodeID: nodeID, NodeName: "ask", NodeDefinitionID: ""}},
		}},
		NodeDefinitions: []service.FormUsageNodeDefinition{{
			ID: "55555555-5555-7555-8555-555555555555", Name: "ask", Version: 1, LineageID: "66666666-6666-7666-8666-666666666666", Type: "input",
		}},
		WorkflowInstances: []service.FormUsageInstance{{
			ID: "77777777-7777-7777-8777-777777777777", WorkflowDefinitionID: "33333333-3333-7333-8333-333333333333",
			Status: "waiting", IsWaitingOnForm: true, CurrentNodeID: &nodeID,
		}},
		InstanceTotal: 1, Page: 1, PerPage: 50,
	}}
	w := performJSON(formRouter(f), http.MethodGet, "/v1/forms/"+testFormID+"/usage", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var resp FormUsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.FormID != testFormID || resp.InstanceTotal != 1 || resp.Page != 1 || resp.PerPage != 50 {
		t.Fatalf("usage envelope = %+v", resp)
	}
	if len(resp.WorkflowDefinitions) != 1 || resp.WorkflowDefinitions[0].Version != 2 ||
		len(resp.WorkflowDefinitions[0].Nodes) != 1 || resp.WorkflowDefinitions[0].Nodes[0].NodeID != nodeID {
		t.Fatalf("workflow definitions = %+v", resp.WorkflowDefinitions)
	}
	if len(resp.NodeDefinitions) != 1 || resp.NodeDefinitions[0].Type != "input" {
		t.Fatalf("node definitions = %+v", resp.NodeDefinitions)
	}
	if len(resp.WorkflowInstances) != 1 || !resp.WorkflowInstances[0].IsWaitingOnForm ||
		resp.WorkflowInstances[0].CurrentNodeID == nil || *resp.WorkflowInstances[0].CurrentNodeID != nodeID {
		t.Fatalf("workflow instances = %+v", resp.WorkflowInstances)
	}
	if f.lastUsageID != testFormID {
		t.Fatalf("service id = %q, want %q", f.lastUsageID, testFormID)
	}
}

func TestFormUsageEmptyListsNotNull(t *testing.T) {
	f := &fakeFormSvc{usage: service.FormUsage{
		FormID:              testFormID,
		WorkflowDefinitions: []service.FormUsageWorkflowDefinition{},
		NodeDefinitions:     []service.FormUsageNodeDefinition{},
		WorkflowInstances:   []service.FormUsageInstance{},
		Page:                1, PerPage: 50,
	}}
	w := performJSON(formRouter(f), http.MethodGet, "/v1/forms/"+testFormID+"/usage", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workflow_definitions", "node_definitions", "workflow_instances"} {
		arr, ok := body[key].([]any)
		if !ok || arr == nil {
			t.Errorf("%s = %v, want [] (never null)", key, body[key])
		}
	}
}

func TestFormUsageBadID(t *testing.T) {
	w := performJSON(formRouter(&fakeFormSvc{}), http.MethodGet, "/v1/forms/not-a-uuid/usage", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestFormUsageUnknownNotFound(t *testing.T) {
	f := &fakeFormSvc{usageErr: model.ErrNotFound}
	w := performJSON(formRouter(f), http.MethodGet, "/v1/forms/"+testFormID+"/usage", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestFormUsagePaginationEcho(t *testing.T) {
	f := &fakeFormSvc{usage: service.FormUsage{
		FormID:              testFormID,
		WorkflowDefinitions: []service.FormUsageWorkflowDefinition{},
		NodeDefinitions:     []service.FormUsageNodeDefinition{},
		WorkflowInstances:   []service.FormUsageInstance{},
		InstanceTotal:       7, Page: 2, PerPage: 3,
	}}
	w := performJSON(formRouter(f), http.MethodGet, "/v1/forms/"+testFormID+"/usage?page=2&per_page=3", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	if f.lastUsagePage != 2 || f.lastUsagePP != 3 {
		t.Fatalf("service pagination = %d/%d, want 2/3", f.lastUsagePage, f.lastUsagePP)
	}
	var resp FormUsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Page != 2 || resp.PerPage != 3 || resp.InstanceTotal != 7 {
		t.Fatalf("pagination echo = %+v, want page 2 per_page 3 total 7", resp)
	}
	for _, path := range []string{"/v1/forms/" + testFormID + "/usage?page=0", "/v1/forms/" + testFormID + "/usage?per_page=999"} {
		w := performJSON(formRouter(&fakeFormSvc{}), http.MethodGet, path, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, w.Code)
		}
	}
}
