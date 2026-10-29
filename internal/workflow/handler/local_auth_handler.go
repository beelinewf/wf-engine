package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/gin-gonic/gin"
)

// LocalAuthHandler serves the public local-auth endpoints: register,
// login, verify-email, and resend-verification. The routes are registered
// before the authentication gate (like /v1/auth/config) and only when
// local auth is enabled, each behind the local-auth rate limiter.
type LocalAuthHandler struct {
	authSvc service.AuthService
}

// NewLocalAuthHandler builds the local-auth handler.
func NewLocalAuthHandler(authSvc service.AuthService) *LocalAuthHandler {
	return &LocalAuthHandler{authSvc: authSvc}
}

// Register handles POST /v1/auth/register.
//
// With email verification on the account pends and the response carries
// no token; otherwise it is a login session at once. Either way the
// account is created, so the status is 201.
//
// @Summary Register a local account
// @Tags auth
// @Accept json
// @Produce json
// @Param request body RegisterRequest true "Account"
// @Success 201 {object} RegisterResponse
// @Failure 400,403,409,422,429,500 {object} Problem
// @Router /v1/auth/register [post]
func (h *LocalAuthHandler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	got, err := h.authSvc.Register(c.Request.Context(), req.Name, req.Email, req.Password)
	if err != nil {
		WriteError(c, err)
		return
	}
	resp := RegisterResponse{User: toUserResponse(got.User), PendingVerification: got.PendingVerification}
	if !got.PendingVerification {
		resp.AccessToken = got.Token
		resp.TokenType = "Bearer"
		resp.ExpiresIn = expiresIn(got.ExpiresAt)
	}
	c.JSON(http.StatusCreated, resp)
}

// Login handles POST /v1/auth/login.
//
// Unknown email, wrong password, and disabled account share one 401, so
// the endpoint cannot probe for accounts. A pending account is 403.
//
// @Summary Log in with email and password
// @Tags auth
// @Accept json
// @Produce json
// @Param request body LoginRequest true "Credentials"
// @Success 200 {object} LoginResponse
// @Failure 400,401,403,429,500 {object} Problem
// @Router /v1/auth/login [post]
func (h *LocalAuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	got, err := h.authSvc.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		writeLocalAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, LoginResponse{
		AccessToken: got.Token,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn(got.ExpiresAt),
		User:        toUserResponse(got.User),
	})
}

// VerifyEmail handles POST /v1/auth/verify-email: it consumes the mailed
// token and returns a login session for the verified account.
//
// @Summary Verify an email with its mailed token
// @Tags auth
// @Accept json
// @Produce json
// @Param request body VerifyEmailRequest true "Token"
// @Success 200 {object} LoginResponse
// @Failure 400,401,422,429,500 {object} Problem
// @Router /v1/auth/verify-email [post]
func (h *LocalAuthHandler) VerifyEmail(c *gin.Context) {
	var req VerifyEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	got, err := h.authSvc.VerifyEmail(c.Request.Context(), req.Token)
	if err != nil {
		writeLocalAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, LoginResponse{
		AccessToken: got.Token,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn(got.ExpiresAt),
		User:        toUserResponse(got.User),
	})
}

// ResendVerification handles POST /v1/auth/resend-verification. It always
// answers the same generic success — unknown email, verified account,
// disabled account, and cooldown suppression included — so the endpoint
// cannot probe for accounts.
//
// @Summary Resend a verification link
// @Tags auth
// @Accept json
// @Produce json
// @Param request body ResendVerificationRequest true "Email"
// @Success 200 {object} ResendVerificationResponse
// @Failure 400,429,500 {object} Problem
// @Router /v1/auth/resend-verification [post]
func (h *LocalAuthHandler) ResendVerification(c *gin.Context) {
	var req ResendVerificationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteProblem(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.authSvc.ResendVerification(c.Request.Context(), req.Email); err != nil {
		WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, ResendVerificationResponse{
		Message: "If the account exists and is pending verification, a new link has been sent.",
	})
}

// writeLocalAuthError maps the local-auth sentinels that WriteError does
// not know: a failed login is 401 with a generic detail, and a pending
// account is 403. Everything else falls through to the domain mapping.
func writeLocalAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidCredentials):
		WriteProblem(c, http.StatusUnauthorized, "invalid email or password")
	case errors.Is(err, service.ErrEmailNotVerified):
		WriteProblem(c, http.StatusForbidden, "email not verified")
	default:
		WriteError(c, err)
	}
}

// expiresIn renders a token expiry as whole seconds from now for the
// expires_in field.
func expiresIn(expiresAt time.Time) int64 {
	secs := int64(time.Until(expiresAt).Seconds())
	if secs < 0 {
		return 0
	}
	return secs
}
