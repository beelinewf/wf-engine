package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	stdmultipart "mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/pkg/ids"
	"github.com/gin-gonic/gin"
)

// defaultMaxBodyBytes caps the JSON bodies UpdateContext and Input read when
// no explicit cap is configured. It matches the engine output and multipart
// text defaults (1 MiB); large file uploads ride the multipart path with its
// own limits.
const defaultMaxBodyBytes = 1048576

// InstanceHandler serves workflow instance routes.
type InstanceHandler struct {
	svc service.InstanceService
	// streamPoll, streamHeartbeat, and streamMax bound StatusStream. The
	// constructor sets the production defaults; tests shrink them to keep
	// stream tests fast.
	streamPoll      time.Duration
	streamHeartbeat time.Duration
	streamMax       time.Duration
	// maxBodyBytes caps the JSON bodies UpdateContext and Input read; a
	// breach answers 413 before the service runs.
	maxBodyBytes int64
}

// NewInstanceHandler builds the handler.
func NewInstanceHandler(svc service.InstanceService) *InstanceHandler {
	return &InstanceHandler{
		svc:             svc,
		streamPoll:      statusStreamPollInterval,
		streamHeartbeat: statusStreamHeartbeatInterval,
		streamMax:       statusStreamMaxDuration,
		maxBodyBytes:    defaultMaxBodyBytes,
	}
}

// WithMaxBodyBytes overrides the JSON body cap. A non-positive max keeps the
// default, so callers without configuration stay protected.
func (h *InstanceHandler) WithMaxBodyBytes(max int64) *InstanceHandler {
	if max > 0 {
		h.maxBodyBytes = max
	}
	return h
}

// Create handles POST /v1/workflow/instance.
//
// @Summary Create workflow instance
// @Tags workflow-instances
// @Accept json
// @Produce json
// @Param request body CreateInstanceRequest true "Workflow instance"
// @Success 202 {object} CreateInstanceResponse
// @Failure 401,403,400,404,409,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance [post]
func (h *InstanceHandler) Create(c *gin.Context) {
	var req CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body")
		return
	}
	inst, err := h.svc.Create(c.Request.Context(), service.CreateInstance{
		WorkflowDefinitionID: req.WorkflowDefinitionID,
		Context:              req.Context,
		Debug:                req.Debug,
		SnapshotForms:        req.SnapshotForms,
		Actor:                principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, CreateInstanceResponse{ID: inst.ID, Status: string(inst.Status)})
}

// List handles GET /v1/workflow/instance.
//
// @Summary List workflow instances
// @Tags workflow-instances
// @Produce json
// @Param page query int false "Page number"
// @Param per_page query int false "Items per page"
// @Param order query string false "Sort order"
// @Param id query []string false "Instance IDs"
// @Param workflow_definition_id query string false "Workflow definition ID"
// @Param status query []string false "Statuses"
// @Param debug query bool false "Filter by step-through (debug) runs"
// @Success 200 {object} ListResponse[InstanceSummaryResponse]
// @Failure 401,403,400,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance [get]
func (h *InstanceHandler) List(c *gin.Context) {
	q, err := ParseInstanceListQuery(c)
	if err != nil {
		WriteProblem(c, http.StatusBadRequest, err.Error())
		return
	}
	items, total, err := h.svc.List(c.Request.Context(), toInstanceListQuery(q), requestPrincipalValue(c))
	if err != nil {
		WriteError(c, err)
		return
	}
	resp := make([]InstanceSummaryResponse, 0, len(items))
	for _, inst := range items {
		resp = append(resp, toInstanceSummaryResponse(inst))
	}
	c.JSON(http.StatusOK, ListResponse[InstanceSummaryResponse]{
		Items:      resp,
		Page:       q.Page,
		PerPage:    q.PerPage,
		Total:      total,
		TotalPages: totalPages(total, q.PerPage),
	})
}

// Status handles GET /v1/workflow/instance/{id}/status.
//
// @Summary Get workflow instance status
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Success 200 {object} InstanceStatusResponse
// @Failure 401,403,404,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/status [get]
func (h *InstanceHandler) Status(c *gin.Context) {
	d, err := h.svc.GetStatusDetail(c.Request.Context(), c.Param("id"), requestPrincipalValue(c))
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toInstanceStatusResponse(d))
}

const (
	// statusStreamPollInterval is the default cadence at which a status
	// stream re-reads the instance. A poll that finds no change emits
	// nothing.
	statusStreamPollInterval = 500 * time.Millisecond
	// statusStreamHeartbeatInterval paces the `: ping` comments that keep
	// idle streams alive through proxies.
	statusStreamHeartbeatInterval = 15 * time.Second
	// statusStreamMaxDuration bounds one stream; the client reconnects
	// afterwards and receives the current snapshot first.
	statusStreamMaxDuration = 30 * time.Minute
	// statusStreamMinEmitInterval is the server-side emission floor:
	// bursts coalesce to the latest snapshot at ~20 events/s at most.
	statusStreamMinEmitInterval = 50 * time.Millisecond
)

// StatusStream handles GET /v1/workflow/instance/{id}/status/stream.
//
// @Summary Stream workflow instance status
// @Tags workflow-instances
// @Produce text/event-stream
// @Param id path string true "Instance ID"
// @Success 200 {object} InstanceStatusResponse
// @Failure 401,403,404,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/status/stream [get]
func (h *InstanceHandler) StatusStream(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	p := requestPrincipalValue(c)

	d, err := h.svc.GetStatusDetail(ctx, id, p)
	if err != nil {
		WriteError(c, err)
		return
	}
	// The first snapshot is encoded before the stream headers go out, so
	// a failure here still reports its HTTP status instead of a bare
	// truncated stream.
	first, err := json.Marshal(toInstanceStatusResponse(d))
	if err != nil {
		WriteProblem(c, http.StatusInternalServerError, "cannot encode status")
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	seq := 0
	emit := func(revision int64, event string, payload []byte) bool {
		seq++
		frame := statusStreamFrame(strconv.FormatInt(revision, 10)+"-"+strconv.Itoa(seq), event, payload)
		if _, err := c.Writer.Write(frame); err != nil {
			return false
		}
		flushStream(c)
		return true
	}
	// After the headers are sent the HTTP status can no longer change, so
	// a mid-stream failure is delivered as a terminal error event.
	emitError := func(revision int64, err error) {
		body, _ := json.Marshal(Problem{
			Type:   "about:blank",
			Title:  "Stream error",
			Status: StatusForError(err),
			Detail: err.Error(),
		})
		emit(revision, "error", body)
	}

	last := first
	lastRev := d.Instance.Revision
	if !emit(lastRev, "status", last) {
		return
	}
	if terminalWorkflowStatus(d.Instance.Status) {
		return
	}
	lastEmit := time.Now()

	poll := time.NewTicker(h.streamPoll)
	defer poll.Stop()
	heartbeat := time.NewTicker(h.streamHeartbeat)
	defer heartbeat.Stop()
	deadline := time.NewTimer(h.streamMax)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			if _, err := c.Writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flushStream(c)
		case <-poll.C:
			d, err := h.svc.GetStatusDetail(ctx, id, p)
			if err != nil {
				emitError(lastRev, err)
				return
			}
			payload, err := json.Marshal(toInstanceStatusResponse(d))
			if err != nil {
				emitError(d.Instance.Revision, err)
				return
			}
			if bytes.Equal(payload, last) {
				continue
			}
			// Every poll re-reads the full state, so skipping an emit
			// only delays delivery to the next poll; nothing is lost.
			if time.Since(lastEmit) < statusStreamMinEmitInterval {
				continue
			}
			last, lastRev = payload, d.Instance.Revision
			lastEmit = time.Now()
			if !emit(lastRev, "status", last) {
				return
			}
			if terminalWorkflowStatus(d.Instance.Status) {
				return
			}
		}
	}
}

// terminalWorkflowStatus reports whether no further status change can
// follow, so the stream may close after delivering it.
func terminalWorkflowStatus(s model.WorkflowStatus) bool {
	switch s {
	case model.WorkflowFinished, model.WorkflowFailed, model.WorkflowStopped:
		return true
	default:
		return false
	}
}

// statusStreamFrame renders one SSE event. Data is split so a multi-line
// payload stays a single event.
func statusStreamFrame(id, event string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("id: " + id + "\nevent: " + event + "\n")
	for _, line := range bytes.Split(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// flushStream pushes buffered bytes to the client. A writer without
// flushing still delivers the bytes, only later.
func flushStream(c *gin.Context) {
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// toInstanceStatusResponse maps a status detail onto the openapi
// InstanceStatus schema. Status and StatusStream share it so the REST and
// SSE payloads never drift.
func toInstanceStatusResponse(d *service.StatusDetail) InstanceStatusResponse {
	inst := d.Instance
	waitingReason := nullableString(string(inst.WaitingReason))
	errorMsg := nullableString(inst.Error)
	return InstanceStatusResponse{
		ID:                    inst.ID,
		WorkflowDefinitionID:  inst.WorkflowDefinitionID,
		ContextMode:           inst.ContextMode,
		Debug:                 inst.Debug,
		Status:                string(inst.Status),
		WaitingReason:         waitingReason,
		PauseRequested:        inst.PauseRequested,
		TerminationPending:    inst.TerminationPending,
		MigratedFrom:          nullableString(inst.MigratedFrom),
		SupersededBy:          nullableString(inst.SupersededBy),
		CurrentGroupID:        nullableString(inst.CurrentGroupID),
		CurrentNodeID:         nullableString(inst.CurrentNodeID),
		CurrentNodeInstanceID: nodeInstanceIDString(inst.ID, d.CurrentNodeInstanceID),
		Attempt:               d.Attempt,
		Counters:              inst.Counters,
		Nodes:                 toNodeOccurrenceResponses(d.Nodes),
		PendingInput:          toPendingInputResponse(d.PendingInput),
		Parallel:              toParallelResponses(d.Parallel),
		Error:                 errorMsg,
		StartedAt:             inst.StartedAt,
		FinishedAt:            inst.FinishedAt,
		CreatedBy:             inst.CreatedBy,
		UpdatedBy:             inst.UpdatedBy,
		CreatedAt:             inst.CreatedAt,
		UpdatedAt:             inst.UpdatedAt,
	}
}

// Context handles GET /v1/workflow/instance/{id}/context.
//
// @Summary Get workflow instance context
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Success 200 {object} InstanceContextResponse
// @Failure 401,403,404,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/context [get]
func (h *InstanceHandler) Context(c *gin.Context) {
	inst, err := h.svc.GetContext(c.Request.Context(), c.Param("id"), requestPrincipalValue(c))
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, InstanceContextResponse{ID: inst.ID, Context: inst.Context})
}

// UpdateContext handles PUT /v1/workflow/instance/{id}/context.
//
// @Summary Replace workflow instance context
// @Tags workflow-instances
// @Accept json
// @Produce json
// @Param id path string true "Instance ID"
// @Param X-Context-Update-Reason header string false "Audit reason"
// @Param request body object true "Full replacement context"
// @Success 200 {object} InstanceContextResponse
// @Failure 401,403,400,404,409,413,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/context [put]
func (h *InstanceHandler) UpdateContext(c *gin.Context) {
	body, err := io.ReadAll(h.cappedBody(c))
	if isBodyTooLarge(err) {
		WriteProblem(c, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	if err != nil || len(body) == 0 || !json.Valid(body) {
		WriteProblem(c, http.StatusBadRequest, "invalid request body")
		return
	}
	inst, err := h.svc.UpdateContext(c.Request.Context(), service.UpdateContext{
		InstanceID: c.Param("id"),
		Context:    body,
		Reason:     c.GetHeader("X-Context-Update-Reason"),
		Principal:  requestPrincipal(c),
		Actor:      principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, InstanceContextResponse{ID: inst.ID, Context: inst.Context})
}

// NodeDebug handles GET /v1/workflow/instance/{id}/status/node/{node_id}.
//
// @Summary Get workflow node debug detail
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Param node_id path string true "Node occurrence ID"
// @Param attempt query int false "Attempt number"
// @Success 200 {object} NodeDebugResponse
// @Failure 401,403,400,404,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/status/node/{node_id} [get]
func (h *InstanceHandler) NodeDebug(c *gin.Context) {
	attempt := 0
	if v := c.Query("attempt"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			WriteProblem(c, http.StatusBadRequest, "query: attempt must be a positive integer")
			return
		}
		attempt = n
	}
	d, err := h.svc.NodeDebug(c.Request.Context(), c.Param("id"), c.Param("node_id"), attempt, requestPrincipalValue(c))
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toNodeDebugResponse(d))
}

// DebugContext handles GET /v1/workflow/instance/{id}/debug/context.
//
// @Summary Get debug-position context as TypeScript
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Param node_id query string false "Graph node or occurrence ID (omitted selects the debug cursor)"
// @Param attempt query int false "Attempt number"
// @Success 200 {object} DebugContextResponse
// @Failure 401,403,400,404,409,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/debug/context [get]
func (h *InstanceHandler) DebugContext(c *gin.Context) {
	nodeID := c.Query("node_id")
	if nodeID != "" && !ids.Valid(nodeID) {
		WriteProblem(c, http.StatusBadRequest, "query: node_id must be a valid uuid")
		return
	}
	attempt := 0
	if v := c.Query("attempt"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			WriteProblem(c, http.StatusBadRequest, "query: attempt must be a positive integer")
			return
		}
		attempt = n
	}
	d, err := h.svc.DebugContext(c.Request.Context(), c.Param("id"), nodeID, attempt, requestPrincipalValue(c))
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDebugContextResponse(d))
}

// Input handles PUT /v1/workflow/instance/{id}/input.
//
// @Summary Deliver input to workflow instance
// @Tags workflow-instances
// @Accept json,multipart/form-data
// @Produce json
// @Param id path string true "Instance ID"
// @Param Idempotency-Key header string true "Idempotency key"
// @Param branch_id query string false "Parallel branch ID holding the parked input node (omit for the instance cursor)"
// @Param request body object false "Input payload (application/json; leave empty when sending multipart/form-data)"
// @Param files formData file false "File parts for multipart/form-data (repeatable; field name becomes the context key, a trailing [] merges with the bare name)"
// @Param field formData string false "Text field for multipart/form-data (repeatable; field name becomes the context key)"
// @Success 202 {object} InputDeliveryResponse
// @Failure 401,403,400,404,409,413,415,422,500 {object} Problem
// @Description This is the only endpoint that serves an anonymous caller, and only when the waiting input node sets public: true. No credential at all is admitted on a public node; a private node answers 403. A credential that is present but invalid is still 401, including on a public node, and never degrades to an accepted anonymous delivery. Status and form reads stay authenticated, so share the payload contract with an anonymous caller out-of-band.
// @Router /v1/workflow/instance/{id}/input [put]
func (h *InstanceHandler) Input(c *gin.Context) {
	mediaType, err := inputMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		WriteProblem(c, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	// The principal is passed through so the service can apply the input
	// node's user and role gates and record the deliverer. With
	// authentication off there is no principal, and a nil one is what the
	// service reads as the service principal: an unauthenticated deployment
	// keeps accepting deliveries on nodes that list allowed_roles or
	// allowed_users.
	//
	// A request that reached this handler with no principal while the route
	// serves anonymous callers is the public input node's case, and it is
	// flagged as such: the service admits it only on a public node and marks
	// the delivery anonymous. The flag comes from the middleware rather than
	// from the absence of a principal, because a missing principal also means
	// "authentication is off", and that deployment must keep the trusted
	// service-principal behavior it has always had.
	principal, authenticated := PrincipalFrom(c)
	req := service.DeliverInput{
		InstanceID:     c.Param("id"),
		BranchID:       c.Query("branch_id"),
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
		Source:         model.InputChannelHTTP,
		ContentType:    mediaType,
	}
	if mediaType == model.MediaTypeMultipart {
		// The body streams through the service without being read here:
		// parts upload only after the delivery is authorized.
		req.MultipartSource = func() (*stdmultipart.Reader, error) {
			return c.Request.MultipartReader()
		}
	} else {
		body, err := io.ReadAll(h.cappedBody(c))
		if isBodyTooLarge(err) {
			WriteProblem(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		if err != nil {
			WriteProblem(c, http.StatusBadRequest, "cannot read request body")
			return
		}
		req.Payload = body
	}
	if authenticated {
		req.Principal = &principal
		req.Actor = principalActor(c)
	} else {
		req.Anonymous = AnonymousFrom(c)
	}
	delivery, err := h.svc.DeliverInput(c.Request.Context(), req)
	if err != nil {
		WriteError(c, err)
		return
	}
	if !delivery.Accepted {
		WriteProblem(c, http.StatusUnprocessableEntity, delivery.Error)
		return
	}
	c.JSON(http.StatusAccepted, InputDeliveryResponse{Accepted: true})
}

// inputMediaType parses the delivery Content-Type into the media type the
// service matches against the node's accept list. An absent header means
// JSON, which is what every client that predates accept sends; anything but
// JSON or multipart is a 415.
func inputMediaType(header string) (string, error) {
	if strings.TrimSpace(header) == "" {
		return model.MediaTypeJSON, nil
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", fmt.Errorf("unsupported content type %q: expected application/json or multipart/form-data", header)
	}
	switch strings.ToLower(mediaType) {
	case model.MediaTypeJSON:
		return model.MediaTypeJSON, nil
	case model.MediaTypeMultipart:
		return model.MediaTypeMultipart, nil
	default:
		return "", fmt.Errorf("unsupported content type %q: expected application/json or multipart/form-data", mediaType)
	}
}

// cappedBody wraps the request body with the handler's size cap. Reads past
// the cap fail with the http.MaxBytesReader overflow error, which callers map
// to 413 via isBodyTooLarge. A non-positive cap falls back to the default so
// a zero-value handler stays protected.
func (h *InstanceHandler) cappedBody(c *gin.Context) io.ReadCloser {
	max := h.maxBodyBytes
	if max <= 0 {
		max = defaultMaxBodyBytes
	}
	return http.MaxBytesReader(c.Writer, c.Request.Body, max)
}

// isBodyTooLarge reports whether err is the http.MaxBytesReader overflow.
// net/http exposes no sentinel for it, so the stable stdlib message is matched.
func isBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

// Pause handles POST /v1/workflow/instance/{id}/pause.
//
// @Summary Pause workflow instance
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Success 200 {object} PauseResponse
// @Success 202 {object} PauseResponse
// @Failure 401,403,404,409,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/pause [post]
func (h *InstanceHandler) Pause(c *gin.Context) {
	res, err := h.svc.Pause(c.Request.Context(), service.ControlRequest{
		InstanceID: c.Param("id"),
		Principal:  requestPrincipal(c),
		Actor:      principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	status := http.StatusOK
	if res.PauseRequested {
		status = http.StatusAccepted
	}
	c.JSON(status, PauseResponse{Status: string(res.Status), PauseRequested: res.PauseRequested})
}

// Resume handles POST /v1/workflow/instance/{id}/resume.
//
// @Summary Resume workflow instance
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Success 200 {object} ResumeResponse
// @Failure 401,403,404,409,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/resume [post]
func (h *InstanceHandler) Resume(c *gin.Context) {
	res, err := h.svc.Resume(c.Request.Context(), service.ControlRequest{
		InstanceID: c.Param("id"),
		Principal:  requestPrincipal(c),
		Actor:      principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, ResumeResponse{Status: string(res.Status)})
}

// Stop handles POST /v1/workflow/instance/{id}/stop.
//
// @Summary Stop workflow instance
// @Tags workflow-instances
// @Produce json
// @Param id path string true "Instance ID"
// @Success 200 {object} StopResponse
// @Failure 401,403,404,409,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/stop [post]
func (h *InstanceHandler) Stop(c *gin.Context) {
	res, err := h.svc.Stop(c.Request.Context(), service.ControlRequest{
		InstanceID: c.Param("id"),
		Reason:     "operator",
		Principal:  requestPrincipal(c),
		Actor:      principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, StopResponse{Status: string(res.Status), TerminationPending: res.TerminationPending})
}

// Rollback handles POST /v1/workflow/instance/{id}/rollback.
//
// @Summary Roll back a paused or failed instance to a prior node occurrence
// @Tags workflow-instances
// @Accept json
// @Produce json
// @Param id path string true "Instance ID"
// @Param request body RollbackRequest true "Rollback target"
// @Success 200 {object} RollbackResponse
// @Failure 401,403,400,404,409,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/rollback [post]
func (h *InstanceHandler) Rollback(c *gin.Context) {
	var req RollbackRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.TargetOccurrenceID == "" {
		WriteProblem(c, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.svc.Rollback(c.Request.Context(), service.RollbackRequest{
		InstanceID:         c.Param("id"),
		TargetOccurrenceID: req.TargetOccurrenceID,
		Reason:             req.Reason,
		Principal:          requestPrincipal(c),
		Actor:              principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, RollbackResponse{Status: string(res.Status), CurrentNodeID: res.CurrentNodeID})
}

// Migrate handles POST /v1/workflow/instance/{id}/migrate.
//
// @Summary Migrate a paused, failed, or input-parked instance to a newer definition
// @Tags workflow-instances
// @Accept json
// @Produce json
// @Param id path string true "Source instance ID"
// @Param request body MigrateRequest true "Migration target"
// @Success 201 {object} MigrateResponse
// @Failure 401,403,400,404,409,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/workflow/instance/{id}/migrate [post]
func (h *InstanceHandler) Migrate(c *gin.Context) {
	var req MigrateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.WorkflowDefinitionID == "" {
		WriteProblem(c, http.StatusBadRequest, "workflow_definition_id is required")
		return
	}
	if req.StartNodeID == "" {
		WriteProblem(c, http.StatusBadRequest, "start_node_id is required")
		return
	}
	startPaused := req.StartPaused == nil || *req.StartPaused
	res, err := h.svc.Migrate(c.Request.Context(), service.MigrateRequest{
		SourceInstanceID:     c.Param("id"),
		WorkflowDefinitionID: req.WorkflowDefinitionID,
		StartNodeID:          req.StartNodeID,
		ContextPatch:         req.ContextPatch,
		StartPaused:          startPaused,
		Reason:               req.Reason,
		Principal:            requestPrincipal(c),
		Actor:                principalActor(c),
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	nxt := res.Instance
	cursor, _ := model.ParseFrame(nxt.Frame)
	c.JSON(http.StatusCreated, MigrateResponse{
		Instance: MigrateInstanceResponse{
			ID:                   nxt.ID,
			WorkflowDefinitionID: nxt.WorkflowDefinitionID,
			Status:               string(nxt.Status),
			WaitingReason:        nullableString(string(nxt.WaitingReason)),
			CurrentNodeID:        nullableString(cursor.CurrentNodeID),
			MigratedFrom:         nullableString(nxt.MigratedFrom),
			CreatedAt:            nxt.CreatedAt,
			UpdatedAt:            nxt.UpdatedAt,
		},
		SourceInstanceID: c.Param("id"),
		SourceStatus:     string(res.SourceStatus),
	})
}

// toNodeOccurrenceResponses maps the service nodes view onto the openapi
// NodeOccurrence schema. A nil map stays nil so the field is omitted.
func toNodeOccurrenceResponses(nodes map[string]service.NodeOccurrence) map[string]NodeOccurrenceResponse {
	if nodes == nil {
		return nil
	}
	out := make(map[string]NodeOccurrenceResponse, len(nodes))
	for id, e := range nodes {
		out[id] = NodeOccurrenceResponse{
			OccurrenceID: e.OccurrenceID,
			Status:       e.Status,
			Attempt:      e.Attempt,
			Rollbackable: e.Rollbackable,
		}
	}
	return out
}

// toPendingInputResponse maps the waiting-input contract onto the openapi
// PendingInput schema. Nil stays nil so the field is omitted; a formless
// input node omits form_id.
func toPendingInputResponse(p *service.PendingInput) *PendingInputResponse {
	if p == nil {
		return nil
	}
	allowed := p.AllowedRoles
	if allowed == nil {
		allowed = []string{}
	}
	users := make([]AllowedUserResponse, 0, len(p.AllowedUsers))
	for _, u := range p.AllowedUsers {
		users = append(users, AllowedUserResponse{ID: u.ID})
	}
	accept := p.Accept
	if accept == nil {
		accept = []string{}
	}
	var form *SnapshotFormResponse
	if p.Form != nil {
		form = &SnapshotFormResponse{Schema: p.Form.Schema, UI: p.Form.UI}
	}
	return &PendingInputResponse{
		NodeID:         p.NodeID,
		Channel:        p.Channel,
		OutputProperty: p.OutputProperty,
		FormID:         p.FormID,
		Form:           form,
		AllowedRoles:   allowed,
		AllowedUsers:   users,
		RecordActor:    p.RecordActor,
		Public:         p.Public,
		Accept:         accept,
	}
}

// toParallelResponses maps the service parallel tree onto the openapi
// ParallelExecution schema. Nil stays nil so the field is omitted.
func toParallelResponses(exs []service.ParallelExecutionView) []ParallelExecutionResponse {
	if exs == nil {
		return nil
	}
	out := make([]ParallelExecutionResponse, 0, len(exs))
	for _, ex := range exs {
		v := ParallelExecutionResponse{
			ID:             ex.ID,
			ParentBranchID: ex.ParentBranchID,
			Depth:          ex.Depth,
			StartNodeID:    ex.StartNodeID,
			EndNodeID:      ex.EndNodeID,
			Status:         ex.Status,
			BranchCount:    ex.BranchCount,
			CompletedCount: ex.CompletedCount,
			Branches:       make([]ParallelBranchResponse, 0, len(ex.Branches)),
		}
		for _, b := range ex.Branches {
			v.Branches = append(v.Branches, ParallelBranchResponse{
				ID:            b.ID,
				Name:          b.Name,
				BranchIndex:   b.BranchIndex,
				StartNodeID:   b.StartNodeID,
				Status:        b.Status,
				WaitingReason: b.WaitingReason,
				Error:         b.Error,
				UpdatedAt:     b.UpdatedAt,
			})
		}
		out = append(out, v)
	}
	return out
}

// toInstanceListQuery converts a parsed list query into the repository query.
func toInstanceListQuery(q *InstanceListQuery) repository.InstanceListQuery {
	return repository.InstanceListQuery{
		Page:                 q.Page,
		PerPage:              q.PerPage,
		Order:                q.Order,
		IDs:                  q.IDs,
		WorkflowDefinitionID: q.WorkflowDefinitionID,
		Statuses:             q.Statuses,
		Debug:                q.Debug,
	}
}

// toInstanceSummaryResponse maps an instance onto the compact list DTO.
func toInstanceSummaryResponse(inst model.WorkflowInstance) InstanceSummaryResponse {
	return InstanceSummaryResponse{
		ID:                   inst.ID,
		WorkflowDefinitionID: inst.WorkflowDefinitionID,
		Status:               string(inst.Status),
		WaitingReason:        nullableString(string(inst.WaitingReason)),
		PauseRequested:       inst.PauseRequested,
		TerminationPending:   inst.TerminationPending,
		Debug:                inst.Debug,
		Error:                nullableString(inst.Error),
		StartedAt:            inst.StartedAt,
		FinishedAt:           inst.FinishedAt,
		CreatedBy:            inst.CreatedBy,
		UpdatedBy:            inst.UpdatedBy,
		CreatedAt:            inst.CreatedAt,
		UpdatedAt:            inst.UpdatedAt,
	}
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nodeInstanceIDString(instanceID string, occurrence *string) *string {
	if occurrence == nil {
		return nil
	}
	id := model.NodeInstanceID{WorkflowInstanceID: instanceID, OccurrenceID: *occurrence}.String()
	return &id
}

// toNodeDebugResponse maps the service detail onto the openapi NodeDebug
// schema. nil raw messages render as JSON null.
func toNodeDebugResponse(d *service.NodeDebugDetail) NodeDebugResponse {
	return NodeDebugResponse{
		OccurrenceID:           d.OccurrenceID,
		SourceNodeDefinitionID: d.SourceNodeDefinitionID,
		Name:                   d.Name,
		Type:                   d.Type,
		SelectedAttempt:        d.SelectedAttempt,
		LatestAttempt:          d.LatestAttempt,
		AttemptCount:           d.AttemptCount,
		Status:                 d.Status,
		ContextBefore:          d.ContextBefore,
		ContextAfter:           d.ContextAfter,
		Input:                  d.Input,
		Output:                 d.Output,
		Error:                  d.Error,
		RecoveryPolicy:         d.RecoveryPolicy,
		RecoveryResult:         d.RecoveryResult,
		Cancelled:              d.Cancelled,
		StartedAt:              d.StartedAt,
		FinishedAt:             d.FinishedAt,
		StoppedAt:              d.StoppedAt,
		DurationMS:             d.DurationMS,
		CreatedAt:              d.CreatedAt,
		UpdatedAt:              d.UpdatedAt,
	}
}

// toDebugContextResponse maps the service detail onto the openapi
// DebugContext schema.
func toDebugContextResponse(d *service.DebugContextDetail) DebugContextResponse {
	return DebugContextResponse{
		InstanceID:    d.InstanceID,
		NodeID:        d.NodeID,
		OccurrenceID:  d.OccurrenceID,
		Attempt:       d.Attempt,
		IsDebugPaused: d.IsDebugPaused,
		TypeScript:    d.TypeScript,
	}
}
