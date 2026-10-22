package handler

import (
	"net/http"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/gin-gonic/gin"
)

// UserHandler serves the read-only user directory.
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
// @Success 200 {object} ListResponse[UserResponse]
// @Failure 401,403,400,500 {object} Problem
// @Security ApiKeyAuth
// @Security BearerAuth
// @Router /v1/users [get]
func (h *UserHandler) List(c *gin.Context) {
	page, perPage, err := parsePagination(c)
	if err != nil {
		WriteProblem(c, http.StatusBadRequest, err.Error())
		return
	}
	users, total, err := h.authSvc.ListUsers(c.Request.Context(), page, perPage)
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
		Page:       page,
		PerPage:    perPage,
		Total:      total,
		TotalPages: totalPages(total, perPage),
	})
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
func toUserResponse(u model.User) UserResponse {
	return UserResponse{
		ID:        u.ID,
		Subject:   nullableString(u.Subject),
		Issuer:    nullableString(u.Issuer),
		Name:      u.Name,
		Email:     u.Email,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}
