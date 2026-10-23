package model

import "errors"

// Domain error sentinels. Services wrap them with context
// (fmt.Errorf("...: %w", model.ErrNotFound)); handlers map them to HTTP
// status codes:
//
//	ErrNotFound           -> 404
//	ErrConflict           -> 409
//	ErrInvalid            -> 422 (request body) or 400 (query)
//	ErrTerminalState      -> 409
//	ErrForbidden          -> 403
//	ErrBadRequest         -> 400
//	ErrPayloadTooLarge    -> 413
//	ErrUnsupportedMedia   -> 415
var (
	ErrNotFound      = errors.New("model: not found")
	ErrConflict      = errors.New("model: conflict")
	ErrInvalid       = errors.New("model: invalid")
	ErrTerminalState = errors.New("model: terminal state")
	// ErrForbidden is a valid credential that failed an authorization
	// gate. It is distinct from a missing or invalid credential, which is
	// a 401: the caller is known, the request is simply not allowed.
	ErrForbidden = errors.New("model: forbidden")
	// ErrBadRequest is a malformed request the server refuses to interpret,
	// such as a multipart body whose field shape violates the contract.
	ErrBadRequest = errors.New("model: bad request")
	// ErrPayloadTooLarge is a delivery breaching a size limit while
	// streaming, such as a file past max_file_bytes.
	ErrPayloadTooLarge = errors.New("model: payload too large")
	// ErrUnsupportedMedia is a delivery whose media type the waiting input
	// node does not accept.
	ErrUnsupportedMedia = errors.New("model: unsupported media type")
)
