package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/gin-gonic/gin"
)

// streamTestDetail builds a minimal status detail for stream tests.
func streamTestDetail(status model.WorkflowStatus, revision int64, errMsg string) *service.StatusDetail {
	now := time.Now().UTC()
	return &service.StatusDetail{Instance: model.WorkflowInstance{
		ID: instanceID, WorkflowDefinitionID: wfDefID,
		Status: status, Revision: revision, Error: errMsg,
		ContextMode: model.ContextModeFull,
		CreatedBy:   instanceID, UpdatedBy: instanceID,
		CreatedAt: now, UpdatedAt: now,
	}}
}

// serveStreamTo runs StatusStream synchronously against a direct gin context,
// so tests can shrink the handler's stream intervals.
func serveStreamTo(t *testing.T, h *InstanceHandler, ctx context.Context, w *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status/stream", nil).WithContext(ctx)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: instanceID}}
	h.StatusStream(c)
}

// streamEvent is one parsed SSE event; heartbeats parse as event "ping".
type streamEvent struct {
	id    string
	event string
	data  string
}

func parseStreamBody(t *testing.T, body string) []streamEvent {
	t.Helper()
	var out []streamEvent
	for _, block := range strings.Split(body, "\n\n") {
		if block == "" {
			continue
		}
		var ev streamEvent
		var data []string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				ev.event = "ping"
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(line, "data:"))
				data[len(data)-1] = strings.TrimPrefix(data[len(data)-1], " ")
			default:
				t.Fatalf("unparseable stream line %q in %q", line, body)
			}
		}
		ev.data = strings.Join(data, "\n")
		out = append(out, ev)
	}
	return out
}

func statusEvents(evs []streamEvent) []streamEvent {
	var out []streamEvent
	for _, ev := range evs {
		if ev.event == "status" {
			out = append(out, ev)
		}
	}
	return out
}

func TestStatusStreamTerminalClosesImmediately(t *testing.T) {
	h := NewInstanceHandler(&fakeInstanceSvc{detail: streamTestDetail(model.WorkflowFinished, 7, "")})
	w := httptest.NewRecorder()
	serveStreamTo(t, h, context.Background(), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	evs := statusEvents(parseStreamBody(t, w.Body.String()))
	if len(evs) != 1 {
		t.Fatalf("status events = %d, want 1 (stream must close after terminal)", len(evs))
	}
	if evs[0].id != "7-1" {
		t.Errorf("event id = %q, want 7-1", evs[0].id)
	}
	var resp InstanceStatusResponse
	if err := json.Unmarshal([]byte(evs[0].data), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "finished" || resp.ID != instanceID {
		t.Errorf("data = %+v, want the finished instance", resp)
	}
}

func TestStatusStreamSnapshotMatchesREST(t *testing.T) {
	// Finished closes the stream after the first event, so the call below
	// returns; a live status would (correctly) stream until cancelled.
	detail := streamTestDetail(model.WorkflowFinished, 3, "")
	h := NewInstanceHandler(&fakeInstanceSvc{detail: detail})
	gin.SetMode(gin.TestMode)

	restW := httptest.NewRecorder()
	restC, _ := gin.CreateTestContext(restW)
	restC.Request = httptest.NewRequest(http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status", nil)
	restC.Params = gin.Params{{Key: "id", Value: instanceID}}
	h.Status(restC)

	streamW := httptest.NewRecorder()
	serveStreamTo(t, h, context.Background(), streamW)

	evs := statusEvents(parseStreamBody(t, streamW.Body.String()))
	if len(evs) == 0 {
		t.Fatal("no status events")
	}
	if !bytes.Equal(restW.Body.Bytes(), []byte(evs[0].data)) {
		t.Errorf("stream data differs from REST body:\nstream: %s\nrest:   %s", evs[0].data, restW.Body.String())
	}
}

func TestStatusStreamNotFound(t *testing.T) {
	h := NewInstanceHandler(&fakeInstanceSvc{statusErr: model.ErrNotFound})
	w := httptest.NewRecorder()
	serveStreamTo(t, h, context.Background(), w)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("content-type = %q, want a problem body not a stream", ct)
	}
	if strings.Contains(w.Body.String(), "event:") {
		t.Errorf("body contains events, want a problem body: %s", w.Body.String())
	}
}

func TestStatusStreamEmitsChangeThenTerminal(t *testing.T) {
	running := streamTestDetail(model.WorkflowRunning, 1, "")
	finished := streamTestDetail(model.WorkflowFinished, 2, "")
	svc := &fakeInstanceSvc{detailFunc: func(call int) (*service.StatusDetail, error) {
		if call >= 3 {
			return finished, nil
		}
		return running, nil
	}}
	h := NewInstanceHandler(svc)
	h.streamPoll = 5 * time.Millisecond
	w := httptest.NewRecorder()
	serveStreamTo(t, h, context.Background(), w)
	evs := statusEvents(parseStreamBody(t, w.Body.String()))
	if len(evs) != 2 {
		t.Fatalf("status events = %d, want 2 (initial + terminal)", len(evs))
	}
	var last InstanceStatusResponse
	if err := json.Unmarshal([]byte(evs[1].data), &last); err != nil {
		t.Fatal(err)
	}
	if last.Status != "finished" {
		t.Errorf("last status = %q, want finished", last.Status)
	}
}

func TestStatusStreamMidStreamError(t *testing.T) {
	svc := &fakeInstanceSvc{detailFunc: func(call int) (*service.StatusDetail, error) {
		if call == 1 {
			return streamTestDetail(model.WorkflowRunning, 1, ""), nil
		}
		return nil, errors.New("db gone")
	}}
	h := NewInstanceHandler(svc)
	h.streamPoll = 5 * time.Millisecond
	w := httptest.NewRecorder()
	serveStreamTo(t, h, context.Background(), w)
	evs := parseStreamBody(t, w.Body.String())
	if len(statusEvents(evs)) != 1 {
		t.Fatalf("status events = %d, want 1 before the error", len(statusEvents(evs)))
	}
	if len(evs) != 2 || evs[1].event != "error" {
		t.Fatalf("events = %+v, want a terminal error event", evs)
	}
	var p Problem
	if err := json.Unmarshal([]byte(evs[1].data), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != http.StatusInternalServerError || p.Detail != "db gone" {
		t.Errorf("error event = %+v, want 500 db gone", p)
	}
}

func TestStatusStreamMidStreamNotFound(t *testing.T) {
	// Headers are already 200 when the row vanishes, so the not-found
	// surfaces as a terminal error event carrying status 404.
	svc := &fakeInstanceSvc{detailFunc: func(call int) (*service.StatusDetail, error) {
		if call == 1 {
			return streamTestDetail(model.WorkflowRunning, 1, ""), nil
		}
		return nil, model.ErrNotFound
	}}
	h := NewInstanceHandler(svc)
	h.streamPoll = 5 * time.Millisecond
	w := httptest.NewRecorder()
	serveStreamTo(t, h, context.Background(), w)
	evs := parseStreamBody(t, w.Body.String())
	if len(statusEvents(evs)) != 1 {
		t.Fatalf("status events = %d, want 1 before the error", len(statusEvents(evs)))
	}
	if len(evs) != 2 || evs[1].event != "error" {
		t.Fatalf("events = %+v, want a terminal error event", evs)
	}
	var p Problem
	if err := json.Unmarshal([]byte(evs[1].data), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != http.StatusNotFound {
		t.Errorf("error event = %+v, want status 404", p)
	}
}

func TestStatusStreamHeartbeatOnIdle(t *testing.T) {
	// One shared detail: rebuilding per call would stamp fresh times and
	// every poll would look like a change.
	detail := streamTestDetail(model.WorkflowRunning, 1, "")
	svc := &fakeInstanceSvc{detailFunc: func(int) (*service.StatusDetail, error) {
		return detail, nil
	}}
	h := NewInstanceHandler(svc)
	h.streamPoll = 5 * time.Millisecond
	h.streamHeartbeat = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	serveStreamTo(t, h, ctx, w)
	evs := parseStreamBody(t, w.Body.String())
	if len(statusEvents(evs)) != 1 {
		t.Errorf("status events = %d, want 1 (unchanged polls emit nothing)", len(statusEvents(evs)))
	}
	if n := strings.Count(w.Body.String(), ": ping"); n < 2 {
		t.Errorf("heartbeats = %d, want at least 2 in 100ms", n)
	}
}

func TestStatusStreamDisconnectStopsPolling(t *testing.T) {
	firstCall := make(chan struct{}, 1)
	svc := &fakeInstanceSvc{detailFunc: func(call int) (*service.StatusDetail, error) {
		if call == 1 {
			select {
			case firstCall <- struct{}{}:
			default:
			}
		}
		return streamTestDetail(model.WorkflowRunning, 1, ""), nil
	}}
	h := NewInstanceHandler(svc)
	h.streamPoll = 5 * time.Millisecond
	h.streamHeartbeat = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		serveStreamTo(t, h, ctx, w)
		close(done)
	}()
	select {
	case <-firstCall:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not poll promptly")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after disconnect")
	}
	if !strings.Contains(w.Body.String(), "event: status") {
		t.Errorf("body has no status event: %q", w.Body.String())
	}
	calls := svc.detailCalls
	time.Sleep(30 * time.Millisecond)
	if svc.detailCalls != calls {
		t.Errorf("polls continued after disconnect: %d -> %d", calls, svc.detailCalls)
	}
}

func TestStatusStreamPacesBursts(t *testing.T) {
	svc := &fakeInstanceSvc{detailFunc: func(call int) (*service.StatusDetail, error) {
		return streamTestDetail(model.WorkflowRunning, int64(call), "boom-"+strconv.Itoa(call)), nil
	}}
	h := NewInstanceHandler(svc)
	h.streamPoll = 5 * time.Millisecond
	h.streamHeartbeat = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 320*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	serveStreamTo(t, h, ctx, w)
	n := len(statusEvents(parseStreamBody(t, w.Body.String())))
	if n < 2 {
		t.Fatalf("status events = %d, want at least 2 (the loop must run)", n)
	}
	// 320ms at the 50ms floor allows ~7 emissions; without pacing a 5ms
	// poll would emit ~60. The bound is one-sided so slow machines pass.
	if n > 10 {
		t.Errorf("status events = %d in 320ms, want at most 10 (~20/s floor)", n)
	}
}

func TestStatusStreamRouteRegistered(t *testing.T) {
	r := NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: &fakeInstanceSvc{
		detail: streamTestDetail(model.WorkflowFinished, 1, ""),
	}})
	w := performJSON(r, http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status/stream", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(w.Body.String(), `"status":"finished"`) {
		t.Errorf("body missing finished event: %s", w.Body.String())
	}
}

func TestStatusStreamBehindInstancesReadGate(t *testing.T) {
	newAuthRouter := func() *gin.Engine {
		return NewRouter(Deps{
			Health:       NewHealth(fakePinger{}),
			Instances:    &fakeInstanceSvc{detail: streamTestDetail(model.WorkflowFinished, 1, "")},
			Auth:         &fakeAuthSvc{},
			AuthSettings: configuration.Auth{Enabled: true, APIToken: "secret"},
			Catalog:      auth.NewCatalog(map[string][]string{"op": {"*"}}),
			SystemUserID: testSystemUserID,
		})
	}
	w := performJSON(newAuthRouter(), http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status/stream", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401", w.Code)
	}
	w = performJSON(newAuthRouter(), http.MethodGet, "/v1/workflow/instance/"+instanceID+"/status/stream", "",
		map[string]string{APIKeyHeader: "secret"})
	if w.Code != http.StatusOK {
		t.Errorf("authenticated status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestStatusStreamFrame(t *testing.T) {
	got := string(statusStreamFrame("7-2", "status", []byte("{\"a\":1}\n{\"b\":2}")))
	want := "id: 7-2\nevent: status\ndata: {\"a\":1}\ndata: {\"b\":2}\n\n"
	if got != want {
		t.Errorf("frame = %q, want %q", got, want)
	}
}

func TestTerminalWorkflowStatus(t *testing.T) {
	cases := map[model.WorkflowStatus]bool{
		model.WorkflowWaiting:  false,
		model.WorkflowRunning:  false,
		model.WorkflowPaused:   false,
		model.WorkflowFinished: true,
		model.WorkflowFailed:   true,
		model.WorkflowStopped:  true,
	}
	for status, want := range cases {
		if got := terminalWorkflowStatus(status); got != want {
			t.Errorf("terminalWorkflowStatus(%q) = %v, want %v", status, got, want)
		}
	}
}
