package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func makeCtx(target string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return c, w
}

func mustParse(t *testing.T, target string) *ListQuery {
	t.Helper()
	return mustParseKind(t, target, ListKindWorkflow)
}

func mustParseKind(t *testing.T, target string, kind ListKind) *ListQuery {
	t.Helper()
	c, _ := makeCtx(target)
	q, err := ParseListQuery(c, kind)
	if err != nil {
		t.Fatalf("ParseListQuery(%s) error = %v", target, err)
	}
	return q
}

func TestParseListQueryDefaults(t *testing.T) {
	q := mustParse(t, "/v1/workflow/definition")
	if q.Page != 1 || q.PerPage != 50 || q.Order != "-created_at" {
		t.Errorf("defaults = page %d per_page %d order %q, want 1/50/-created_at", q.Page, q.PerPage, q.Order)
	}
}

func TestParseListQueryExplicitValues(t *testing.T) {
	q := mustParse(t, "/v1/workflow/definition?page=3&per_page=25&order=name&name=flow&latest_only=true&version=2")
	if q.Page != 3 || q.PerPage != 25 || q.Order != "name" {
		t.Errorf("got page %d per_page %d order %q", q.Page, q.PerPage, q.Order)
	}
	if q.Name != "flow" || !q.LatestOnly || q.Version == nil || *q.Version != 2 {
		t.Errorf("filters mismatch: %+v", q)
	}
}

func TestParseListQueryMultiID(t *testing.T) {
	const (
		a = "11111111-1111-7111-8111-111111111111"
		b = "22222222-2222-7222-8222-222222222222"
		c = "33333333-3333-7333-8333-333333333333"
	)
	q := mustParse(t, "/v1/workflow/definition?id="+a+"&id="+b+"&id="+c)
	if len(q.IDs) != 3 || q.IDs[0] != a || q.IDs[2] != c {
		t.Errorf("IDs = %v, want [%s %s %s]", q.IDs, a, b, c)
	}
}

func TestParseListQueryLineage(t *testing.T) {
	q := mustParse(t, "/v1/workflow/definition?lineage_id=11111111-1111-7111-8111-111111111111")
	if q.LineageID != "11111111-1111-7111-8111-111111111111" {
		t.Errorf("lineage_id = %q", q.LineageID)
	}
}

func TestParseListQueryInvalidValues(t *testing.T) {
	bad := []string{
		"?page=0",
		"?page=-1",
		"?page=abc",
		"?per_page=0",
		"?per_page=201",
		"?order=unknown",
		"?order=-unknown",
		"?id=not-a-uuid",
		"?lineage_id=not-a-uuid",
		"?version=abc",
		"?version=0",
		"?latest_only=banana",
	}
	for _, suffix := range bad {
		c, _ := makeCtx("/v1/workflow/definition" + suffix)
		if _, err := ParseListQuery(c, ListKindWorkflow); err == nil {
			t.Errorf("ParseListQuery(%q) error = nil, want error", suffix)
		}
	}
}

func TestParseListQueryTooManyIDs(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 101; i++ {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString("id=")
		sb.WriteString("11111111-1111-7111-8111-111111111111")
	}
	c, _ := makeCtx("/v1/workflow/definition?" + sb.String())
	if _, err := ParseListQuery(c, ListKindWorkflow); err == nil {
		t.Error("ParseListQuery with 101 ids error = nil, want error")
	}
}

func TestParseListQueryNodeType(t *testing.T) {
	q := mustParseKind(t, "/v1/node/definition?type=script", ListKindNode)
	if q.Type != "script" {
		t.Errorf("type = %q, want script", q.Type)
	}
	// type filter is only allowed for node definitions
	c, _ := makeCtx("/v1/workflow/definition?type=script")
	if _, err := ParseListQuery(c, ListKindWorkflow); err == nil {
		t.Error("type filter on workflow list error = nil, want error")
	}
}

func TestParseInstanceListQueryDefaults(t *testing.T) {
	c, _ := makeCtx("/v1/workflow/instance")
	q, err := ParseInstanceListQuery(c)
	if err != nil {
		t.Fatalf("ParseInstanceListQuery error = %v", err)
	}
	if q.Page != 1 || q.PerPage != 50 || q.Order != "-created_at" {
		t.Errorf("defaults = page %d per_page %d order %q, want 1/50/-created_at", q.Page, q.PerPage, q.Order)
	}
}

func TestParseInstanceListQueryFilters(t *testing.T) {
	const (
		secondID = "33333333-3333-7333-8333-333333333333"
		otherWD  = "44444444-4444-7444-8444-444444444444"
	)
	c, _ := makeCtx("/v1/workflow/instance?page=2&per_page=25&order=status" +
		"&id=" + instanceID + "&id=" + secondID +
		"&workflow_definition_id=" + otherWD +
		"&status=waiting&status=finished")
	q, err := ParseInstanceListQuery(c)
	if err != nil {
		t.Fatalf("ParseInstanceListQuery error = %v", err)
	}
	if q.Page != 2 || q.PerPage != 25 || q.Order != "status" {
		t.Errorf("got page %d per_page %d order %q", q.Page, q.PerPage, q.Order)
	}
	if len(q.IDs) != 2 || q.IDs[0] != instanceID || q.IDs[1] != secondID {
		t.Errorf("IDs = %v, want [%s %s]", q.IDs, instanceID, secondID)
	}
	if q.WorkflowDefinitionID != otherWD {
		t.Errorf("workflow_definition_id = %q, want %q", q.WorkflowDefinitionID, otherWD)
	}
	if len(q.Statuses) != 2 || q.Statuses[0] != "waiting" || q.Statuses[1] != "finished" {
		t.Errorf("statuses = %v, want [waiting finished]", q.Statuses)
	}
}

func TestParseInstanceListQueryDebugFilter(t *testing.T) {
	c, _ := makeCtx("/v1/workflow/instance?debug=true")
	q, err := ParseInstanceListQuery(c)
	if err != nil {
		t.Fatalf("ParseInstanceListQuery error = %v", err)
	}
	if q.Debug == nil || !*q.Debug {
		t.Errorf("debug=true: got %+v", q)
	}

	c, _ = makeCtx("/v1/workflow/instance?debug=false")
	q, err = ParseInstanceListQuery(c)
	if err != nil {
		t.Fatalf("ParseInstanceListQuery error = %v", err)
	}
	if q.Debug == nil || *q.Debug {
		t.Errorf("debug=false: got %+v", q)
	}

	c, _ = makeCtx("/v1/workflow/instance")
	q, err = ParseInstanceListQuery(c)
	if err != nil {
		t.Fatalf("ParseInstanceListQuery error = %v", err)
	}
	if q.Debug != nil {
		t.Errorf("debug omitted: got %v, want nil (unfiltered)", *q.Debug)
	}
}

func TestParseInstanceListQueryOrderAllowlist(t *testing.T) {
	for _, order := range []string{
		"id", "-id",
		"workflow_definition_id", "-workflow_definition_id",
		"status", "-status",
		"created_at", "-created_at",
		"updated_at", "-updated_at",
	} {
		c, _ := makeCtx("/v1/workflow/instance?order=" + order)
		if _, err := ParseInstanceListQuery(c); err != nil {
			t.Errorf("order %q error = %v, want nil", order, err)
		}
	}
}

func TestParseInstanceListQueryTooManyIDs(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 101; i++ {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString("id=")
		sb.WriteString(instanceID)
	}
	c, _ := makeCtx("/v1/workflow/instance?" + sb.String())
	if _, err := ParseInstanceListQuery(c); err == nil {
		t.Error("ParseInstanceListQuery with 101 ids error = nil, want error")
	}
}

func TestParseInstanceListQueryInvalidValues(t *testing.T) {
	bad := []string{
		"?page=0",
		"?page=-1",
		"?page=abc",
		"?per_page=0",
		"?per_page=201",
		"?order=unknown",
		"?order=frame",
		"?order=--created_at",
		"?id=not-a-uuid",
		"?workflow_definition_id=not-a-uuid",
		"?status=",
		"?status=done",
		"?status=Running",
		"?debug=maybe",
	}
	for _, suffix := range bad {
		c, _ := makeCtx("/v1/workflow/instance" + suffix)
		if _, err := ParseInstanceListQuery(c); err == nil {
			t.Errorf("ParseInstanceListQuery(%q) error = nil, want error", suffix)
		}
	}
}

func TestParsePaginationRejectsHugePage(t *testing.T) {
	c, _ := makeCtx("/v1/workflow/instance?page=99999999")
	if _, err := ParseInstanceListQuery(c); err == nil {
		t.Error("ParseInstanceListQuery(page=99999999) error = nil, want error")
	}
}

func TestParseInstanceListQueryTooManyStatuses(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 101; i++ {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString("status=running")
	}
	c, _ := makeCtx("/v1/workflow/instance?" + sb.String())
	if _, err := ParseInstanceListQuery(c); err == nil {
		t.Error("ParseInstanceListQuery with 101 statuses error = nil, want error")
	}
}

func TestWriteProblem(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/x", nil)

	WriteProblem(c, http.StatusUnprocessableEntity, "bad content")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if p.Status != 422 || p.Title == "" || p.Detail != "bad content" || p.Type == "" {
		t.Errorf("problem = %+v", p)
	}
}

func mustParseUser(t *testing.T, target string) *UserListQuery {
	t.Helper()
	c, _ := makeCtx(target)
	q, err := ParseUserListQuery(c)
	if err != nil {
		t.Fatalf("ParseUserListQuery(%s) error = %v", target, err)
	}
	return q
}

func TestParseUserListQueryDefaults(t *testing.T) {
	q := mustParseUser(t, "/v1/users")
	if q.Page != 1 || q.PerPage != 50 || q.Order != "-created_at" {
		t.Errorf("defaults = page %d per_page %d order %q, want 1/50/-created_at", q.Page, q.PerPage, q.Order)
	}
	if len(q.IDs) != 0 || len(q.Roles) != 0 || len(q.Issuers) != 0 {
		t.Errorf("multi filters = %v/%v/%v, want empty", q.IDs, q.Roles, q.Issuers)
	}
	if q.IsLocal != nil || q.EmailVerified != nil || q.IsActive != nil {
		t.Errorf("bool filters = %v/%v/%v, want nil", q.IsLocal, q.EmailVerified, q.IsActive)
	}
	if q.Email != "" || q.Name != "" {
		t.Errorf("search = %q/%q, want empty", q.Email, q.Name)
	}
}

func TestParseUserListQueryFilters(t *testing.T) {
	const (
		a = "11111111-1111-7111-8111-111111111111"
		b = "22222222-2222-7222-8222-222222222222"
	)
	q := mustParseUser(t, "/v1/users?order=created_at&id="+a+","+b+"&role=admin,viewer"+
		"&issuer=local,https://provider.example.test&is_local=true&email_verified=false&is_active=1"+
		"&email=ADA@&name=+Ada+")
	if q.Order != "created_at" {
		t.Errorf("order = %q, want created_at", q.Order)
	}
	if len(q.IDs) != 2 || q.IDs[0] != a || q.IDs[1] != b {
		t.Errorf("IDs = %v", q.IDs)
	}
	if len(q.Roles) != 2 || q.Roles[0] != "admin" || q.Roles[1] != "viewer" {
		t.Errorf("Roles = %v", q.Roles)
	}
	if len(q.Issuers) != 2 || q.Issuers[0] != "local" || q.Issuers[1] != "https://provider.example.test" {
		t.Errorf("Issuers = %v", q.Issuers)
	}
	if q.IsLocal == nil || !*q.IsLocal || q.EmailVerified == nil || *q.EmailVerified || q.IsActive == nil || !*q.IsActive {
		t.Errorf("bools = %v/%v/%v, want true/false/true", q.IsLocal, q.EmailVerified, q.IsActive)
	}
	if q.Email != "ADA@" || q.Name != "Ada" {
		t.Errorf("search = %q/%q, want ADA@/Ada", q.Email, q.Name)
	}
}

func TestParseUserListQueryInvalidValues(t *testing.T) {
	bad := []string{
		"?page=0",
		"?per_page=201",
		"?order=name",
		"?order=-unknown",
		"?order=--created_at",
		"?id=not-a-uuid",
		"?id=11111111-1111-7111-8111-111111111111,not-a-uuid",
		"?is_local=yes",
		"?email_verified=2",
		"?is_active=maybe",
	}
	for _, suffix := range bad {
		c, _ := makeCtx("/v1/users" + suffix)
		if _, err := ParseUserListQuery(c); err == nil {
			t.Errorf("ParseUserListQuery(%s) error = nil, want error", suffix)
		}
	}
}

func TestParseUserListQueryTooMany(t *testing.T) {
	manyIDs := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		manyIDs = append(manyIDs, "11111111-1111-7111-8111-111111111111")
	}
	manyRoles := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		manyRoles = append(manyRoles, "viewer")
	}
	for name, suffix := range map[string]string{
		"ids":     "?id=" + strings.Join(manyIDs, ","),
		"roles":   "?role=" + strings.Join(manyRoles, ","),
		"issuers": "?issuer=" + strings.Join(manyRoles, ","),
	} {
		c, _ := makeCtx("/v1/users" + suffix)
		if _, err := ParseUserListQuery(c); err == nil {
			t.Errorf("%s: ParseUserListQuery() error = nil, want cap error", name)
		}
	}
}

func TestStatusForError(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{model.ErrNotFound, 404},
		{model.ErrConflict, 409},
		{model.ErrInvalid, 422},
		{model.ErrTerminalState, 409},
		{fmt.Errorf("wrapped: %w", model.ErrNotFound), 404},
		{errors.New("generic"), 500},
	}
	for _, tc := range cases {
		if got := StatusForError(tc.err); got != tc.want {
			t.Errorf("StatusForError(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}
