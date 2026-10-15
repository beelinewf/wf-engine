package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Types handles GET /v1/node/types.
//
// @Summary List node types and their schemas
// @Tags node-definitions
// @Produce json
// @Success 200 {object} NodeTypeListResponse
// @Failure 401,403,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/node/types [get]
func (h *NodeDefinitionHandler) Types(c *gin.Context) {
	types := h.svc.Types()
	resp := make([]NodeTypeResponse, 0, len(types))
	for _, t := range types {
		resp = append(resp, NodeTypeResponse{Type: t.Type, Builtin: t.Builtin, Schema: t.Schema})
	}
	c.JSON(http.StatusOK, NodeTypeListResponse{Items: resp})
}
