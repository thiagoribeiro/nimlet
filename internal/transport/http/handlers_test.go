package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/appservice"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	inhttp "github.com/thiagoribeiro/nimlet/internal/transport/http"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
	"github.com/thiagoribeiro/nimlet/testkit"
)

var testSecret = []byte("test-secret-at-least-32-chars!!!")

// --- helpers ---

func postJSON(handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func putJSON(handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func withBearerToken(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// makeValidToken returns a token and a session mock that accepts it.
func makeValidToken(t *testing.T, sessionID, userID, deviceID string, deviceDisabled bool) (string, *testkit.MockSessionStore) {
	t.Helper()
	token, err := crypto.NewSessionToken(sessionID, testSecret)
	require.NoError(t, err)

	auth := &domain.SessionAuth{
		Session: domain.Session{
			ID:        sessionID,
			UserID:    userID,
			DeviceID:  deviceID,
			ExpiresAt: time.Now().Add(24 * time.Hour),
		},
		DeviceDisabled: deviceDisabled,
	}
	sessions := &testkit.MockSessionStore{
		GetAuthByTokenHashFn: func(_ context.Context, _ string) (*domain.SessionAuth, error) {
			return auth, nil
		},
	}
	return token, sessions
}

// --- AuthMiddleware tests ---

func TestAuthMiddleware_MissingToken_Returns401(t *testing.T) {
	mw := inhttp.NewAuthMiddleware(&testkit.MockSessionStore{}, testSecret)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	mw.Authenticate(next).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestAuthMiddleware_InvalidTokenSignature_Returns401(t *testing.T) {
	mw := inhttp.NewAuthMiddleware(&testkit.MockSessionStore{}, testSecret)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withBearerToken(req, "notavalidtoken")
	rr := httptest.NewRecorder()
	mw.Authenticate(next).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestAuthMiddleware_ExpiredSession_Returns401(t *testing.T) {
	sessionID := "sess-expired"
	token, _ := crypto.NewSessionToken(sessionID, testSecret)

	sessions := &testkit.MockSessionStore{
		GetAuthByTokenHashFn: func(_ context.Context, _ string) (*domain.SessionAuth, error) {
			return &domain.SessionAuth{Session: domain.Session{
				ID:        sessionID,
				UserID:    "user1",
				DeviceID:  "dev1",
				ExpiresAt: time.Now().Add(-1 * time.Hour), // already expired
			}}, nil
		},
	}

	mw := inhttp.NewAuthMiddleware(sessions, testSecret)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withBearerToken(req, token)
	rr := httptest.NewRecorder()
	mw.Authenticate(next).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestAuthMiddleware_DisabledDevice_Returns403(t *testing.T) {
	sessionID := "sess-disabled"
	token, sessions := makeValidToken(t, sessionID, "user1", "dev-disabled", true)

	mw := inhttp.NewAuthMiddleware(sessions, testSecret)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withBearerToken(req, token)
	rr := httptest.NewRecorder()
	mw.Authenticate(next).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestAuthMiddleware_ValidToken_PassesThrough(t *testing.T) {
	token, sessions := makeValidToken(t, "sess1", "user1", "dev1", false)
	mw := inhttp.NewAuthMiddleware(sessions, testSecret)

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withBearerToken(req, token)
	rr := httptest.NewRecorder()
	mw.Authenticate(next).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, reached)
}

// --- handleRegister ---

func TestHandleRegister_Success_Returns201(t *testing.T) {
	svc := &mockAuthSvc{
		registerFn: func(_ context.Context, cmd in.RegisterCmd) (*in.RegisterResult, error) {
			return &in.RegisterResult{
				User:   &domain.User{ID: "u1", Username: cmd.Username},
				Device: &domain.Device{ID: "d1"},
				Token:  "tok.sig",
			}, nil
		},
	}

	handler := inhttp.BuildTestRegisterHandler(svc)
	rr := postJSON(handler, "/register", map[string]any{
		"username": "alice", "password": "pass", "display_name": "Alice", "device_name": "iPhone",
	})

	assert.Equal(t, http.StatusCreated, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "u1", body["user_id"])
	assert.Equal(t, "tok.sig", body["access_token"])
}

func TestHandleRegister_InvalidJSON_Returns400(t *testing.T) {
	handler := inhttp.BuildTestRegisterHandler(&mockAuthSvc{})
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader("not-json"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleRegister_DuplicateUsername_Returns409(t *testing.T) {
	svc := &mockAuthSvc{
		registerFn: func(_ context.Context, _ in.RegisterCmd) (*in.RegisterResult, error) {
			return nil, domain.ErrAlreadyExists
		},
	}
	handler := inhttp.BuildTestRegisterHandler(svc)
	rr := postJSON(handler, "/register", map[string]any{"username": "x", "password": "y"})
	assert.Equal(t, http.StatusConflict, rr.Code)
}

// --- handleLogin ---

func TestHandleLogin_Success_Returns200(t *testing.T) {
	svc := &mockAuthSvc{
		loginFn: func(_ context.Context, cmd in.LoginCmd) (*in.LoginResult, error) {
			return &in.LoginResult{
				Session: &domain.Session{ID: "sess1"},
				Token:   "tok.sig",
			}, nil
		},
	}
	handler := inhttp.BuildTestLoginHandler(svc)
	rr := postJSON(handler, "/login", map[string]any{"username": "bob", "password": "pw", "device_name": "laptop"})
	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "tok.sig", body["access_token"])
}

func TestHandleLogin_WrongPassword_Returns401(t *testing.T) {
	svc := &mockAuthSvc{
		loginFn: func(_ context.Context, _ in.LoginCmd) (*in.LoginResult, error) {
			return nil, domain.ErrInvalidPassword
		},
	}
	handler := inhttp.BuildTestLoginHandler(svc)
	rr := postJSON(handler, "/login", map[string]any{"username": "bob", "password": "wrong"})
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestHandleLogin_DisabledUser_Returns403(t *testing.T) {
	svc := &mockAuthSvc{
		loginFn: func(_ context.Context, _ in.LoginCmd) (*in.LoginResult, error) {
			return nil, domain.ErrUserDisabled
		},
	}
	handler := inhttp.BuildTestLoginHandler(svc)
	rr := postJSON(handler, "/login", map[string]any{"username": "dis", "password": "pw"})
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// --- handleLogout ---

func TestHandleLogout_Success_Returns204(t *testing.T) {
	called := false
	svc := &mockAuthSvc{
		logoutFn: func(_ context.Context, sessionID string) error {
			called = true
			assert.Equal(t, "sess-x", sessionID)
			return nil
		},
	}
	handler := inhttp.BuildTestLogoutHandler(svc, "sess-x", "user1", "dev1")
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)
	assert.True(t, called)
}

// --- handleCreateRoom ---

func TestHandleCreateRoom_Success_Returns201(t *testing.T) {
	svc := &mockRoomSvc{
		createRoomFn: func(_ context.Context, cmd in.CreateRoomCmd, _ string) (*domain.Room, error) {
			return &domain.Room{ID: "room1", Name: cmd.Name, CreatorID: "user1"}, nil
		},
	}
	handler := inhttp.BuildTestCreateRoomHandler(svc, "user1")
	rr := postJSON(handler, "/rooms", map[string]any{"name": "general"})
	assert.Equal(t, http.StatusCreated, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "room1", body["room_id"])
	assert.Equal(t, "general", body["name"])
}

// --- handleInviteUser ---

func TestHandleInviteUser_PermissionDenied_Returns403(t *testing.T) {
	svc := &mockRoomSvc{
		inviteFn: func(_ context.Context, _ in.InviteCmd, _ string) error {
			return domain.ErrPermissionDenied
		},
	}
	r := chi.NewRouter()
	r.Post("/rooms/{roomId}/invite", inhttp.BuildTestInviteHandler(svc, "user1"))
	rr := postJSON(r, "/rooms/room1/invite", map[string]any{"user_id": "user2"})
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// --- handleJoinRoom ---

func TestHandleJoinRoom_Success_Returns204(t *testing.T) {
	called := false
	svc := &mockRoomSvc{
		joinFn: func(_ context.Context, roomID, actorID string) error {
			called = true
			assert.Equal(t, "room1", roomID)
			return nil
		},
	}
	r := chi.NewRouter()
	r.Post("/rooms/{roomId}/join", inhttp.BuildTestJoinRoomHandler(svc, "user1"))
	req := httptest.NewRequest(http.MethodPost, "/rooms/room1/join", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNoContent, rr.Code)
	assert.True(t, called)
}

// --- handleSendEvent ---

func TestHandleSendEvent_NotJoined_Returns403(t *testing.T) {
	svc := &mockRoomSvc{
		sendEventFn: func(_ context.Context, _ in.SendEventCmd, _, _ string) (*domain.Event, error) {
			return nil, domain.ErrPermissionDenied
		},
	}
	r := chi.NewRouter()
	r.Put("/rooms/{roomId}/send/{eventType}/{txnId}", inhttp.BuildTestSendEventHandler(svc, "user1", "dev1"))
	rr := putJSON(r, "/rooms/room1/send/m.room.message/txn1", map[string]any{"body": "hi"})
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestHandleSendEvent_Success_Returns200(t *testing.T) {
	svc := &mockRoomSvc{
		sendEventFn: func(_ context.Context, _ in.SendEventCmd, _, _ string) (*domain.Event, error) {
			return &domain.Event{ID: "evt1", StreamPos: 7}, nil
		},
	}
	r := chi.NewRouter()
	r.Put("/rooms/{roomId}/send/{eventType}/{txnId}", inhttp.BuildTestSendEventHandler(svc, "user1", "dev1"))
	rr := putJSON(r, "/rooms/room1/send/m.room.message/txn1", map[string]any{"body": "hi"})
	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "evt1", body["event_id"])
	assert.NotContains(t, body, "stream_pos", "global stream positions are never exposed")
}

// --- handleGetMessages ---

func TestHandleGetMessages_NotJoined_Returns403(t *testing.T) {
	svc := &mockRoomSvc{
		getMessagesFn: func(_ context.Context, _ in.GetMessagesCmd, _ string) ([]*domain.Event, error) {
			return nil, domain.ErrRoomNotJoined
		},
	}
	r := chi.NewRouter()
	r.Get("/rooms/{roomId}/messages", inhttp.BuildTestGetMessagesHandler(svc, "user1"))
	req := httptest.NewRequest(http.MethodGet, "/rooms/room1/messages", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// --- handleSync ---

func TestHandleSync_FastPath_Returns200WithEvents(t *testing.T) {
	svc := &mockSyncSvc{
		syncFn: func(_ context.Context, cmd in.SyncCmd) (*in.SyncResult, error) {
			return &in.SyncResult{
				NextBatch: 5,
				Events:    []*domain.Event{{ID: "e1", StreamPos: 5}},
			}, nil
		},
	}
	handler := inhttp.BuildTestSyncHandler(svc, "user1")
	req := httptest.NewRequest(http.MethodGet, "/sync?timeout=0", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	next, ok := body["next_batch"].(string)
	require.True(t, ok, "next_batch is an opaque string")
	pos, err := inhttp.TestCursors.Decode(cursor.Sync, "user1", next)
	require.NoError(t, err)
	assert.Equal(t, int64(5), pos)
	events, _ := body["events"].([]any)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0], "stream_pos")
}

func TestHandleSync_Timeout_Returns200Empty(t *testing.T) {
	svc := &mockSyncSvc{
		syncFn: func(_ context.Context, _ in.SyncCmd) (*in.SyncResult, error) {
			return &in.SyncResult{NextBatch: 0, Events: nil}, nil
		},
	}
	handler := inhttp.BuildTestSyncHandler(svc, "user1")
	req := httptest.NewRequest(http.MethodGet, "/sync?timeout=1", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

// --- handleUploadMedia ---

// multipartBody builds a real multipart/form-data body the way an actual HTTP client
// (browser fetch with FormData, or curl -F) would — this is what regressed silently before:
// the handler used to treat the whole multipart envelope as the file, so a test posting a
// raw body without going through multipart.Writer wouldn't have caught the bug.
func multipartBody(t *testing.T, filename, contentType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
		"Content-Type":        {contentType},
	})
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return body, w.FormDataContentType()
}

func TestHandleUploadMedia_ExtractsFilePartNotEnvelope(t *testing.T) {
	var savedContentType string
	var savedBytes []byte
	store := &testkit.MockMediaStore{
		SaveFn: func(_ context.Context, _, ownerID, contentType string, r io.Reader) error {
			assert.Equal(t, "user1", ownerID)
			savedContentType = contentType
			b, err := io.ReadAll(r)
			require.NoError(t, err)
			savedBytes = b
			return nil
		},
	}
	svc := usecase.NewMediaService(store, testkit.NewMockMediaIndex(), "nimlet")
	handler := inhttp.BuildTestUploadMediaHandler(svc, "user1")

	fileContent := []byte("not really a png but stands in for one")
	body, contentType := multipartBody(t, "test.png", "image/png", fileContent)

	req := httptest.NewRequest(http.MethodPost, "/media/upload", body)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	// The stored content type must be the file part's own type ("image/png"), not the
	// outer envelope's ("multipart/form-data; boundary=..."), and the stored bytes must be
	// exactly the file content, not the raw multipart-encoded body around it.
	assert.Equal(t, "image/png", savedContentType)
	assert.Equal(t, fileContent, savedBytes)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.NotEmpty(t, resp["media_id"])
	assert.Contains(t, resp["content_uri"], "mxc://nimlet/")
}

func TestHandleUploadMedia_NonMultipartBody_Returns400(t *testing.T) {
	store := &testkit.MockMediaStore{}
	svc := usecase.NewMediaService(store, testkit.NewMockMediaIndex(), "nimlet")
	handler := inhttp.BuildTestUploadMediaHandler(svc, "user1")

	req := httptest.NewRequest(http.MethodPost, "/media/upload", strings.NewReader("raw bytes, not multipart"))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// --- AppService handlers ---

func TestHandleASCreateUser_Success_Returns201WithSessionToken(t *testing.T) {
	svc := &mockAuthSvc{
		ensureServiceUserFn: func(_ context.Context, username string) (string, string, error) {
			assert.Equal(t, "customer_9", username)
			return "u-9", "sess.tok", nil
		},
	}
	handler := inhttp.BuildTestASCreateUserHandler(svc)
	rr := postJSON(handler, "/appservice/acme/users", map[string]any{"username": "customer_9"})

	assert.Equal(t, http.StatusCreated, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "u-9", body["user_id"])
	assert.Equal(t, "sess.tok", body["session_token"])
}

func TestHandleASCreateRoom_PassesAliasAndTTL_Returns201(t *testing.T) {
	var gotCmd in.CreateRoomCmd
	svc := &mockRoomSvc{
		createRoomFn: func(_ context.Context, cmd in.CreateRoomCmd, actor string) (*domain.Room, error) {
			gotCmd = cmd
			assert.Equal(t, "acme-sys", actor)
			return &domain.Room{ID: "r-1", Name: cmd.Name, CreatorID: actor, JoinRule: domain.JoinRuleInviteOnly}, nil
		},
	}
	r := chi.NewRouter()
	r.Post("/appservice/{asId}/rooms", inhttp.BuildTestASCreateRoomHandler(svc, "acme-sys"))
	rr := postJSON(r, "/appservice/acme/rooms", map[string]any{
		"name": "order_1", "alias": "merchant:1", "ttl_seconds": 3600,
	})

	assert.Equal(t, http.StatusCreated, rr.Code)
	assert.Equal(t, "merchant:1", gotCmd.Alias)
	assert.Equal(t, 3600, gotCmd.TTLSeconds)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "r-1", body["room_id"])
}

func TestHandleASAddMember_Success_Returns204(t *testing.T) {
	var gotRoom, gotTarget, gotActor string
	svc := &mockRoomSvc{
		addMemberFn: func(_ context.Context, roomID, targetUserID, actor string) error {
			gotRoom, gotTarget, gotActor = roomID, targetUserID, actor
			return nil
		},
	}
	r := chi.NewRouter()
	r.Post("/appservice/{asId}/rooms/{roomId}/members", inhttp.BuildTestASAddMemberHandler(svc, "acme-sys"))
	rr := postJSON(r, "/appservice/acme/rooms/room-7/members", map[string]any{"user_id": "customer-9"})

	assert.Equal(t, http.StatusNoContent, rr.Code)
	assert.Equal(t, "room-7", gotRoom)
	assert.Equal(t, "customer-9", gotTarget)
	assert.Equal(t, "acme-sys", gotActor)
}

func TestHandleASAddMember_PermissionDenied_Returns403(t *testing.T) {
	svc := &mockRoomSvc{
		addMemberFn: func(_ context.Context, _, _, _ string) error { return domain.ErrPermissionDenied },
	}
	r := chi.NewRouter()
	r.Post("/appservice/{asId}/rooms/{roomId}/members", inhttp.BuildTestASAddMemberHandler(svc, "acme-sys"))
	rr := postJSON(r, "/appservice/acme/rooms/room-7/members", map[string]any{"user_id": "x"})

	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// --- local service mocks (handler-level, not reusing testkit to avoid import cycle) ---

type mockAuthSvc struct {
	registerFn          func(context.Context, in.RegisterCmd) (*in.RegisterResult, error)
	loginFn             func(context.Context, in.LoginCmd) (*in.LoginResult, error)
	logoutFn            func(context.Context, string) error
	resolveUsernameFn   func(context.Context, string) (*domain.User, error)
	resolveUserIDFn     func(context.Context, string) (*domain.User, error)
	setPushTokenFn      func(context.Context, string, string) error
	ensureServiceUserFn func(context.Context, string) (string, string, error)
}

func (m *mockAuthSvc) Register(ctx context.Context, cmd in.RegisterCmd) (*in.RegisterResult, error) {
	if m.registerFn != nil {
		return m.registerFn(ctx, cmd)
	}
	return nil, nil
}
func (m *mockAuthSvc) Login(ctx context.Context, cmd in.LoginCmd) (*in.LoginResult, error) {
	if m.loginFn != nil {
		return m.loginFn(ctx, cmd)
	}
	return nil, nil
}
func (m *mockAuthSvc) Logout(ctx context.Context, sessionID string) error {
	if m.logoutFn != nil {
		return m.logoutFn(ctx, sessionID)
	}
	return nil
}
func (m *mockAuthSvc) ResolveUsername(ctx context.Context, _, username string) (*domain.User, error) {
	if m.resolveUsernameFn != nil {
		return m.resolveUsernameFn(ctx, username)
	}
	return nil, nil
}
func (m *mockAuthSvc) ResolveUserID(ctx context.Context, userID string) (*domain.User, error) {
	if m.resolveUserIDFn != nil {
		return m.resolveUserIDFn(ctx, userID)
	}
	return nil, nil
}
func (m *mockAuthSvc) SetPushToken(ctx context.Context, deviceID, pushToken string) error {
	if m.setPushTokenFn != nil {
		return m.setPushTokenFn(ctx, deviceID, pushToken)
	}
	return nil
}
func (m *mockAuthSvc) EnsureServiceUser(ctx context.Context, _, username string) (string, string, error) {
	if m.ensureServiceUserFn != nil {
		return m.ensureServiceUserFn(ctx, username)
	}
	return "", "", nil
}

type mockRoomSvc struct {
	createRoomFn  func(context.Context, in.CreateRoomCmd, string) (*domain.Room, error)
	inviteFn      func(context.Context, in.InviteCmd, string) error
	joinFn        func(context.Context, string, string) error
	addMemberFn   func(context.Context, string, string, string) error
	sendEventFn   func(context.Context, in.SendEventCmd, string, string) (*domain.Event, error)
	getMessagesFn func(context.Context, in.GetMessagesCmd, string) ([]*domain.Event, error)
	listRoomsFn   func(context.Context, string) ([]*in.RoomMembership, error)
}

func (m *mockRoomSvc) AddMember(ctx context.Context, roomID, targetUserID string, actor in.Actor) error {
	if m.addMemberFn != nil {
		return m.addMemberFn(ctx, roomID, targetUserID, actor.UserID)
	}
	return nil
}

func (m *mockRoomSvc) CreateRoom(ctx context.Context, cmd in.CreateRoomCmd, actor in.Actor) (*domain.Room, error) {
	if m.createRoomFn != nil {
		return m.createRoomFn(ctx, cmd, actor.UserID)
	}
	return nil, nil
}
func (m *mockRoomSvc) InviteUser(ctx context.Context, cmd in.InviteCmd, actor in.Actor) error {
	if m.inviteFn != nil {
		return m.inviteFn(ctx, cmd, actor.UserID)
	}
	return nil
}
func (m *mockRoomSvc) JoinRoom(ctx context.Context, roomID string, actor in.Actor) error {
	if m.joinFn != nil {
		return m.joinFn(ctx, roomID, actor.UserID)
	}
	return nil
}
func (m *mockRoomSvc) SendEvent(ctx context.Context, cmd in.SendEventCmd, actor, device string) (*domain.Event, error) {
	if m.sendEventFn != nil {
		return m.sendEventFn(ctx, cmd, actor, device)
	}
	return nil, nil
}
func (m *mockRoomSvc) GetMessages(ctx context.Context, cmd in.GetMessagesCmd, actor string) ([]*domain.Event, error) {
	if m.getMessagesFn != nil {
		return m.getMessagesFn(ctx, cmd, actor)
	}
	return nil, nil
}
func (m *mockRoomSvc) GetOrCreateDM(_ context.Context, _ in.Actor, _ string) (*domain.Room, error) {
	return nil, nil
}
func (m *mockRoomSvc) ListRooms(ctx context.Context, actor, _ string, _ int) ([]*in.RoomMembership, string, error) {
	if m.listRoomsFn != nil {
		rooms, err := m.listRoomsFn(ctx, actor)
		return rooms, "", err
	}
	return nil, "", nil
}

type mockSyncSvc struct {
	syncFn func(context.Context, in.SyncCmd) (*in.SyncResult, error)
}

func (m *mockSyncSvc) Sync(ctx context.Context, cmd in.SyncCmd) (*in.SyncResult, error) {
	if m.syncFn != nil {
		return m.syncFn(ctx, cmd)
	}
	return &in.SyncResult{}, nil
}

// --- AppService auth ---

// The {asId} in the path must be the AppService the token belongs to; the actor then
// carries that AppService's namespace.
func TestAppServiceAuth_PathMustMatchToken(t *testing.T) {
	reg, err := appservice.New(
		&domain.AppService{ID: "bot-a", ASToken: "tok-a", NamespaceID: "ns-a", SenderUserID: "sender-a"},
		&domain.AppService{ID: "bot-b", ASToken: "tok-b", NamespaceID: "ns-b", SenderUserID: "sender-b"},
	)
	require.NoError(t, err)
	mw := inhttp.NewAppServiceAuthMiddleware(reg)

	var gotNS string
	r := chi.NewRouter()
	r.Route("/appservice/{asId}", func(r chi.Router) {
		r.Use(mw.Authenticate)
		r.Post("/users", inhttp.ActorNamespaceProbe(&gotNS))
	})

	do := func(path, token string) int {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code
	}

	assert.Equal(t, http.StatusOK, do("/appservice/bot-a/users", "tok-a"))
	assert.Equal(t, "ns-a", gotNS)
	assert.Equal(t, http.StatusForbidden, do("/appservice/bot-b/users", "tok-a"), "token of bot-a under bot-b's path")
	assert.Equal(t, http.StatusUnauthorized, do("/appservice/bot-a/users", "nope"))
}

func TestAuthMiddleware_DisabledNamespace_Returns403(t *testing.T) {
	token, err := crypto.NewSessionToken("sess-ns", testSecret)
	require.NoError(t, err)
	sessions := &testkit.MockSessionStore{
		GetAuthByTokenHashFn: func(_ context.Context, _ string) (*domain.SessionAuth, error) {
			return &domain.SessionAuth{
				Session:           domain.Session{ID: "sess-ns", NamespaceID: "ns-off", UserID: "u", ExpiresAt: time.Now().Add(time.Hour)},
				NamespaceDisabled: true,
			}, nil
		},
	}
	mw := inhttp.NewAuthMiddleware(sessions, testSecret)
	req := withBearerToken(httptest.NewRequest(http.MethodGet, "/", nil), token)
	rr := httptest.NewRecorder()
	mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// A database failure while loading the session must not look like an invalid session:
// clients discard their token on 401.
func TestAuthMiddleware_DatabaseError_Returns503(t *testing.T) {
	token, err := crypto.NewSessionToken("sess-db", testSecret)
	require.NoError(t, err)
	sessions := &testkit.MockSessionStore{
		GetAuthByTokenHashFn: func(_ context.Context, _ string) (*domain.SessionAuth, error) {
			return nil, errors.New("pq: connection refused at 10.0.0.5:5432")
		},
	}
	mw := inhttp.NewAuthMiddleware(sessions, testSecret)
	rr := httptest.NewRecorder()
	mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(rr, withBearerToken(httptest.NewRequest(http.MethodGet, "/", nil), token))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.NotContains(t, rr.Body.String(), "10.0.0.5", "internal error detail must not reach the client")
}

// A numeric (legacy) cursor, or one issued to another user, is rejected.
func TestHandleSync_RejectsForeignOrNumericCursor(t *testing.T) {
	svc := &mockSyncSvc{syncFn: func(_ context.Context, _ in.SyncCmd) (*in.SyncResult, error) {
		t.Fatal("sync must not run with an invalid cursor")
		return nil, nil
	}}
	handler := inhttp.BuildTestSyncHandler(svc, "user1")
	foreign := inhttp.TestCursors.Encode(cursor.Sync, "user2", 10)
	for _, since := range []string{"1500", foreign} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sync?timeout=1&since="+since, nil))
		assert.Equal(t, http.StatusBadRequest, rr.Code, since)
		assert.Contains(t, rr.Body.String(), "INVALID_CURSOR")
	}
}
