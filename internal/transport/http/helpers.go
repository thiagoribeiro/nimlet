package http

import (
	"encoding/json"
	"errors"
	"net/http"
)

// limitBody caps the request body at max bytes; reading past it fails with
// *http.MaxBytesError, which writeError reports as 413.
func limitBody(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

// decodeBody decodes JSON from r.Body into v.
// On failure it writes a 400 (or 413 past the body limit) and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, err)
			return false
		}
		writeJSON(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: err.Error()})
		return false
	}
	return true
}
