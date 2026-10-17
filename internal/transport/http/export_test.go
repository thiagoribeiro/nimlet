package http

import (
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"net/http"

	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

// Test helpers that expose internal handlers with a pre-injected actor context.
// Compiled only into tests; used by the external package http_test.

func BuildTestRegisterHandler(svc in.AuthService) http.Handler {
	return handleRegister(svc)
}

func BuildTestLoginHandler(svc in.AuthService) http.Handler {
	return handleLogin(svc, newRateLimiter(1000, 1000))
}

// BuildTestLogoutHandler wraps handleLogout and injects a fixed actor.
func BuildTestLogoutHandler(svc in.AuthService, sessionID, userID, deviceID string) http.Handler {
	inner := handleLogout(svc)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{SessionID: sessionID, UserID: userID, DeviceID: deviceID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

func BuildTestCreateRoomHandler(svc in.RoomService, userID string) http.Handler {
	inner := handleCreateRoom(svc)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

func BuildTestInviteHandler(svc in.RoomService, userID string) http.HandlerFunc {
	inner := handleInviteUser(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

func BuildTestJoinRoomHandler(svc in.RoomService, userID string) http.HandlerFunc {
	inner := handleJoinRoom(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

func BuildTestSendEventHandler(svc in.RoomService, userID, deviceID string) http.HandlerFunc {
	inner := handleSendEvent(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID, DeviceID: deviceID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

func BuildTestGetMessagesHandler(svc in.RoomService, userID string) http.HandlerFunc {
	inner := handleGetMessages(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

func BuildTestSyncHandler(svc in.SyncService, userID string) http.Handler {
	inner := handleSync(svc, TestCursors)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

func BuildTestASCreateUserHandler(svc in.AuthService) http.Handler {
	return handleASCreateUser(svc)
}

func BuildTestASCreateRoomHandler(svc in.RoomService, actorUserID string) http.HandlerFunc {
	inner := handleASCreateRoom(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: actorUserID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

func BuildTestASAddMemberHandler(svc in.RoomService, actorUserID string) http.HandlerFunc {
	inner := handleASAddMember(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: actorUserID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

func BuildTestUploadMediaHandler(svc in.MediaService, userID string) http.HandlerFunc {
	inner := handleUploadMedia(svc)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := withActor(r.Context(), Actor{UserID: userID})
		inner.ServeHTTP(w, r.WithContext(ctx))
	}
}

// ActorNamespaceProbe records the authenticated actor's namespace — lets tests assert
// what identity a middleware produced.
func ActorNamespaceProbe(dst *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*dst = actorFrom(r.Context()).NamespaceID
		w.WriteHeader(http.StatusOK)
	}
}

// TestCursors is the cursor codec of the Build* handlers, so tests can decode the tokens
// they return.
var TestCursors = func() *cursor.Codec {
	c, err := cursor.New([]byte("test-cursor-secret-at-least-32-chars"))
	if err != nil {
		panic(err)
	}
	return c
}()
