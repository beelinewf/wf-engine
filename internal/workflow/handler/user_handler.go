package handler

import (
	"net/http"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/gin-gonic/gin"
)

// UserHandler serves the user directory: reads for users:read holders,
// writes for users:write holders, and self-service profile edits on PUT.
type UserHandler struct {
	authSvc service.AuthService
}

// NewUserHandler builds the user handler.
func NewUserHandler(authSvc service.AuthService) *UserHandler {
	return &UserHandler{authSvc: authSvc}
}

// List handles GET /v1/users.
//
// @Summary List users
// @Tags auth
// @Produce json
// @Param page query int false "Page number"
// @Param per_page query int false "Items per page"
// @Param order query string false "Sort order: created_at or -created_at (default)"
// @Param id query string false "Comma-separated user IDs"
// @Param role query string false "Comma-separated membership filter; only local users match"
// @Param issuer query string false "Comma-separated issuer filter"
// @Param is_local query bool false "Only password accounts (true) or provider/system rows (false)"
// @Param email_verified query bool false "Only proven (true) or pending (false) mailboxes"
// @Param is_active query bool false "Only enabled (true) or disabled (false) accounts"
// @Param email query string false "Case-insensitive email substring"
// @Param name query string false "Case-insensitive name substring"
// @Success 200 {object} ListResponse[UserResponse]
// @Failure 401,403,400,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users [get]
func (h *UserHandler) List(c *gin.Context) {
	q, err := ParseUserListQuery(c)
	if err != nil {
		WriteProblem(c, http.StatusBadRequest, err.Error())
		return
	}
	users, total, err := h.authSvc.ListUsers(c.Request.Context(), toUserListQuery(q))
	if err != nil {
		WriteError(c, err)
		return
	}
	items := make([]UserResponse, 0, len(users))
	for _, u := range users {
		items = append(items, toUserResponse(u))
	}
	c.JSON(http.StatusOK, ListResponse[UserResponse]{
		Items:      items,
		Page:       q.Page,
		PerPage:    q.PerPage,
		Total:      total,
		TotalPages: totalPages(total, q.PerPage),
	})
}

// toUserListQuery converts a parsed list query into the repository query.
func toUserListQuery(q *UserListQuery) repository.UserListQuery {
	return repository.UserListQuery{
		Page:          q.Page,
		PerPage:       q.PerPage,
		Order:         q.Order,
		IDs:           q.IDs,
		Roles:         q.Roles,
		Issuers:       q.Issuers,
		IsLocal:       q.IsLocal,
		EmailVerified: q.EmailVerified,
		IsActive:      q.IsActive,
		Email:         q.Email,
		Name:          q.Name,
	}
}

// Get handles GET /v1/users/{id}.
//
// A malformed id is 422 rather than 404: it names no user for any caller, so
// no lookup could ever succeed against it.
//
// @Summary Get one user
// @Tags auth
// @Produce json
// @Param id path string true "User ID"
// @Success 200 {object} UserResponse
// @Failure 401,403,404,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users/{id} [get]
func (h *UserHandler) Get(c *gin.Context) {
	user, err := h.authSvc.GetUser(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(user))
}

// toUserResponse maps one user onto the response shape. Subject and issuer
// are null for a user with no provider identity, like the system user.
// Roles is an empty array rather than null when the user holds none.
func toUserResponse(u model.User) UserResponse {
	roles := u.Roles
	if roles == nil {
		roles = []string{}
	}
	return UserResponse{
		ID:            u.ID,
		Subject:       nullableString(u.Subject),
		Issuer:        nullableString(u.Issuer),
		Name:          u.Name,
		Email:         u.Email,
		Roles:         roles,
		IsActive:      u.IsActive,
		EmailVerified: u.EmailVerified(),
		IsLocal:       u.IsLocal(),
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
	}
}

// Create handles POST /v1/users.
//
// @Summary Create a local user
// @Tags auth
// @Accept json
// @Produce json
// @Param request body CreateUserRequest true "User"
// @Success 201 {object} UserResponse
// @Failure 401,403,400,409,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users [post]
func (h *UserHandler) Create(c *gin.Context) {
	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	user, err := h.authSvc.CreateUser(c.Request.Context(), req.Name, req.Email, req.Password, req.Roles)
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toUserResponse(user))
}

// Update handles PUT /v1/users/{id}. A nil field keeps the stored value.
// The route carries no resource-action gate of its own: the service admits
// a users:write holder for every field and the account itself for name,
// email, and password only.
//
// @Summary Update a local user
// @Tags auth
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Param request body UpdateUserRequest true "User"
// @Success 200 {object} UserResponse
// @Failure 401,403,404,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users/{id} [put]
func (h *UserHandler) Update(c *gin.Context) {
	var req UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	principal := updateActor(c)
	user, err := h.authSvc.UpdateUser(c.Request.Context(), principal, c.Param("id"), service.UpdateUserParams{
		Name:            req.Name,
		Email:           req.Email,
		Roles:           req.Roles,
		Password:        req.Password,
		CurrentPassword: req.CurrentPassword,
		IsActive:        req.IsActive,
		EmailVerified:   req.EmailVerified,
	})
	if err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(user))
}

// Delete handles DELETE /v1/users/{id}. It disables the account: the row
// survives for the audit trail, but the credential is cleared and the
// account can no longer authenticate.
//
// @Summary Disable a local user
// @Tags auth
// @Param id path string true "User ID"
// @Success 204
// @Failure 401,403,400,404,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users/{id} [delete]
func (h *UserHandler) Delete(c *gin.Context) {
	principal := updateActor(c)
	if _, err := h.authSvc.DisableUser(c.Request.Context(), principal, c.Param("id")); err != nil {
		WriteError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ResendVerification handles POST /v1/users/{id}/resend-verification: the
// admin variant of the public resend. A verified account is a silent
// success; an unknown id is 404.
//
// @Summary Resend a user's verification link
// @Tags auth
// @Param id path string true "User ID"
// @Success 204
// @Failure 401,403,400,404,422,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users/{id}/resend-verification [post]
func (h *UserHandler) ResendVerification(c *gin.Context) {
	principal := updateActor(c)
	if err := h.authSvc.ResendVerificationForUser(c.Request.Context(), principal, c.Param("id")); err != nil {
		WriteError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// updateActor returns the caller for user administration: the
// authenticated principal, or the service principal when the deployment
// runs without authentication and every route is open.
func updateActor(c *gin.Context) auth.Principal {
	if p, ok := PrincipalFrom(c); ok {
		return p
	}
	return auth.SystemPrincipal("")
}
