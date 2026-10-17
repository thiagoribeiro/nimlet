//go:build integration

package testkit

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// RunEventStoreContract verifies the critical event store contract for any adapter.
// pool is used only for FK fixture seeding (users/rooms), not for the store under test.
func RunEventStoreContract(t *testing.T, store out.EventStore, pool *pgxpool.Pool) {
	t.Helper()

	// Seed fixtures to satisfy FK constraints.
	userA := uuid.NewString()
	userB := uuid.NewString()
	roomID := uuid.NewString()
	deviceID := uuid.NewString()

	SeedUser(pool, userA)
	SeedUser(pool, userB)
	SeedRoom(pool, roomID, userA)
	// Visibility rows are written for the room's joined members.
	SeedMembership(pool, roomID, userA, "join")
	SeedMembership(pool, roomID, userB, "join")

	newEvent := func(txnSuffix string) *domain.Event {
		return &domain.Event{
			ID:        uuid.NewString(),
			RoomID:    roomID,
			SenderID:  userA,
			EventType: "m.room.message",
			Content:   json.RawMessage(`{"body":"hello"}`),
			TxnKey:    deviceID + ":" + txnSuffix,
			CreatedAt: time.Now().UTC(),
		}
	}

	t.Run("create and get visible events", func(t *testing.T) {
		ctx := context.Background()
		e := newEvent(uuid.NewString())
		created, recipients, err := store.Create(ctx, e, true)
		require.NoError(t, err)
		assert.Subset(t, recipients, []string{userA, userB})
		assert.Greater(t, created.StreamPos, int64(0))

		events, err := store.GetVisible(ctx, userA, 0, 10)
		require.NoError(t, err)
		require.NotEmpty(t, events)

		found := false
		for _, ev := range events {
			if ev.ID == created.ID {
				found = true
				break
			}
		}
		assert.True(t, found, "created event should be visible to userA")
	})

	t.Run("idempotent send returns same event", func(t *testing.T) {
		ctx := context.Background()
		txnID := uuid.NewString()
		e := newEvent(txnID)

		first, _, err := store.Create(ctx, e, true)
		require.NoError(t, err)

		// Send again with same txn_key but different UUID — must return the first event.
		dup := newEvent(txnID)
		dup.ID = uuid.NewString()
		second, again, err := store.Create(ctx, dup, true)
		require.NoError(t, err)

		assert.Equal(t, first.ID, second.ID)
		assert.Equal(t, first.StreamPos, second.StreamPos)
		assert.Empty(t, again, "a replay writes no visibility and reports no recipients")
	})

	t.Run("GetVisible only returns events after since_pos", func(t *testing.T) {
		ctx := context.Background()
		userC := uuid.NewString()
		SeedUser(pool, userC)
		SeedMembership(pool, roomID, userC, "join")

		e1, _, err := store.Create(ctx, newEvent(uuid.NewString()), true)
		require.NoError(t, err)
		e2, _, err := store.Create(ctx, newEvent(uuid.NewString()), true)
		require.NoError(t, err)

		events, err := store.GetVisible(ctx, userC, e1.StreamPos, 10)
		require.NoError(t, err)
		assert.Len(t, events, 1)
		assert.Equal(t, e2.ID, events[0].ID)
	})

	t.Run("CurrentStreamPos increases monotonically", func(t *testing.T) {
		ctx := context.Background()
		before, err := store.CurrentStreamPos(ctx)
		require.NoError(t, err)

		_, _, err = store.Create(ctx, newEvent(uuid.NewString()), true)
		require.NoError(t, err)

		after, err := store.CurrentStreamPos(ctx)
		require.NoError(t, err)
		assert.Greater(t, after, before)
	})

	// Large rooms get no visibility rows; GetVisible must find their events through the
	// reader's joined memberships, and stop once the reader has left.
	t.Run("GetVisible includes joined large rooms", func(t *testing.T) {
		ctx := context.Background()
		member := uuid.NewString()
		leaver := uuid.NewString()
		largeRoom := uuid.NewString()
		SeedUser(pool, member)
		SeedUser(pool, leaver)
		SeedLargeRoom(pool, largeRoom, userA)
		SeedMembership(pool, largeRoom, member, "join")
		SeedMembership(pool, largeRoom, leaver, "leave")

		e := newEvent(uuid.NewString())
		e.RoomID = largeRoom
		created, _, err := store.Create(ctx, e, false)
		require.NoError(t, err)

		got, err := store.GetVisible(ctx, member, created.StreamPos-1, 10)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, created.ID, got[0].ID)

		got, err = store.GetVisible(ctx, leaver, created.StreamPos-1, 10)
		require.NoError(t, err)
		assert.Empty(t, got, "a member who left must not see new large-room events")
	})

	// Fan-out targets exactly the joined members at write time and reports them.
	t.Run("fan-out reaches joined members only", func(t *testing.T) {
		ctx := context.Background()
		room := uuid.NewString()
		joined, invited, left := uuid.NewString(), uuid.NewString(), uuid.NewString()
		for _, u := range []string{joined, invited, left} {
			SeedUser(pool, u)
		}
		SeedRoom(pool, room, userA)
		SeedMembership(pool, room, userA, "join")
		SeedMembership(pool, room, joined, "join")
		SeedMembership(pool, room, invited, "invite")
		SeedMembership(pool, room, left, "leave")

		e := newEvent(uuid.NewString())
		e.RoomID = room
		created, recipients, err := store.Create(ctx, e, true)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{userA, joined}, recipients)
		for _, u := range []string{invited, left} {
			got, err := store.GetVisible(ctx, u, created.StreamPos-1, 10)
			require.NoError(t, err)
			assert.Empty(t, got, "non-joined members get no visibility row")
		}
	})

	// room_seq numbers a room's events in exactly the order /sync delivers them, even when
	// sends race: clients order and dedupe by it.
	t.Run("room_seq follows delivery order per room", func(t *testing.T) {
		ctx := context.Background()
		room := uuid.NewString()
		reader := uuid.NewString()
		SeedUser(pool, reader)
		SeedRoom(pool, room, userA)
		SeedMembership(pool, room, userA, "join")
		SeedMembership(pool, room, reader, "join")

		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 10 {
					e := newEvent(uuid.NewString())
					e.RoomID = room
					created, _, err := store.Create(ctx, e, true)
					if err != nil {
						t.Errorf("create: %v", err)
						return
					}
					if created.RoomSeq <= 0 {
						t.Errorf("room_seq not assigned")
					}
				}
			}()
		}
		wg.Wait()

		events, err := store.GetVisible(ctx, reader, 0, 1000)
		require.NoError(t, err)
		require.Len(t, events, 160)
		for i := 1; i < len(events); i++ {
			require.Greater(t, events[i].RoomSeq, events[i-1].RoomSeq, "room_seq must increase in delivery order")
		}
		assert.Equal(t, int64(1), events[0].RoomSeq, "a room's first event is 1")

		page, err := store.GetByRoom(ctx, room, events[99].RoomSeq, "f", 10)
		require.NoError(t, err)
		require.Len(t, page, 10)
		assert.Equal(t, events[100].ID, page[0].ID, "paging by room_seq resumes right after from")
	})

	// A reader that follows next_batch (= last returned stream_pos) must never skip an event,
	// even when concurrent sends commit out of stream_pos order. Regression test for the
	// commit-order race recorded in docs/benchmarks.md (phase 0).
	t.Run("concurrent creates are never skipped by a cursor-following reader", func(t *testing.T) {
		ctx := context.Background()
		reader := uuid.NewString()
		SeedUser(pool, reader)
		SeedMembership(pool, roomID, reader, "join")

		const writers, perWriter = 32, 40
		var created sync.Map // event ID → struct{}
		var done atomic.Bool
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					e, _, err := store.Create(ctx, newEvent(uuid.NewString()), true)
					if err != nil {
						t.Errorf("create: %v", err)
						return
					}
					created.Store(e.ID, struct{}{})
				}
			}()
		}
		go func() { wg.Wait(); done.Store(true) }()

		seen := make(map[string]struct{})
		var since int64
		for {
			finished := done.Load() // read before querying so the final pass sees every commit
			events, err := store.GetVisible(ctx, reader, since, 100)
			require.NoError(t, err)
			for _, e := range events {
				seen[e.ID] = struct{}{}
				since = e.StreamPos
			}
			if finished && len(events) == 0 {
				break
			}
		}

		var missing int
		created.Range(func(k, _ any) bool {
			if _, ok := seen[k.(string)]; !ok {
				missing++
			}
			return true
		})
		assert.Zero(t, missing, "reader skipped %d of %d events", missing, writers*perWriter)
	})
}
