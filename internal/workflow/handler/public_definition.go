package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/gin-gonic/gin"
)

// PublicWorkflowDefinitionResponse is the anonymous GET
// /v1/workflow/definition/{id} body: the definition metadata and graph a
// public-input frontend needs, and nothing else. It is an allowlist
// projection — fields are copied in one by one below, so a definition
// field added tomorrow stays out until it is deliberately admitted.
type PublicWorkflowDefinitionResponse struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Version int                   `json:"version"`
	Content PublicWorkflowContent `json:"content"`
}

// PublicWorkflowContent is the filtered workflow graph.
type PublicWorkflowContent struct {
	StartNodeID string            `json:"start_node_id"`
	Keys        map[string]string `json:"keys,omitempty"`
	Nodes       []PublicNode      `json:"nodes"`
}

// PublicNode is one filtered graph node. Every node carries its identity
// and graph edges; only input nodes additionally carry the delivery
// contract (form, channel, accept) the frontend renders and delivers
// against. Scripts, conditions, validation, hooks, HTTP/execution/poller
// configs, custom configs, metadata, and both authorization gates are
// never copied.
type PublicNode struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	Name             string            `json:"name,omitempty"`
	NodeDefinitionID string            `json:"node_definition_id,omitempty"`
	FormID           string            `json:"form_id,omitempty"`
	Channel          string            `json:"channel,omitempty"`
	OutputProperty   string            `json:"output_property,omitempty"`
	Accept           []string          `json:"accept,omitempty"`
	Public           bool              `json:"public,omitempty"`
	Storage          *PublicStorage    `json:"storage,omitempty"`
	NextNode         string            `json:"next_node,omitempty"`
	StartNodeID      string            `json:"start_node_id,omitempty"`
	Keys             map[string]string `json:"keys,omitempty"`
	Branches         map[string]string `json:"branches,omitempty"`
	ParallelEndNode  string            `json:"parallel_end_node_id,omitempty"`
	Nodes            []PublicNode      `json:"nodes,omitempty"`
}

// PublicStorage is the upload-constraint half of an input node's storage
// block. The bucket and prefix name infrastructure and stay out; the
// frontend constrains its picker from these two fields.
type PublicStorage struct {
	AllowedMIMETypes []string `json:"allowed_mime_types,omitempty"`
	MaxFileBytes     int64    `json:"max_file_bytes,omitempty"`
}

// getPublic serves the anonymous projection of one workflow definition. It
// resolves the materialized tree so referenced input nodes report their
// effective type and form, then projects the allowlist. A definition that
// no longer resolves degrades to its declared fields instead of failing
// the read, mirroring the authenticated degraded-read contract.
func (h *WorkflowDefinitionHandler) getPublic(c *gin.Context, id string) {
	ctx := c.Request.Context()
	if tree, err := h.svc.Resolve(ctx, id); err == nil {
		def, getErr := h.svc.Get(ctx, id)
		if getErr != nil {
			WriteError(c, getErr)
			return
		}
		writePublic(c, PublicWorkflowDefinitionResponse{
			ID:      def.ID,
			Name:    def.Name,
			Version: def.Version,
			Content: PublicWorkflowContent{
				StartNodeID: tree.StartNodeID,
				Keys:        tree.Keys,
				Nodes:       projectPublicNodes(tree.Nodes),
			},
		})
		return
	} else if !errors.Is(err, model.ErrInvalid) {
		WriteError(c, err)
		return
	}
	def, err := h.svc.Get(ctx, id)
	if err != nil {
		WriteError(c, err)
		return
	}
	content, err := publicContentFromRaw(def.Content)
	if err != nil {
		WriteError(c, err)
		return
	}
	writePublic(c, PublicWorkflowDefinitionResponse{
		ID:      def.ID,
		Name:    def.Name,
		Version: def.Version,
		Content: content,
	})
}

// writePublic writes the anonymous body with the headers a shared URL
// needs: the same path serves the full definition to authenticated
// callers, so the response is never stored and always varies on the
// credential headers.
func writePublic(c *gin.Context, resp PublicWorkflowDefinitionResponse) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "Authorization, X-Api-Token")
	c.JSON(http.StatusOK, resp)
}

// projectPublicNodes copies the allowlisted fields of a materialized node
// tree. The tree is shared and read-only; only scalars and references are
// read, nothing is mutated.
func projectPublicNodes(nodes []*model.NodeContent) []PublicNode {
	out := make([]PublicNode, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		p := PublicNode{
			ID:               n.ID,
			Type:             string(n.Type),
			Name:             n.Name,
			NodeDefinitionID: n.NodeDefinitionID,
			NextNode:         n.NextNode,
		}
		if n.Type == model.NodeTypeInput {
			p.FormID = n.FormID
			p.Channel = n.Channel
			p.OutputProperty = n.OutputProperty
			p.Accept = n.EffectiveAccept()
			p.Public = n.Public
			if n.Storage != nil {
				p.Storage = &PublicStorage{
					AllowedMIMETypes: n.Storage.AllowedMIMETypes,
					MaxFileBytes:     n.Storage.MaxFileBytes,
				}
			}
		}
		if n.Group != nil {
			p.StartNodeID = n.Group.StartNodeID
			p.Keys = n.Group.Keys
			p.Nodes = projectPublicNodes(n.Group.Nodes)
		}
		if len(n.ParallelBranches) > 0 {
			p.Branches = n.ParallelBranches
		}
		if n.ParallelEndNodeID != "" {
			p.ParallelEndNode = n.ParallelEndNodeID
		}
		out = append(out, p)
	}
	return out
}

// rawPublicWorkflow mirrors the JSON shape of workflow content with only
// the allowlisted keys declared, so unparsing-sensitive fields are dropped
// by the decoder itself on the degraded path.
type rawPublicWorkflow struct {
	StartNodeID string            `json:"start_node_id"`
	Keys        map[string]string `json:"keys"`
	Nodes       []rawPublicNode   `json:"nodes"`
}

// rawPublicNode mirrors the JSON shape of one node with only the
// allowlisted keys declared.
type rawPublicNode struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	Name             string            `json:"name"`
	NodeDefinitionID string            `json:"node_definition_id"`
	FormID           string            `json:"form_id"`
	Channel          string            `json:"channel"`
	OutputProperty   string            `json:"output_property"`
	Accept           []string          `json:"accept"`
	Public           bool              `json:"public"`
	Storage          *rawPublicStorage `json:"storage"`
	NextNode         string            `json:"next_node"`
	StartNodeID      string            `json:"start_node_id"`
	Keys             map[string]string `json:"keys"`
	Branches         map[string]string `json:"branches"`
	ParallelEndNode  string            `json:"parallel_end_node_id"`
	Nodes            []rawPublicNode   `json:"nodes"`
}

// rawPublicStorage mirrors the JSON shape of an input node's storage block
// with only the upload constraints declared; bucket and prefix are dropped
// by the decoder.
type rawPublicStorage struct {
	AllowedMIMETypes []string `json:"allowed_mime_types"`
	MaxFileBytes     int64    `json:"max_file_bytes"`
}

// publicContentFromRaw projects stored content that no longer resolves:
// every declared allowlisted scalar is copied, including input scalars on
// nodes whose type the current parser rejects, so a legacy frontend flow
// keeps working while every sensitive key stays dropped.
func publicContentFromRaw(raw json.RawMessage) (PublicWorkflowContent, error) {
	var doc rawPublicWorkflow
	if err := json.Unmarshal(raw, &doc); err != nil {
		return PublicWorkflowContent{}, err
	}
	return PublicWorkflowContent{
		StartNodeID: doc.StartNodeID,
		Keys:        doc.Keys,
		Nodes:       projectRawPublicNodes(doc.Nodes),
	}, nil
}

func projectRawPublicNodes(nodes []rawPublicNode) []PublicNode {
	out := make([]PublicNode, 0, len(nodes))
	for _, n := range nodes {
		p := PublicNode{
			ID:               n.ID,
			Type:             n.Type,
			Name:             n.Name,
			NodeDefinitionID: n.NodeDefinitionID,
			FormID:           n.FormID,
			Channel:          n.Channel,
			OutputProperty:   n.OutputProperty,
			Accept:           n.Accept,
			Public:           n.Public,
			NextNode:         n.NextNode,
			StartNodeID:      n.StartNodeID,
			Keys:             n.Keys,
			Branches:         n.Branches,
			ParallelEndNode:  n.ParallelEndNode,
			Nodes:            projectRawPublicNodes(n.Nodes),
		}
		if n.Storage != nil {
			p.Storage = &PublicStorage{
				AllowedMIMETypes: n.Storage.AllowedMIMETypes,
				MaxFileBytes:     n.Storage.MaxFileBytes,
			}
		}
		out = append(out, p)
	}
	return out
}
