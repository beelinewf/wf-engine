package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// FormRepository persists the forms library and the definition references
// that guard form deletion.
type FormRepository interface {
	Create(ctx context.Context, name, contentType string, schema, ui []byte) (Form, error)
	GetByID(ctx context.Context, id string) (Form, error)
	List(ctx context.Context, page, perPage int) ([]Form, int64, error)
	// Update fully overwrites name/content_type/schema/ui; a nil ui
	// clears it. Update has no reference guard: a referenced form stays
	// updatable.
	Update(ctx context.Context, id, name, contentType string, schema, ui []byte) (Form, error)
	// Delete removes an unreferenced form. A form named by any workflow
	// or node definition reference yields model.ErrConflict.
	Delete(ctx context.Context, id string) error
	// SaveWorkflowFormRefs replaces the forms a workflow definition
	// references (delete-conflict tracking).
	SaveWorkflowFormRefs(ctx context.Context, workflowDefinitionID string, formIDs []string) error
	// SaveNodeFormRefs replaces the forms a node definition references
	// (delete-conflict tracking).
	SaveNodeFormRefs(ctx context.Context, nodeDefinitionID string, formIDs []string) error
	// WorkflowFormRefs lists the form ids referenced by a workflow
	// definition.
	WorkflowFormRefs(ctx context.Context, workflowDefinitionID string) ([]string, error)
	// NodeFormRefs lists the form ids referenced by a node definition.
	NodeFormRefs(ctx context.Context, nodeDefinitionID string) ([]string, error)
	// Usage lists the workflow definitions, node definitions, and
	// workflow instances that use a form. A workflow definition uses a
	// form directly (a row in workflow_definition_form_refs) or
	// indirectly (it references a node definition with a row in
	// node_definition_form_refs). Instances are paginated; definition
	// sets are returned whole. All slices are non-nil.
	Usage(ctx context.Context, formID string, page, perPage int) (defs []FormUsageWorkflowDefinition, nodeDefs []FormUsageNodeDefinition, instances []FormUsageInstance, instanceTotal int64, err error)
}

// FormUsageWorkflowDefinition is a workflow definition that uses a form.
// Content is the stored JSON so callers can pinpoint using nodes without
// a second fetch.
type FormUsageWorkflowDefinition struct {
	ID        string
	Name      string
	Version   int
	LineageID string
	Content   json.RawMessage
}

// FormUsageNodeDefinition is a node definition that references a form.
type FormUsageNodeDefinition struct {
	ID        string
	Name      string
	Version   int
	LineageID string
	Type      string
}

// FormUsageInstance is an instance of a workflow definition that uses a
// form.
type FormUsageInstance struct {
	ID                   string
	WorkflowDefinitionID string
	Status               string
	CurrentNodeID        *string
}

// Form is one forms-library entry.
type Form struct {
	ID          string
	Name        string
	ContentType string
	Schema      json.RawMessage
	UI          json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type gormFormRepository struct {
	db *gorm.DB
}

// NewFormRepository builds the GORM-backed repository.
func NewFormRepository(db *gorm.DB) FormRepository {
	return &gormFormRepository{db: db}
}

func (r *gormFormRepository) Create(ctx context.Context, name, contentType string, schema, ui []byte) (Form, error) {
	now := time.Now().UTC()
	row := FormModel{
		ID:          newRepoID(),
		Name:        name,
		ContentType: contentType,
		Schema:      datatypes.JSON(schema),
		UI:          nullableJSON(ui),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Form{}, fmt.Errorf("form create: %w", err)
	}
	return formFromModel(row), nil
}

func (r *gormFormRepository) GetByID(ctx context.Context, id string) (Form, error) {
	var row FormModel
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Form{}, fmt.Errorf("%w: form %s", model.ErrNotFound, id)
	}
	if err != nil {
		return Form{}, fmt.Errorf("form get: %w", err)
	}
	return formFromModel(row), nil
}

func (r *gormFormRepository) List(ctx context.Context, page, perPage int) ([]Form, int64, error) {
	query := r.db.WithContext(ctx).Model(&FormModel{})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("form count: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}
	var rows []FormModel
	if err := query.Order(`"created_at" ASC, "id" ASC`).
		Offset((page - 1) * perPage).
		Limit(perPage).
		Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("form list: %w", err)
	}
	items := make([]Form, 0, len(rows))
	for _, row := range rows {
		items = append(items, formFromModel(row))
	}
	return items, total, nil
}

func (r *gormFormRepository) Update(ctx context.Context, id, name, contentType string, schema, ui []byte) (Form, error) {
	res := r.db.WithContext(ctx).Model(&FormModel{}).Where("id = ?", id).Updates(map[string]any{
		"name":         name,
		"content_type": contentType,
		"schema":       datatypes.JSON(schema),
		"ui":           nullableJSON(ui),
		"updated_at":   time.Now().UTC(),
	})
	if res.Error != nil {
		return Form{}, fmt.Errorf("form update: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return Form{}, fmt.Errorf("%w: form %s", model.ErrNotFound, id)
	}
	return r.GetByID(ctx, id)
}

func (r *gormFormRepository) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var refs int64
		if err := tx.Model(&WorkflowDefinitionFormRefModel{}).
			Where("form_id = ?", id).Count(&refs).Error; err != nil {
			return fmt.Errorf("form refs check: %w", err)
		}
		if refs == 0 {
			if err := tx.Model(&NodeDefinitionFormRefModel{}).
				Where("form_id = ?", id).Count(&refs).Error; err != nil {
				return fmt.Errorf("form refs check: %w", err)
			}
		}
		if refs > 0 {
			return fmt.Errorf("%w: form %s is referenced by definitions", model.ErrConflict, id)
		}
		res := tx.Delete(&FormModel{}, "id = ?", id)
		if res.Error != nil {
			return fmt.Errorf("form delete: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: form %s", model.ErrNotFound, id)
		}
		return nil
	})
}

func (r *gormFormRepository) SaveWorkflowFormRefs(ctx context.Context, workflowDefinitionID string, formIDs []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("workflow_definition_id = ?", workflowDefinitionID).
			Delete(&WorkflowDefinitionFormRefModel{}).Error; err != nil {
			return fmt.Errorf("workflow definition form refs clear: %w", err)
		}
		if len(formIDs) == 0 {
			return nil
		}
		now := time.Now().UTC()
		rows := make([]WorkflowDefinitionFormRefModel, 0, len(formIDs))
		for _, formID := range formIDs {
			rows = append(rows, WorkflowDefinitionFormRefModel{
				WorkflowDefinitionID: workflowDefinitionID,
				FormID:               formID,
				CreatedAt:            now,
			})
		}
		if err := tx.Create(&rows).Error; err != nil {
			return fmt.Errorf("workflow definition form refs create: %w", err)
		}
		return nil
	})
}

func (r *gormFormRepository) SaveNodeFormRefs(ctx context.Context, nodeDefinitionID string, formIDs []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("node_definition_id = ?", nodeDefinitionID).
			Delete(&NodeDefinitionFormRefModel{}).Error; err != nil {
			return fmt.Errorf("node definition form refs clear: %w", err)
		}
		if len(formIDs) == 0 {
			return nil
		}
		now := time.Now().UTC()
		rows := make([]NodeDefinitionFormRefModel, 0, len(formIDs))
		for _, formID := range formIDs {
			rows = append(rows, NodeDefinitionFormRefModel{
				NodeDefinitionID: nodeDefinitionID,
				FormID:           formID,
				CreatedAt:        now,
			})
		}
		if err := tx.Create(&rows).Error; err != nil {
			return fmt.Errorf("node definition form refs create: %w", err)
		}
		return nil
	})
}

func (r *gormFormRepository) WorkflowFormRefs(ctx context.Context, workflowDefinitionID string) ([]string, error) {
	var rows []WorkflowDefinitionFormRefModel
	if err := r.db.WithContext(ctx).
		Where("workflow_definition_id = ?", workflowDefinitionID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("workflow definition form refs list: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.FormID)
	}
	return ids, nil
}

func (r *gormFormRepository) NodeFormRefs(ctx context.Context, nodeDefinitionID string) ([]string, error) {
	var rows []NodeDefinitionFormRefModel
	if err := r.db.WithContext(ctx).
		Where("node_definition_id = ?", nodeDefinitionID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("node definition form refs list: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.FormID)
	}
	return ids, nil
}

func (r *gormFormRepository) Usage(ctx context.Context, formID string, page, perPage int) ([]FormUsageWorkflowDefinition, []FormUsageNodeDefinition, []FormUsageInstance, int64, error) {
	defs := []FormUsageWorkflowDefinition{}
	nodeDefs := []FormUsageNodeDefinition{}
	instances := []FormUsageInstance{}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}

	var directIDs []string
	if err := r.db.WithContext(ctx).Model(&WorkflowDefinitionFormRefModel{}).
		Where("form_id = ?", formID).
		Pluck("workflow_definition_id", &directIDs).Error; err != nil {
		return nil, nil, nil, 0, fmt.Errorf("form usage workflow refs: %w", err)
	}
	var indirectIDs []string
	if err := r.db.WithContext(ctx).Table("workflow_definition_node_refs AS w").
		Select("DISTINCT w.workflow_definition_id").
		Joins("JOIN node_definition_form_refs AS n ON n.node_definition_id = w.node_definition_id").
		Where("n.form_id = ?", formID).
		Pluck("w.workflow_definition_id", &indirectIDs).Error; err != nil {
		return nil, nil, nil, 0, fmt.Errorf("form usage indirect refs: %w", err)
	}
	usingIDs := dedupeIDs(directIDs, indirectIDs)
	if len(usingIDs) > 0 {
		var rows []WorkflowDefinitionModel
		if err := r.db.WithContext(ctx).
			Where("id IN ?", usingIDs).
			Order(`"created_at" ASC, "id" ASC`).
			Find(&rows).Error; err != nil {
			return nil, nil, nil, 0, fmt.Errorf("form usage workflow definitions: %w", err)
		}
		for _, row := range rows {
			defs = append(defs, FormUsageWorkflowDefinition{
				ID:        row.ID,
				Name:      row.Name,
				Version:   row.Version,
				LineageID: row.LineageID,
				Content:   json.RawMessage(row.Content),
			})
		}
	}

	var nodeRows []NodeDefinitionModel
	if err := r.db.WithContext(ctx).
		Joins("JOIN node_definition_form_refs AS r ON r.node_definition_id = node_definitions.id").
		Where("r.form_id = ?", formID).
		Order(`"node_definitions"."created_at" ASC, "node_definitions"."id" ASC`).
		Find(&nodeRows).Error; err != nil {
		return nil, nil, nil, 0, fmt.Errorf("form usage node definitions: %w", err)
	}
	for _, row := range nodeRows {
		nodeDefs = append(nodeDefs, FormUsageNodeDefinition{
			ID:        row.ID,
			Name:      row.Name,
			Version:   row.Version,
			LineageID: row.LineageID,
			Type:      row.Type,
		})
	}

	if len(usingIDs) == 0 {
		return defs, nodeDefs, instances, 0, nil
	}
	query := r.db.WithContext(ctx).Model(&WorkflowInstanceModel{}).
		Where("workflow_definition_id IN ?", usingIDs)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, nil, nil, 0, fmt.Errorf("form usage instance count: %w", err)
	}
	var instanceRows []WorkflowInstanceModel
	if err := query.Order(`"created_at" ASC, "id" ASC`).
		Offset((page - 1) * perPage).
		Limit(perPage).
		Find(&instanceRows).Error; err != nil {
		return nil, nil, nil, 0, fmt.Errorf("form usage instances: %w", err)
	}
	for _, row := range instanceRows {
		instances = append(instances, FormUsageInstance{
			ID:                   row.ID,
			WorkflowDefinitionID: row.WorkflowDefinitionID,
			Status:               row.Status,
			CurrentNodeID:        row.CurrentNodeID,
		})
	}
	return defs, nodeDefs, instances, total, nil
}

// dedupeIDs unions id sets, preserving first-seen order.
func dedupeIDs(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range sets {
		for _, id := range set {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// nullableJSON maps an absent document to SQL NULL so an omitted ui stays
// absent instead of becoming JSON null.
func nullableJSON(raw []byte) datatypes.JSON {
	if len(raw) == 0 {
		return nil
	}
	return datatypes.JSON(raw)
}

func formFromModel(row FormModel) Form {
	return Form{
		ID:          row.ID,
		Name:        row.Name,
		ContentType: row.ContentType,
		Schema:      json.RawMessage(row.Schema),
		UI:          json.RawMessage(row.UI),
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}
