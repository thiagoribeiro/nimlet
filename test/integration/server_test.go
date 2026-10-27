//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/filesystem"
	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/adapter/redis"
	"github.com/thiagoribeiro/nimlet/internal/app"
	"github.com/thiagoribeiro/nimlet/internal/appservice"
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
	"github.com/thiagoribeiro/nimlet/internal/push"
	nimsync "github.com/thiagoribeiro/nimlet/internal/sync"
	transphttp "github.com/thiagoribeiro/nimlet/internal/transport/http"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
)

var hmacSecret = []byte("integration-test-secret-32-chars!!")

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "TEST_POSTGRES_DSN not set, skipping integration tests")
		os.Exit(0)
	}

	// app.Migrate serialises with other test packages migrating the same database.
	if err := app.Migrate(context.Background(), dsn, "up"); err != nil {
		fmt.Fprintf(os.Stderr, "migration: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// testServer wires the full server stack and returns an httptest.Server + cleanup.
func testServer(t *testing.T) *httptest.Server {
	return newTestEnv(t, nil).ts
}

// testEnv is the full server stack plus direct store access, for tests that need to set up
// namespaces or AppServices the HTTP API cannot create.
type testEnv struct {
	ts         *httptest.Server
	pool       *pgxpool.Pool
	namespaces out.NamespaceStore
	users      out.UserStore
	devices    out.DeviceStore
}

// newTestEnv starts a server; asReg (optional) registers AppServices, whose NamespaceID,
// SenderUserID and SenderDeviceID the caller must already have resolved.
func newTestEnv(t *testing.T, asReg *appservice.Registry) *testEnv {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	ctx := context.Background()
	pool, err := postgres.Open(ctx, dsn, 10)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	redisClient, err := redis.NewClient(redisAddr, "", 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	userStore := postgres.NewUserStore(pool)
	deviceStore := postgres.NewDeviceStore(pool)
	sessionStore := postgres.NewSessionStore(pool)
	roomStore := postgres.NewRoomStore(pool)
	membershipStore := postgres.NewMembershipStore(pool)
	// Grouped writes and reads, as in production (internal/app): every HTTP-level test
	// then runs through the batched paths.
	eventStore := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
		Writes: postgres.GroupLimits{MaxItems: 64, Workers: 4},
		Reads:  postgres.GroupLimits{MaxItems: 64, Workers: 8},
	})
	t.Cleanup(eventStore.Close)
	namespaceStore := postgres.NewNamespaceStore(pool)
	txr := postgres.NewTransactor(pool)
	bus := redis.NewBus(redisClient, 4)

	mediaStore := filesystem.NewMediaStore(t.TempDir())

	presenceStore := redis.NewPresenceStore(redisClient)

	dispatchCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	var asDispatcher in.AppServiceDispatcher = appservice.NopDispatcher{}
	if asReg == nil {
		asReg = appservice.Empty()
	} else {
		d := appservice.NewDispatcher(asReg)
		d.Start(dispatchCtx)
		asDispatcher = d
	}

	authSvc := usecase.NewAuthService(userStore, deviceStore, sessionStore, namespaceStore, txr, hmacSecret)
	roomSvc := usecase.NewRoomService(roomStore, membershipStore, eventStore, bus, txr, asDispatcher, push.NopDispatcher{})
	mediaSvc := usecase.NewMediaService(mediaStore, postgres.NewMediaIndex(pool), "nimlet")

	registry := nimsync.NewWaiterRegistry()
	syncSvc := usecase.NewSyncService(eventStore, membershipStore, registry, presenceStore)

	dispatcher := nimsync.NewDispatcher(bus, registry)
	go dispatcher.Run(dispatchCtx)

	authMiddleware := transphttp.NewAuthMiddleware(sessionStore, hmacSecret)
	asAuthMiddleware := transphttp.NewAppServiceAuthMiddleware(asReg)
	srv := transphttp.NewServer(":0", authMiddleware, asAuthMiddleware, authSvc, roomSvc, syncSvc, mediaSvc, transphttp.Options{Cursors: testCursors(t), MaxBodyBytes: 64 << 10, MaxUploadBytes: 1 << 20, MetricsOnAPI: true, AuthPerIPPerMinute: authPerIPPerMinute})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, pool: pool, namespaces: namespaceStore, users: userStore, devices: deviceStore}
}

// authPerIPPerMinute is the per-IP /login+/register budget of test servers: high enough
// for fixtures (every request comes from 127.0.0.1); TestRateLimits checks enforcement.
const authPerIPPerMinute = 200

// runSuffix keeps usernames unique per run so the suite can repeat against the same database.
var runSuffix = "_" + uuid.NewString()[:8]

// --- helpers ---

func apiPost(t *testing.T, ts *httptest.Server, path string, body any, token string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func apiGet(t *testing.T, ts *httptest.Server, path string, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func apiPut(t *testing.T, ts *httptest.Server, path string, body any, token string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, ts.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
	return m
}

// register creates a new user and returns their access token.
func register(t *testing.T, ts *httptest.Server, username string) string {
	t.Helper()
	resp := apiPost(t, ts, "/register", map[string]any{
		"username":     username,
		"password":     "testpassword",
		"display_name": username,
		"device_name":  "test-device",
	}, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode, "register %s", username)
	body := decodeJSON(t, resp)
	token, ok := body["access_token"].(string)
	require.True(t, ok, "access_token missing from register response")
	return token
}

// --- Tests ---

func TestE2E_RegisterAndLogin(t *testing.T) {
	ts := testServer(t)

	// Register
	resp := apiPost(t, ts, "/register", map[string]any{
		"username": "alice_e2e" + runSuffix, "password": "pw", "display_name": "Alice", "device_name": "pc",
	}, "")
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	body := decodeJSON(t, resp)
	assert.NotEmpty(t, body["access_token"])

	// Duplicate register returns 409
	resp2 := apiPost(t, ts, "/register", map[string]any{
		"username": "alice_e2e" + runSuffix, "password": "pw", "display_name": "Alice", "device_name": "pc",
	}, "")
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
	resp2.Body.Close()

	// Login
	resp3 := apiPost(t, ts, "/login", map[string]any{
		"username": "alice_e2e" + runSuffix, "password": "pw", "device_name": "mobile",
	}, "")
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	b3 := decodeJSON(t, resp3)
	assert.NotEmpty(t, b3["access_token"])
}

func TestE2E_RoomFlow(t *testing.T) {
	ts := testServer(t)

	ownerToken := register(t, ts, "owner_e2e"+runSuffix)
	memberToken := register(t, ts, "member_e2e"+runSuffix)

	// Create room
	resp := apiPost(t, ts, "/rooms", map[string]any{"name": "general"}, ownerToken)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	roomBody := decodeJSON(t, resp)
	roomID, _ := roomBody["room_id"].(string)
	require.NotEmpty(t, roomID)

	// Get member user ID for invite — register returns no user_id in body?
	// Re-register to get the user_id from the register response.
	// Actually the register response does include user_id:
	resp2 := apiPost(t, ts, "/register", map[string]any{
		"username": "invitee_e2e" + runSuffix, "password": "pw", "display_name": "inv", "device_name": "d",
	}, "")
	require.Equal(t, http.StatusCreated, resp2.StatusCode)
	invBody := decodeJSON(t, resp2)
	invToken, _ := invBody["access_token"].(string)
	invUserID, _ := invBody["user_id"].(string)
	require.NotEmpty(t, invUserID)

	// Invite as owner
	resp3 := apiPost(t, ts, "/rooms/"+roomID+"/invite", map[string]any{"user_id": invUserID}, ownerToken)
	assert.Equal(t, http.StatusNoContent, resp3.StatusCode)
	resp3.Body.Close()

	// Invite by non-owner fails
	resp4 := apiPost(t, ts, "/rooms/"+roomID+"/invite", map[string]any{"user_id": "someone"}, memberToken)
	assert.Equal(t, http.StatusForbidden, resp4.StatusCode)
	resp4.Body.Close()

	// Invitee joins
	resp5 := apiPost(t, ts, "/rooms/"+roomID+"/join", nil, invToken)
	assert.Equal(t, http.StatusNoContent, resp5.StatusCode)
	resp5.Body.Close()

	// Join is idempotent
	resp6 := apiPost(t, ts, "/rooms/"+roomID+"/join", nil, invToken)
	assert.Equal(t, http.StatusNoContent, resp6.StatusCode)
	resp6.Body.Close()
}

func TestE2E_SendEvent_And_GetMessages(t *testing.T) {
	ts := testServer(t)

	ownerToken := register(t, ts, "sender_e2e"+runSuffix)

	// Create room
	resp := apiPost(t, ts, "/rooms", map[string]any{"name": "chat"}, ownerToken)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	roomBody := decodeJSON(t, resp)
	roomID, _ := roomBody["room_id"].(string)

	// Send event
	body := map[string]any{"body": "hello world"}
	resp2 := apiPut(t, ts, fmt.Sprintf("/rooms/%s/send/m.room.message/txn-1", roomID), body, ownerToken)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	evtBody := decodeJSON(t, resp2)
	assert.NotEmpty(t, evtBody["event_id"])
	assert.NotContains(t, evtBody, "stream_pos", "global stream positions are never exposed")

	// Idempotent re-send returns same event
	resp3 := apiPut(t, ts, fmt.Sprintf("/rooms/%s/send/m.room.message/txn-1", roomID), body, ownerToken)
	require.Equal(t, http.StatusOK, resp3.StatusCode)
	evtBody2 := decodeJSON(t, resp3)
	assert.Equal(t, evtBody["event_id"], evtBody2["event_id"])

	// GetMessages
	resp4 := apiGet(t, ts, fmt.Sprintf("/rooms/%s/messages?dir=f&limit=10", roomID), ownerToken)
	require.Equal(t, http.StatusOK, resp4.StatusCode)
	msgBody := decodeJSON(t, resp4)
	chunk, _ := msgBody["chunk"].([]any)
	assert.GreaterOrEqual(t, len(chunk), 1)
}

// TestE2E_SyncLongPoll_EventArrivesWhileWaiting is the critical integration test.
// It verifies the full pipeline:
// client → /sync (long-poll, waits) → event published → Redis → Dispatcher → WaiterRegistry → /sync returns
func TestE2E_SyncLongPoll_EventArrivesWhileWaiting(t *testing.T) {
	ts := testServer(t)

	ownerToken := register(t, ts, "sync_owner_e2e"+runSuffix)
	memberToken := register(t, ts, "sync_member_e2e"+runSuffix)

	// Get member user ID
	resp := apiPost(t, ts, "/register", map[string]any{
		"username": "sync_invited_e2e" + runSuffix, "password": "pw", "display_name": "inv", "device_name": "d",
	}, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	invBody := decodeJSON(t, resp)
	invToken, _ := invBody["access_token"].(string)
	invUserID, _ := invBody["user_id"].(string)

	// Create room, invite, join
	roomResp := apiPost(t, ts, "/rooms", map[string]any{"name": "sync-test"}, ownerToken)
	require.Equal(t, http.StatusCreated, roomResp.StatusCode)
	roomBody := decodeJSON(t, roomResp)
	roomID, _ := roomBody["room_id"].(string)

	invResp := apiPost(t, ts, "/rooms/"+roomID+"/invite", map[string]any{"user_id": invUserID}, ownerToken)
	require.Equal(t, http.StatusNoContent, invResp.StatusCode)
	invResp.Body.Close()

	joinResp := apiPost(t, ts, "/rooms/"+roomID+"/join", nil, invToken)
	require.Equal(t, http.StatusNoContent, joinResp.StatusCode)
	joinResp.Body.Close()

	// Suppress unused var
	_ = memberToken

	// Start long-poll sync for invitee, with 5s timeout.
	// The invitee has no events yet → will block waiting.
	syncDone := make(chan map[string]any, 1)
	syncErr := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/sync?timeout=5000", nil)
		if err != nil {
			syncErr <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+invToken)
		syncClient := &http.Client{Timeout: 8 * time.Second}
		syncResp, err := syncClient.Do(req)
		if err != nil {
			syncErr <- err
			return
		}
		defer syncResp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(syncResp.Body).Decode(&m); err != nil {
			syncErr <- err
			return
		}
		syncDone <- m
	}()

	// Wait a moment for the long-poll to register as a waiter.
	time.Sleep(100 * time.Millisecond)

	// Owner sends a message — this triggers the wakeup pipeline.
	sendResp := apiPut(t, ts, fmt.Sprintf("/rooms/%s/send/m.room.message/txn-sync-1", roomID),
		map[string]any{"body": "wake up!"}, ownerToken)
	require.Equal(t, http.StatusOK, sendResp.StatusCode)
	sendResp.Body.Close()

	// Expect sync to return with the new event within 3 seconds.
	select {
	case m := <-syncDone:
		events, _ := m["events"].([]any)
		assert.GreaterOrEqual(t, len(events), 1, "sync must return the new event")
		nextBatch, _ := m["next_batch"].(string)
		assert.True(t, strings.HasPrefix(nextBatch, "c1."), "next_batch is an opaque cursor")
	case err := <-syncErr:
		t.Fatalf("sync request failed: %v", err)
	case <-time.After(4 * time.Second):
		t.Fatal("sync did not return within 4s after event was sent")
	}
}

func TestE2E_Sync_Timeout_ReturnsEmpty(t *testing.T) {
	ts := testServer(t)

	token := register(t, ts, "sync_timeout_e2e"+runSuffix)

	start := time.Now()
	resp := apiGet(t, ts, "/sync?timeout=150", token)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSON(t, resp)
	events, _ := body["events"].([]any)
	assert.Empty(t, events, "no events on timeout")
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond, "must wait at least the timeout duration")
}

func TestE2E_AuthMiddleware_Protects_AuthedEndpoints(t *testing.T) {
	ts := testServer(t)

	// Without token
	resp := apiGet(t, ts, "/sync?timeout=0", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// With garbage token
	resp2 := apiGet(t, ts, "/sync?timeout=0", "garbage.token")
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
	resp2.Body.Close()
}

func testCursors(t *testing.T) *cursor.Codec {
	t.Helper()
	c, err := cursor.New(hmacSecret)
	require.NoError(t, err)
	return c
}
