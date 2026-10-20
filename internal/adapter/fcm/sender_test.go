package fcm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/push"
)

func testSender(t *testing.T, handler http.HandlerFunc) *Sender {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Sender{
		projectID:   "test-project",
		tokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-access-token"}),
		httpClient:  server.Client(),
		baseURL:     server.URL,
	}
}

func TestSender_Send_BuildsCorrectRequest(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody sendRequest

	sender := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
	})

	digest := push.Digest{
		Rooms: []push.RoomDigest{
			{RoomID: "room1", RoomName: "general", Count: 3},
			{RoomID: "room2", RoomName: "random", Count: 1},
		},
		TotalCount: 4,
	}

	err := sender.Send(context.Background(), "device-token-1", digest)
	require.NoError(t, err)

	assert.Equal(t, "/v1/projects/test-project/messages:send", gotPath)
	assert.Equal(t, "Bearer test-access-token", gotAuth)
	assert.Equal(t, "device-token-1", gotBody.Message.Token)
	assert.Equal(t, "digest", gotBody.Message.Data["type"])
	assert.Equal(t, "4", gotBody.Message.Data["total_count"])

	var gotRooms []push.RoomDigest
	require.NoError(t, json.Unmarshal([]byte(gotBody.Message.Data["rooms"]), &gotRooms))
	assert.Equal(t, digest.Rooms, gotRooms)

	// No "notification" field should ever be sent — this is a data-only message,
	// the client fully controls display (see Send's doc comment).
	assert.NotContains(t, gotBody.Message.Data, "notification")
}

func TestSender_Send_NonOKStatusReturnsError(t *testing.T) {
	sender := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	})

	err := sender.Send(context.Background(), "bad-token", push.Digest{TotalCount: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestSender_SendMessage_DataOnlyWithHighPriority(t *testing.T) {
	var raw map[string]any
	sender := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		w.WriteHeader(http.StatusOK)
	})

	err := sender.SendMessage(context.Background(), "device-token-1", push.MessagePush{
		RoomID: "room1", RoomName: "Família", EventID: "evt1", SenderID: "alice",
		SenderName: "Alice", Kind: domain.PreviewText, Preview: "oi",
	})
	require.NoError(t, err)

	message := raw["message"].(map[string]any)
	assert.Equal(t, "device-token-1", message["token"])
	assert.Equal(t, map[string]any{"priority": "HIGH"}, message["android"])
	assert.NotContains(t, message, "notification", "the client renders the notification")
	assert.Equal(t, map[string]any{
		"type": "message", "room_id": "room1", "room_name": "Família", "event_id": "evt1",
		"sender_id": "alice", "sender_name": "Alice", "kind": "text", "preview": "oi",
	}, message["data"])
}

func TestSender_Send_DigestKeepsDefaultPriority(t *testing.T) {
	var raw map[string]any
	sender := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&raw))
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, sender.Send(context.Background(), "t", push.Digest{TotalCount: 1}))
	assert.NotContains(t, raw["message"].(map[string]any), "android")
}
