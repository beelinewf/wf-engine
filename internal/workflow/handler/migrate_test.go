package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
)

const (
	migrateDefID   = "22222222-2222-7222-8222-222222222222"
	migrateStartID = "33333333-3333-7333-8333-333333333333"
	migrateNewID   = "44444444-4444-7444-8444-444444444444"
)

func migrateResult() *service.MigrateResult {
	frame, _ := model.NewFrame(migrateStartID).JSON()
	now := time.Now().UTC().Truncate(time.Second)
	return &service.MigrateResult{
		Instance: model.WorkflowInstance{
			ID:                   migrateNewID,
			WorkflowDefinitionID: migrateDefID,
			Status:               model.WorkflowPaused,
			Frame:                frame,
			MigratedFrom:         instanceID,
			CreatedAt:            now,
			UpdatedAt:            now,
		},
		SourceStatus: model.WorkflowStopped,
	}
}

func TestInstanceMigrateCreated(t *testing.T) {
	svc := &fakeInstanceSvc{migrateRes: migrateResult()}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
	body := `{"workflow_definition_id":"` + migrateDefID + `","start_node_id":"` + migrateStartID +
		`","context_patch":{"b":1},"start_paused":false,"reason":"upgrade"}`
	w := performJSON(r, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	var resp MigrateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Instance.ID != migrateNewID || resp.Instance.WorkflowDefinitionID != migrateDefID {
		t.Errorf("instance = %+v, want new ids", resp.Instance)
	}
	if resp.Instance.Status != "paused" || resp.Instance.CurrentNodeID == nil || *resp.Instance.CurrentNodeID != migrateStartID {
		t.Errorf("instance = %+v, want paused at start node", resp.Instance)
	}
	if resp.Instance.MigratedFrom == nil || *resp.Instance.MigratedFrom != instanceID {
		t.Errorf("migrated_from = %+v, want source", resp.Instance.MigratedFrom)
	}
	if resp.SourceInstanceID != instanceID || resp.SourceStatus != "stopped" {
		t.Errorf("resp = %+v, want source link + stopped", resp)
	}
	req := svc.migrateReq
	if req == nil {
		t.Fatal("service Migrate not called")
	}
	if req.SourceInstanceID != instanceID || req.WorkflowDefinitionID != migrateDefID || req.StartNodeID != migrateStartID {
		t.Errorf("service req = %+v, want path + body ids", req)
	}
	if req.StartPaused {
		t.Error("StartPaused = true, want explicit false")
	}
	if string(req.ContextPatch) != `{"b":1}` || req.Reason != "upgrade" {
		t.Errorf("service req = %+v, want patch + reason", req)
	}
}

func TestInstanceMigrateDefaultsStartPaused(t *testing.T) {
	svc := &fakeInstanceSvc{migrateRes: migrateResult()}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
	body := `{"workflow_definition_id":"` + migrateDefID + `","start_node_id":"` + migrateStartID + `"}`
	w := performJSON(r, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if svc.migrateReq == nil || !svc.migrateReq.StartPaused {
		t.Errorf("StartPaused = %+v, want default true", svc.migrateReq)
	}
}

func TestInstanceMigrateInvalidBody(t *testing.T) {
	// A non-object patch binds fine here and is the service's 422, so it
	// is not in this table.
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"empty":         {``, "invalid request body"},
		"garbage":       {`{{{`, "invalid request body"},
		"missing def":   {`{"start_node_id":"` + migrateStartID + `"}`, "workflow_definition_id is required"},
		"missing start": {`{"workflow_definition_id":"` + migrateDefID + `"}`, "start_node_id is required"},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeInstanceSvc{migrateRes: migrateResult()}
			r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
			w := performJSON(r, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", tc.body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			var prob Problem
			if err := json.Unmarshal(w.Body.Bytes(), &prob); err != nil {
				t.Fatal(err)
			}
			if prob.Detail != tc.want {
				t.Errorf("detail = %q, want %q", prob.Detail, tc.want)
			}
		})
	}
}

func TestInstanceMigrateErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{model.ErrNotFound, http.StatusNotFound},
		{model.ErrConflict, http.StatusConflict},
		{model.ErrInvalid, http.StatusUnprocessableEntity},
	} {
		svc := &fakeInstanceSvc{migrateErr: c.err}
		r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
		body := `{"workflow_definition_id":"` + migrateDefID + `","start_node_id":"` + migrateStartID + `"}`
		w := performJSON(r, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", body, nil)
		if w.Code != c.want {
			t.Errorf("status = %d, want %d", w.Code, c.want)
		}
	}
}

func TestInstanceStatusIncludesMigrationLinks(t *testing.T) {
	linked := model.WorkflowInstance{ID: instanceID, MigratedFrom: "src-id", SupersededBy: "new-id"}
	svc := &fakeInstanceSvc{detail: &service.StatusDetail{Instance: linked}}
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
	w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["migrated_from"] != "src-id" || resp["superseded_by"] != "new-id" {
		t.Errorf("links = %v/%v, want src-id/new-id", resp["migrated_from"], resp["superseded_by"])
	}

	plain := model.WorkflowInstance{ID: instanceID}
	svc = &fakeInstanceSvc{detail: &service.StatusDetail{Instance: plain}}
	r = NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
	w = performJSON(r, http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status", "", nil)
	resp = map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if v, ok := resp["migrated_from"]; !ok || v != nil {
		t.Errorf("migrated_from = %v, want explicit null", v)
	}
	if v, ok := resp["superseded_by"]; !ok || v != nil {
		t.Errorf("superseded_by = %v, want explicit null", v)
	}
}

func TestRouterMigrateRouteRequiresMigrateAction(t *testing.T) {
	catalog := auth.NewCatalog(map[string][]string{
		"operator": {auth.ActionInstancesMigrate},
	})
	body := `{"workflow_definition_id":"` + migrateDefID + `","start_node_id":"` + migrateStartID + `"}`

	admitted := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Instances:    &fakeInstanceSvc{migrateRes: migrateResult()},
		Auth:         &fakeAuthSvc{},
		AuthSettings: authConfigForTest(),
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: []string{"operator"}}},
		Catalog:      catalog,
	})
	w := performJSON(admitted, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", body,
		map[string]string{BearerHeader: "Bearer test-jwt"})
	if w.Code != http.StatusCreated {
		t.Errorf("operator status = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	refused := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Instances:    &fakeInstanceSvc{migrateRes: migrateResult()},
		Auth:         &fakeAuthSvc{},
		AuthSettings: authConfigForTest(),
		OIDC:         stubVerifier{principal: auth.Principal{Subject: "s1", Issuer: testIssuer, UserID: testSystemUserID, Roles: []string{"viewer"}}},
		Catalog:      catalog,
	})
	w = performJSON(refused, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", body,
		map[string]string{BearerHeader: "Bearer test-jwt"})
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer status = %d, want 403", w.Code)
	}
}

func TestRouterMigrateAcceptsServiceToken(t *testing.T) {
	r := NewRouter(Deps{
		Health:       NewHealth(fakePinger{}),
		Instances:    &fakeInstanceSvc{migrateRes: migrateResult()},
		AuthSettings: authConfigForTest(),
	})
	body := `{"workflow_definition_id":"` + migrateDefID + `","start_node_id":"` + migrateStartID + `"}`
	w := performJSON(r, http.MethodPost, "/v1/workflow/instance/"+instanceID+"/migrate", body,
		map[string]string{APIKeyHeader: "test-token"})
	if w.Code != http.StatusCreated {
		t.Errorf("service token status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
}
