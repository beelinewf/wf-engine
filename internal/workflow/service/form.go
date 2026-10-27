package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beelinewf/wf-engine/internal/workflow/form"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
)

// FormService is the use-case boundary for the forms library.
type FormService interface {
	Create(ctx context.Context, name, contentType string, schema, ui []byte) (repository.Form, error)
	Get(ctx context.Context, id string) (repository.Form, error)
	List(ctx context.Context, page, perPage int) ([]repository.Form, int64, error)
	Update(ctx context.Context, id, name, contentType string, schema, ui []byte) (repository.Form, error)
	Delete(ctx context.Context, id string) error
	Usage(ctx context.Context, id string, page, perPage int) (FormUsage, error)
}

// WorkflowDefinitionResolver resolves a stored workflow definition to its
// materialized node tree. WorkflowDefinitionService satisfies it; the
// narrow interface keeps the form service decoupled.
type WorkflowDefinitionResolver interface {
	Resolve(ctx context.Context, defID string) (*model.WorkflowContent, error)
}

// FormUsage is the aggregated usage of one form: every definition and
// instance that references it. Instances are paginated; definition sets
// are returned whole.
type FormUsage struct {
	FormID              string
	WorkflowDefinitions []FormUsageWorkflowDefinition
	NodeDefinitions     []FormUsageNodeDefinition
	WorkflowInstances   []FormUsageInstance
	InstanceTotal       int64
	Page                int
	PerPage             int
}

// FormUsageWorkflowDefinition is a using workflow definition with the
// exact nodes that reference the form pinpointed.
type FormUsageWorkflowDefinition struct {
	ID        string
	Name      string
	Version   int
	LineageID string
	Nodes     []FormUsageNode
}

// FormUsageNode pinpoints one input node that references the form.
// NodeDefinitionID is set when the node inherits the form through a
// node definition reference, empty for an inline form_id.
type FormUsageNode struct {
	NodeID           string
	NodeName         string
	NodeDefinitionID string
}

// FormUsageNodeDefinition is a using node definition.
type FormUsageNodeDefinition struct {
	ID        string
	Name      string
	Version   int
	LineageID string
	Type      string
}

// FormUsageInstance is an instance of a using workflow definition.
// IsWaitingOnForm flags an instance parked on an input node that uses
// the form.
type FormUsageInstance struct {
	ID                   string
	WorkflowDefinitionID string
	Status               string
	IsWaitingOnForm      bool
	CurrentNodeID        *string
}

type formService struct {
	repo     repository.FormRepository
	resolver WorkflowDefinitionResolver
}

// NewFormService builds the form service.
func NewFormService(repo repository.FormRepository, resolver WorkflowDefinitionResolver) FormService {
	return &formService{repo: repo, resolver: resolver}
}

func (s *formService) Create(ctx context.Context, name, contentType string, schema, ui []byte) (repository.Form, error) {
	ct, err := normalizeContentType(contentType)
	if err != nil {
		return repository.Form{}, err
	}
	if err := validateFormInput(name, schema, ui); err != nil {
		return repository.Form{}, err
	}
	return s.repo.Create(ctx, name, ct, schema, ui)
}

func (s *formService) Get(ctx context.Context, id string) (repository.Form, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *formService) List(ctx context.Context, page, perPage int) ([]repository.Form, int64, error) {
	return s.repo.List(ctx, page, perPage)
}

func (s *formService) Update(ctx context.Context, id, name, contentType string, schema, ui []byte) (repository.Form, error) {
	ct, err := normalizeContentType(contentType)
	if err != nil {
		return repository.Form{}, err
	}
	if err := validateFormInput(name, schema, ui); err != nil {
		return repository.Form{}, err
	}
	return s.repo.Update(ctx, id, name, ct, schema, ui)
}

func (s *formService) Delete(ctx context.Context, id string) error {
	return s.repo.Delete(ctx, id)
}

func (s *formService) Usage(ctx context.Context, id string, page, perPage int) (FormUsage, error) {
	if _, err := s.repo.GetByID(ctx, id); err != nil {
		return FormUsage{}, err
	}
	defs, nodeDefs, instances, total, err := s.repo.Usage(ctx, id, page, perPage)
	if err != nil {
		return FormUsage{}, err
	}
	trees := make(map[string]*model.WorkflowContent, len(defs))
	resolve := func(defID string) (*model.WorkflowContent, error) {
		if tree, ok := trees[defID]; ok {
			return tree, nil
		}
		tree, err := s.resolver.Resolve(ctx, defID)
		if err != nil {
			return nil, err
		}
		trees[defID] = tree
		return tree, nil
	}

	out := FormUsage{
		FormID:              id,
		WorkflowDefinitions: make([]FormUsageWorkflowDefinition, 0, len(defs)),
		NodeDefinitions:     make([]FormUsageNodeDefinition, 0, len(nodeDefs)),
		WorkflowInstances:   make([]FormUsageInstance, 0, len(instances)),
		InstanceTotal:       total,
		Page:                page,
		PerPage:             perPage,
	}
	for _, def := range defs {
		tree, err := resolve(def.ID)
		if err != nil {
			return FormUsage{}, err
		}
		out.WorkflowDefinitions = append(out.WorkflowDefinitions, FormUsageWorkflowDefinition{
			ID:        def.ID,
			Name:      def.Name,
			Version:   def.Version,
			LineageID: def.LineageID,
			Nodes:     collectUsageNodes(tree.Nodes, id),
		})
	}
	for _, def := range nodeDefs {
		out.NodeDefinitions = append(out.NodeDefinitions, FormUsageNodeDefinition{
			ID:        def.ID,
			Name:      def.Name,
			Version:   def.Version,
			LineageID: def.LineageID,
			Type:      def.Type,
		})
	}
	for _, inst := range instances {
		waiting := false
		if inst.Status == string(model.WorkflowWaiting) && inst.CurrentNodeID != nil && *inst.CurrentNodeID != "" {
			tree, err := resolve(inst.WorkflowDefinitionID)
			if err != nil {
				return FormUsage{}, err
			}
			if node := findUsageNode(tree.Nodes, *inst.CurrentNodeID); node != nil && node.FormID == id {
				waiting = true
			}
		}
		out.WorkflowInstances = append(out.WorkflowInstances, FormUsageInstance{
			ID:                   inst.ID,
			WorkflowDefinitionID: inst.WorkflowDefinitionID,
			Status:               inst.Status,
			IsWaitingOnForm:      waiting,
			CurrentNodeID:        inst.CurrentNodeID,
		})
	}
	return out, nil
}

// collectUsageNodes pinpoints the nodes in a materialized tree that
// reference a form, walking group children too.
func collectUsageNodes(nodes []*model.NodeContent, formID string) []FormUsageNode {
	out := []FormUsageNode{}
	var walk func(nodes []*model.NodeContent)
	walk = func(nodes []*model.NodeContent) {
		for _, n := range nodes {
			if n == nil {
				continue
			}
			if n.FormID == formID {
				out = append(out, FormUsageNode{NodeID: n.ID, NodeName: n.Name, NodeDefinitionID: n.NodeDefinitionID})
			}
			if n.Group != nil {
				walk(n.Group.Nodes)
			}
		}
	}
	walk(nodes)
	return out
}

// findUsageNode locates a node by id in a materialized tree, walking
// group children too.
func findUsageNode(nodes []*model.NodeContent, nodeID string) *model.NodeContent {
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.ID == nodeID {
			return n
		}
		if n.Group != nil {
			if found := findUsageNode(n.Group.Nodes, nodeID); found != nil {
				return found
			}
		}
	}
	return nil
}

// normalizeContentType maps an omitted content type to JSON and passes
// the two supported media types through; anything else is invalid.
func normalizeContentType(contentType string) (string, error) {
	switch contentType {
	case "":
		return model.MediaTypeJSON, nil
	case model.MediaTypeJSON, model.MediaTypeMultipart:
		return contentType, nil
	default:
		return "", fmt.Errorf("%w: content type must be %q or %q", model.ErrInvalid, model.MediaTypeJSON, model.MediaTypeMultipart)
	}
}

// validateFormInput enforces the write-time contract shared by create and
// full-overwrite update: a non-blank name, a schema document that compiles,
// and a ui that is either absent or a JSON object. An explicit null ui is
// rejected like any other non-object; omission is how a caller leaves ui
// absent (create) or clears it (update).
func validateFormInput(name string, schema, ui []byte) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: form name is required", model.ErrInvalid)
	}
	if err := form.CheckSchema(schema); err != nil {
		return fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}
	if err := form.CheckFileFields(schema); err != nil {
		return fmt.Errorf("%w: %w", model.ErrInvalid, err)
	}
	if len(ui) > 0 {
		var uiDoc any
		if err := json.Unmarshal(ui, &uiDoc); err != nil {
			return fmt.Errorf("%w: ui must be valid JSON: %w", model.ErrInvalid, err)
		}
		if _, ok := uiDoc.(map[string]any); !ok {
			return fmt.Errorf("%w: ui must be an object", model.ErrInvalid)
		}
	}
	return nil
}
