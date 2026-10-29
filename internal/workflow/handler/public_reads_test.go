package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

const (
	publicDefID     = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
	publicFormID    = "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb"
	publicNodeInput = "cccccccc-cccc-7ccc-8ccc-cccccccccccc"
	publicNodeRun   = "dddddddd-dddd-7ddd-8ddd-dddddddddddd"
	publicNodeCall  = "eeeeeeee-eeee-7eee-8eee-eeeeeeeeeeee"
	publicNodeGroup = "ffffffff-ffff-7fff-8fff-ffffffffffff"
	publicNodeNest  = "99999999-9999-7999-8999-999999999999"
	publicNodeFork  = "88888888-8888-7888-8888-888888888888"
	publicNodeJoin  = "77777777-7777-7888-8888-777777777777"
)

// publicWorkflowSvc is a WorkflowDefinitionService fake with a scripted
// Resolve tree, for the anonymous projection tests.
type publicWorkflowSvc struct {
	def        model.WorkflowDefinition
	getErr     error
	tree       *model.WorkflowContent
	resolveErr error
}

func (f *publicWorkflowSvc) Create(_ context.Context, _ service.CreateWorkflowDefinition) (model.WorkflowDefinition, error) {
	return model.WorkflowDefinition{}, errors.New("publicWorkflowSvc: Create not implemented")
}

func (f *publicWorkflowSvc) Get(_ context.Context, _ string) (model.WorkflowDefinition, error) {
	return f.def, f.getErr
}

func (f *publicWorkflowSvc) List(_ context.Context, _ repository.DefinitionListQuery) ([]model.WorkflowDefinition, int64, error) {
	return nil, 0, nil
}

func (f *publicWorkflowSvc) Delete(_ context.Context, _ string) error { return nil }

func (f *publicWorkflowSvc) Materialize(_ context.Context, wc *model.WorkflowContent) (*model.WorkflowContent, error) {
	return wc, nil
}

func (f *publicWorkflowSvc) Resolve(_ context.Context, _ string) (*model.WorkflowContent, error) {
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.tree, nil
}

func (f *publicWorkflowSvc) Schemas(_ context.Context, _ json.RawMessage) map[string]json.RawMessage {
	return map[string]json.RawMessage{}
}

// publicTestTree is a materialized definition exercising every projection
// rule: an input node with the full delivery contract and authorization
// surface, executable nodes, a nested group, and a parallel fork/join.
func publicTestTree() *model.WorkflowContent {
	return &model.WorkflowContent{
		StartNodeID: publicNodeInput,
		Keys:        map[string]string{"approved": publicNodeRun},
		StatusUpdate: &model.StatusUpdateConfig{
			HTTP: &model.HTTPStatusUpdateConfig{
				URL: "https://hooks.example/w", Method: "POST",
				Headers: map[string]string{"Authorization": "Bearer hook-secret"},
			},
		},
		Nodes: []*model.NodeContent{
			{
				Type: model.NodeTypeInput, ID: publicNodeInput, Name: "ask",
				Channel: "http", FormID: publicFormID, OutputProperty: "answer",
				Validation:   &model.ValidationScript{Script: "return input != null;"},
				AllowedRoles: []string{"finance"},
				AllowedUsers: []model.AllowedUser{{ID: testSystemUserID}},
				RecordActor:  true,
				Public:       true,
				Accept:       []string{model.MediaTypeJSON, model.MediaTypeMultipart},
				Storage: &model.InputStorageRef{
					Bucket: "uploads", Prefix: "pub/",
					AllowedMIMETypes: []string{"image/*"}, MaxFileBytes: 1024,
				},
				NextNode:  publicNodeRun,
				Metadata:  map[string]any{"owner": "ops"},
				PreScript: &model.HookScript{Script: "pre();"},
			},
			{
				Type: model.NodeTypeScript, ID: publicNodeRun, Name: "run",
				Script: "return secret.API_KEY;",
			},
			{
				Type: model.NodeTypeExternalCall, ID: publicNodeCall, Name: "call",
				HTTP: &model.HTTPConfig{
					URL: "https://internal.example/hook", Method: "POST",
					Headers: map[string]string{"X-Token": "s3cr3t"},
					Body:    json.RawMessage(`{"a":1}`),
				},
			},
			{
				Type: model.NodeTypeGroup, ID: publicNodeGroup, Name: "g",
				Group: &model.GroupContent{
					StartNodeID: publicNodeNest,
					Keys:        map[string]string{"done": publicNodeNest},
					Nodes: []*model.NodeContent{
						{
							Type: model.NodeTypeInput, ID: publicNodeNest,
							Name: "nested-ask", Channel: "http", FormID: publicFormID,
						},
					},
				},
			},
			{
				Type: model.NodeTypeParallelStart, ID: publicNodeFork,
				ParallelBranches:  map[string]string{"a": publicNodeRun, "b": publicNodeCall},
				ParallelEndNodeID: publicNodeJoin,
			},
			{
				Type: model.NodeTypeParallelEnd, ID: publicNodeJoin,
				CombiningScript: "return merge(a, b);",
			},
		},
	}
}

// publicRawContent is stored definition content carrying the same sensitive
// surface as raw JSON, for the degraded-path tests.
func publicRawContent() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"start_node_id": %q,
		"keys": {"approved": %q},
		"status_update": {"http": {"url": "https://hooks.example/w", "headers": {"Authorization": "Bearer hook-secret"}}},
		"nodes": [
			{"id": %q, "type": "input", "name": "ask", "channel": "http", "form_id": %q,
			 "output_property": "answer", "validation": {"script": "return true;"},
			 "allowed_roles": ["finance"], "allowed_users": [{"id": %q}],
			 "record_actor": true, "public": true, "accept": ["application/json"],
			 "storage": {"bucket": "uploads", "allowed_mime_types": ["image/*"], "max_file_bytes": 1024},
			 "next_node": %q, "metadata": {"owner": "ops"}, "pre_script": {"script": "pre();"}},
			{"id": %q, "type": "script", "name": "run", "script": "return secret.API_KEY;"},
			{"id": %q, "type": "external_call", "http_config": {"url": "https://internal.example/hook", "headers": {"X-Token": "s3cr3t"}}},
			{"id": %q, "type": "group", "start_node_id": %q, "nodes": [
				{"id": %q, "type": "input", "name": "nested-ask", "channel": "http", "form_id": %q}
			]}
		]}`,
		publicNodeInput, publicNodeRun,
		publicNodeInput, publicFormID, testSystemUserID, publicNodeRun,
		publicNodeRun, publicNodeCall, publicNodeGroup, publicNodeNest, publicNodeNest, publicFormID))
}

// publicForbiddenKeys must never appear anywhere in an anonymous workflow
// body, at any depth.
var publicForbiddenKeys = []string{
	"script", "conditions", "validation", "pre_script", "post_script",
	"http_config", "execution_config", "combining_script",
	"allowed_roles", "allowed_users", "record_actor",
	"status_update", "schemas",
	"created_by", "updated_by", "created_at", "updated_at",
	"lineage_id", "previous_version_id",
	"headers", "command", "stdin", "metadata", "config",
	"context_path", "timeout", "input_data", "until", "delay",
	"bucket", "prefix", "url", "method", "body",
}

func assertNoPublicLeak(t *testing.T, body []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("anonymous body is not valid JSON: %v", err)
	}
	forbidden := make(map[string]bool, len(publicForbiddenKeys))
	for _, k := range publicForbiddenKeys {
		forbidden[k] = true
	}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch typed := v.(type) {
		case map[string]any:
			for k, child := range typed {
				if forbidden[k] {
					t.Errorf("anonymous body leaks %q at %s", k, path)
				}
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range typed {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		case string:
			// Secret-shaped values must not survive even under an
			// allowlisted key.
			for _, secret := range []string{"s3cr3t", "hook-secret", "secret.API_KEY", "uploads", "internal.example"} {
				if strings.Contains(typed, secret) {
					t.Errorf("anonymous body leaks value %q at %s", secret, path)
				}
			}
		}
	}
	walk(decoded, "$")
}

func publicTestDef() model.WorkflowDefinition {
	return model.WorkflowDefinition{
		ID: publicDefID, Name: "flow", Version: 3,
		LineageID: "66666666-6666-7666-8666-666666666666",
		Content:   publicRawContent(),
		CreatedBy: testSystemUserID, UpdatedBy: testSystemUserID,
	}
}

func publicReadsRouter(wf *publicWorkflowSvc, form *fakeFormSvc) *gin.Engine {
	return NewRouter(Deps{
		Health:              NewHealth(fakePinger{}),
		WorkflowDefinitions: wf,
		Forms:               form,
		Auth:                &fakeAuthSvc{},
		AuthSettings:        authConfigForTest(),
		SystemUserID:        testSystemUserID,
	})
}

func publicOIDCRouter(wf *publicWorkflowSvc, form *fakeFormSvc, roles []string) *gin.Engine {
	return NewRouter(Deps{
		Health:              NewHealth(fakePinger{}),
		WorkflowDefinitions: wf,
		Forms:               form,
		Auth:                &fakeAuthSvc{},
		AuthSettings:        configuration.Auth{},
		OIDC:                stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: roles}},
		Catalog:             auth.NewCatalog(map[string][]string{"auditor": {auth.ActionRolesRead}}),
		SystemUserID:        testSystemUserID,
	})
}

// The projector keeps the frontend contract of every node kind and drops
// every executable and internal field.
func TestPublicProjectionFiltersKitchenSink(t *testing.T) {
	out := projectPublicNodes(publicTestTree().Nodes)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPublicLeak(t, raw)

	var nodes []map[string]any
	if err := json.Unmarshal(raw, &nodes); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, n := range nodes {
		byID[n["id"].(string)] = n
	}
	input := byID[publicNodeInput]
	if input["form_id"] != publicFormID || input["channel"] != "http" || input["output_property"] != "answer" {
		t.Errorf("input delivery contract = %v, want form/channel/output_property", input)
	}
	if input["public"] != true {
		t.Errorf("input public = %v, want true", input["public"])
	}
	accept, _ := input["accept"].([]any)
	if len(accept) != 2 {
		t.Errorf("input accept = %v, want the two authored media types", input["accept"])
	}
	storage, _ := input["storage"].(map[string]any)
	if storage["max_file_bytes"] != float64(1024) {
		t.Errorf("input storage = %v, want the upload constraints without bucket/prefix", input["storage"])
	}
	if byID[publicNodeRun]["type"] != "script" || byID[publicNodeRun]["name"] != "run" {
		t.Errorf("script node = %v, want id/type/name only", byID[publicNodeRun])
	}
	fork := byID[publicNodeFork]
	branches, _ := fork["branches"].(map[string]any)
	if branches["a"] != publicNodeRun || branches["b"] != publicNodeCall || fork["parallel_end_node_id"] != publicNodeJoin {
		t.Errorf("fork = %v, want branch targets and the join id", fork)
	}
	group := byID[publicNodeGroup]
	children, _ := group["nodes"].([]any)
	if len(children) != 1 {
		t.Fatalf("group children = %v, want the nested input", group["nodes"])
	}
	child := children[0].(map[string]any)
	if child["form_id"] != publicFormID {
		t.Errorf("nested input = %v, want its form_id", child)
	}
	childAccept, _ := child["accept"].([]any)
	if len(childAccept) != 1 || childAccept[0] != "application/json" {
		t.Errorf("nested input accept = %v, want the JSON-only default", child["accept"])
	}
}

// Stored content that no longer resolves still reads, with the same
// allowlist applied to its declared fields.
func TestPublicContentFromRawDegrades(t *testing.T) {
	content, err := publicContentFromRaw(publicRawContent())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPublicLeak(t, raw)
	if content.StartNodeID != publicNodeInput {
		t.Errorf("start_node_id = %q, want the declared one", content.StartNodeID)
	}
	if len(content.Nodes) != 4 || content.Nodes[0].FormID != publicFormID {
		t.Errorf("nodes = %+v, want the declared nodes with the input form", content.Nodes)
	}
}

// With auth on and no credential, the definition read succeeds and serves
// the filtered body with the headers a shared URL needs.
func TestPublicWorkflowDefinitionAnonymousFiltered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := publicReadsRouter(&publicWorkflowSvc{def: publicTestDef(), tree: publicTestTree()}, &fakeFormSvc{})
	w := performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "X-Api-Token") {
		t.Errorf("Vary = %q, want the credential headers", got)
	}
	assertNoPublicLeak(t, w.Body.Bytes())
	var resp PublicWorkflowDefinitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != publicDefID || resp.Name != "flow" || resp.Version != 3 {
		t.Errorf("envelope = %+v, want id/name/version", resp)
	}
	if resp.Content.StartNodeID != publicNodeInput || len(resp.Content.Nodes) != 6 {
		t.Errorf("content = %+v, want the graph skeleton", resp.Content)
	}
}

// A definition that no longer resolves still serves anonymously from its
// declared fields.
func TestPublicWorkflowDefinitionAnonymousDegraded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := publicReadsRouter(&publicWorkflowSvc{def: publicTestDef(), resolveErr: model.ErrInvalid}, &fakeFormSvc{})
	w := performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	assertNoPublicLeak(t, w.Body.Bytes())
	var resp PublicWorkflowDefinitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Content.Nodes) == 0 || resp.Content.Nodes[0].FormID != publicFormID {
		t.Errorf("nodes = %+v, want the declared form_id", resp.Content.Nodes)
	}
}

// Unknown ids stay 404 on the anonymous path, whether the resolve or the
// metadata read reports them missing.
func TestPublicWorkflowDefinitionAnonymousNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, svc := range map[string]*publicWorkflowSvc{
		"resolve reports missing": {resolveErr: model.ErrNotFound},
		"metadata read missing":   {tree: publicTestTree(), getErr: model.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			r := publicReadsRouter(svc, &fakeFormSvc{})
			w := performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", nil)
			if w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// A credentialed caller with the read permission keeps the full body,
// without the anonymous cache headers.
func TestPublicWorkflowDefinitionCredentialedSeesFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := publicReadsRouter(&publicWorkflowSvc{def: publicTestDef(), tree: publicTestTree()}, &fakeFormSvc{})
	w := performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", map[string]string{APIKeyHeader: "test-token"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got == "no-store" {
		t.Errorf("Cache-Control = no-store on the authenticated body, want none")
	}
	var resp WorkflowDefinitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Content), "secret.API_KEY") || !strings.Contains(string(resp.Content), "allowed_users") {
		t.Errorf("content = %s, want the full stored definition", resp.Content)
	}
}

// A credentialed caller without the read permission is refused on its own
// terms, and a bad credential never degrades to anonymous.
func TestPublicWorkflowDefinitionCredentialedGates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &publicWorkflowSvc{def: publicTestDef(), tree: publicTestTree()}

	r := publicOIDCRouter(svc, &fakeFormSvc{}, []string{"finance"})
	w := performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", map[string]string{BearerHeader: "Bearer x"})
	if w.Code != http.StatusForbidden {
		t.Errorf("role without definitions:read status = %d, want 403 (%s)", w.Code, w.Body.String())
	}

	r = publicReadsRouter(svc, &fakeFormSvc{})
	w = performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", map[string]string{APIKeyHeader: "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401 (%s)", w.Code, w.Body.String())
	}

	w = performJSON(r, http.MethodGet, "/v1/workflow/definition/not-a-uuid", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// The form render contract is already frontend-only, so the anonymous read
// serves the same body — while the credentialed gates still apply.
func TestPublicFormAnonymous(t *testing.T) {
	gin.SetMode(gin.TestMode)
	form := &fakeFormSvc{get: repository.Form{
		ID: testFormID, Name: "contact", ContentType: model.MediaTypeJSON,
		Schema: json.RawMessage(`{"type":"object"}`), UI: json.RawMessage(`{"order":["email"]}`),
	}}
	r := publicReadsRouter(&publicWorkflowSvc{}, form)
	w := performJSON(r, http.MethodGet, "/v1/forms/"+testFormID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var resp FormResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Schema) != `{"type":"object"}` || string(resp.UI) != `{"order":["email"]}` {
		t.Errorf("form = %+v, want the full render contract", resp)
	}

	w = performJSON(r, http.MethodGet, "/v1/forms/"+testFormID, "", map[string]string{APIKeyHeader: "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401 (%s)", w.Code, w.Body.String())
	}

	r = publicOIDCRouter(&publicWorkflowSvc{}, form, []string{"finance"})
	w = performJSON(r, http.MethodGet, "/v1/forms/"+testFormID, "", map[string]string{BearerHeader: "Bearer x"})
	if w.Code != http.StatusForbidden {
		t.Errorf("role without forms:read status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
}

// With auth off both reads behave exactly as before: the full bodies, with
// no anonymous branch involved.
func TestPublicReadsAuthDisabledUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	form := &fakeFormSvc{get: repository.Form{
		ID: testFormID, Name: "contact", ContentType: model.MediaTypeJSON,
		Schema: json.RawMessage(`{"type":"object"}`),
	}}
	r := NewRouter(Deps{
		Health:              NewHealth(fakePinger{}),
		WorkflowDefinitions: &publicWorkflowSvc{def: publicTestDef(), tree: publicTestTree()},
		Forms:               form,
	})
	w := performJSON(r, http.MethodGet, "/v1/workflow/definition/"+publicDefID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("definition status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var defResp WorkflowDefinitionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &defResp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(defResp.Content), "secret.API_KEY") {
		t.Errorf("content = %s, want the full stored definition", defResp.Content)
	}
	w = performJSON(r, http.MethodGet, "/v1/forms/"+testFormID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("form status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

const publicInstanceID = "55555555-5555-7555-8555-555555555555"

// publicParkedDetail scripts a waiting instance parked on a public input
// node, carrying sensitive-adjacent fields (error, counters, audit) the
// anonymous projection must drop. mut adjusts the script per test.
func publicParkedDetail(mut func(*service.StatusDetail)) *service.StatusDetail {
	d := &service.StatusDetail{
		Instance: model.WorkflowInstance{
			ID:                   publicInstanceID,
			WorkflowDefinitionID: publicDefID,
			Status:               model.WorkflowWaiting,
			WaitingReason:        model.WaitingReasonInput,
			ContextMode:          model.ContextModeFull,
			CurrentNodeID:        publicNodeInput,
			Counters:             json.RawMessage(`{"n":1}`),
			Error:                "boom",
			CreatedBy:            testSystemUserID,
			UpdatedBy:            testSystemUserID,
		},
		PendingInput: &service.PendingInput{
			NodeID:         publicNodeInput,
			Channel:        "http",
			OutputProperty: "answer",
			FormID:         publicFormID,
			Public:         true,
			Accept:         []string{model.MediaTypeJSON},
		},
	}
	if mut != nil {
		mut(d)
	}
	return d
}

func publicStatusRouter(inst *fakeInstanceSvc) *gin.Engine {
	return NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Instances:    inst,
		Auth:         &fakeAuthSvc{},
		AuthSettings: authConfigForTest(),
		SystemUserID: testSystemUserID,
	})
}

func publicStatusOIDCRouter(inst *fakeInstanceSvc, roles []string) *gin.Engine {
	return NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Instances:    inst,
		Auth:         &fakeAuthSvc{},
		AuthSettings: configuration.Auth{},
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: roles}},
		Catalog:      auth.NewCatalog(map[string][]string{"auditor": {auth.ActionRolesRead}}),
		SystemUserID: testSystemUserID,
	})
}

// With auth on and no credential, a status read for an instance parked on
// a public input node succeeds with the filtered body: the ids a
// public-input frontend needs to fetch the definition and the form.
func TestPublicInstanceStatusAnonymousPublicParked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := publicStatusRouter(&fakeInstanceSvc{detail: publicParkedDetail(nil)})
	w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "X-Api-Token") {
		t.Errorf("Vary = %q, want the credential headers", got)
	}
	assertNoPublicLeak(t, w.Body.Bytes())
	var resp PublicInstanceStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != publicInstanceID || resp.WorkflowDefinitionID != publicDefID {
		t.Errorf("envelope = %+v, want the instance and definition ids", resp)
	}
	if resp.Status != "waiting" || resp.PendingInput.FormID != publicFormID {
		t.Errorf("resp = %+v, want waiting status with the pending form_id", resp)
	}
	if resp.PendingInput.NodeID != publicNodeInput || !resp.PendingInput.Public {
		t.Errorf("pending_input = %+v, want the public node", resp.PendingInput)
	}
	if resp.PendingInput.Form != nil {
		t.Errorf("pending_input.form = %+v, want absent for a live instance", resp.PendingInput.Form)
	}
}

// A snapshot instance serves its frozen form bytes on the anonymous
// status, exactly like the authenticated one; the frontend renders them
// instead of fetching the live library.
func TestPublicInstanceStatusAnonymousSnapshotForm(t *testing.T) {
	gin.SetMode(gin.TestMode)
	detail := publicParkedDetail(func(d *service.StatusDetail) {
		d.PendingInput.Form = &service.SnapshotForm{
			Schema: json.RawMessage(`{"type":"object"}`),
			UI:     json.RawMessage(`{"order":["answer"]}`),
		}
	})
	r := publicStatusRouter(&fakeInstanceSvc{detail: detail})
	w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var resp PublicInstanceStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.PendingInput.Form == nil || string(resp.PendingInput.Form.Schema) != `{"type":"object"}` {
		t.Errorf("pending_input.form = %+v, want the frozen bytes", resp.PendingInput.Form)
	}
}

// Anonymous status is refused unless the instance waits on a public input
// node: a private node, a non-input park, and a terminal run all answer
// 403, matching the input-delivery precedent.
func TestPublicInstanceStatusAnonymousRefusedWhenNotPublic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	private := publicParkedDetail(func(d *service.StatusDetail) { d.PendingInput.Public = false })
	unparked := publicParkedDetail(func(d *service.StatusDetail) {
		d.Instance.Status = model.WorkflowRunning
		d.Instance.WaitingReason = model.WaitingReasonRunnable
		d.PendingInput = nil
	})
	done := publicParkedDetail(func(d *service.StatusDetail) {
		d.Instance.Status = model.WorkflowFinished
		d.PendingInput = nil
	})
	for name, detail := range map[string]*service.StatusDetail{
		"private input node": private,
		"running instance":   unparked,
		"finished instance":  done,
	} {
		t.Run(name, func(t *testing.T) {
			r := publicStatusRouter(&fakeInstanceSvc{detail: detail})
			w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", nil)
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// Unknown ids stay 404 on the anonymous path.
func TestPublicInstanceStatusAnonymousNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := publicStatusRouter(&fakeInstanceSvc{statusErr: model.ErrNotFound})
	w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// A credentialed caller keeps the full body, including on a
// private-parked instance: the public-node gate is anonymous-only. The
// credentialed gates still apply.
func TestPublicInstanceStatusCredentialed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	detail := publicParkedDetail(nil)

	r := publicStatusRouter(&fakeInstanceSvc{detail: detail})
	w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", map[string]string{APIKeyHeader: "test-token"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got == "no-store" {
		t.Errorf("Cache-Control = no-store on the authenticated body, want none")
	}
	var full InstanceStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	if full.CreatedBy != testSystemUserID || full.PendingInput == nil {
		t.Errorf("resp = %+v, want the full status body", full)
	}

	r = publicStatusRouter(&fakeInstanceSvc{detail: publicParkedDetail(func(d *service.StatusDetail) {
		d.PendingInput.Public = false
	})})
	w = performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", map[string]string{APIKeyHeader: "test-token"})
	if w.Code != http.StatusOK {
		t.Errorf("private-parked credentialed status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	w = performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", map[string]string{APIKeyHeader: "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401 (%s)", w.Code, w.Body.String())
	}

	r = publicStatusOIDCRouter(&fakeInstanceSvc{detail: detail}, []string{"finance"})
	w = performJSON(r, http.MethodGet, "/v1/workflow/instance/"+publicInstanceID+"/status", "", map[string]string{BearerHeader: "Bearer [REDACTED]"})
	if w.Code != http.StatusForbidden {
		t.Errorf("role without instances:read status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
}
