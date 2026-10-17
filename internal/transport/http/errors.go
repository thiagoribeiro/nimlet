package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequestID is set on 5xx responses: the detail stays in the server log, and this ID
	// lets an operator find it.
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps err to a status and code. Client errors carry their message; server
// errors never do — their text can include SQL, constraint names or paths — and are logged
// with the request ID returned to the client instead.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	code, status := mapError(err)
	if status >= http.StatusInternalServerError {
		reqID := chimiddleware.GetReqID(r.Context())
		slog.ErrorContext(r.Context(), "request failed", "error", err, "path", r.URL.Path, "request_id", reqID)
		writeJSON(w, status, apiError{Code: code, Message: http.StatusText(status), RequestID: reqID})
		return
	}
	writeJSON(w, status, apiError{Code: code, Message: err.Error()})
}

// errUnavailable marks a failure of a dependency (database) rather than of the request.
var errUnavailable = errors.New("service unavailable")

func mapError(err error) (code string, status int) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return "PAYLOAD_TOO_LARGE", http.StatusRequestEntityTooLarge
	case errors.Is(err, errUnavailable):
		return "UNAVAILABLE", http.StatusServiceUnavailable
	case errors.Is(err, cursor.ErrInvalid):
		return "INVALID_CURSOR", http.StatusBadRequest
	case errors.Is(err, crypto.ErrBusy):
		return "BUSY", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrNotFound):
		return "NOT_FOUND", http.StatusNotFound
	case errors.Is(err, domain.ErrAlreadyExists):
		return "CONFLICT", http.StatusConflict
	case errors.Is(err, domain.ErrPermissionDenied):
		return "FORBIDDEN", http.StatusForbidden
	case errors.Is(err, domain.ErrInvalidToken),
		errors.Is(err, domain.ErrSessionExpired),
		errors.Is(err, domain.ErrInvalidPassword):
		return "UNAUTHORIZED", http.StatusUnauthorized
	case errors.Is(err, domain.ErrUserDisabled),
		errors.Is(err, domain.ErrDeviceDisabled):
		return "FORBIDDEN", http.StatusForbidden
	case errors.Is(err, domain.ErrNamespaceDisabled):
		return "NAMESPACE_DISABLED", http.StatusForbidden
	case errors.Is(err, domain.ErrRegistrationClosed):
		return "REGISTRATION_CLOSED", http.StatusForbidden
	case errors.Is(err, domain.ErrInvalidArgument):
		return "BAD_REQUEST", http.StatusBadRequest
	case errors.Is(err, domain.ErrRoomNotJoined):
		return "NOT_JOINED", http.StatusForbidden
	case errors.Is(err, domain.ErrAlreadyMember):
		return "ALREADY_MEMBER", http.StatusConflict
	default:
		return "INTERNAL_ERROR", http.StatusInternalServerError
	}
}
