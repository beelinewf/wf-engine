package handler

import (
	"net/http"

	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/pkg/ids"
	"github.com/gin-gonic/gin"
)

// FormHandler serves /v1/forms routes.
type FormHandler struct {
	svc service.FormService
}

// NewFormHandler builds the handler.
func NewFormHandler(svc service.FormService) *FormHandler {
	return &FormHandler{svc: svc}
}

// Create handles POST /v1/forms.
//
// @Summary Create form
// @Tags forms
// @Accept json
// @Produce json
// @Param request body SaveFormRequest true "Form"
// @Success 201 {object} FormResponse
// @Failure 401,403,400,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/forms [post]
func (h *FormHandler) Create(c *gin.Context) {
	var req SaveFormRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" || len(req.Schema) == 0 {
		WriteProblem(c, http.StatusUnprocessableEntity, "name and schema are required")
		return
	}
	form, err := h.svc.Create(c.Request.Context(), req.Name, req.ContentType, req.Schema, req.UI)
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toFormResponse(form))
}

// List handles GET /v1/forms.
//
// @Summary List forms
// @Tags forms
// @Produce json
// @Param page query int false "Page number"
// @Param per_page query int false "Items per page"
// @Success 200 {object} ListResponse[FormResponse]
// @Failure 401,403,400,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/forms [get]
func (h *FormHandler) List(c *gin.Context) {
	page, perPage, err := parsePagination(c)
	if err != nil {
		WriteProblem(c, http.StatusBadRequest, err.Error())
		return
	}
	forms, total, err := h.svc.List(c.Request.Context(), page, perPage)
	if err != nil {
		WriteError(c, err)
		return
	}
	items := make([]FormResponse, 0, len(forms))
	for _, form := range forms {
		items = append(items, toFormResponse(form))
	}
	c.JSON(http.StatusOK, ListResponse[FormResponse]{
		Items:      items,
		Page:       page,
		PerPage:    perPage,
		Total:      total,
		TotalPages: totalPages(total, perPage),
	})
}

// Get handles GET /v1/forms/{id}.
//
// @Summary Get form
// @Tags forms
// @Produce json
// @Param id path string true "Form ID"
// @Success 200 {object} FormResponse
// @Failure 401,403,400,404,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/forms/{id} [get]
func (h *FormHandler) Get(c *gin.Context) {
	id := c.Param("id")
	if !ids.Valid(id) {
		WriteProblem(c, http.StatusBadRequest, "id must be a valid uuid")
		return
	}
	form, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toFormResponse(form))
}

// Update handles PUT /v1/forms/{id}. It fully overwrites name/schema/ui;
// an omitted ui clears it.
//
// @Summary Update form
// @Tags forms
// @Accept json
// @Produce json
// @Param id path string true "Form ID"
// @Param request body SaveFormRequest true "Form"
// @Success 200 {object} FormResponse
// @Failure 401,403,400,404,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/forms/{id} [put]
func (h *FormHandler) Update(c *gin.Context) {
	id := c.Param("id")
	if !ids.Valid(id) {
		WriteProblem(c, http.StatusBadRequest, "id must be a valid uuid")
		return
	}
	var req SaveFormRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" || len(req.Schema) == 0 {
		WriteProblem(c, http.StatusUnprocessableEntity, "name and schema are required")
		return
	}
	form, err := h.svc.Update(c.Request.Context(), id, req.Name, req.ContentType, req.Schema, req.UI)
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toFormResponse(form))
}

// Delete handles DELETE /v1/forms/{id}. A form referenced by any workflow
// or node definition is refused with 409.
//
// @Summary Delete form
// @Tags forms
// @Param id path string true "Form ID"
// @Success 204
// @Failure 401,403,400,404,409,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/forms/{id} [delete]
func (h *FormHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if !ids.Valid(id) {
		WriteProblem(c, http.StatusBadRequest, "id must be a valid uuid")
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		WriteError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Usage handles GET /v1/forms/{id}/usage.
// Instances are paginated; definition sets are returned whole.
//
// @Summary Get form usage
// @Tags forms
// @Produce json
// @Param id path string true "Form ID"
// @Param page query int false "Page number"
// @Param per_page query int false "Items per page"
// @Success 200 {object} FormUsageResponse
// @Failure 401,403,400,404,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/forms/{id}/usage [get]
func (h *FormHandler) Usage(c *gin.Context) {
	id := c.Param("id")
	if !ids.Valid(id) {
		WriteProblem(c, http.StatusBadRequest, "id must be a valid uuid")
		return
	}
	page, perPage, err := parsePagination(c)
	if err != nil {
		WriteProblem(c, http.StatusBadRequest, err.Error())
		return
	}
	usage, err := h.svc.Usage(c.Request.Context(), id, page, perPage)
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toFormUsageResponse(usage))
}

func toFormUsageResponse(usage service.FormUsage) FormUsageResponse {
	defs := make([]FormUsageWorkflowDefinitionResponse, 0, len(usage.WorkflowDefinitions))
	for _, def := range usage.WorkflowDefinitions {
		nodes := make([]FormUsageNodeResponse, 0, len(def.Nodes))
		for _, n := range def.Nodes {
			nodes = append(nodes, FormUsageNodeResponse{
				NodeID:           n.NodeID,
				NodeName:         n.NodeName,
				NodeDefinitionID: n.NodeDefinitionID,
			})
		}
		defs = append(defs, FormUsageWorkflowDefinitionResponse{
			ID:        def.ID,
			Name:      def.Name,
			Version:   def.Version,
			LineageID: def.LineageID,
			Nodes:     nodes,
		})
	}
	nodeDefs := make([]FormUsageNodeDefinitionResponse, 0, len(usage.NodeDefinitions))
	for _, def := range usage.NodeDefinitions {
		nodeDefs = append(nodeDefs, FormUsageNodeDefinitionResponse{
			ID:        def.ID,
			Name:      def.Name,
			Version:   def.Version,
			LineageID: def.LineageID,
			Type:      def.Type,
		})
	}
	instances := make([]FormUsageInstanceResponse, 0, len(usage.WorkflowInstances))
	for _, inst := range usage.WorkflowInstances {
		instances = append(instances, FormUsageInstanceResponse{
			ID:                   inst.ID,
			WorkflowDefinitionID: inst.WorkflowDefinitionID,
			Status:               inst.Status,
			IsWaitingOnForm:      inst.IsWaitingOnForm,
			CurrentNodeID:        inst.CurrentNodeID,
		})
	}
	return FormUsageResponse{
		FormID:              usage.FormID,
		WorkflowDefinitions: defs,
		NodeDefinitions:     nodeDefs,
		WorkflowInstances:   instances,
		InstanceTotal:       usage.InstanceTotal,
		Page:                usage.Page,
		PerPage:             usage.PerPage,
	}
}

func toFormResponse(form repository.Form) FormResponse {
	return FormResponse{
		ID:          form.ID,
		Name:        form.Name,
		ContentType: form.ContentType,
		Schema:      form.Schema,
		UI:          form.UI,
		CreatedAt:   form.CreatedAt,
		UpdatedAt:   form.UpdatedAt,
	}
}
