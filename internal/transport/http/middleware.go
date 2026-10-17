package http

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/thiagoribeiro/nimlet/internal/appservice"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// AuthMiddleware validates Bearer tokens and injects an Actor into context.
type AuthMiddleware struct {
	sessions out.SessionStore
	secret   []byte
}

func NewAuthMiddleware(sessions out.SessionStore, secret []byte) *AuthMiddleware {
	return &AuthMiddleware{sessions: sessions, secret: secret}
}

func (m *AuthMiddleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "MISSING_TOKEN", Message: "authorization header required"})
			return
		}

		sessionID, err := crypto.ParseSessionToken(token, m.secret)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "invalid token"})
			return
		}

		tokenHash := crypto.TokenHash(token)
		auth, err := m.sessions.GetAuthByTokenHash(r.Context(), tokenHash)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeJSON(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "session not found"})
				return
			}
			// Not the client's fault: a 401 here would make every client discard its session
			// during a database hiccup.
			writeError(w, r, fmt.Errorf("auth: load session: %w: %v", errUnavailable, err))
			return
		}
		sess := &auth.Session
		if subtle.ConstantTimeCompare([]byte(sess.ID), []byte(sessionID)) != 1 {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "invalid token"})
			return
		}
		if sess.IsExpired() {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: domain.ErrSessionExpired.Error()})
			return
		}

		if auth.DeviceDisabled {
			writeJSON(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: "device disabled or not found"})
			return
		}
		if auth.UserDisabled {
			writeJSON(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: domain.ErrUserDisabled.Error()})
			return
		}
		// A disabled namespace stops everything: no reads, writes or sync for its sessions.
		if auth.NamespaceDisabled {
			writeJSON(w, http.StatusForbidden, apiError{Code: "NAMESPACE_DISABLED", Message: domain.ErrNamespaceDisabled.Error()})
			return
		}

		ctx := withActor(r.Context(), Actor{
			SessionID:   sess.ID,
			NamespaceID: sess.NamespaceID,
			UserID:      sess.UserID,
			DeviceID:    sess.DeviceID,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AppServiceAuthMiddleware authenticates requests from AppServices using their as_token.
// It maps the token to the AS's provisioned user+device identity via the Actor context.
type AppServiceAuthMiddleware struct {
	registry *appservice.Registry
}

func NewAppServiceAuthMiddleware(registry *appservice.Registry) *AppServiceAuthMiddleware {
	return &AppServiceAuthMiddleware{registry: registry}
}

func (m *AppServiceAuthMiddleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "MISSING_TOKEN", Message: "authorization header required"})
			return
		}

		as, ok := m.registry.GetByToken(token)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "invalid appservice token"})
			return
		}
		// The token identifies the AppService; the {asId} in the path must name that same
		// one, so one AS's token can never act under another's URL (or namespace).
		if asID := chi.URLParam(r, "asId"); asID != as.ID {
			writeJSON(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: "appservice token does not match path"})
			return
		}

		ctx := withActor(r.Context(), Actor{
			NamespaceID: as.NamespaceID,
			UserID:      as.SenderUserID,
			DeviceID:    as.SenderDeviceID,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// observe logs every request and records its metrics, labelled by the chi route pattern
// (filled in while routing, so it is read after the handler ran).
func observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)

		pattern := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			pattern = rc.RoutePattern()
		}
		metrics.ObserveHTTP(r.Method, pattern, sw.code(), elapsed)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.code(),
			"duration_ms", elapsed.Milliseconds(),
			"request_id", chimiddleware.GetReqID(r.Context()),
		)
	})
}

func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(v, "Bearer "); ok {
		return after
	}
	return ""
}

// statusWriter records the response status for observe. It forwards everything else to
// the real writer: Unwrap lets http.ResponseController set per-route deadlines, and ReadFrom
// lets io.Copy reach net/http's sendfile path for file downloads (zero copy).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
