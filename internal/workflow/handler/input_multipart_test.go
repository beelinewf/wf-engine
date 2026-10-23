package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	stdmultipart "mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/gin-gonic/gin"
)

// mpTestRouter wires a router with authentication off and a delivery the
// service accepts, so these tests observe only the media-type dispatch.
func mpTestRouter(svc *fakeInstanceSvc) *gin.Engine {
	return NewRouter(Deps{Health: NewHealth(fakePinger{}), Instances: svc})
}

func putMultipart(t *testing.T, r *gin.Engine, body []byte, contentType string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflow/instance/"+instanceID+"/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Idempotency-Key", "mp-1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(w, req)
	return w
}

// A multipart delivery passes the streaming source through without the
// handler reading the body: the factory the service receives still opens
// every part.
func TestInputMultipartPassesStreamingSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeInstanceSvc{delivery: &model.InputDelivery{ID: occurrenceID, Accepted: true}}
	r := mpTestRouter(svc)

	var buf bytes.Buffer
	w := stdmultipart.NewWriter(&buf)
	if err := w.WriteField("name", "Ada"); err != nil {
		t.Fatal(err)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="images[]"; filename="a.jpg"`)
	h.Set("Content-Type", "image/jpeg")
	fw, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 100)...)
	if _, err := fw.Write(jpeg); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rec := putMultipart(t, r, buf.Bytes(), w.FormDataContentType(), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if svc.deliverReq == nil {
		t.Fatal("DeliverInput was not called")
	}
	req := svc.deliverReq
	if req.ContentType != model.MediaTypeMultipart {
		t.Errorf("content type = %q, want multipart/form-data", req.ContentType)
	}
	if len(req.Payload) != 0 {
		t.Errorf("payload has %d bytes, want the body unread by the handler", len(req.Payload))
	}
	if req.MultipartSource == nil {
		t.Fatal("multipart source is nil, want the streaming factory")
	}
	mr, err := req.MultipartSource()
	if err != nil {
		t.Fatalf("MultipartSource() error = %v", err)
	}
	var names []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart() error = %v", err)
		}
		names = append(names, part.FormName()+":"+part.FileName())
		if _, err := io.Copy(io.Discard, part); err != nil {
			t.Fatal(err)
		}
	}
	if len(names) != 2 || names[0] != "name:" || names[1] != "images[]:a.jpg" {
		t.Errorf("streamed parts = %v, want the untouched body", names)
	}
}

// Anything but JSON or multipart is a 415, and the service is never called.
func TestInputRejectsUnknownContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, contentType := range map[string]string{
		"text":     "text/plain",
		"form url": "application/x-www-form-urlencoded",
		"garbage":  "not-a-media-type;;",
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeInstanceSvc{delivery: &model.InputDelivery{Accepted: true}}
			rec := putMultipart(t, mpTestRouter(svc), []byte(`{"a":1}`), contentType, nil)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415 (%s)", rec.Code, rec.Body.String())
			}
			if svc.deliverReq != nil {
				t.Error("DeliverInput was called, want the refusal before the service")
			}
		})
	}
}

// A missing Content-Type keeps the historical behavior: the body is read as
// JSON, so clients that predate the dispatch keep working.
func TestInputMissingContentTypeIsJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeInstanceSvc{delivery: &model.InputDelivery{ID: occurrenceID, Accepted: true}}
	r := mpTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflow/instance/"+instanceID+"/input",
		bytes.NewReader([]byte(`{"approved":true}`)))
	req.Header.Set("Idempotency-Key", "no-ct-1")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if svc.deliverReq == nil {
		t.Fatal("DeliverInput was not called")
	}
	if svc.deliverReq.ContentType != model.MediaTypeJSON {
		t.Errorf("content type = %q, want application/json", svc.deliverReq.ContentType)
	}
	if string(svc.deliverReq.Payload) != `{"approved":true}` {
		t.Errorf("payload = %q, want the whole body", svc.deliverReq.Payload)
	}
	if svc.deliverReq.MultipartSource != nil {
		t.Error("multipart source is set, want the JSON path")
	}
}

// A JSON body with parameters still dispatches as JSON.
func TestInputJSONWithCharsetIsJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeInstanceSvc{delivery: &model.InputDelivery{ID: occurrenceID, Accepted: true}}
	rec := putMultipart(t, mpTestRouter(svc), []byte(`{"a":1}`), "application/json; charset=utf-8", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if svc.deliverReq.ContentType != model.MediaTypeJSON {
		t.Errorf("content type = %q, want application/json", svc.deliverReq.ContentType)
	}
}

// The service's multipart failures surface with their own statuses: a
// multipart delivery to a JSON-only node answers 415.
func TestInputMapsServiceMultipartErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"shape", fmt.Errorf("field: %w", model.ErrBadRequest), http.StatusBadRequest},
		{"too large", fmt.Errorf("file: %w", model.ErrPayloadTooLarge), http.StatusRequestEntityTooLarge},
		{"unaccepted media", fmt.Errorf("node: %w", model.ErrUnsupportedMedia), http.StatusUnsupportedMediaType},
		{"mime rejection", fmt.Errorf("mime: %w", model.ErrInvalid), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeInstanceSvc{deliveryErr: tc.err}
			var buf bytes.Buffer
			w := stdmultipart.NewWriter(&buf)
			if err := w.WriteField("name", "x"); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			rec := putMultipart(t, mpTestRouter(svc), buf.Bytes(), w.FormDataContentType(), nil)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestStatusForErrorMappings(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{model.ErrNotFound, http.StatusNotFound},
		{model.ErrForbidden, http.StatusForbidden},
		{model.ErrConflict, http.StatusConflict},
		{model.ErrTerminalState, http.StatusConflict},
		{model.ErrInvalid, http.StatusUnprocessableEntity},
		{model.ErrBadRequest, http.StatusBadRequest},
		{model.ErrPayloadTooLarge, http.StatusRequestEntityTooLarge},
		{model.ErrUnsupportedMedia, http.StatusUnsupportedMediaType},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := StatusForError(fmt.Errorf("wrap: %w", tc.err)); got != tc.status {
			t.Errorf("StatusForError(%v) = %d, want %d", tc.err, got, tc.status)
		}
	}
}

// The status contract carries the node's accept list; a nil list degrades
// to empty rather than null, like allowed_roles.
func TestPendingInputResponseCarriesAccept(t *testing.T) {
	got := toPendingInputResponse(&service.PendingInput{
		NodeID: "n", Channel: "http", Accept: []string{"application/json", "multipart/form-data"},
	})
	if len(got.Accept) != 2 || got.Accept[1] != "multipart/form-data" {
		t.Errorf("accept = %v, want both media types", got.Accept)
	}
	empty := toPendingInputResponse(&service.PendingInput{NodeID: "n", Channel: "http"})
	if empty.Accept == nil || len(empty.Accept) != 0 {
		t.Errorf("accept = %v, want an empty non-nil list", empty.Accept)
	}
	if toPendingInputResponse(nil) != nil {
		t.Error("toPendingInputResponse(nil) != nil, want nil")
	}
}

// The boundary parameter rides the Content-Type header: the dispatch parses
// the media type, not the raw header.
func TestInputMultipartWithBoundaryDispatches(t *testing.T) {
	if _, _, err := mime.ParseMediaType("multipart/form-data; boundary=abc"); err != nil {
		t.Fatalf("fixture content type does not parse: %v", err)
	}
	mediaType, err := inputMediaType("multipart/form-data; boundary=abc")
	if err != nil {
		t.Fatalf("inputMediaType() error = %v", err)
	}
	if mediaType != model.MediaTypeMultipart {
		t.Errorf("media type = %q, want multipart/form-data", mediaType)
	}
}
