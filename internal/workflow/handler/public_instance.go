package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// PublicInstanceStatusResponse is the anonymous GET
// /v1/workflow/instance/{id}/status body: the ids a public-input frontend
// needs to fetch the definition and the form, plus the pending delivery
// contract. It is an allowlist projection — fields are copied in one by
// one below, so a status field added tomorrow stays out until it is
// deliberately admitted. It serves only while the instance waits on a
// public input node; any other state is 403.
type PublicInstanceStatusResponse struct {
	ID                   string             `json:"id"`
	WorkflowDefinitionID string             `json:"workflow_definition_id"`
	Status               string             `json:"status"`
	WaitingReason        *string            `json:"waiting_reason"`
	CurrentNodeID        *string            `json:"current_node_id"`
	PendingInput         PublicPendingInput `json:"pending_input"`
}

// PublicPendingInput is the anonymous delivery contract: what to render
// and where to deliver. It mirrors the pending-input shape minus the
// authorization gates (a public node ignores them) and the actor
// attribution (a delivery detail, not a render input). Form carries the
// frozen bytes when the instance was created with snapshot_forms;
// otherwise the frontend fetches FormID from the public form read.
type PublicPendingInput struct {
	NodeID         string                `json:"node_id"`
	Channel        string                `json:"channel"`
	OutputProperty string                `json:"output_property"`
	FormID         string                `json:"form_id,omitempty"`
	Form           *SnapshotFormResponse `json:"form,omitempty"`
	Accept         []string              `json:"accept"`
	Public         bool                  `json:"public"`
}

// getPublicStatus serves the anonymous projection of one instance status.
// Unknown ids stay 404; an instance that is not parked on a public input
// node is 403, matching the input-delivery precedent for private nodes.
func (h *InstanceHandler) getPublicStatus(c *gin.Context, id string) {
	d, err := h.svc.GetStatusDetail(c.Request.Context(), id, requestPrincipalValue(c))
	if err != nil {
		WriteError(c, err)
		return
	}
	p := d.PendingInput
	if p == nil || !p.Public {
		WriteProblem(c, http.StatusForbidden, "anonymous status requires an instance waiting on a public input node")
		return
	}
	accept := p.Accept
	if accept == nil {
		accept = []string{}
	}
	var form *SnapshotFormResponse
	if p.Form != nil {
		form = &SnapshotFormResponse{Schema: p.Form.Schema, UI: p.Form.UI}
	}
	inst := d.Instance
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization, X-Api-Token")
	c.JSON(http.StatusOK, PublicInstanceStatusResponse{
		ID:                   inst.ID,
		WorkflowDefinitionID: inst.WorkflowDefinitionID,
		Status:               string(inst.Status),
		WaitingReason:        nullableString(string(inst.WaitingReason)),
		CurrentNodeID:        nullableString(inst.CurrentNodeID),
		PendingInput: PublicPendingInput{
			NodeID:         p.NodeID,
			Channel:        p.Channel,
			OutputProperty: p.OutputProperty,
			FormID:         p.FormID,
			Form:           form,
			Accept:         accept,
			Public:         p.Public,
		},
	})
}
