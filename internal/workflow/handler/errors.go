package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/gin-gonic/gin"
)

// Problem is an RFC 7807 application/problem+json body.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance,omitempty"`
}

// WriteProblem writes an application/problem+json response.
func WriteProblem(c *gin.Context, status int, detail string) {
	p := Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}
	if p.Title == "" {
		p.Title = "Error"
	}
	if c.Request != nil {
		p.Instance = c.Request.URL.Path
	}
	c.Header("Content-Type", "application/problem+json; charset=utf-8")
	c.AbortWithStatusJSON(status, p)
}

// StatusForError maps domain errors to HTTP status codes:
//
//	ErrNotFound        -> 404
//	ErrForbidden       -> 403
//	ErrConflict        -> 409
//	ErrInvalid         -> 422
//	ErrTerminalState   -> 409
//	ErrBadRequest      -> 400
//	ErrPayloadTooLarge -> 413
//	ErrUnsupportedMedia -> 415
//	anything else      -> 500
func StatusForError(err error) int {
	switch {
	case errors.Is(err, model.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, model.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, model.ErrConflict), errors.Is(err, model.ErrTerminalState):
		return http.StatusConflict
	case errors.Is(err, model.ErrInvalid):
		return http.StatusUnprocessableEntity
	case errors.Is(err, model.ErrBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, model.ErrPayloadTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, model.ErrUnsupportedMedia):
		return http.StatusUnsupportedMediaType
	default:
		return http.StatusInternalServerError
	}
}

// WriteError writes a problem response derived from a domain error.
// 5xx responses carry a generic detail; the full error is logged
// server-side so DB/driver internals never reach clients.
func WriteError(c *gin.Context, err error) {
	status := StatusForError(err)
	if status < http.StatusInternalServerError {
		WriteProblem(c, status, err.Error())
		return
	}
	path := ""
	if c.Request != nil {
		path = c.Request.URL.Path
	}
	slog.Error("request failed", "status", status, "path", path, "error", err)
	WriteProblem(c, status, http.StatusText(status))
}
