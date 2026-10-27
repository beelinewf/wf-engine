package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

func TestWriteErrorHidesInternalDetailOn5xx(t *testing.T) {
	c, w := makeCtx("/v1/x")

	WriteError(c, errors.New("pq: duplicate key value violates unique constraint"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if p.Detail != http.StatusText(http.StatusInternalServerError) {
		t.Errorf("detail = %q, want generic %q", p.Detail, http.StatusText(http.StatusInternalServerError))
	}
}

func TestWriteErrorKeepsClientDetailOn4xx(t *testing.T) {
	c, w := makeCtx("/v1/x")

	WriteError(c, model.ErrNotFound)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if p.Detail == "" || p.Detail == http.StatusText(http.StatusInternalServerError) {
		t.Errorf("detail = %q, want client-visible message", p.Detail)
	}
}
