//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/appservice"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Cross-namespace isolation, end to end. Two namespaces A and B hold the *same* usernames,
// passwords differ; every flow below must stay inside its own namespace.

type nsUser struct {
	ns     string // slug
	name   string
	userID string
	token  string
}

type isolationFixture struct {
	env          *testEnv
	slugA, slugB string
	aliceA, bobA nsUser
	aliceB, bobB nsUser
	asAEvents    *eventSink
	asAID, asBID string
	asAToken     string
	nsAID, nsBID string
}

// eventSink is a fake AppService endpoint that records delivered event IDs.
type eventSink struct {
	mu  sync.Mutex
	ids []string
}

func (s *eventSink) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	var p struct {
		Events []struct {
			ID string `json:"event_id"`
		} `json:"events"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range p.Events {
		s.ids = append(s.ids, e.ID)
	}
}

func (s *eventSink) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.ids {
		if x == id {
			return true
		}
	}
	return false
}

func newIsolationFixture(t *testing.T) *isolationFixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	f := &isolationFixture{slugA: "iso-a-" + suffix, slugB: "iso-b-" + suffix}

	// Namespaces and their AppServices must exist before the server starts (as in prod,
	// where the registry is resolved at boot). Use a bootstrap env for the stores.
	boot := newTestEnv(t, nil)
	for _, slug := range []string{f.slugA, f.slugB} {
		require.NoError(t, boot.namespaces.Create(ctx, &domain.Namespace{
			ID: uuid.NewString(), Slug: slug, Status: domain.NamespaceActive,
			AllowRegistration: true, CreatedAt: time.Now().UTC(),
		}))
	}
	nsA, err := boot.namespaces.GetBySlug(ctx, f.slugA)
	require.NoError(t, err)
	nsB, err := boot.namespaces.GetBySlug(ctx, f.slugB)
	require.NoError(t, err)
	f.nsAID, f.nsBID = nsA.ID, nsB.ID

	provisionAS := func(id string, ns *domain.Namespace, endpoint string) *domain.AppService {
		sender := &domain.User{ID: uuid.NewString(), NamespaceID: ns.ID, Username: "bot", DisplayName: id,
			Type: domain.UserTypeService, CreatedAt: time.Now().UTC()}
		require.NoError(t, boot.users.Create(ctx, sender, ""))
		dev := &domain.Device{ID: "as-device-" + id, UserID: sender.ID, DisplayName: id, CreatedAt: time.Now().UTC()}
		require.NoError(t, boot.devices.Create(ctx, dev))
		return &domain.AppService{
			ID: id, NamespaceSlug: ns.Slug, NamespaceID: ns.ID, Endpoint: endpoint,
			HSToken: "hs-" + id, ASToken: "as-" + id, SenderUsername: "bot",
			SenderUserID: sender.ID, SenderDeviceID: dev.ID, InterestedEvents: []string{"*"},
		}
	}
	f.asAEvents = &eventSink{}
	sinkA := httptest.NewServer(f.asAEvents)
	t.Cleanup(sinkA.Close)
	sinkB := httptest.NewServer(&eventSink{})
	t.Cleanup(sinkB.Close)
	asA := provisionAS("bot-a-"+suffix, nsA, sinkA.URL)
	asB := provisionAS("bot-b-"+suffix, nsB, sinkB.URL)
	f.asAID, f.asBID, f.asAToken = asA.ID, asB.ID, asA.ASToken
	reg, err := appservice.New(asA, asB)
	require.NoError(t, err)

	f.env = newTestEnv(t, reg)
	f.aliceA = registerIn(t, f.env.ts, f.slugA, "alice", "pw-alice-a")
	f.bobA = registerIn(t, f.env.ts, f.slugA, "bob", "pw-bob-a")
	f.aliceB = registerIn(t, f.env.ts, f.slugB, "alice", "pw-alice-b")
	f.bobB = registerIn(t, f.env.ts, f.slugB, "bob", "pw-bob-b")
	return f
}

func registerIn(t *testing.T, ts *httptest.Server, ns, username, password string) nsUser {
	t.Helper()
	resp := apiPost(t, ts, "/register", map[string]any{
		"namespace": ns, "username": username, "password": password, "device_name": "d",
	}, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode, "register %s@%s", username, ns)
	body := decodeJSON(t, resp)
	return nsUser{ns: ns, name: username, userID: body["user_id"].(string), token: body["access_token"].(string)}
}

func login(t *testing.T, ts *httptest.Server, ns, username, password string) int {
	t.Helper()
	resp := apiPost(t, ts, "/login", map[string]any{
		"namespace": ns, "username": username, "password": password, "device_name": "d",
	}, "")
	resp.Body.Close()
	return resp.StatusCode
}

func createRoom(t *testing.T, ts *httptest.Server, token, visibility string) string {
	t.Helper()
	resp := apiPost(t, ts, "/rooms", map[string]any{"name": "general", "visibility": visibility}, token)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return decodeJSON(t, resp)["room_id"].(string)
}

func send(t *testing.T, ts *httptest.Server, token, roomID, txnID, body string) string {
	t.Helper()
	resp := apiPut(t, ts, fmt.Sprintf("/rooms/%s/send/m.room.message/%s", roomID, txnID), map[string]any{"body": body}, token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return decodeJSON(t, resp)["event_id"].(string)
}

// syncAll drains /sync for token from the start and returns every event ID seen.
func syncAll(t *testing.T, ts *httptest.Server, token string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var since string
	for {
		resp := apiGet(t, ts, "/sync?timeout=1&since="+since, token)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := decodeJSON(t, resp)
		events, _ := body["events"].([]any)
		for _, e := range events {
			seen[e.(map[string]any)["event_id"].(string)] = true
		}
		since = body["next_batch"].(string)
		if len(events) == 0 {
			return seen
		}
	}
}

func status(t *testing.T, resp *http.Response) int {
	t.Helper()
	resp.Body.Close()
	return resp.StatusCode
}

func TestNamespaceIsolation(t *testing.T) {
	f := newIsolationFixture(t)
	ts := f.env.ts

	t.Run("login is scoped to the selected namespace", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, login(t, ts, f.slugA, "alice", "pw-alice-a"))
		assert.Equal(t, http.StatusOK, login(t, ts, f.slugB, "alice", "pw-alice-b"))
		assert.Equal(t, http.StatusUnauthorized, login(t, ts, f.slugA, "alice", "pw-alice-b"), "B's password must not open A's account")
		assert.Equal(t, http.StatusUnauthorized, login(t, ts, "", "alice", "pw-alice-a"), "no selector means default, where alice does not exist")
		assert.Equal(t, http.StatusUnauthorized, login(t, ts, "no-such-ns", "alice", "pw-alice-a"))
		assert.NotEqual(t, f.aliceA.userID, f.aliceB.userID)
	})

	t.Run("username lookup resolves inside the caller's namespace", func(t *testing.T) {
		body := decodeJSON(t, apiGet(t, ts, "/users/by-username/bob", f.aliceA.token))
		assert.Equal(t, f.bobA.userID, body["user_id"])
		body = decodeJSON(t, apiGet(t, ts, "/users/by-username/bob", f.aliceB.token))
		assert.Equal(t, f.bobB.userID, body["user_id"])
	})

	roomA := createRoom(t, ts, f.aliceA.token, "public")
	roomB := createRoom(t, ts, f.aliceB.token, "public")

	t.Run("no cross-namespace memberships or DMs", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, status(t, apiPost(t, ts, "/rooms/"+roomA+"/invite", map[string]any{"user_id": f.bobB.userID}, f.aliceA.token)), "invite B's user into A's room")
		assert.Equal(t, http.StatusNotFound, status(t, apiPost(t, ts, "/rooms/"+roomB+"/join", nil, f.bobA.token)), "join B's public room from A")
		assert.Equal(t, http.StatusNotFound, status(t, apiPost(t, ts, "/dm/"+f.bobB.userID, nil, f.aliceA.token)), "DM a user of B from A")
		assert.Equal(t, http.StatusOK, status(t, apiPost(t, ts, "/dm/"+f.bobA.userID, nil, f.aliceA.token)), "DM inside A works")
	})

	t.Run("the database itself rejects a cross-namespace membership", func(t *testing.T) {
		_, err := f.env.pool.Exec(context.Background(), `
			INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role)
			VALUES ($1, $2, $3, 'join', 'member')`, f.nsAID, roomA, f.bobB.userID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "foreign key")
	})

	require.Equal(t, http.StatusNoContent, status(t, apiPost(t, ts, "/rooms/"+roomA+"/join", nil, f.bobA.token)))
	require.Equal(t, http.StatusNoContent, status(t, apiPost(t, ts, "/rooms/"+roomB+"/join", nil, f.bobB.token)))

	// Same txnId on both sides: independent events (txn keys are per internal device).
	evA := send(t, ts, f.aliceA.token, roomA, "txn-1", "hello A")
	evB := send(t, ts, f.aliceB.token, roomB, "txn-1", "hello B")
	require.NotEqual(t, evA, evB)

	t.Run("sync never crosses namespaces", func(t *testing.T) {
		seenA, seenB := syncAll(t, ts, f.bobA.token), syncAll(t, ts, f.bobB.token)
		assert.True(t, seenA[evA])
		assert.False(t, seenA[evB])
		assert.True(t, seenB[evB])
		assert.False(t, seenB[evA])
	})

	t.Run("large-room path never crosses namespaces", func(t *testing.T) {
		_, err := f.env.pool.Exec(context.Background(), `UPDATE rooms SET member_count = $2 WHERE id = $1`, roomA, domain.LargeRoomThreshold)
		require.NoError(t, err)
		evLarge := send(t, ts, f.aliceA.token, roomA, "txn-large", "large A")
		assert.True(t, syncAll(t, ts, f.bobA.token)[evLarge])
		assert.False(t, syncAll(t, ts, f.bobB.token)[evLarge])
		assert.False(t, syncAll(t, ts, f.aliceB.token)[evLarge])
	})

	t.Run("history of another namespace's room is not readable", func(t *testing.T) {
		assert.NotEqual(t, http.StatusOK, status(t, apiGet(t, ts, "/rooms/"+roomA+"/messages?dir=b&limit=10", f.aliceB.token)))
	})

	t.Run("media is only served inside its namespace", func(t *testing.T) {
		mediaID := upload(t, ts, f.aliceA.token)
		assert.Equal(t, http.StatusOK, status(t, apiGet(t, ts, "/media/"+mediaID, f.bobA.token)))
		assert.Equal(t, http.StatusNotFound, status(t, apiGet(t, ts, "/media/"+mediaID, f.aliceB.token)), "known media ID from another namespace")
	})

	t.Run("appservices are confined to their namespace", func(t *testing.T) {
		asPost := func(token, path string, body any) int {
			return status(t, apiPost(t, ts, path, body, token))
		}
		// A's token under B's path.
		assert.Equal(t, http.StatusForbidden, asPost(f.asAToken, "/appservice/"+f.asBID+"/users", map[string]any{"username": "x"}))

		// A's AS creates a room in A and cannot pull B's users into it.
		asPath := "/appservice/" + f.asAID
		resp := apiPost(t, ts, asPath+"/rooms", map[string]any{"name": "bridge"}, f.asAToken)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		asRoom := decodeJSON(t, resp)["room_id"].(string)
		assert.Equal(t, http.StatusNotFound, asPost(f.asAToken, asPath+"/rooms/"+asRoom+"/members", map[string]any{"user_id": f.bobB.userID}))
		assert.Equal(t, http.StatusNoContent, asPost(f.asAToken, asPath+"/rooms/"+asRoom+"/members", map[string]any{"user_id": f.bobA.userID}))

		// It cannot take over a human account by username either.
		assert.Equal(t, http.StatusForbidden, asPost(f.asAToken, asPath+"/users", map[string]any{"username": "alice"}))

		// Delivery: A's AS gets A's events, never B's.
		evA2 := send(t, ts, f.aliceA.token, roomA, "txn-as", "for the bridge")
		evB2 := send(t, ts, f.aliceB.token, roomB, "txn-as", "not for the bridge")
		require.Eventually(t, func() bool { return f.asAEvents.has(evA2) }, 3*time.Second, 20*time.Millisecond)
		time.Sleep(100 * time.Millisecond)
		assert.False(t, f.asAEvents.has(evB2))
		assert.False(t, f.asAEvents.has(evB))
	})

	t.Run("disabling a namespace stops it without touching the other", func(t *testing.T) {
		require.NoError(t, f.env.namespaces.SetStatus(context.Background(), f.nsAID, domain.NamespaceDisabled))
		t.Cleanup(func() { _ = f.env.namespaces.SetStatus(context.Background(), f.nsAID, domain.NamespaceActive) })

		assert.Equal(t, http.StatusForbidden, status(t, apiGet(t, ts, "/sync?timeout=1", f.bobA.token)))
		assert.Equal(t, http.StatusForbidden, login(t, ts, f.slugA, "alice", "pw-alice-a"))
		assert.Equal(t, http.StatusUnauthorized, login(t, ts, f.slugA, "alice", "wrong"), "disabled state only revealed after the password")
		assert.Equal(t, http.StatusOK, status(t, apiGet(t, ts, "/sync?timeout=1", f.bobB.token)))
	})
}

func upload(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, _ = part.Write([]byte(strings.Repeat("x", 32)))
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/media/upload", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return decodeJSON(t, resp)["media_id"].(string)
}

// Cursors are opaque and bound to their user: a cursor from one namespace's user is useless
// in another's session, and no response carries a numeric stream position.
func TestOpaqueCursors(t *testing.T) {
	f := newIsolationFixture(t)
	ts := f.env.ts
	room := createRoom(t, ts, f.aliceA.token, "private")
	send(t, ts, f.aliceA.token, room, "c-1", "one")

	resp := apiGet(t, ts, "/sync?timeout=1", f.aliceA.token)
	raw := decodeJSON(t, resp)
	cursorA := raw["next_batch"].(string)
	require.True(t, strings.HasPrefix(cursorA, "c1."))
	for _, e := range raw["events"].([]any) {
		assert.NotContains(t, e, "stream_pos")
	}

	assert.Equal(t, http.StatusBadRequest, status(t, apiGet(t, ts, "/sync?timeout=1&since="+cursorA, f.aliceB.token)), "cursor of another user/namespace")
	assert.Equal(t, http.StatusBadRequest, status(t, apiGet(t, ts, "/sync?timeout=1&since=12345", f.aliceA.token)), "numeric cursors are not accepted")
	fromStart := decodeJSON(t, apiGet(t, ts, "/sync?timeout=1&since=0", f.aliceA.token))
	assert.NotEmpty(t, fromStart["events"], "since=0 (old clients' first sync) means from the start")
	assert.Equal(t, http.StatusOK, status(t, apiGet(t, ts, "/sync?timeout=1&since="+cursorA, f.aliceA.token)))

	// /messages pages by room_seq: newest first without from, then back from end.
	send(t, ts, f.aliceA.token, room, "c-2", "two")
	page := decodeJSON(t, apiGet(t, ts, "/rooms/"+room+"/messages?dir=b&limit=1", f.aliceA.token))
	chunk := page["chunk"].([]any)
	require.Len(t, chunk, 1)
	newest := chunk[0].(map[string]any)
	assert.Equal(t, "two", newest["content"].(map[string]any)["body"])
	assert.Equal(t, float64(2), newest["room_seq"], "room_seq counts this room's events only")
	assert.Equal(t, float64(3), page["start"], "no open-ended sentinel in the response")
	older := decodeJSON(t, apiGet(t, ts, fmt.Sprintf("/rooms/%s/messages?dir=b&limit=1&from=%d", room, int64(page["end"].(float64))), f.aliceA.token))
	assert.Equal(t, "one", older["chunk"].([]any)[0].(map[string]any)["content"].(map[string]any)["body"])
}
