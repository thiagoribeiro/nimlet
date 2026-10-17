//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Membership authorization and counting through the real API and database (review P0-1,
// P0-2, P1-14 and the DM join hole).
func TestMembershipRules(t *testing.T) {
	env := newTestEnv(t, nil)
	ts := env.ts
	ns := "" // default namespace
	sfx := uuid.NewString()[:8]
	owner := registerIn(t, ts, ns, "owner_"+sfx, "pw")
	alice := registerIn(t, ts, ns, "alice_"+sfx, "pw")
	bob := registerIn(t, ts, ns, "bob_"+sfx, "pw")
	mallory := registerIn(t, ts, ns, "mallory_"+sfx, "pw")

	memberCount := func(roomID string) int {
		var n int
		require.NoError(t, env.pool.QueryRow(context.Background(), `SELECT member_count FROM rooms WHERE id = $1`, roomID).Scan(&n))
		return n
	}

	t.Run("concurrent joins count once", func(t *testing.T) {
		room := createRoom(t, ts, owner.token, "public")
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				status(t, apiPost(t, ts, "/rooms/"+room+"/join", nil, alice.token))
			}()
		}
		wg.Wait()
		assert.Equal(t, 2, memberCount(room), "owner + alice")
	})

	t.Run("a member who left cannot rejoin an invite-only room", func(t *testing.T) {
		room := createRoom(t, ts, owner.token, "private")
		require.Equal(t, http.StatusNoContent, status(t, apiPost(t, ts, "/rooms/"+room+"/invite", map[string]any{"user_id": alice.userID}, owner.token)))
		require.Equal(t, http.StatusNoContent, status(t, apiPost(t, ts, "/rooms/"+room+"/join", nil, alice.token)))
		_, err := env.pool.Exec(context.Background(), `UPDATE room_memberships SET state = 'leave' WHERE room_id = $1 AND user_id = $2`, room, alice.userID)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, status(t, apiPost(t, ts, "/rooms/"+room+"/join", nil, alice.token)))

		_, err = env.pool.Exec(context.Background(), `UPDATE room_memberships SET state = 'ban' WHERE room_id = $1 AND user_id = $2`, room, alice.userID)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, status(t, apiPost(t, ts, "/rooms/"+room+"/invite", map[string]any{"user_id": alice.userID}, owner.token)), "an invite does not lift a ban")
	})

	t.Run("an admin's invite cannot demote the owner", func(t *testing.T) {
		room := createRoom(t, ts, owner.token, "private")
		require.Equal(t, http.StatusNoContent, status(t, apiPost(t, ts, "/rooms/"+room+"/invite", map[string]any{"user_id": bob.userID}, owner.token)))
		require.Equal(t, http.StatusNoContent, status(t, apiPost(t, ts, "/rooms/"+room+"/join", nil, bob.token)))
		_, err := env.pool.Exec(context.Background(), `UPDATE room_memberships SET role = 'admin' WHERE room_id = $1 AND user_id = $2`, room, bob.userID)
		require.NoError(t, err)

		assert.Equal(t, http.StatusConflict, status(t, apiPost(t, ts, "/rooms/"+room+"/invite", map[string]any{"user_id": owner.userID}, bob.token)))
		var state, role string
		require.NoError(t, env.pool.QueryRow(context.Background(), `SELECT state, role FROM room_memberships WHERE room_id = $1 AND user_id = $2`, room, owner.userID).Scan(&state, &role))
		assert.Equal(t, []string{"join", "owner"}, []string{state, role})
	})

	t.Run("only the two participants can join a DM", func(t *testing.T) {
		resp := apiPost(t, ts, "/dm/"+bob.userID, nil, alice.token)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		dm := decodeJSON(t, resp)["room_id"].(string)
		assert.Equal(t, http.StatusForbidden, status(t, apiPost(t, ts, "/rooms/"+dm+"/join", nil, mallory.token)))
		assert.Equal(t, http.StatusForbidden, status(t, apiPost(t, ts, "/rooms/"+dm+"/invite", map[string]any{"user_id": mallory.userID}, alice.token)))
		assert.Equal(t, 2, memberCount(dm))
	})
}

// GET /rooms pages by room ID: every room exactly once across pages (review P2-20).
func TestListRoomsPagination(t *testing.T) {
	ts := testServer(t)
	u := registerIn(t, ts, "", "pager_"+uuid.NewString()[:8], "pw")
	want := map[string]bool{}
	for range 5 {
		want[createRoom(t, ts, u.token, "private")] = true
	}
	seen := map[string]bool{}
	after, pages := "", 0
	for {
		body := decodeJSON(t, apiGet(t, ts, "/rooms?limit=2&after="+after, u.token))
		pages++
		for _, r := range body["rooms"].([]any) {
			id := r.(map[string]any)["room_id"].(string)
			assert.False(t, seen[id], "room %s returned twice", id)
			seen[id] = true
		}
		next, _ := body["next"].(string)
		if next == "" {
			break
		}
		after = next
	}
	assert.Equal(t, want, seen)
	assert.Equal(t, 3, pages)
}
