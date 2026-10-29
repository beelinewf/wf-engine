// Package service implements the use-case orchestration for immutable
// definitions, workflow instances, input delivery, and runtime controls.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/kernel"
	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/multipart"
	"github.com/beelinewf/wf-engine/internal/workflow/objectstore"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/pkg/contextdiff"
	"github.com/beelinewf/wf-engine/pkg/envsnapshot"
)

// CreateInstance starts a workflow instance. Debug marks a step-through
// run: the instance starts paused and re-pauses after every node until
// termination. It is immutable after create.
type CreateInstance struct {
	WorkflowDefinitionID string
	Context              json.RawMessage
	Debug                bool
	// SnapshotForms freezes every form the definition references into the
	// instance row at create; status then serves the frozen bytes. Off by
	// default: status serves only form_id and the frontend reads the live
	// forms library.
	SnapshotForms bool
	// Actor is the users.id uuid recorded as created_by/updated_by. An
	// empty Actor falls back to the service default (the system user), so
	// a caller that predates per-request actors keeps working unchanged.
	Actor string
}

// UpdateContext replaces the full context of a paused instance. Reason is an
// optional audit annotation recorded on the context_updated event.
type UpdateContext struct {
	InstanceID string
	Context    json.RawMessage
	Reason     string
	// Principal is the caller. A nil Principal is the service principal
	// (API token, broker, or internal caller), which bypasses the object
	// check exactly as it bypasses the endpoint gate.
	Principal *auth.Principal
	// Actor is the users.id uuid recorded on the update. Empty falls back
	// to the resolved principal's user id and then to the service default.
	Actor string
}

// DeliverInput delivers a payload to a waiting input node. Source is the
// transport the payload arrived on ("http", "redis", or "rabbitmq"); an
// empty Source defaults to "http".
type DeliverInput struct {
	InstanceID string
	// BranchID targets a branch-parked input node instead of the instance
	// cursor. Empty selects the instance cursor (the parent scope).
	BranchID       string
	IdempotencyKey string
	Payload        []byte
	Source         string
	// ContentType is the request media type ("application/json" or
	// "multipart/form-data"). Empty means JSON, which is what every
	// caller that predates accept sends.
	ContentType string
	// MultipartSource opens the streaming multipart reader. Required when
	// ContentType is multipart/form-data; ignored otherwise. The stream
	// is pulled only after the delivery is authorized, so a refused
	// caller uploads nothing.
	MultipartSource multipart.ReaderFactory
	// Principal is the caller. The API-token and broker paths pass the
	// service principal, which skips both authorization gates but is still
	// recorded as the deliverer. A nil Principal means the request carried
	// no credential at all: an anonymous caller. It is refused everywhere
	// except a public input node, which accepts it and marks the delivery
	// as anonymous in the audit trail.
	Principal *auth.Principal
	// Actor is the users.id uuid recorded on the delivery and the audit
	// event. Empty falls back to the resolved principal's user id and then
	// to the service default.
	Actor string
	// Anonymous is true when the HTTP layer saw no credential on a route
	// whose auth is optional. It separates "the caller proved nothing" from
	// "the caller proved the wrong thing": an anonymous request is refused
	// on a private node, and on a public node it is accepted and audited as
	// anonymous rather than as the service principal. The broker paths
	// leave it false so they keep their trusted-internal bypass.
	Anonymous bool
}

// StatusDetail is the status view with the current node occurrence resolved.
type StatusDetail struct {
	Instance              model.WorkflowInstance
	CurrentNodeInstanceID *string
	Attempt               int
	// Nodes maps every workflow graph node id (groups included) to its
	// occurrence state. Nil when the definition cannot be loaded, so the
	// status response omits the map instead of failing.
	Nodes map[string]NodeOccurrence
	// PendingInput describes the input the instance waits for. Non-nil only
	// when the instance waits on an input node; FormID is empty when that
	// node references no form.
	PendingInput *PendingInput
	// Parallel lists every parallel execution of the instance in creation
	// order with its branches. Nil when the instance never forked. Loops
	// fork repeatedly, so this is a list rather than the single object an
	// early sketch showed; nested executions link to their owner branch
	// via ParentBranchID.
	Parallel []ParallelExecutionView
}

// ParallelExecutionView is one parallel fork/join scope on status: cursors
// and counters without lease internals or branch contexts.
type ParallelExecutionView struct {
	ID             string
	ParentBranchID *string
	Depth          int
	StartNodeID    string
	EndNodeID      string
	Status         string
	BranchCount    int
	CompletedCount int
	Branches       []ParallelBranchView
}

// ParallelBranchView is one branch on status: identity, lifecycle state,
// and the last error. WaitingReason is "" unless the branch waits.
type ParallelBranchView struct {
	ID            string
	Name          string
	BranchIndex   int
	StartNodeID   string
	Status        string
	WaitingReason string
	Error         string
	UpdatedAt     time.Time
}

// PendingInput is the waiting-input contract served on status: the frontend
// fetches the form named by FormID from the forms library, renders it, then
// delivers the payload.
// SnapshotForm is one frozen forms-library entry served on a snapshot
// instance's pending input: the schema/ui bytes as they were when the
// instance started.
type SnapshotForm struct {
	Schema json.RawMessage
	UI     json.RawMessage
}

type PendingInput struct {
	NodeID         string
	Channel        string
	OutputProperty string
	// FormID names the forms-library entry the frontend renders. Empty when
	// the node references no form. The engine never resolves it.
	FormID string
	// Form carries the frozen bytes when the instance was created with
	// snapshot mode on. Nil for live instances and for formless nodes.
	Form *SnapshotForm
	// AllowedRoles is the node's role gate, so a frontend can hide or
	// explain a delivery the caller's roles would be refused for. It is
	// empty when the node is open; when AllowedUsers is also non-empty a
	// listed role still admits the caller, and a public node reports it
	// empty: the node ignores both gates, so an authored list must never
	// hide its form.
	AllowedRoles []string
	// AllowedUsers is the node's user gate in the same [{"id": ...}] shape
	// the definition carries. Like AllowedRoles it is reported empty on a
	// public node.
	AllowedUsers []model.AllowedUser
	// RecordActor reports whether an accepted delivery is written as the
	// attribution envelope {user_id, input_data}, so the frontend builds
	// the follow-up form against the right shape.
	RecordActor bool
	// Public reports that the node accepts anonymous deliveries over HTTP
	// with no credential, even when auth is on. While the instance parks
	// here, anonymous callers read this contract from the public status
	// projection and the form from the public form read.
	Public bool
	// Accept lists the request media types the node delivers, so a
	// frontend knows whether multipart is welcome. It is never empty:
	// nodes that predate accept report JSON only.
	Accept []string
}

// formSnapshotEntry is one frozen forms-library entry: the schema/ui bytes
// as they were when the instance started. UI is omitted when the form has
// none.
type formSnapshotEntry struct {
	Schema json.RawMessage `json:"schema"`
	UI     json.RawMessage `json:"ui,omitempty"`
}

// parseFormSnapshot decodes the frozen forms map. A corrupt value degrades
// to nil so status still serves form_id.
func parseFormSnapshot(raw json.RawMessage) map[string]formSnapshotEntry {
	var snap map[string]formSnapshotEntry
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil
	}
	return snap
}

// NodeOccurrence is the per-node status view: the already-executed
// occurrence id (nil when never ran) plus whether it is a valid rollback
// target. Rollbackable is advisory and instance-aware (false unless the
// instance itself is paused or failed); the rollback endpoint stays the
// source of truth.
type NodeOccurrence struct {
	OccurrenceID *string
	Status       string
	Attempt      *int
	Rollbackable bool
}

// NodeDebugDetail is the resolved node-debug view. Occurrences that never ran
// carry status "not_started", nil attempts, and empty snapshots.
type NodeDebugDetail struct {
	OccurrenceID           string
	SourceNodeDefinitionID string
	Name                   string
	Type                   string
	SelectedAttempt        *int
	LatestAttempt          *int
	AttemptCount           int
	Status                 string
	ContextBefore          json.RawMessage
	ContextAfter           json.RawMessage
	Input                  json.RawMessage
	Output                 json.RawMessage
	Error                  *string
	RecoveryPolicy         *string
	RecoveryResult         *string
	Cancelled              bool
	StartedAt              *time.Time
	FinishedAt             *time.Time
	StoppedAt              *time.Time
	DurationMS             *int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// DebugContextDetail is the resolved debug-position context view: the
// redacted snapshot rendered as a TypeScript declaration. OccurrenceID
// and Attempt are nil when the position has no occurrence (the live
// instance context is the source).
type DebugContextDetail struct {
	InstanceID    string
	NodeID        string
	OccurrenceID  *string
	Attempt       *int
	IsDebugPaused bool
	TypeScript    string
}

// ControlResult reports the post-transition state of a pause/resume/stop
// control call.
type ControlResult struct {
	Status             model.WorkflowStatus
	PauseRequested     bool
	TerminationPending bool
}

// ControlRequest is the input for a pause, resume, or stop control. The
// three share one shape so a single caller identity carries through every
// control path.
type ControlRequest struct {
	InstanceID string
	// Reason is the caller-supplied stop reason, recorded on the event.
	Reason string
	// Principal is the caller. A nil Principal is the service principal
	// (API token, broker, or internal caller), which bypasses the object
	// check exactly as it bypasses the endpoint gate.
	Principal *auth.Principal
	// Actor is the users.id uuid recorded on the control event. Empty falls
	// back to the resolved principal's user id and then to the service
	// default, so a service call or an unauthenticated deployment keeps
	// working.
	Actor string
}

// RollbackRequest moves a paused or failed instance's cursor back to an
// already-executed node occurrence. Reason is an optional audit annotation
// recorded on the rollback event only.
type RollbackRequest struct {
	InstanceID         string
	TargetOccurrenceID string
	Reason             string
	// Principal is the caller. A nil Principal is the service principal
	// (API token, broker, or internal caller), which bypasses the object
	// check exactly as it bypasses the endpoint gate.
	Principal *auth.Principal
	// Actor is the users.id uuid recorded on the rollback. Empty falls back
	// to the resolved principal's user id and then to the service default.
	Actor string
}

// RollbackResult reports the post-rollback cursor: the instance is always
// paused, parked on CurrentNodeID with GroupStack recomputed from the
// materialized definition.
type RollbackResult struct {
	Status        model.WorkflowStatus
	CurrentNodeID string
	GroupStack    []string
}

// MigrateRequest moves a paused, failed, or input-parked instance onto a
// newer definition of the same lineage, starting a fresh run at
// StartNodeID with the source's user context carried over.
type MigrateRequest struct {
	SourceInstanceID     string
	WorkflowDefinitionID string
	StartNodeID          string
	// ContextPatch deep-merges over the carried source context (objects
	// merge, arrays and scalars replace). Empty or null means no patch; a
	// present patch must decode to a JSON object.
	ContextPatch json.RawMessage
	// StartPaused parks the replacement paused for inspect-fix-resume. The
	// handler defaults an absent flag to true; the service takes it
	// literally.
	StartPaused bool
	// Reason is an audit annotation recorded on both sides' events.
	Reason string
	// Principal is the caller. A nil Principal is the service principal
	// (API token, broker, or internal caller), which bypasses the object
	// check exactly as it bypasses the endpoint gate.
	Principal *auth.Principal
	// Actor is the users.id uuid recorded on the migration. Empty falls back
	// to the resolved principal's user id and then to the service default.
	Actor string
}

// MigrateResult reports the replacement run and the stopped source.
type MigrateResult struct {
	Instance     model.WorkflowInstance
	SourceStatus model.WorkflowStatus
}

// Canceller interrupts the local in-flight transition of an instance. The
// engine implements it via its cancellation registry; a nil Canceller
// disables local cancellation (cross-replica propagation still works
// through the dispatcher heartbeat).
type Canceller interface {
	Cancel(instanceID string)
}

// WorkflowMaterializer resolves node_definition_id references into the
// executable node tree. DeliverInput must use the same materialized graph
// as the engine so input nodes supplied through reusable node definitions
// are recognized.
type WorkflowMaterializer interface {
	Materialize(ctx context.Context, wc *model.WorkflowContent) (*model.WorkflowContent, error)
	// Resolve loads a stored definition by ID and returns its cached
	// materialized tree. The returned tree is shared and READ-ONLY.
	Resolve(ctx context.Context, defID string) (*model.WorkflowContent, error)
}

// InstanceService is the use-case boundary for workflow instances. Read
// and control calls take the caller's resolved users.id as actor alongside
// the request principal where one exists: the route permission gate answers
// "may this caller act at all", while the service answers "may this caller
// touch this instance" from the principal, so a service principal (API
// token, broker, or internal caller) bypasses the object check exactly as
// it bypasses the endpoint gate.
type InstanceService interface {
	Create(ctx context.Context, req CreateInstance) (model.WorkflowInstance, error)
	GetStatus(ctx context.Context, id string, p auth.Principal) (*model.WorkflowInstance, error)
	// GetStatusDetail resolves the current node occurrence for the status
	// response.
	GetStatusDetail(ctx context.Context, id string, p auth.Principal) (*StatusDetail, error)
	// List returns the instances matching the query with pagination and
	// ordering, mirroring the repository query.
	List(ctx context.Context, q repository.InstanceListQuery, p auth.Principal) ([]model.WorkflowInstance, int64, error)
	GetContext(ctx context.Context, id string, p auth.Principal) (*model.WorkflowInstance, error)
	// UpdateContext replaces the full context of a paused instance. The
	// body must be a JSON object; a non-paused instance conflicts.
	UpdateContext(ctx context.Context, req UpdateContext) (*model.WorkflowInstance, error)
	// DeliverInput validates, records, and resumes an input delivery. The
	// Source (transport) must match the channel of the input node the
	// instance is parked on. A rejected payload yields a delivery with
	// Accepted=false and the validation message in Error. Replays of an
	// idempotency key return the originally recorded delivery.
	DeliverInput(ctx context.Context, req DeliverInput) (*model.InputDelivery, error)
	// NodeDebug resolves the debug detail for one node occurrence of an
	// instance. nodeID is either the workflow graph node id or the
	// occurrence id; attempt <= 0 selects the latest attempt, a positive
	// attempt selects an exact loop execution.
	NodeDebug(ctx context.Context, instanceID, nodeID string, attempt int, p auth.Principal) (*NodeDebugDetail, error)
	// DebugContext resolves the debug-position context of a debug instance
	// as a redacted TypeScript declaration plus inferred JSON Schema.
	// nodeID is either the workflow graph node id or the occurrence id;
	// empty selects the debug cursor (falling back to the live instance
	// context when the cursor is empty or terminal). attempt <= 0 selects
	// the latest attempt, a positive attempt selects an exact execution.
	DebugContext(ctx context.Context, instanceID, nodeID string, attempt int, p auth.Principal) (*DebugContextDetail, error)
	// Pause pauses a waiting instance immediately and requests a deferred
	// pause for a running instance. Idempotent while paused.
	Pause(ctx context.Context, req ControlRequest) (*ControlResult, error)
	// Resume returns a paused instance to waiting and clears a pending
	// pause on a running instance. Idempotent on active instances.
	Resume(ctx context.Context, req ControlRequest) (*ControlResult, error)
	// Stop moves an instance to the terminal stopped state, fences worker
	// commits, and signals local cancellation. Idempotent on stopped
	// instances.
	Stop(ctx context.Context, req ControlRequest) (*ControlResult, error)
	// Rollback moves a paused or failed instance's cursor back to an
	// already-executed node occurrence so the next resume re-executes
	// forward from there. The instance is always paused afterwards and its
	// context is restored from the target occurrence's ContextBefore.
	// History is immutable; the next execution increments the target
	// occurrence's attempt. A live parked input attempt never blocks the
	// rollback: targeting the park itself is a no-op, any other target
	// supersedes (closes) it atomically in the same transaction.
	Rollback(ctx context.Context, req RollbackRequest) (*RollbackResult, error)
	// Migrate moves a paused, failed, or input-parked instance onto a
	// newer definition of the same lineage, starting a fresh run at
	// StartNodeID with the source's context carried over. The source is
	// stopped atomically in the same transaction and linked both ways
	// (migrated_from/superseded_by); its history and error stay behind.
	// The start node must sit in the target graph outside any parallel
	// region's interior. The replacement starts paused when StartPaused
	// parks it for inspection, waiting otherwise.
	Migrate(ctx context.Context, req MigrateRequest) (*MigrateResult, error)
}

type instanceService struct {
	instances    repository.InstanceRepository
	parallel     repository.ParallelRepository
	wfDefs       repository.WorkflowDefinitionRepository
	secrets      SecretSnapshotter
	forms        repository.FormRepository
	materializer WorkflowMaterializer
	validator    *executor.InputExecutor
	hooks        *executor.HookRunner
	actor        string
	limits       model.NodeLimits
	cancels      Canceller
	leanOptions  model.LeanOptions
	// catalog is the shared role catalog used to turn a caller's roles
	// into the permissions the input endpoint gate checks.
	catalog *auth.Catalog
	// multipart carries the object store and streaming limits multipart
	// input deliveries upload through.
	multipart MultipartDeps
	metrics   *metrics.Recorder
}

// SetMetrics installs the optional metrics recorder. Nil disables recording.
func (s *instanceService) SetMetrics(rec *metrics.Recorder) {
	s.metrics = rec
}

// MultipartDeps carries the object store and streaming limits multipart
// input deliveries upload through. A nil Store means storage is disabled:
// definitions still parse, but a multipart delivery fails at delivery time.
type MultipartDeps struct {
	Store  objectstore.Store
	Limits multipart.Limits
}

// NewInstanceService builds the instance service. cancels may be nil; when
// set it receives the local cancellation signal for stopped instances. The
// catalog is consulted only by the input authorization gate.
func NewInstanceService(
	instances repository.InstanceRepository,
	parallel repository.ParallelRepository,
	wfDefs repository.WorkflowDefinitionRepository,
	secrets SecretSnapshotter,
	forms repository.FormRepository,
	materializer WorkflowMaterializer,
	validator *executor.InputExecutor,
	hooks *executor.HookRunner,
	actor string,
	limits model.NodeLimits,
	cancels Canceller,
	leanOptions model.LeanOptions,
) InstanceService {
	return newInstanceService(
		instances, parallel, wfDefs, secrets, forms, materializer, validator, hooks,
		actor, limits, cancels, leanOptions, auth.NewCatalog(nil), MultipartDeps{},
	)
}

// NewInstanceServiceWithCatalog is NewInstanceService plus the role catalog
// the input authorization gate reads. An empty catalog denies every
// resource-action, which is the correct default for a deployment that never
// configured roles.
func NewInstanceServiceWithCatalog(
	instances repository.InstanceRepository,
	parallel repository.ParallelRepository,
	wfDefs repository.WorkflowDefinitionRepository,
	secrets SecretSnapshotter,
	forms repository.FormRepository,
	materializer WorkflowMaterializer,
	validator *executor.InputExecutor,
	hooks *executor.HookRunner,
	actor string,
	limits model.NodeLimits,
	cancels Canceller,
	leanOptions model.LeanOptions,
	catalog *auth.Catalog,
) InstanceService {
	return newInstanceService(
		instances, parallel, wfDefs, secrets, forms, materializer, validator, hooks,
		actor, limits, cancels, leanOptions, catalog, MultipartDeps{},
	)
}

// NewInstanceServiceWithMultipart is NewInstanceServiceWithCatalog plus the
// object store and streaming limits multipart input deliveries upload
// through. Production passes the configured store; tests substitute a fake.
func NewInstanceServiceWithMultipart(
	instances repository.InstanceRepository,
	parallel repository.ParallelRepository,
	wfDefs repository.WorkflowDefinitionRepository,
	secrets SecretSnapshotter,
	forms repository.FormRepository,
	materializer WorkflowMaterializer,
	validator *executor.InputExecutor,
	hooks *executor.HookRunner,
	actor string,
	limits model.NodeLimits,
	cancels Canceller,
	leanOptions model.LeanOptions,
	catalog *auth.Catalog,
	mp MultipartDeps,
) InstanceService {
	return newInstanceService(
		instances, parallel, wfDefs, secrets, forms, materializer, validator, hooks,
		actor, limits, cancels, leanOptions, catalog, mp,
	)
}

func newInstanceService(
	instances repository.InstanceRepository,
	parallel repository.ParallelRepository,
	wfDefs repository.WorkflowDefinitionRepository,
	secrets SecretSnapshotter,
	forms repository.FormRepository,
	materializer WorkflowMaterializer,
	validator *executor.InputExecutor,
	hooks *executor.HookRunner,
	actor string,
	limits model.NodeLimits,
	cancels Canceller,
	leanOptions model.LeanOptions,
	catalog *auth.Catalog,
	mp MultipartDeps,
) InstanceService {
	return &instanceService{
		instances:    instances,
		parallel:     parallel,
		wfDefs:       wfDefs,
		secrets:      secrets,
		forms:        forms,
		materializer: materializer,
		validator:    validator,
		hooks:        hooks,
		actor:        actor,
		limits:       limits,
		cancels:      cancels,
		leanOptions:  leanOptions,
		catalog:      catalog,
		multipart:    mp,
	}
}

// createdEventData renders the instance_created audit payload. Debug runs
// carry debug:true so step-through runs are distinguishable in the trail.
func createdEventData(definitionID string, debug bool) json.RawMessage {
	if !debug {
		return json.RawMessage(`{"workflow_definition_id":"` + definitionID + `"}`)
	}
	return json.RawMessage(`{"workflow_definition_id":"` + definitionID + `","debug":true}`)
}

func (s *instanceService) Create(ctx context.Context, req CreateInstance) (model.WorkflowInstance, error) {
	if strings.TrimSpace(req.WorkflowDefinitionID) == "" {
		return model.WorkflowInstance{}, fmt.Errorf("%w: workflow_definition_id is required", model.ErrInvalid)
	}
	def, err := s.wfDefs.GetByID(ctx, req.WorkflowDefinitionID)
	if err != nil {
		return model.WorkflowInstance{}, err
	}
	wc, err := model.ParseWorkflowContent(def.Content, s.limits)
	if err != nil {
		return model.WorkflowInstance{}, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}
	contextRaw := req.Context
	if len(contextRaw) == 0 || string(contextRaw) == "null" {
		contextRaw = json.RawMessage("{}")
	}
	var obj map[string]any
	if err := json.Unmarshal(contextRaw, &obj); err != nil || obj == nil {
		return model.WorkflowInstance{}, fmt.Errorf("%w: context must be a JSON object", model.ErrInvalid)
	}
	// Snapshot BEELINEWF_* process env under the reserved env root so every
	// existing {{ }} render site resolves {{ env.BEELINEWF_X }} with zero
	// template-engine changes.
	if err := s.snapshotEnvSecrets(ctx, obj); err != nil {
		return model.WorkflowInstance{}, err
	}
	if rebased, err := json.Marshal(obj); err == nil {
		contextRaw = rebased
	} else {
		return model.WorkflowInstance{}, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}

	frameRaw, err := model.NewFrame(wc.StartNodeID).JSON()
	if err != nil {
		return model.WorkflowInstance{}, err
	}
	now := nowUTC()
	// A request carrying its own actor records that user; every other
	// caller (a service call, a test, the broker) records the system user.
	actor := s.actorOrDefault(req.Actor)
	contextMode := s.resolveContextMode(wc)
	w := model.WorkflowInstance{
		ID:                   mustNewID(),
		WorkflowDefinitionID: def.ID,
		ContextMode:          contextMode,
		Debug:                req.Debug,
		Status:               model.WorkflowWaiting,
		WaitingReason:        model.WaitingReasonRunnable,
		Frame:                frameRaw,
		Context:              contextRaw,
		Counters:             mustMarshal(model.Counters{}),
		CreatedBy:            actor,
		UpdatedBy:            actor,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if req.Debug {
		// Debug runs start paused so nothing executes before the first
		// manual resume. The dispatcher only claims waiting/runnable
		// rows, so the instance parks until resumed.
		w.Status = model.WorkflowPaused
	}
	if req.SnapshotForms {
		if s.forms == nil {
			return model.WorkflowInstance{}, fmt.Errorf("%w: snapshot_forms requires a form repository", model.ErrInvalid)
		}
		materialized, err := s.materializer.Materialize(ctx, wc)
		if err != nil {
			return model.WorkflowInstance{}, err
		}
		formIDs := collectFormIDs(materialized.Nodes)
		if err := ensureFormsExist(ctx, s.forms, formIDs); err != nil {
			return model.WorkflowInstance{}, err
		}
		if len(formIDs) > 0 {
			snap := make(map[string]formSnapshotEntry, len(formIDs))
			for _, id := range formIDs {
				f, err := s.forms.GetByID(ctx, id)
				if err != nil {
					return model.WorkflowInstance{}, fmt.Errorf("%w: snapshot form %q: %w", model.ErrInvalid, id, err)
				}
				if len(f.Schema) == 0 {
					return model.WorkflowInstance{}, fmt.Errorf("%w: snapshot form %q has no schema", model.ErrInvalid, id)
				}
				snap[id] = formSnapshotEntry{Schema: f.Schema, UI: f.UI}
			}
			frozen, err := json.Marshal(snap)
			if err != nil {
				return model.WorkflowInstance{}, fmt.Errorf("snapshot forms: %w", err)
			}
			w.FormSnapshot = frozen
		}
	}
	if err := s.instances.Insert(ctx, w); err != nil {
		return model.WorkflowInstance{}, err
	}
	if contextMode == model.ContextModeLean {
		// The create context is the replay base but has no history row yet.
		// Anchor it at create so later UpdateContext baselines and
		// post-rollback anchors replay from the true initial object instead
		// of an empty map.
		if err := s.appendLeanAnchor(ctx, w.ID, contextRaw); err != nil {
			return model.WorkflowInstance{}, err
		}
	}
	_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
		ID: mustNewID(), WorkflowInstanceID: w.ID, Type: "instance_created",
		Data:      createdEventData(def.ID, req.Debug),
		CreatedBy: actor, CreatedAt: now,
	})
	s.metrics.InstanceCreated()
	return w, nil
}

// requestPrincipal resolves the caller of a request carrying an optional
// principal. A nil Principal is the service principal: the API-token path,
// the broker consumers, and any internal caller carry no other credential,
// and they are exactly the paths the bypass describes.
func (s *instanceService) requestPrincipal(p *auth.Principal) auth.Principal {
	if p != nil {
		return *p
	}
	return auth.SystemPrincipal(s.actor)
}

// actorOrDefault resolves the audit actor of a request: an explicit actor
// wins, otherwise the call is attributed to the system user.
func (s *instanceService) actorOrDefault(actor string) string {
	return resolveActor(actor, s.actor)
}

// requestActor picks the users.id uuid recorded on a request. An explicit
// Actor wins, then the resolved principal's user id, then the service
// default, so an audited write is never recorded without an actor.
func (s *instanceService) requestActor(p *auth.Principal, actor string) string {
	if actor != "" {
		return actor
	}
	if p != nil && p.UserID != "" {
		return p.UserID
	}
	return s.actor
}

// resolveActor is the shared form of actorOrDefault, for the services that
// carry a default actor but are not instanceService. An empty actor always
// falls back to the configured system user, so a request that predates
// per-request actors keeps working unchanged.
func resolveActor(actor, fallback string) string {
	if actor != "" {
		return actor
	}
	return fallback
}

func (s *instanceService) GetStatus(ctx context.Context, id string, p auth.Principal) (*model.WorkflowInstance, error) {
	inst, err := s.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, p); err != nil {
		return nil, err
	}
	return redactInstanceView(inst), nil
}

func (s *instanceService) List(ctx context.Context, q repository.InstanceListQuery, p auth.Principal) ([]model.WorkflowInstance, int64, error) {
	items, total, err := s.instances.List(ctx, ownedInstances(q, p))
	if err != nil {
		return nil, 0, err
	}
	var errorIDs []string
	for _, item := range items {
		if item.Error != "" {
			errorIDs = append(errorIDs, item.ID)
		}
	}
	if len(errorIDs) == 0 {
		return items, total, nil
	}
	reader, ok := s.instances.(repository.InstanceContextReader)
	if !ok {
		for i := range items {
			if items[i].Error != "" {
				items[i].Error = SecretMask
			}
		}
		return items, total, nil
	}
	contexts, err := reader.GetContextsByIDs(ctx, errorIDs)
	if err != nil {
		return nil, 0, err
	}
	redactInstanceListItems(items, contexts)
	return items, total, nil
}

func (s *instanceService) GetStatusDetail(ctx context.Context, id string, p auth.Principal) (*StatusDetail, error) {
	inst, err := s.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, p); err != nil {
		return nil, err
	}
	d := &StatusDetail{Instance: *redactInstanceView(inst)}
	// Single-pass fetch: the materialized tree, the executions, and the
	// branches each load once and are shared by every section below.
	wc, err := s.materializer.Resolve(ctx, inst.WorkflowDefinitionID)
	if err != nil {
		wc = nil
	}
	exs, exErr := s.parallel.ListExecutions(ctx, inst.ID)
	var branchesByEx map[string][]model.ParallelBranch
	branchesErr := exErr
	if exErr == nil && len(exs) > 0 {
		ids := make([]string, 0, len(exs))
		for _, ex := range exs {
			ids = append(ids, ex.ID)
		}
		branchesByEx, branchesErr = s.parallel.ListBranchesByExecutionIDs(ctx, ids)
	}
	d.Nodes = s.statusNodes(ctx, inst, wc, exs, exErr)
	if exErr == nil && branchesErr == nil {
		d.Parallel = statusParallel(exs, branchesByEx)
		// Branch errors can embed rendered secrets; scrub them against the
		// pre-redaction snapshot. inst.Context still holds plaintext here
		// because redactInstanceView copies.
		redactParallelBranchErrors(d.Parallel, inst.Context)
	}
	frame, err := model.ParseFrame(inst.Frame)
	if err != nil {
		return d, nil
	}
	if frame.CurrentNodeID == "" {
		return d, nil
	}
	attempt, err := s.instances.GetNodeInstanceByNode(ctx, inst.ID, frame.CurrentNodeID)
	if err != nil {
		return d, nil
	}
	nodeInstanceID := attempt.ID
	d.CurrentNodeInstanceID = &nodeInstanceID
	d.Attempt = attempt.Attempt
	d.PendingInput = pendingInput(wc, inst, frame.CurrentNodeID)
	return d, nil
}

// pendingInput resolves the waiting-input contract for status responses. It
// returns nil unless the instance waits on an input node, and on any graph
// load failure (status never fails for graph reasons). The tree is the one
// GetStatusDetail already resolved; a nil tree means that load failed.
func pendingInput(wc *model.WorkflowContent, inst *model.WorkflowInstance, currentNodeID string) *PendingInput {
	if inst.Status != model.WorkflowWaiting || inst.WaitingReason != model.WaitingReasonInput {
		return nil
	}
	if wc == nil {
		return nil
	}
	graph := &contentGraph{wc: wc}
	node, err := graph.FindNode(currentNodeID)
	if err != nil {
		return nil
	}
	if node.Type != model.NodeTypeInput {
		return nil
	}
	key := strings.TrimSpace(node.OutputProperty)
	if key == "" {
		key = node.ID
	}
	p := &PendingInput{
		NodeID:         node.ID,
		Channel:        node.Channel,
		OutputProperty: key,
		FormID:         node.FormID,
		AllowedRoles:   node.AllowedRoles,
		AllowedUsers:   node.AllowedUsers,
		RecordActor:    node.RecordActor,
		Public:         node.Public,
		Accept:         node.EffectiveAccept(),
	}
	// The contract reports the effective gate: a public node ignores both
	// lists, so they are hidden rather than shown as a restriction.
	if p.Public {
		p.AllowedRoles = nil
		p.AllowedUsers = nil
	}
	if p.FormID != "" && len(inst.FormSnapshot) > 0 {
		if e, ok := parseFormSnapshot(inst.FormSnapshot)[p.FormID]; ok {
			p.Form = &SnapshotForm{Schema: e.Schema, UI: e.UI}
		}
	}
	return p
}

func (s *instanceService) GetContext(ctx context.Context, id string, p auth.Principal) (*model.WorkflowInstance, error) {
	inst, err := s.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, p); err != nil {
		return nil, err
	}
	return redactInstanceView(inst), nil
}

// leanMode reports whether the instance runs with replayable diff history.
func leanMode(inst *model.WorkflowInstance) bool {
	return inst != nil && inst.ContextMode == model.ContextModeLean
}

// resolveContextMode picks the instance context mode: the definition
// override wins, then the global lean default, then full.
func (s *instanceService) resolveContextMode(wc *model.WorkflowContent) string {
	if wc.ContextMode != nil {
		return *wc.ContextMode
	}
	if s.leanOptions.LeanContextDefault {
		return model.ContextModeLean
	}
	return model.ContextModeFull
}

// snapshotEnvSecrets overlays fresh env and secret snapshots onto obj with
// Create precedence: the snapshot wins per key so callers cannot shadow
// env-provided credentials, and the caller base is stripped of denied names
// first so a request body cannot smuggle a denied var past the snapshot.
func (s *instanceService) snapshotEnvSecrets(ctx context.Context, obj map[string]any) error {
	obj["env"] = envsnapshot.Merge(envsnapshot.StripDenied(obj["env"]), envsnapshot.Snapshot(envsnapshot.Prefix))
	var secretSnapshot map[string]string
	if s.secrets != nil {
		var err error
		secretSnapshot, err = s.secrets.GetAll(ctx)
		if err != nil {
			return err
		}
	}
	mergeSecretSnapshot(obj, secretSnapshot)
	return nil
}

// appendLeanAnchor records contextRaw as the new lean replay baseline for
// the instance.
func (s *instanceService) appendLeanAnchor(ctx context.Context, instanceID string, contextRaw json.RawMessage) error {
	anchor := leanAnchorRow(contextRaw)
	anchor.WorkflowInstanceID = instanceID
	return s.instances.AppendHistory(ctx, *anchor)
}

// mapHistoryError maps history replay failures to 409 conflict semantics:
// a gap or depth overflow fails closed, never with a silent wrong restore.
func mapHistoryError(err error) error {
	if errors.Is(err, repository.ErrHistoryGap) || errors.Is(err, repository.ErrHistoryTooDeep) {
		return fmt.Errorf("%w: %w", model.ErrConflict, err)
	}
	return err
}

// leanHistoryCursorForOccurrence resolves the non-superseded history cursor
// for one occurrence: the earliest non-superseded row carrying that
// occurrence id, so reconstruction returns that occurrence's before
// context. The boolean reports presence. A positive attempt selects that
// exact attempt row.
func leanHistoryCursorForOccurrence(rows []model.NodeContextHistory, occurrenceID string, attempt int) (model.HistoryCursor, bool) {
	var cursor model.HistoryCursor
	found := false
	for _, row := range rows {
		if row.Superseded || row.OccurrenceID != occurrenceID {
			continue
		}
		if attempt > 0 && row.Attempt != attempt {
			continue
		}
		if !found || row.Cursor().Before(cursor) {
			cursor = row.Cursor()
			found = true
		}
	}
	return cursor, found
}

// leanOccurrenceSet builds the set of occurrence ids with non-superseded
// history rows. Callers load history once and reuse the set per node.
func leanOccurrenceSet(rows []model.NodeContextHistory) map[string]struct{} {
	out := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.Superseded || row.OccurrenceID == "" {
			continue
		}
		out[row.OccurrenceID] = struct{}{}
	}
	return out
}

// leanReconstructBefore replays lean history up to (excluding) the target
// cursor, mapping replay failures to 409. The create anchor (or a later
// UpdateContext/rollback anchor) is the replay base, so the passed initial
// seed is only a fallback for instances created before anchors existed.
func (s *instanceService) leanReconstructBefore(ctx context.Context, inst *model.WorkflowInstance, target model.HistoryCursor) (json.RawMessage, error) {
	initial := json.RawMessage("{}")
	restored, err := s.instances.ReconstructBefore(ctx, inst.ID, target, initial)
	if err != nil {
		return nil, mapHistoryError(err)
	}
	return restored, nil
}

// leanAnchorRow builds a post-commit anchor snapshot row for the restored
// full context. Occurrence id stays empty: the row is a system baseline,
// not a node commit.
func leanAnchorRow(restored json.RawMessage) *model.NodeContextHistory {
	return &model.NodeContextHistory{
		IsAnchor: true,
		Snapshot: restored,
		Diff:     json.RawMessage("null"),
	}
}

// validateRestoredObject ensures the replayed context is a JSON object
// before it becomes the instance context. Anything else fails closed.
func validateRestoredObject(restored json.RawMessage) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(restored, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("restored context is not a JSON object")
	}
	return obj, nil
}

// toStringMap normalizes a decoded JSON value to map[string]any. Non-map
// inputs (nil, scalars, slices) yield an empty map so Merge overlays a
// clean snapshot instead of inheriting garbage.
func toStringMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// statusNodes builds the graph-node-id → occurrence map for the status
// response. It returns nil when the definition cannot be loaded, parsed, or
// materialized, so the status view degrades to omitting the map instead of
// failing the whole call. The tree and executions are the ones GetStatusDetail
// already fetched; a nil tree means that load failed, and a non-nil exErr
// leaves the rollback gate to the endpoint, which rechecks authoritatively.
func (s *instanceService) statusNodes(ctx context.Context, inst *model.WorkflowInstance, wc *model.WorkflowContent, exs []model.ParallelExecution, exErr error) map[string]NodeOccurrence {
	if wc == nil {
		return nil
	}
	nodeMap := flattenNodeMap(wc.Nodes)
	if len(nodeMap) == 0 {
		return nil
	}
	occs, err := s.instances.ListNodeInstances(ctx, inst.ID)
	if err != nil {
		return nil
	}
	byNode := make(map[string]*model.NodeInstance, len(occs))
	for i := range occs {
		byNode[occs[i].NodeID] = &occs[i]
	}
	instanceGate := inst.Status == model.WorkflowPaused || inst.Status == model.WorkflowFailed
	instanceGate = instanceGate && !inst.TerminationPending
	if instanceGate {
		// Mirror the rollback endpoint: live parallel branches block
		// every target. A load failure leaves the gate to the
		// endpoint, which rechecks authoritatively.
		if exErr == nil && hasLiveExecution(exs) {
			instanceGate = false
		}
	}
	// Lean mode gates rollbackability on history presence, not on
	// ContextBefore parsing (lean occurrence rows store null). The history
	// occurrence-id set loads once per call: no per-node query fan-out.
	var leanSet map[string]struct{}
	if leanMode(inst) && instanceGate {
		if rows, herr := s.instances.LoadHistory(ctx, inst.ID); herr == nil {
			leanSet = leanOccurrenceSet(rows)
		}
	}
	out := make(map[string]NodeOccurrence, len(nodeMap))
	for id, nc := range nodeMap {
		occ, ok := byNode[id]
		if !ok {
			out[id] = NodeOccurrence{Status: "not_started"}
			continue
		}
		e := NodeOccurrence{Status: string(occ.Status)}
		occID := occ.ID
		e.OccurrenceID = &occID
		attempt := occ.Attempt
		e.Attempt = &attempt
		if leanSet != nil {
			e.Rollbackable = instanceGate && leanRollbackableOccurrence(nc.Type, occ, leanSet)
		} else {
			e.Rollbackable = instanceGate && rollbackableOccurrence(nc.Type, occ)
		}
		out[id] = e
	}
	return out
}

// statusParallel assembles the parallel execution tree for the status
// response from one execution list and one batched branch map. It returns
// nil when the instance never forked. Executions missing from the branch
// map render an empty branch list. GetStatusDetail degrades execution or
// branch load failures to a nil section instead of calling this.
func statusParallel(exs []model.ParallelExecution, branchesByEx map[string][]model.ParallelBranch) []ParallelExecutionView {
	if len(exs) == 0 {
		return nil
	}
	out := make([]ParallelExecutionView, 0, len(exs))
	for _, ex := range exs {
		branches := branchesByEx[ex.ID]
		v := ParallelExecutionView{
			ID:             ex.ID,
			ParentBranchID: ex.ParentBranchID,
			Depth:          ex.Depth,
			StartNodeID:    ex.StartNodeID,
			EndNodeID:      ex.EndNodeID,
			Status:         string(ex.Status),
			BranchCount:    ex.BranchCount,
			CompletedCount: ex.CompletedCount,
			Branches:       make([]ParallelBranchView, 0, len(branches)),
		}
		for _, b := range branches {
			v.Branches = append(v.Branches, ParallelBranchView{
				ID:            b.ID,
				Name:          b.Name,
				BranchIndex:   b.BranchIndex,
				StartNodeID:   b.StartNodeID,
				Status:        string(b.Status),
				WaitingReason: string(b.WaitingReason),
				Error:         b.Error,
				UpdatedAt:     b.UpdatedAt,
			})
		}
		out = append(out, v)
	}
	return out
}

// flattenNodeMap indexes every graph node in the materialized tree by id,
// including group nodes themselves and their nested children. Nil nodes
// are tolerated, matching the old flatten-then-lookup walk.
func flattenNodeMap(nodes []*model.NodeContent) map[string]*model.NodeContent {
	out := make(map[string]*model.NodeContent)
	var walk func(ns []*model.NodeContent)
	walk = func(ns []*model.NodeContent) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			out[n.ID] = n
			if n.Group != nil {
				walk(n.Group.Nodes)
			}
		}
	}
	walk(nodes)
	return out
}

// leanRollbackableOccurrence mirrors rollbackableOccurrence for lean mode:
// group nodes never qualify, only terminal occurrence states qualify, and
// the occurrence must have non-superseded history (lean rows store null
// ContextBefore, so parsing it would always fail).
func leanRollbackableOccurrence(typ model.NodeType, occ *model.NodeInstance, set map[string]struct{}) bool {
	if typ == model.NodeTypeGroup {
		return false
	}
	if occ.BranchID != "" {
		return false
	}
	switch occ.Status {
	case model.NodeFinished, model.NodeFailed, model.NodeStopped:
	default:
		return false
	}
	if set == nil {
		return false
	}
	_, ok := set[occ.ID]
	return ok
}

// rollbackableOccurrence mirrors the rollback endpoint's target validation:
// group nodes never qualify (they have no occurrence), only terminal
// occurrence states qualify, and the ContextBefore snapshot must parse as
// a JSON object. Branch-owned occurrences never qualify: rollback moves
// the parent cursor only. The caller gates on instance status.
func rollbackableOccurrence(typ model.NodeType, occ *model.NodeInstance) bool {
	if typ == model.NodeTypeGroup {
		return false
	}
	if occ.BranchID != "" {
		return false
	}
	switch occ.Status {
	case model.NodeFinished, model.NodeFailed, model.NodeStopped:
	default:
		return false
	}
	return restorableContext(occ.ContextBefore)
}

// restorableContext reports whether raw is a JSON object (the rollback
// endpoint restores target.ContextBefore as the instance context).
func restorableContext(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return false
	}
	return true
}

func (s *instanceService) UpdateContext(ctx context.Context, req UpdateContext) (*model.WorkflowInstance, error) {
	if strings.TrimSpace(req.InstanceID) == "" {
		return nil, fmt.Errorf("%w: instance id is required", model.ErrInvalid)
	}
	if len(req.Context) == 0 {
		return nil, fmt.Errorf("%w: context must be a JSON object", model.ErrInvalid)
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Context, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%w: context must be a JSON object", model.ErrInvalid)
	}
	// Carry stored env and secret snapshots forward. Replacement bodies must
	// not drop them or inject values under reserved roots; stored values win.
	// Denied names are stripped from the replacement body for the same reason
	// they are excluded from the snapshot.
	var storedEnv any
	storedSecrets := map[string]string{}
	cur, curErr := s.instances.GetByID(ctx, req.InstanceID)
	if curErr != nil && !errors.Is(curErr, repository.ErrInstanceNotFound) {
		return nil, curErr
	}
	// Authorize before validating stored state: a 409 on a corrupt row would
	// confirm its existence to a caller not entitled to know it.
	principal := s.requestPrincipal(req.Principal)
	if err := authorizeInstance(cur, principal); err != nil {
		return nil, err
	}
	if cur != nil {
		curObj, ok := decodeContextObject(cur.Context)
		if !ok {
			return nil, fmt.Errorf("%w: stored context is invalid", model.ErrConflict)
		}
		var trusted bool
		storedSecrets, trusted = secretValuesFromObject(curObj)
		if !trusted {
			return nil, fmt.Errorf("%w: stored secret snapshot is invalid", model.ErrConflict)
		}
		storedEnv = curObj["env"]
	}
	obj["env"] = envsnapshot.Merge(envsnapshot.StripDenied(obj["env"]), toStringMap(storedEnv))
	mergeSecretSnapshot(obj, storedSecrets)
	rebased, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}
	req.Context = rebased
	inst, err := s.instances.ReplaceContext(ctx, repository.ContextUpdate{
		InstanceID: req.InstanceID,
		Context:    req.Context,
		Actor:      s.requestActor(req.Principal, req.Actor),
		Reason:     req.Reason,
	})
	if err != nil {
		if errors.Is(err, repository.ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: instance %s", model.ErrNotFound, req.InstanceID)
		}
		if errors.Is(err, repository.ErrStatusConflict) {
			return nil, fmt.Errorf("%w: instance %s is not paused", model.ErrConflict, req.InstanceID)
		}
		return nil, err
	}
	// Lean mode appends the replaced context as a new anchor baseline so
	// later replays start from it. The prefix stays intact: no supersede.
	if leanMode(inst) {
		if err := s.appendLeanAnchor(ctx, inst.ID, req.Context); err != nil {
			return nil, err
		}
		updated, err := s.instances.GetByID(ctx, req.InstanceID)
		if err != nil {
			return nil, err
		}
		return redactInstanceView(updated), nil
	}
	return redactInstanceView(inst), nil
}

func (s *instanceService) NodeDebug(ctx context.Context, instanceID, nodeID string, attempt int, p auth.Principal) (*NodeDebugDetail, error) {
	inst, err := s.instances.GetByID(ctx, instanceID)
	if err != nil {
		if errors.Is(err, repository.ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: instance %s", model.ErrNotFound, instanceID)
		}
		return nil, err
	}
	if err := authorizeInstance(inst, p); err != nil {
		return nil, err
	}
	wf, err := s.wfDefs.GetByID(ctx, inst.WorkflowDefinitionID)
	if err != nil {
		return nil, err
	}
	wc, err := model.ParseWorkflowContent(wf.Content, s.limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}
	graph := &contentGraph{wc: wc}

	// nodeID is the graph node id; an occurrence id is resolved to its graph
	// node as a fallback.
	var nodeInst *model.NodeInstance
	nc, err := graph.FindNode(nodeID)
	if err != nil {
		occ, gerr := s.instances.GetNodeInstance(ctx, instanceID, nodeID)
		if gerr != nil {
			return nil, fmt.Errorf("%w: node %q does not exist in instance %s", model.ErrNotFound, nodeID, instanceID)
		}
		nc, err = graph.FindNode(occ.NodeID)
		if err != nil {
			return nil, fmt.Errorf("%w: node %q does not exist in instance %s", model.ErrNotFound, nodeID, instanceID)
		}
		nodeInst = occ
	} else {
		occ, gerr := s.instances.GetNodeInstanceByNode(ctx, instanceID, nc.ID)
		if gerr != nil && !errors.Is(gerr, repository.ErrNodeInstanceNotFound) {
			return nil, gerr
		}
		nodeInst = occ
	}

	d := &NodeDebugDetail{
		SourceNodeDefinitionID: nc.NodeDefinitionID,
		Name:                   nc.Name,
		Type:                   string(nc.Type),
		Status:                 "not_started",
	}
	if nodeInst == nil {
		d.OccurrenceID = nc.ID
		return d, nil
	}

	selected := attempt
	if selected <= 0 {
		selected = nodeInst.Attempt
	}
	if attempt > nodeInst.Attempt {
		return nil, fmt.Errorf("%w: attempt %d of node %q never ran (latest %d)", model.ErrNotFound, attempt, nodeID, nodeInst.Attempt)
	}
	latest := nodeInst.Attempt
	d.OccurrenceID = nodeInst.ID
	d.SelectedAttempt = &selected
	d.LatestAttempt = &latest
	d.AttemptCount = nodeInst.Attempt
	d.Status = string(nodeInst.Status)
	d.ContextBefore = nodeInst.ContextBefore
	d.ContextAfter = nodeInst.ContextAfter
	if leanMode(inst) {
		// Lean occurrence rows store null ContextBefore and diff
		// ContextAfter; reconstruct full snapshots without changing the
		// DTO shape. Anchor targets use their snapshot, diff targets
		// replay history then apply the occurrence diff.
		before, after, derr := s.leanNodeDebugContexts(ctx, inst, nodeInst, selected)
		if derr != nil {
			return nil, derr
		}
		d.ContextBefore = before
		d.ContextAfter = after
	}
	d.Input = nodeInst.Input
	d.Output = nodeInst.Output
	d.Error = nullableString(nodeInst.Error)
	d.RecoveryPolicy = nullableString(nodeInst.RecoveryPolicy)
	d.RecoveryResult = nullableString(nodeInst.RecoveryResult)
	d.Cancelled = nodeInst.Cancelled
	d.StartedAt = nodeInst.StartedAt
	d.FinishedAt = nodeInst.FinishedAt
	d.StoppedAt = nodeInst.StoppedAt
	if nodeInst.StartedAt != nil && nodeInst.FinishedAt != nil {
		ms := nodeInst.FinishedAt.Sub(*nodeInst.StartedAt).Milliseconds()
		d.DurationMS = &ms
	}
	d.CreatedAt = nodeInst.CreatedAt
	d.UpdatedAt = nodeInst.UpdatedAt
	secrets, trusted := secretValuesFromContext(inst.Context)
	redactNodeDebugSecrets(d, secrets, trusted)
	return d, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// leanNodeDebugContexts reconstructs full before/after contexts for a lean
// occurrence. The after context replays history through the selected row;
// the before context replays through the immediately preceding history
// row. Anchor rows use their snapshot as the after context. Failures map
// to 409 conflict semantics.
func (s *instanceService) leanNodeDebugContexts(ctx context.Context, inst *model.WorkflowInstance, occ *model.NodeInstance, selected int) (json.RawMessage, json.RawMessage, error) {
	rows, err := s.instances.LoadHistory(ctx, inst.ID)
	if err != nil {
		return nil, nil, err
	}
	target, found := leanHistoryCursorForOccurrence(rows, occ.ID, 0)
	if !found {
		return nil, nil, fmt.Errorf("%w: occurrence %q has no history", model.ErrConflict, occ.ID)
	}
	_ = selected
	before, err := s.leanReconstructBefore(ctx, inst, target)
	if err != nil {
		return nil, nil, err
	}
	// The selected row's own history payload is the after context: anchors
	// carry snapshots and diffs apply on the before context. Replaying
	// through a successor can skip this rule, so resolve directly.
	for _, row := range rows {
		if row.Superseded || row.OccurrenceID != occ.ID {
			continue
		}
		if row.Cursor() != target {
			continue
		}
		if row.IsAnchor {
			if _, verr := validateRestoredObject(row.Snapshot); verr != nil {
				return nil, nil, fmt.Errorf("%w: %w", model.ErrConflict, verr)
			}
			return before, row.Snapshot, nil
		}
		targetDiff, derr := contextdiff.ParseDiff(row.Diff)
		if derr != nil {
			return nil, nil, fmt.Errorf("%w: %w", model.ErrConflict, derr)
		}
		beforeMap, merr := unmarshalJSON(before)
		if merr != nil {
			return nil, nil, fmt.Errorf("%w: %w", model.ErrConflict, merr)
		}
		afterMap, aerr := contextdiff.Apply(beforeMap, targetDiff)
		if aerr != nil {
			return nil, nil, fmt.Errorf("%w: %w", model.ErrConflict, aerr)
		}
		after, merr := json.Marshal(afterMap)
		if merr != nil {
			return nil, nil, fmt.Errorf("%w: %w", model.ErrConflict, merr)
		}
		return before, after, nil
	}
	return nil, nil, fmt.Errorf("%w: occurrence %q has no history", model.ErrConflict, occ.ID)
}

func (s *instanceService) Pause(ctx context.Context, req ControlRequest) (*ControlResult, error) {
	id := req.InstanceID
	principal := s.requestPrincipal(req.Principal)
	actor := s.requestActor(req.Principal, req.Actor)
	inst, err := s.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, principal); err != nil {
		return nil, err
	}
	switch inst.Status {
	case model.WorkflowPaused:
		return &ControlResult{Status: model.WorkflowPaused}, nil
	case model.WorkflowFinished, model.WorkflowFailed, model.WorkflowStopped:
		return nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, id)
	}
	deferred, err := s.instances.Pause(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrStatusConflict) {
			return nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, id)
		}
		return nil, err
	}
	if deferred {
		_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
			ID: mustNewID(), WorkflowInstanceID: id, Type: "pause_requested",
			Data: json.RawMessage(`{}`), CreatedBy: actor, CreatedAt: nowUTC(),
		})
		return &ControlResult{Status: model.WorkflowRunning, PauseRequested: true}, nil
	}
	_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
		ID: mustNewID(), WorkflowInstanceID: id, Type: "paused",
		Data: json.RawMessage(`{}`), CreatedBy: actor, CreatedAt: nowUTC(),
	})
	return &ControlResult{Status: model.WorkflowPaused}, nil
}

func (s *instanceService) Resume(ctx context.Context, req ControlRequest) (*ControlResult, error) {
	id := req.InstanceID
	principal := s.requestPrincipal(req.Principal)
	actor := s.requestActor(req.Principal, req.Actor)
	inst, err := s.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, principal); err != nil {
		return nil, err
	}
	if inst.Debug && (inst.Status == model.WorkflowPaused ||
		(inst.Status == model.WorkflowWaiting && inst.WaitingReason == model.WaitingReasonParallel)) {
		// Step-through: each resume advances exactly one paused
		// branch while the parent stays parked. A branch input
		// delivery can leave paused branches behind a waiting
		// parent, so waiting parents step branches too; a generic
		// parent wake here would join-spin against paused branches.
		if stepped, err := s.resumeDebugStep(ctx, inst, actor); err != nil || stepped != nil {
			return stepped, err
		}
	}
	switch inst.Status {
	case model.WorkflowWaiting:
		return &ControlResult{Status: model.WorkflowWaiting}, nil
	case model.WorkflowFinished, model.WorkflowFailed, model.WorkflowStopped:
		return nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, id)
	}
	if err := s.instances.Resume(ctx, id); err != nil {
		if errors.Is(err, repository.ErrStatusConflict) {
			return nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, id)
		}
		return nil, err
	}
	eventType := "resumed"
	if inst.Status == model.WorkflowRunning {
		eventType = "resume"
	}
	_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
		ID: mustNewID(), WorkflowInstanceID: id, Type: eventType,
		Data: json.RawMessage(`{}`), CreatedBy: actor, CreatedAt: nowUTC(),
	})
	return &ControlResult{Status: model.WorkflowWaiting}, nil
}

// resumeDebugStep wakes one paused branch for a debug step-through
// resume: the shallowest execution wins, then the alphabetically first
// branch, so stepping order is deterministic. It returns nil when no
// branch needs stepping so the caller wakes the parent instead. A branch
// with a step already in flight (running or claimed-runnable) conflicts:
// two steps at once would break the one-step-per-resume contract.
func (s *instanceService) resumeDebugStep(ctx context.Context, inst *model.WorkflowInstance, actor string) (*ControlResult, error) {
	exs, err := s.parallel.ListExecutions(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	if len(exs) == 0 {
		return nil, nil
	}
	depth := make(map[string]int, len(exs))
	for _, ex := range exs {
		depth[ex.ID] = ex.Depth
	}
	var pick *model.ParallelBranch
	for _, ex := range exs {
		branches, err := s.parallel.ListBranches(ctx, ex.ID)
		if err != nil {
			return nil, err
		}
		for i := range branches {
			b := &branches[i]
			switch {
			case b.Status == model.ParallelBranchRunning ||
				(b.Status == model.ParallelBranchWaiting && b.WaitingReason == model.WaitingReasonRunnable):
				return nil, fmt.Errorf("%w: branch %q has a step in flight", model.ErrConflict, b.Name)
			case b.Status == model.ParallelBranchWaiting && b.WaitingReason == model.WaitingReasonPaused:
				if pick == nil || debugPickBefore(depth, b, pick) {
					c := *b
					pick = &c
				}
			}
		}
	}
	if pick == nil {
		return nil, nil
	}
	if err := s.parallel.WakePausedBranch(ctx, pick.ID); err != nil {
		if errors.Is(err, repository.ErrStatusConflict) || errors.Is(err, repository.ErrParallelBranchNotFound) {
			return nil, fmt.Errorf("%w: branch %q is no longer paused", model.ErrConflict, pick.Name)
		}
		return nil, err
	}
	_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
		ID: mustNewID(), WorkflowInstanceID: inst.ID, Type: "parallel_branch_resumed",
		Data: mustMarshal(map[string]any{
			"branch_id":             pick.ID,
			"parallel_execution_id": pick.ParallelExecutionID,
			"branch_name":           pick.Name,
			"branch_index":          pick.BranchIndex,
			"start_node_id":         pick.StartNodeID,
		}),
		CreatedBy: actor, CreatedAt: nowUTC(),
	})
	return &ControlResult{Status: inst.Status}, nil
}

// debugPickBefore orders paused branches for step-through: shallowest
// execution first, then branch index (alphabetical), then oldest, then id.
func debugPickBefore(depth map[string]int, b, pick *model.ParallelBranch) bool {
	if depth[b.ParallelExecutionID] != depth[pick.ParallelExecutionID] {
		return depth[b.ParallelExecutionID] < depth[pick.ParallelExecutionID]
	}
	if b.BranchIndex != pick.BranchIndex {
		return b.BranchIndex < pick.BranchIndex
	}
	if !b.CreatedAt.Equal(pick.CreatedAt) {
		return b.CreatedAt.Before(pick.CreatedAt)
	}
	return b.ID < pick.ID
}

func (s *instanceService) Stop(ctx context.Context, req ControlRequest) (*ControlResult, error) {
	id := req.InstanceID
	principal := s.requestPrincipal(req.Principal)
	actor := s.requestActor(req.Principal, req.Actor)
	inst, err := s.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, principal); err != nil {
		return nil, err
	}
	switch inst.Status {
	case model.WorkflowStopped:
		return &ControlResult{Status: model.WorkflowStopped, TerminationPending: inst.TerminationPending}, nil
	case model.WorkflowFinished, model.WorkflowFailed:
		return nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, id)
	}
	pending, err := s.instances.Stop(ctx, id, req.Reason)
	if err != nil {
		if errors.Is(err, repository.ErrStatusConflict) {
			return nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, id)
		}
		return nil, err
	}
	_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
		ID: mustNewID(), WorkflowInstanceID: id, Type: "stop",
		Data:      mustMarshal(map[string]string{"reason": req.Reason}),
		CreatedBy: actor, CreatedAt: nowUTC(),
	})
	// A parked input attempt has no worker to interrupt; stop it here.
	if !pending {
		_ = s.instances.StopRunningAttempts(ctx, id)
	}
	if pending && s.cancels != nil {
		s.cancels.Cancel(id)
	}
	s.metrics.InstanceTerminal(string(model.WorkflowStopped), nowUTC().Sub(inst.CreatedAt))
	return &ControlResult{Status: model.WorkflowStopped, TerminationPending: pending}, nil
}

// instance loads an instance for the control endpoints, mapping a missing
// row to the domain not-found error.
func (s *instanceService) instance(ctx context.Context, id string) (*model.WorkflowInstance, error) {
	inst, err := s.instances.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: instance %s", model.ErrNotFound, id)
		}
		return nil, err
	}
	return inst, nil
}

func (s *instanceService) Rollback(ctx context.Context, req RollbackRequest) (*RollbackResult, error) {
	if strings.TrimSpace(req.InstanceID) == "" {
		return nil, fmt.Errorf("%w: instance id is required", model.ErrInvalid)
	}
	if strings.TrimSpace(req.TargetOccurrenceID) == "" {
		return nil, fmt.Errorf("%w: target_occurrence_id is required", model.ErrInvalid)
	}
	inst, err := s.instance(ctx, req.InstanceID)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, s.requestPrincipal(req.Principal)); err != nil {
		return nil, err
	}
	switch inst.Status {
	case model.WorkflowPaused, model.WorkflowFailed:
	default:
		return nil, fmt.Errorf("%w: instance %s is not paused or failed", model.ErrConflict, req.InstanceID)
	}
	if inst.TerminationPending {
		return nil, fmt.Errorf("%w: instance %s has termination pending", model.ErrConflict, req.InstanceID)
	}
	if live, err := s.hasLiveParallel(ctx, inst.ID); err != nil {
		return nil, err
	} else if live {
		return nil, fmt.Errorf("%w: instance %s has live parallel branches; rollback is supported only before a parallel block or after it completes", model.ErrConflict, req.InstanceID)
	}

	// The target is an already-executed node occurrence: its row carries
	// the graph node id and the ContextBefore snapshot to restore. Nodes
	// that never ran (NodeDebug "not_started") have no occurrence.
	occ, err := s.instances.GetNodeInstance(ctx, inst.ID, req.TargetOccurrenceID)
	if err != nil {
		if errors.Is(err, repository.ErrNodeInstanceNotFound) {
			return nil, fmt.Errorf("%w: occurrence %q of instance %s", model.ErrNotFound, req.TargetOccurrenceID, req.InstanceID)
		}
		return nil, err
	}
	if occ.WorkflowInstanceID != inst.ID {
		return nil, fmt.Errorf("%w: occurrence %q of instance %s", model.ErrNotFound, req.TargetOccurrenceID, req.InstanceID)
	}
	if occ.BranchID != "" {
		return nil, fmt.Errorf("%w: occurrence %q ran inside a parallel branch; roll back to a node before or after the parallel block", model.ErrInvalid, req.TargetOccurrenceID)
	}

	// Resolve pre-shapes errors exactly as the triple did: fetch failures
	// pass through, content failures arrive as model.ErrInvalid.
	wc, err := s.materializer.Resolve(ctx, inst.WorkflowDefinitionID)
	if err != nil {
		return nil, err
	}
	target, err := (&contentGraph{wc: wc}).FindNode(occ.NodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: occurrence node %q is not in the workflow definition", model.ErrInvalid, occ.NodeID)
	}
	if target.Type == model.NodeTypeGroup {
		return nil, fmt.Errorf("%w: group nodes cannot be rollback targets", model.ErrInvalid)
	}
	running, rerr := s.instances.GetRunningNodeInstance(ctx, inst.ID)
	if rerr != nil && !errors.Is(rerr, repository.ErrNodeInstanceNotFound) {
		return nil, rerr
	}
	hasLive := rerr == nil

	frame, err := model.ParseFrame(inst.Frame)
	if err != nil {
		return nil, err
	}

	// Self rollback: paused on the live park itself. No-op without writes.
	if hasLive && inst.Status == model.WorkflowPaused &&
		running.ID == occ.ID && frame.CurrentNodeID == target.ID {
		return &RollbackResult{
			Status:        model.WorkflowPaused,
			CurrentNodeID: target.ID,
			GroupStack:    frame.GroupStack,
		}, nil
	}
	if target.Type == model.NodeTypeInput {
		// Rolling back to an input node re-arms its wait as a fresh
		// attempt of the same finished occurrence (delivery history stays
		// attached for audit) so the next resume re-parks it and a fresh
		// delivery with a new idempotency key is accepted. Any other live
		// park is superseded (closed) atomically by the repository.
		if occ.Status != model.NodeFinished {
			return nil, fmt.Errorf("%w: input occurrence %q is not finished", model.ErrConflict, req.TargetOccurrenceID)
		}
	}
	stack, err := (&contentGraph{wc: wc}).GroupStack(occ.NodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}
	restoreCtx, restoreCursor, rerr := s.resolveRollbackContext(ctx, inst, occ, req.TargetOccurrenceID)
	if rerr != nil {
		return nil, rerr
	}

	// The park reason follows the target: input targets stay parked for
	// their delivery, everything else becomes runnable. Rolling back to a
	// finished input occurrence re-arms it as a fresh attempt of the same
	// row (delivery history stays attached for audit), so a fresh
	// Idempotency-Key is accepted on resume.
	waitingReason := model.WaitingReasonRunnable
	rearm := false
	if target.Type == model.NodeTypeInput {
		waitingReason = model.WaitingReasonInput
		rearm = true
	}
	// Any live attempt other than the re-armed target is superseded: the
	// repository closes it in the same transaction so the cursor and the
	// park never diverge.
	supersede := hasLive && (!rearm || running.ID != occ.ID)
	supersededOccurrence := ""
	supersededNode := ""
	if supersede {
		supersededOccurrence = running.ID
		supersededNode = running.NodeID
	}
	rolled, err := s.instances.RollbackInstance(ctx, repository.RollbackUpdate{
		InstanceID:           inst.ID,
		Frame:                model.Frame{CurrentNodeID: target.ID, GroupStack: stack},
		Context:              restoreCtx,
		Actor:                s.requestActor(req.Principal, req.Actor),
		Reason:               req.Reason,
		FromNode:             frame.CurrentNodeID,
		ToNode:               target.ID,
		ToOccurrence:         occ.ID,
		WaitingReason:        waitingReason,
		RearmInputOccurrence: rearm,
		SupersedeRunning:     supersede,
		SupersededOccurrence: supersededOccurrence,
		SupersededNode:       supersededNode,
	})
	if err != nil {
		if errors.Is(err, repository.ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: instance %s", model.ErrNotFound, req.InstanceID)
		}
		if errors.Is(err, repository.ErrStatusConflict) {
			return nil, fmt.Errorf("%w: instance %s is not paused or failed", model.ErrConflict, req.InstanceID)
		}
		return nil, err
	}
	// Lean mode supersedes the target occurrence row and everything after
	// it, then appends a post-rollback anchor. The start anchor (create or
	// UpdateContext baseline) is never superseded, so the next replay
	// starts from the restored context while older non-superseded rows
	// stay available for a second rollback to an older target.
	if leanMode(inst) && restoreCursor != nil {
		if serr := s.instances.SupersedeAfter(ctx, inst.ID, *restoreCursor); serr != nil {
			return nil, mapHistoryError(serr)
		}
		if aerr := s.appendLeanAnchor(ctx, inst.ID, restoreCtx); aerr != nil {
			return nil, aerr
		}
	}
	newFrame, err := model.ParseFrame(rolled.Frame)
	if err != nil {
		return nil, err
	}
	return &RollbackResult{Status: rolled.Status, CurrentNodeID: newFrame.CurrentNodeID, GroupStack: newFrame.GroupStack}, nil
}

func (s *instanceService) Migrate(ctx context.Context, req MigrateRequest) (*MigrateResult, error) {
	if strings.TrimSpace(req.SourceInstanceID) == "" {
		return nil, fmt.Errorf("%w: source instance id is required", model.ErrInvalid)
	}
	if strings.TrimSpace(req.WorkflowDefinitionID) == "" {
		return nil, fmt.Errorf("%w: workflow_definition_id is required", model.ErrInvalid)
	}
	if strings.TrimSpace(req.StartNodeID) == "" {
		return nil, fmt.Errorf("%w: start_node_id is required", model.ErrInvalid)
	}
	inst, err := s.instance(ctx, req.SourceInstanceID)
	if err != nil {
		return nil, err
	}
	if err := authorizeInstance(inst, s.requestPrincipal(req.Principal)); err != nil {
		return nil, err
	}

	switch {
	case inst.Status == model.WorkflowRunning:
		return nil, fmt.Errorf("%w: instance %s is running; pause first", model.ErrConflict, req.SourceInstanceID)
	case inst.Status == model.WorkflowPaused, inst.Status == model.WorkflowFailed,
		inst.Status == model.WorkflowWaiting && inst.WaitingReason == model.WaitingReasonInput:
	default:
		return nil, fmt.Errorf("%w: instance %s is not paused, failed, or input-parked", model.ErrConflict, req.SourceInstanceID)
	}
	if inst.TerminationPending {
		return nil, fmt.Errorf("%w: instance %s has termination pending", model.ErrConflict, req.SourceInstanceID)
	}

	targetDef, err := s.wfDefs.GetByID(ctx, req.WorkflowDefinitionID)
	if err != nil {
		return nil, err
	}
	sourceDef, err := s.wfDefs.GetByID(ctx, inst.WorkflowDefinitionID)
	if err != nil {
		return nil, err
	}
	if targetDef.LineageID != sourceDef.LineageID {
		return nil, fmt.Errorf("%w: definition %s is not in the source lineage", model.ErrInvalid, req.WorkflowDefinitionID)
	}
	if targetDef.Version <= sourceDef.Version {
		return nil, fmt.Errorf("%w: definition %s must be newer than version %d", model.ErrInvalid, req.WorkflowDefinitionID, sourceDef.Version)
	}

	wc, err := s.materializer.Resolve(ctx, targetDef.ID)
	if err != nil {
		return nil, err
	}
	if _, err := (&contentGraph{wc: wc}).FindNode(req.StartNodeID); err != nil {
		return nil, fmt.Errorf("%w: start node %q is not in the target definition", model.ErrInvalid, req.StartNodeID)
	}
	if _, interior := model.ParallelInterior(wc)[req.StartNodeID]; interior {
		return nil, fmt.Errorf("%w: start node %q is inside a parallel region", model.ErrInvalid, req.StartNodeID)
	}
	stack, err := (&contentGraph{wc: wc}).GroupStack(req.StartNodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}

	carried, err := unmarshalJSON(inst.Context)
	if err != nil {
		return nil, fmt.Errorf("migrate: source context is not a JSON object: %v", err)
	}
	if len(req.ContextPatch) != 0 && string(req.ContextPatch) != "null" {
		patch, ok := decodeContextObject(req.ContextPatch)
		if !ok {
			return nil, fmt.Errorf("%w: context_patch must be a JSON object", model.ErrInvalid)
		}
		deepMerge(carried, patch)
	}
	// Fresh snapshots apply last with Create precedence, so a patch key
	// can neither smuggle a credential value nor pin a stale one.
	if err := s.snapshotEnvSecrets(ctx, carried); err != nil {
		return nil, err
	}
	contextRaw, err := json.Marshal(carried)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}

	frameRaw, err := model.Frame{CurrentNodeID: req.StartNodeID, GroupStack: stack}.JSON()
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	actor := s.requestActor(req.Principal, req.Actor)
	contextMode := s.resolveContextMode(wc)
	status := model.WorkflowWaiting
	if req.StartPaused {
		status = model.WorkflowPaused
	}
	w := model.WorkflowInstance{
		ID:                   mustNewID(),
		WorkflowDefinitionID: targetDef.ID,
		ContextMode:          contextMode,
		Debug:                false,
		Status:               status,
		WaitingReason:        model.WaitingReasonRunnable,
		Frame:                frameRaw,
		Context:              contextRaw,
		Counters:             mustMarshal(model.Counters{}),
		FormSnapshot:         inst.FormSnapshot,
		MigratedFrom:         inst.ID,
		CreatedBy:            actor,
		UpdatedBy:            actor,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := s.instances.MigrateInstance(ctx, repository.MigrateUpdate{
		New:            w,
		SourceID:       inst.ID,
		SourceRevision: inst.Revision,
		Actor:          actor,
		NewEventData: mustMarshal(map[string]string{
			"workflow_definition_id": targetDef.ID,
			"migrated_from":          inst.ID,
			"source_definition_id":   inst.WorkflowDefinitionID,
			"reason":                 req.Reason,
		}),
		SourceEventData: mustMarshal(map[string]string{
			"new_instance_id":        w.ID,
			"workflow_definition_id": targetDef.ID,
			"start_node_id":          req.StartNodeID,
			"reason":                 req.Reason,
		}),
	}); err != nil {
		if errors.Is(err, repository.ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: instance %s", model.ErrNotFound, req.SourceInstanceID)
		}
		if errors.Is(err, repository.ErrStatusConflict) {
			return nil, fmt.Errorf("%w: instance %s is no longer migratable (concurrent transition won)", model.ErrConflict, req.SourceInstanceID)
		}
		return nil, err
	}
	if contextMode == model.ContextModeLean {
		if err := s.appendLeanAnchor(ctx, w.ID, contextRaw); err != nil {
			return nil, err
		}
	}
	// The source was never running, so nothing local needs interrupting in
	// the common case; Cancel is a no-op then, and fences an in-flight
	// branch worker when the source carried live parallel rows.
	if s.cancels != nil {
		s.cancels.Cancel(inst.ID)
	}
	s.metrics.InstanceCreated()
	s.metrics.InstanceTerminal(string(model.WorkflowStopped), now.Sub(inst.CreatedAt))
	return &MigrateResult{Instance: w, SourceStatus: model.WorkflowStopped}, nil
}

// hasLiveParallel reports whether an instance has a parallel execution
// still waiting for branches or for its join. Completed, failed, and
// cancelled executions are history and never block rollback.
func (s *instanceService) hasLiveParallel(ctx context.Context, instanceID string) (bool, error) {
	exs, err := s.parallel.ListExecutions(ctx, instanceID)
	if err != nil {
		return false, err
	}
	return hasLiveExecution(exs), nil
}

// hasLiveExecution is the pure core of hasLiveParallel: the status path
// calls it on the already-fetched executions instead of reloading them.
func hasLiveExecution(exs []model.ParallelExecution) bool {
	for _, ex := range exs {
		switch ex.Status {
		case model.ParallelExecutionCompleted, model.ParallelExecutionFailed, model.ParallelExecutionCancelled:
		default:
			return true
		}
	}
	return false
}

// resolveRollbackContext restores the full context for a rollback target.
// Full mode restores the occurrence ContextBefore snapshot; lean mode
// resolves the target occurrence cursor over non-superseded history and
// replays from the instance create context, validating the result is a JSON
// object. The returned cursor is nil for full mode.
func (s *instanceService) resolveRollbackContext(ctx context.Context, inst *model.WorkflowInstance, occ *model.NodeInstance, targetOccurrenceID string) (json.RawMessage, *model.HistoryCursor, error) {
	if !leanMode(inst) {
		var restore map[string]any
		if err := json.Unmarshal(occ.ContextBefore, &restore); err != nil || restore == nil {
			return nil, nil, fmt.Errorf("%w: occurrence %q has no restorable context", model.ErrInvalid, targetOccurrenceID)
		}
		return occ.ContextBefore, nil, nil
	}
	rows, err := s.instances.LoadHistory(ctx, inst.ID)
	if err != nil {
		return nil, nil, err
	}
	cursor, found := leanHistoryCursorForOccurrence(rows, occ.ID, 0)
	if !found {
		return nil, nil, fmt.Errorf("%w: occurrence %q has no history", model.ErrConflict, targetOccurrenceID)
	}
	restored, err := s.leanReconstructBefore(ctx, inst, cursor)
	if err != nil {
		return nil, nil, err
	}
	if _, verr := validateRestoredObject(restored); verr != nil {
		return nil, nil, fmt.Errorf("%w: %w", model.ErrConflict, verr)
	}
	return restored, &cursor, nil
}

// contentGraph loads and materializes the executable graph of the
// definition an instance was created from, so a delivery decision reads
// the same node tree the engine executes.
func (s *instanceService) contentGraph(ctx context.Context, inst *model.WorkflowInstance) (*contentGraph, error) {
	// Resolve pre-shapes errors exactly as the triple did: fetch failures
	// pass through, content failures arrive as model.ErrInvalid.
	wc, err := s.materializer.Resolve(ctx, inst.WorkflowDefinitionID)
	if err != nil {
		return nil, err
	}
	return &contentGraph{wc: wc}, nil
}

// replayDelivery answers a repeated delivery. The stored row is the first
// writer's result and is never rewritten, but the caller is authorized
// against the node that delivery targeted, so a key alone does not hand a
// refused caller someone else's delivery. A mismatched payload is a
// conflict: the first delivery wins, so a corrected payload needs a fresh
// key. The target node is resolved fail-closed: a row whose node no longer
// resolves, or resolves to something that is not an input, denies rather
// than returning an unauthorized stored result.
func (s *instanceService) replayDelivery(
	ctx context.Context,
	inst *model.WorkflowInstance,
	graph *contentGraph,
	existing *model.InputDelivery,
	req DeliverInput,
	p auth.Principal,
) (*model.InputDelivery, error) {
	// A multipart replay skips the payload-equality check: byte equality
	// is impossible without uploading first, which is exactly what replay
	// must avoid. The stored delivery returns without re-uploading, still
	// re-authorized against its target node below.
	if !isMultipartRequest(req) && !jsonEqual(existing.Payload, req.Payload) {
		return nil, fmt.Errorf("%w: Idempotency-Key %q was already used with a different payload", model.ErrConflict, req.IdempotencyKey)
	}
	// The row records the occurrence it targeted, so the gate reads the
	// node the delivery actually addressed even when the instance has
	// since advanced past it or finished.
	occ, err := s.instances.GetNodeInstance(ctx, inst.ID, existing.NodeInstanceID)
	if err != nil {
		return nil, fmt.Errorf("%w: delivery %s targets an unresolvable node instance", model.ErrForbidden, existing.ID)
	}
	if occ.BranchID != req.BranchID {
		return nil, fmt.Errorf("%w: Idempotency-Key %q was already used for a different branch", model.ErrConflict, req.IdempotencyKey)
	}
	nc, err := graph.FindNode(occ.NodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: delivery %s targets an unknown node %q", model.ErrForbidden, existing.ID, occ.NodeID)
	}
	if nc.Type != model.NodeTypeInput {
		return nil, fmt.Errorf("%w: delivery %s targets node %q, which is not an input node", model.ErrForbidden, existing.ID, occ.NodeID)
	}
	// A replay re-authorizes whoever is asking now, not whoever wrote the
	// row: an anonymous caller replays only a public node's delivery, and
	// an authenticated caller on a public node still passes the endpoint
	// permission like any other delivery.
	if err := s.authorizeDelivery(ctx, inst, nc, p, isAnonymousDelivery(req, p)); err != nil {
		return nil, err
	}
	return existing, nil
}

// inputTarget is the resolved cursor of an input delivery: the instance
// cursor for parent deliveries, a branch cursor for branch deliveries.
// Resolution loads the cursor and node ahead of authorization (the gate
// needs the node); parked-state assertions stay behind the channel match,
// where they were, so error precedence is unchanged.
type inputTarget struct {
	frame  model.Frame
	node   *model.NodeContent
	branch *model.ParallelBranch
	// branchID is "" for the parent scope.
	branchID string
	attempt  *model.NodeInstance
	// startCtx is the pre-delivery context the lean history diff starts
	// from on post failures.
	startCtx map[string]any
}

func (s *instanceService) resolveInputTarget(ctx context.Context, inst *model.WorkflowInstance, graph *contentGraph, req DeliverInput) (*inputTarget, error) {
	if req.BranchID == "" {
		frame, err := model.ParseFrame(inst.Frame)
		if err != nil {
			return nil, err
		}
		node, err := graph.FindNode(frame.CurrentNodeID)
		if err != nil {
			return nil, err
		}
		if node.Type != model.NodeTypeInput {
			return nil, fmt.Errorf("%w: current node is not an input node", model.ErrConflict)
		}
		return &inputTarget{frame: frame, node: node}, nil
	}
	branch, err := s.parallel.GetBranch(ctx, req.BranchID)
	if err != nil {
		if errors.Is(err, repository.ErrParallelBranchNotFound) {
			return nil, fmt.Errorf("%w: branch %s not found", model.ErrNotFound, req.BranchID)
		}
		return nil, err
	}
	if branch.InstanceID != inst.ID {
		return nil, fmt.Errorf("%w: branch %s does not belong to instance %s", model.ErrConflict, req.BranchID, req.InstanceID)
	}
	frame, err := model.ParseFrame(branch.Frame)
	if err != nil {
		return nil, err
	}
	node, err := graph.FindNode(frame.CurrentNodeID)
	if err != nil {
		return nil, err
	}
	if node.Type != model.NodeTypeInput {
		return nil, fmt.Errorf("%w: current node is not an input node", model.ErrConflict)
	}
	return &inputTarget{frame: frame, node: node, branch: branch, branchID: branch.ID}, nil
}

// assertInputParked verifies the target is parked on its input node and
// returns the live attempt with the target context. The repository
// re-validates under lock; these checks only shape the error.
func (s *instanceService) assertInputParked(ctx context.Context, inst *model.WorkflowInstance, tgt *inputTarget, req DeliverInput) (*model.NodeInstance, map[string]any, error) {
	if tgt.branchID == "" {
		attempt, err := s.instances.GetLiveNodeInstanceByNode(ctx, inst.ID, tgt.node.ID)
		if err != nil {
			if errors.Is(err, repository.ErrNodeInstanceNotFound) {
				return nil, nil, fmt.Errorf("%w: input node is not waiting for input", model.ErrConflict)
			}
			return nil, nil, err
		}
		if inst.Status != model.WorkflowWaiting || inst.WaitingReason != model.WaitingReasonInput {
			return nil, nil, fmt.Errorf("%w: instance %s is not waiting for input", model.ErrConflict, req.InstanceID)
		}
		ctxMap, err := unmarshalJSON(inst.Context)
		if err != nil {
			return nil, nil, err
		}
		return attempt, ctxMap, nil
	}
	// Branch deliveries require the parent parked on its join, waiting
	// or paused (debug step-through and manual pauses hold the parent
	// paused while its branches run). A running parent must not have its
	// branches advanced underneath it.
	if inst.WaitingReason != model.WaitingReasonParallel ||
		(inst.Status != model.WorkflowWaiting && inst.Status != model.WorkflowPaused) {
		return nil, nil, fmt.Errorf("%w: instance %s is not parked on a parallel join", model.ErrConflict, req.InstanceID)
	}
	if tgt.branch.Status != model.ParallelBranchWaiting || tgt.branch.WaitingReason != model.WaitingReasonInput {
		return nil, nil, fmt.Errorf("%w: branch %s is not waiting for input", model.ErrConflict, tgt.branchID)
	}
	attempt, err := s.instances.GetLiveBranchNodeInstanceByNode(ctx, tgt.branchID, tgt.node.ID)
	if err != nil {
		if errors.Is(err, repository.ErrNodeInstanceNotFound) {
			return nil, nil, fmt.Errorf("%w: input node is not waiting for input", model.ErrConflict)
		}
		return nil, nil, err
	}
	ctxMap, err := unmarshalJSON(tgt.branch.Context)
	if err != nil {
		return nil, nil, err
	}
	return attempt, ctxMap, nil
}

// deliveryResolution carries the authorized target of an input delivery from
// resolveAndAuthorize through validation and commit. The live attempt and
// the pre-delivery context live on target (attempt/startCtx); the rest is
// what validation, merge, and commit need without re-resolving.
type deliveryResolution struct {
	inst      *model.WorkflowInstance
	graph     *contentGraph
	target    *inputTarget
	principal auth.Principal
	actor     string
	anonymous bool
}

// resolveAndAuthorize loads the instance, replays an already-recorded
// delivery for the idempotency key, refuses terminal instances, resolves
// the input target, authorizes the caller, and checks channel, parked
// state, and media type — in that order, so error precedence matches the
// inline flow this replaces. A replay hit returns the stored delivery as
// the second value with a nil resolution; otherwise the resolution carries
// forward to validation and commit.
func (s *instanceService) resolveAndAuthorize(ctx context.Context, req DeliverInput) (*deliveryResolution, *model.InputDelivery, error) {
	inst, err := s.instances.GetByID(ctx, req.InstanceID)
	if err != nil {
		return nil, nil, err
	}

	principal := s.deliveryPrincipal(req)
	actor := s.deliveryActor(req, principal)
	anonymous := isAnonymousDelivery(req, principal)
	if err := authorizeInstance(inst, principal); err != nil {
		return nil, nil, err
	}

	// Idempotent replay: an already recorded delivery for this key returns
	// the stored result. Reusing a key with a different payload is a client
	// error (409): the first delivery wins, so a corrected payload needs a
	// fresh key. The lookup comes first because a finished workflow no
	// longer has a cursor: the replay is authorized against the node its
	// own delivery targeted, which the row records.
	existing, err := s.instances.GetDeliveryByKey(ctx, inst.ID, req.IdempotencyKey)
	switch {
	case err == nil:
		graph, gerr := s.contentGraph(ctx, inst)
		if gerr != nil {
			return nil, nil, gerr
		}
		replay, rerr := s.replayDelivery(ctx, inst, graph, existing, req, principal)
		if rerr != nil {
			return nil, nil, rerr
		}
		return nil, replay, nil
	case !errors.Is(err, repository.ErrDeliveryNotFound):
		return nil, nil, err
	}

	// A terminal instance is refused once the key is known to be fresh, so a
	// retry of the delivery that finished the workflow still replays its
	// stored result.
	switch inst.Status {
	case model.WorkflowFinished, model.WorkflowFailed, model.WorkflowStopped:
		return nil, nil, fmt.Errorf("%w: instance %s is terminal", model.ErrConflict, req.InstanceID)
	}

	graph, err := s.contentGraph(ctx, inst)
	if err != nil {
		return nil, nil, err
	}
	tgt, err := s.resolveInputTarget(ctx, inst, graph, req)
	if err != nil {
		return nil, nil, err
	}
	inputNode := tgt.node
	// Authorization is decided here, once the node the delivery targets is
	// known, and before the channel match, so a source header cannot skip
	// the gate. The endpoint permission and the node gate are independent:
	// a caller may hold input:deliver and still be refused by a node that
	// lists neither their user id nor one of their roles. An anonymous
	// caller passes only on a public node and
	// is marked anonymous in the audit trail; an invalid credential never
	// degrades to anonymous, because the router authenticates first.
	if err := s.authorizeDelivery(ctx, inst, inputNode, principal, anonymous); err != nil {
		return nil, nil, err
	}
	source := req.Source
	if source == "" {
		source = model.InputChannelHTTP
	}
	if inputNode.Channel != source {
		return nil, nil, fmt.Errorf("%w: input source %q does not match input node channel %q", model.ErrConflict, source, inputNode.Channel)
	}
	attempt, ctxMap, err := s.assertInputParked(ctx, inst, tgt, req)
	if err != nil {
		return nil, nil, err
	}
	tgt.attempt = attempt
	tgt.startCtx = ctxMap
	// The accept check runs after authorization and the parked check, so a
	// refused or misparked caller learns nothing about the node's media
	// types. A JSON delivery to a JSON-accepting node — every delivery
	// that predates accept — passes through unchanged.
	mediaType := req.ContentType
	if mediaType == "" {
		mediaType = model.MediaTypeJSON
	}
	if !inputNode.Accepts(mediaType) {
		return nil, nil, fmt.Errorf("%w: input node does not accept %s", model.ErrUnsupportedMedia, mediaType)
	}
	return &deliveryResolution{
		inst: inst, graph: graph, target: tgt,
		principal: principal, actor: actor, anonymous: anonymous,
	}, nil, nil
}

// buildDeliveryPayload streams a multipart body into the object store and
// rewrites req.Payload with the synthesized JSON the validation/merge tail
// runs on. JSON deliveries pass through unchanged. The returned bucket and
// keys feed the caller's cleanup guard: they are populated even on failure,
// so a failed build still deletes every committed object.
func (s *instanceService) buildDeliveryPayload(ctx context.Context, inputNode *model.NodeContent, req *DeliverInput) (bucket string, uploaded []string, err error) {
	if !isMultipartRequest(*req) {
		return "", nil, nil
	}
	if s.multipart.Store == nil {
		return "", nil, errors.New("multipart input needs infra.storage, which is not configured on this deployment")
	}
	if req.MultipartSource == nil {
		return "", nil, fmt.Errorf("%w: multipart delivery needs a body source", model.ErrBadRequest)
	}
	mr, err := req.MultipartSource()
	if err != nil {
		return "", nil, fmt.Errorf("%w: cannot read multipart body: %w", model.ErrBadRequest, err)
	}
	if mr == nil {
		return "", nil, fmt.Errorf("%w: multipart delivery needs a body source", model.ErrBadRequest)
	}
	// The build streams file parts into the object store and synthesizes
	// the JSON payload the tail validates, merges, and persists exactly
	// like a JSON delivery. Its keys feed the cleanup guard on every
	// failure past this point.
	result, err := multipart.Build(ctx, mr, inputNode, s.multipart.Store, s.multipart.Limits)
	if err != nil {
		return result.Bucket, result.Keys, err
	}
	req.Payload = result.Payload
	return result.Bucket, result.Keys, nil
}

// deliveryIdentity builds the executor identity validation, hooks, and
// group posts observe for a delivery: the instance and attempt plus the
// branch scope when the delivery targets a branch cursor.
func deliveryIdentity(inst *model.WorkflowInstance, tgt *inputTarget) executor.Identity {
	ident := executor.Identity{
		WorkflowInstanceID: inst.ID,
		NodeInstanceID:     tgt.attempt.ID,
		BranchID:           tgt.branchID,
		NodeID:             tgt.node.ID,
	}
	if tgt.branch != nil {
		ident.ParallelExecutionID = tgt.branch.ParallelExecutionID
		ident.BranchName = tgt.branch.Name
		ident.BranchIndex = tgt.branch.BranchIndex
	}
	return ident
}

// validateAndMerge runs the validation script, merges an accepted payload
// into the context, and runs the post hook. A rejected payload persists
// Accepted=false and returns the stored delivery as the second value; a
// post-hook failure persists via failAcceptedInput the same way. Otherwise
// it returns the merged post-hook context.
func (s *instanceService) validateAndMerge(ctx context.Context, res *deliveryResolution, req DeliverInput) (map[string]any, *model.InputDelivery, error) {
	inst, tgt := res.inst, res.target
	inputNode, ctxMap := tgt.node, tgt.startCtx
	attempt := tgt.attempt
	// The validation script is the only backend gate on the payload. A
	// referenced form is never consulted: forms are a frontend concern, and
	// a payload violating the form's schema but passing the script is
	// accepted. Rejection persists Accepted=false; anonymous deliveries
	// record the anonymous marker alongside it, so the audit trail still
	// distinguishes them.
	ident := deliveryIdentity(inst, tgt)
	vr, err := s.validator.Validate(ctx, executor.Request{
		Node: inputNode, Context: ctxMap, Payload: req.Payload,
		InstanceID: inst.ID, NodeInstanceID: attempt.ID,
		BranchID: tgt.branchID, ParallelExecutionID: ident.ParallelExecutionID,
		BranchName: ident.BranchName, BranchIndex: ident.BranchIndex,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: input validation failed: %w", model.ErrInvalid, err)
	}
	if !vr.Valid {
		rejected, rerr := s.instances.DeliverInput(ctx, repository.InputCompletion{
			InstanceID:     inst.ID,
			NodeInstanceID: attempt.ID,
			IdempotencyKey: req.IdempotencyKey,
			Payload:        req.Payload,
			Accepted:       false,
			Error:          vr.Message,
			CreatedBy:      res.actor,
			Anonymous:      res.anonymous,
		})
		if rerr != nil {
			return nil, nil, rerr
		}
		return nil, rejected, nil
	}

	var payload any
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return nil, nil, fmt.Errorf("%w: input body must be valid JSON: %w", model.ErrInvalid, err)
	}
	newCtx, err := cloneContext(ctxMap)
	if err != nil {
		return nil, nil, err
	}
	key := strings.TrimSpace(inputNode.OutputProperty)
	if key == "" {
		key = inputNode.ID
	}
	// The form schema, the validation script, and the post hook's frozen
	// output global all see the raw payload. Only the context merge
	// changes, so record_actor cannot be used to smuggle a different shape
	// past validation.
	newCtx[key] = inputContextValue(inputNode, res.principal, res.actor, payload)

	// The post hook sees the accepted payload as a frozen output global and
	// may transform the context after the payload was written.
	postCtx, err := s.hooks.RunPost(ctx, inputNode, newCtx, payload, ident)
	if err != nil {
		failed, ferr := s.failAcceptedInput(ctx, inst, tgt, req, model.NodeFailed, newCtx, err)
		if ferr != nil {
			return nil, nil, ferr
		}
		return nil, failed, nil
	}
	return postCtx, nil, nil
}

// commitDelivery advances the cursor past the input node, runs exited group
// posts, and persists the accepted delivery. Group-post and
// branch-termination failures persist via failAcceptedInput and return
// committed=false, so the caller still cleans up multipart uploads; only a
// fully successful commit returns committed=true.
func (s *instanceService) commitDelivery(ctx context.Context, res *deliveryResolution, req DeliverInput, newCtx map[string]any) (*model.InputDelivery, bool, error) {
	inst, tgt := res.inst, res.target
	inputNode := tgt.node
	frame := tgt.frame
	ident := deliveryIdentity(inst, tgt)
	next := inputNode.NextNode
	done, exited, err := kernel.Advance(&frame, res.graph, next)
	if err != nil {
		return nil, false, err
	}
	if len(exited) > 0 {
		finalCtx, herr := kernel.RunExitedGroupPosts(ctx, s.hooks, res.graph, exited, newCtx, ident)
		if herr != nil {
			// The input attempt finished; the structural group hook failure
			// fails the scope with the latest completed context.
			failed, ferr := s.failAcceptedInput(ctx, inst, tgt, req, model.NodeFinished, finalCtx, herr)
			if ferr != nil {
				return nil, false, ferr
			}
			return failed, false, nil
		}
		newCtx = finalCtx
	}
	c := repository.InputCompletion{
		InstanceID:     inst.ID,
		BranchID:       tgt.branchID,
		NodeInstanceID: tgt.attempt.ID,
		IdempotencyKey: req.IdempotencyKey,
		Payload:        req.Payload,
		Accepted:       true,
		NewFrame:       frame,
		NewContext:     mustMarshal(newCtx),
		CreatedBy:      res.actor,
		Anonymous:      res.anonymous,
	}
	if tgt.branchID != "" {
		if done {
			// Unreachable on validated graphs: every branch path
			// reaches the join. Fail the branch loudly instead of
			// dropping it.
			failed, ferr := s.failAcceptedInput(ctx, inst, tgt, req, model.NodeFinished, newCtx, errors.New("branch terminated without reaching its parallel_end"))
			if ferr != nil {
				return nil, false, ferr
			}
			return failed, false, nil
		}
		delivery, err := s.instances.DeliverInput(ctx, c)
		if err != nil {
			return nil, false, err
		}
		return delivery, true, nil
	}
	status := model.WorkflowWaiting
	var finished *time.Time
	if done {
		status = model.WorkflowFinished
		t := nowUTC()
		finished = &t
	} else if inst.Debug {
		// Step-through: a delivery that advances the cursor parks
		// paused, not runnable, so the next node waits for a resume.
		// A delivery that finishes the workflow stays terminal.
		status = model.WorkflowPaused
	}
	c.Status = status
	c.FinishedAt = finished
	c.History = s.leanInputHistory(inst, tgt.startCtx, newCtx, tgt.attempt)
	delivery, err := s.instances.DeliverInput(ctx, c)
	if err != nil {
		return nil, false, err
	}
	return delivery, true, nil
}

func (s *instanceService) DeliverInput(ctx context.Context, req DeliverInput) (*model.InputDelivery, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: Idempotency-Key header is required", model.ErrInvalid)
	}
	isMultipart := isMultipartRequest(req)
	if !isMultipart && len(req.Payload) == 0 {
		return nil, fmt.Errorf("%w: input body is required", model.ErrInvalid)
	}
	// Multipart uploads commit objects before the delivery succeeds, so
	// every failure past the build deletes them and a failed delivery
	// leaves nothing behind. The flag flips only when the commit below
	// reports a persisted success: a persisted rejection or post failure
	// still cleans up. The JSON path never uploads, so the guard is a
	// no-op there.
	var uploadBucket string
	var uploaded []string
	delivered := false
	defer func() {
		if !delivered && len(uploaded) > 0 {
			s.deleteUploads(context.WithoutCancel(ctx), uploadBucket, uploaded)
		}
	}()
	res, replay, err := s.resolveAndAuthorize(ctx, req)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return replay, nil
	}
	uploadBucket, uploaded, err = s.buildDeliveryPayload(ctx, res.target.node, &req)
	if err != nil {
		return nil, err
	}
	newCtx, done, err := s.validateAndMerge(ctx, res, req)
	if err != nil {
		return nil, err
	}
	if done != nil {
		return done, nil
	}
	delivery, committed, err := s.commitDelivery(ctx, res, req, newCtx)
	if err != nil {
		return nil, err
	}
	delivered = committed
	return delivery, nil
}

// isMultipartRequest reports whether the delivery carries a streaming
// multipart body rather than a JSON payload.
func isMultipartRequest(req DeliverInput) bool {
	return req.ContentType == model.MediaTypeMultipart
}

// deleteUploads best-effort deletes the objects a failed multipart delivery
// committed. A delete that itself fails is logged and skipped: cleanup must
// never mask the delivery error.
func (s *instanceService) deleteUploads(ctx context.Context, bucket string, keys []string) {
	if s.multipart.Store == nil {
		return
	}
	for _, key := range keys {
		if err := s.multipart.Store.Delete(ctx, bucket, key); err != nil {
			slog.Warn("multipart cleanup delete failed", "bucket", bucket, "key", key, "error", err)
		}
	}
}

// inputContextValue is what an accepted delivery writes under the node's
// output property. Without record_actor that is the bare payload, exactly
// as before the field existed. With it, the value is the attribution
// envelope, which carries the deliverer alongside the payload and nothing
// else, so a template reads {{ key.input_data.x }} and the acting user
// {{ key.user_id }}.
func inputContextValue(nc *model.NodeContent, p auth.Principal, actor string, payload any) any {
	if nc == nil || !nc.RecordActor {
		return payload
	}
	userID := actor
	if userID == "" {
		userID = p.UserID
	}
	return map[string]any{
		"user_id":    userID,
		"input_data": payload,
	}
}

// deliveryPrincipal resolves the caller of a delivery. A request that
// carries no principal is the service principal: the broker consumers and
// any internal caller carry no credential, and they are exactly the paths
// the bypass matrix describes. An anonymous HTTP delivery (Anonymous with
// no valid principal) is not the service principal: it skips only the
// public-node path, and the actor resolution marks it instead of
// attributing it to the system user.
func (s *instanceService) deliveryPrincipal(req DeliverInput) auth.Principal {
	if req.Principal != nil {
		return *req.Principal
	}
	return auth.SystemPrincipal(s.actor)
}

// isAnonymousDelivery reports whether the request is an unauthenticated
// HTTP delivery rather than a trusted internal one. The flag travels on
// the request so a present-but-invalid credential is never confused with
// no credential: only the router sets it, and only when the route's auth
// is optional and the request carried nothing to check. A nil principal
// resolves to the service principal with the configured system user, so
// the check reads the flag rather than the resolved identity.
func isAnonymousDelivery(req DeliverInput, p auth.Principal) bool {
	return req.Anonymous && req.Principal == nil && req.Actor == ""
}

// deliveryActor picks the users.id uuid recorded on the delivery. An
// explicit Actor wins, then the resolved principal's user id, then the
// service default, so a delivery is never written without an actor.
func (s *instanceService) deliveryActor(req DeliverInput, p auth.Principal) string {
	if req.Actor != "" {
		return req.Actor
	}
	if p.UserID != "" {
		return p.UserID
	}
	return s.actor
}

// nodeGateReason names the node gate a refused delivery failed. A node
// carrying both gates names both, since either would have admitted the
// caller; a single-gate node keeps the message it has always produced.
func nodeGateReason(inputNode *model.NodeContent) string {
	if len(inputNode.AllowedUsers) > 0 && len(inputNode.AllowedRoles) > 0 {
		return "users [" + strings.Join(inputNode.AllowedUserIDs(), " ") +
			"] or roles [" + strings.Join(inputNode.AllowedRoles, " ") +
			"] are required to deliver to this input node"
	}
	if len(inputNode.AllowedUsers) > 0 {
		return "users [" + strings.Join(inputNode.AllowedUserIDs(), " ") +
			"] are required to deliver to this input node"
	}
	return "roles [" + strings.Join(inputNode.AllowedRoles, " ") +
		"] are required to deliver to this input node"
}

// authorizeDelivery applies the input gates in order: the endpoint
// permission, then the node's user or role gate. A service principal
// passes all of them: it is a trusted internal caller with the wildcard
// permission and no identity to intersect. An anonymous delivery passes
// only on a public node, which is open and unattributed by construction,
// and a public node ignores both node gates for credentialed callers too.
// A denial writes no delivery row and records an input_forbidden audit
// event carrying the instance, the node, and the refused caller, never the
// payload.
func (s *instanceService) authorizeDelivery(
	ctx context.Context,
	inst *model.WorkflowInstance,
	inputNode *model.NodeContent,
	p auth.Principal,
	anonymous bool,
) error {
	// An anonymous delivery is the broker-shaped principal with the
	// anonymous flag: it passes only on a public node and never touches
	// the endpoint permission or the role gate, which need a real caller.
	// A present-but-invalid credential never sets the flag, so it cannot
	// degrade to anonymous here.
	if anonymous {
		if inputNode.Public {
			return nil
		}
		return s.denyAnonymousInput(ctx, inst, inputNode, p, "anonymous delivery requires a public input node")
	}
	if p.Service {
		return nil
	}
	if !p.HasPermission(auth.ActionInputDeliver, s.catalog) {
		return s.denyInput(ctx, inst, inputNode, p, "the "+auth.ActionInputDeliver+" permission is required")
	}
	// A public node ignores both node gates: anyone may deliver to it,
	// credentialed or not. The endpoint permission above still applies to
	// a caller that presented a credential.
	if inputNode.Public {
		return nil
	}
	// The two node gates are a union: when the node lists both users and
	// roles, a listed user passes however their roles read and a listed
	// role passes however the user list reads; only a caller matching
	// neither is refused. A node carrying one gate consults only that
	// gate — the branch matters because CanSatisfyRoles reads an empty
	// list as open, which would admit everyone past a users-only node —
	// and a node carrying neither gate stays open.
	hasUsers := len(inputNode.AllowedUsers) > 0
	hasRoles := len(inputNode.AllowedRoles) > 0
	switch {
	case hasUsers && hasRoles:
		if p.MatchesUserID(inputNode.AllowedUserIDs()) {
			return nil
		}
		if !p.CanSatisfyRoles(inputNode.AllowedRoles) {
			return s.denyInput(ctx, inst, inputNode, p, nodeGateReason(inputNode))
		}
	case hasUsers:
		if !p.MatchesUserID(inputNode.AllowedUserIDs()) {
			return s.denyInput(ctx, inst, inputNode, p, nodeGateReason(inputNode))
		}
	case hasRoles:
		if !p.CanSatisfyRoles(inputNode.AllowedRoles) {
			return s.denyInput(ctx, inst, inputNode, p, nodeGateReason(inputNode))
		}
	}
	return nil
}

// denyInput records the refusal and returns the 403-mapped error. The event
// names the instance, the node, and the identity the caller actually held
// (roles plus resolved user id), so an operator can tell a wrong role from
// a missing permission. An anonymous refusal carries the anonymous marker alongside
// the empty caller identity, so it reads as "nobody knocked" rather than as
// the system user. It never carries the payload: a refused delivery is not
// data the engine stored.
func (s *instanceService) denyInput(
	ctx context.Context,
	inst *model.WorkflowInstance,
	inputNode *model.NodeContent,
	p auth.Principal,
	reason string,
) error {
	return s.denyInputAs(ctx, inst, inputNode, p, reason, false)
}

// denyAnonymousInput records an anonymous refusal the same way denyInput
// does, except the marker names the caller anonymous: the resolved
// principal is the broker-shaped service principal, which would otherwise
// mislabel the event as a service-principal refusal.
func (s *instanceService) denyAnonymousInput(
	ctx context.Context,
	inst *model.WorkflowInstance,
	inputNode *model.NodeContent,
	p auth.Principal,
	reason string,
) error {
	return s.denyInputAs(ctx, inst, inputNode, p, reason, true)
}

func (s *instanceService) denyInputAs(
	ctx context.Context,
	inst *model.WorkflowInstance,
	inputNode *model.NodeContent,
	p auth.Principal,
	reason string,
	anonymous bool,
) error {
	actor := p.UserID
	if actor == "" {
		actor = s.actor
	}
	roles := p.Roles
	if roles == nil {
		roles = []string{}
	}
	actorType := deliveryActorType(p)
	if anonymous {
		actorType = "anonymous"
	}
	data, err := json.Marshal(map[string]any{
		"node_id":      inputNode.ID,
		"channel":      inputNode.Channel,
		"reason":       reason,
		"roles":        roles,
		"subject":      p.Subject,
		"user_id":      p.UserID,
		"delivered_by": actorType,
	})
	if err != nil {
		data = json.RawMessage(`{}`)
	}
	_ = s.instances.AppendEvent(ctx, model.WorkflowInstanceEvent{
		ID: mustNewID(), WorkflowInstanceID: inst.ID, Type: "input_forbidden",
		Data: data, CreatedBy: actor, CreatedAt: nowUTC(),
	})
	return fmt.Errorf("%w: %s", model.ErrForbidden, reason)
}

// deliveryActorType names the kind of caller a delivery event records. The
// audit trail reads "who" from created_by and "what kind of who" from this:
// the service principal is the operator's machine identity, a user is a
// resolved human, and anonymous is a caller that proved nothing on a public
// node. A zero principal only reaches here for an unauthenticated
// deployment, where the pre-authentication behavior is preserved.
func deliveryActorType(p auth.Principal) string {
	if p.Service {
		return "service"
	}
	if p.UserID != "" || p.Subject != "" {
		return "user"
	}
	return "anonymous"
}

// leanInputHistory computes the delivery diff in the service (DiffMaps of
// the pre-delivery request context to the final context) so the repository
// persists history atomically with delivery without duplicating diff logic.
func (s *instanceService) leanInputHistory(inst *model.WorkflowInstance, before, after map[string]any, attempt *model.NodeInstance) *model.NodeContextHistory {
	if !leanMode(inst) {
		return nil
	}
	diff, err := contextdiff.DiffMaps(before, after)
	if err != nil {
		return &model.NodeContextHistory{
			WorkflowInstanceID: inst.ID,
			OccurrenceID:       attempt.ID,
			NodeID:             attempt.NodeID,
			Attempt:            attempt.Attempt,
			Diff:               json.RawMessage(`{"set":{},"unset":[]}`),
		}
	}
	return &model.NodeContextHistory{
		WorkflowInstanceID: inst.ID,
		OccurrenceID:       attempt.ID,
		NodeID:             attempt.NodeID,
		Attempt:            attempt.Attempt,
		Diff:               diff.JSON(),
	}
}

// failAcceptedInput records an accepted input delivery whose post processing
// failed: the delivery stays accepted (the API returns 202), the node
// attempt takes nodeStatus, and the workflow fails with the merged context.
func (s *instanceService) failAcceptedInput(ctx context.Context, inst *model.WorkflowInstance, tgt *inputTarget, req DeliverInput, nodeStatus model.NodeStatus, ctxMap map[string]any, cause error) (*model.InputDelivery, error) {
	c := repository.InputCompletion{
		InstanceID:     inst.ID,
		BranchID:       tgt.branchID,
		NodeInstanceID: tgt.attempt.ID,
		IdempotencyKey: req.IdempotencyKey,
		Payload:        req.Payload,
		Accepted:       true,
		PostFailure:    true,
		NodeStatus:     nodeStatus,
		NewContext:     mustMarshal(ctxMap),
		Error:          cause.Error(),
		CreatedBy:      s.deliveryActor(req, s.deliveryPrincipal(req)),
	}
	if tgt.branchID == "" {
		// Branches run full-context snapshots; their diffs never join
		// the instance history the parent replays.
		c.History = s.leanInputHistory(inst, tgt.startCtx, ctxMap, tgt.attempt)
	}
	return s.instances.DeliverInput(ctx, c)
}

// contentGraph adapts a parsed workflow content to the engine cursor graph.
type contentGraph struct {
	wc *model.WorkflowContent
}

func (g *contentGraph) FindNode(id string) (*model.NodeContent, error) {
	n, _ := g.findNodeAndStack(g.wc.Nodes, id, nil)
	if n == nil {
		return nil, fmt.Errorf("service: unknown node %q in workflow graph", id)
	}
	return n, nil
}

// Node implements kernel.ExitedGroupLookup.
func (g *contentGraph) Node(id string) (*model.NodeContent, error) {
	return g.FindNode(id)
}

func (g *contentGraph) GroupStack(id string) ([]string, error) {
	n, stack := g.findNodeAndStack(g.wc.Nodes, id, nil)
	if n == nil {
		return nil, fmt.Errorf("service: unknown node %q in workflow graph", id)
	}
	return stack, nil
}

// findNodeAndStack walks the materialized tree once, returning the node and
// its ancestor group chain outermost-first. A nil node means unknown id;
// callers map that to the shared unknown-node error.
func (g *contentGraph) findNodeAndStack(nodes []*model.NodeContent, id string, ancestors []string) (*model.NodeContent, []string) {
	for _, n := range nodes {
		if n.ID == id {
			return n, ancestors
		}
		if n.Group != nil {
			if found, stack := g.findNodeAndStack(n.Group.Nodes, id, append(ancestors, n.ID)); found != nil {
				return found, stack
			}
		}
	}
	return nil, nil
}

func (g *contentGraph) TypeOf(id string) (model.NodeType, error) {
	n, err := g.FindNode(id)
	if err != nil {
		return "", err
	}
	return n.Type, nil
}

func (g *contentGraph) NextOf(id string) (string, error) {
	n, err := g.FindNode(id)
	if err != nil {
		return "", err
	}
	return n.NextNode, nil
}

func (g *contentGraph) StartOf(id string) (string, error) {
	n, err := g.FindNode(id)
	if err != nil {
		return "", err
	}
	if n.Group == nil {
		return "", fmt.Errorf("service: node %q is not a group", id)
	}
	return n.Group.StartNodeID, nil
}

func unmarshalJSON(raw json.RawMessage) (map[string]any, error) {
	m := map[string]any{}
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("service: parse context: %w", err)
	}
	return m, nil
}

// deepMerge overlays patch onto dst in place: objects merge recursively,
// arrays and scalars replace. A patch key set to null clears the carried
// value to null rather than deleting it.
func deepMerge(dst, patch map[string]any) {
	for k, v := range patch {
		pv, pok := v.(map[string]any)
		dv, dok := dst[k].(map[string]any)
		if pok && dok {
			deepMerge(dv, pv)
			continue
		}
		dst[k] = v
	}
}

func cloneContext(m map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// jsonEqual compares two JSON payloads semantically: whitespace and key
// order do not count as a difference. Either side failing to parse falls
// back to a trimmed byte comparison.
func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	return reflect.DeepEqual(va, vb)
}
