package http

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Server wraps net/http.Server with chi routing and graceful shutdown.
type Server struct {
	http *http.Server
}

// Options configures limits and optional routes of the HTTP server.
type Options struct {
	MaxBodyBytes   int64 // every JSON body, event content included
	MaxUploadBytes int64 // one media upload
	// MetricsOnAPI mounts /metrics on the API router. Leave it off when metrics are
	// served on their own listener (NIMLET_METRICS_ADDR).
	MetricsOnAPI bool
	// TrustProxyHeaders takes the client IP (for rate limits and logs) from
	// X-Forwarded-For / X-Real-IP. Only enable it behind a proxy that sets them: otherwise
	// clients can spoof their IP and dodge per-IP limits.
	TrustProxyHeaders bool
	// Cursors seals stream positions into the opaque tokens clients see (required).
	Cursors *cursor.Codec
	// AuthPerIPPerMinute limits /login + /register per client IP (burst = the same value).
	// Each attempt costs an Argon2 run, so this also bounds hashing load.
	AuthPerIPPerMinute int
}

// Per-account login limit: tight on purpose (brute force from many IPs).
const (
	loginPerAccountPerMin = 10
	loginPerAccountBurst  = 5
)

func NewServer(
	addr string,
	auth *AuthMiddleware,
	asAuth *AppServiceAuthMiddleware,
	authSvc in.AuthService,
	roomSvc in.RoomService,
	syncSvc in.SyncService,
	mediaSvc in.MediaService,
	opts Options,
) *Server {
	r := chi.NewRouter()
	if opts.TrustProxyHeaders {
		r.Use(chimiddleware.RealIP)
	}
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.Recoverer)
	r.Use(observe)
	r.Use(corsMiddleware)

	if opts.MetricsOnAPI {
		r.Get("/metrics", metrics.Handler().ServeHTTP)
	}

	jsonBody := limitBody(opts.MaxBodyBytes)

	// Public endpoints.
	perIP := limitByIP(newRateLimiter(opts.AuthPerIPPerMinute, opts.AuthPerIPPerMinute))
	perAccount := newRateLimiter(loginPerAccountPerMin, loginPerAccountBurst)
	r.With(perIP, jsonBody).Post("/register", handleRegister(authSvc))
	r.With(perIP, jsonBody).Post("/login", handleLogin(authSvc, perAccount))

	// Authenticated endpoints.
	r.Group(func(r chi.Router) {
		r.Use(auth.Authenticate)

		r.Group(func(r chi.Router) {
			r.Use(jsonBody)
			r.Delete("/logout", handleLogout(authSvc))
			r.Get("/me", handleMe(authSvc))
			r.Put("/me/push-token", handleSetPushToken(authSvc))
			r.Get("/users/by-username/{username}", handleResolveUsername(authSvc))

			r.Post("/rooms", handleCreateRoom(roomSvc))
			r.Get("/rooms", handleListRooms(roomSvc))
			r.Post("/rooms/{roomId}/invite", handleInviteUser(roomSvc))
			r.Post("/rooms/{roomId}/join", handleJoinRoom(roomSvc))
			r.Post("/dm/{userId}", handleGetOrCreateDM(roomSvc))
			r.Put("/rooms/{roomId}/send/{eventType}/{txnId}", handleSendEvent(roomSvc))
			r.Get("/rooms/{roomId}/messages", handleGetMessages(roomSvc))

			r.Get("/sync", handleSync(syncSvc, opts.Cursors))
		})

		r.With(limitBody(opts.MaxUploadBytes)).Post("/media/upload", handleUploadMedia(mediaSvc))
		r.Get("/media/{mediaId}", handleDownloadMedia(mediaSvc))
	})

	// AppService endpoints — authenticated via as_token, identity mapped to AS's system user.
	// Mounted under /appservice/{asId} so the middleware sees {asId} and can match it
	// against the token.
	r.Route("/appservice/{asId}", func(r chi.Router) {
		r.Use(asAuth.Authenticate)
		r.Use(jsonBody)
		r.Post("/users", handleASCreateUser(authSvc))
		r.Post("/rooms", handleASCreateRoom(roomSvc))
		r.Post("/rooms/{roomId}/members", handleASAddMember(roomSvc))
		r.Put("/rooms/{roomId}/send/{eventType}/{txnId}", handleSendEvent(roomSvc))
	})

	return &Server{
		http: &http.Server{
			Addr:    addr,
			Handler: r,
			// ReadTimeout covers slow-client attacks (Slowloris).
			// WriteTimeout must exceed the max /sync long-poll window (60s) plus margin.
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 70 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
	}
}

func (s *Server) ListenAndServe() error {
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http: listen: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// Handler returns the underlying http.Handler, used in tests with httptest.NewServer.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}
