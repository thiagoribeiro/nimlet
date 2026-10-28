//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/testkit"
)

// The grouped statement itself must succeed: readGroup falls back to one query per request
// on error, which hides a broken statement from result-level tests while throttling /sync.
// Run with a write in flight so the held-back probe is evaluated too.
func TestGetVisibleGroup_Succeeds(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn, 4)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	s := &batchedEventStore{eventStore: &eventStore{pool: pool}}

	owner, room, large := uuid.NewString(), uuid.NewString(), uuid.NewString()
	testkit.SeedUser(pool, owner)
	testkit.SeedRoom(pool, room, owner)
	testkit.SeedLargeRoom(pool, large, owner)
	testkit.SeedMembership(pool, room, owner, "join")
	testkit.SeedMembership(pool, large, owner, "join")
	e, _, err := s.eventStore.Create(ctx, &domain.Event{
		ID: uuid.NewString(), RoomID: room, SenderID: owner, EventType: "m.room.message",
		Content: []byte(`{}`), TxnKey: uuid.NewString(), CreatedAt: time.Now().UTC(),
	}, true)
	require.NoError(t, err)

	var slowPos int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO stream_inflight (pos) VALUES (nextval('events_stream_pos_seq')) RETURNING pos`).Scan(&slowPos))
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM stream_inflight WHERE pos = $1`, slowPos) })

	group := []visibleReq{
		{userID: owner, sincePos: 0, limit: 10},
		{userID: owner, sincePos: e.StreamPos, limit: 10}, // caught up, nothing held
		{userID: uuid.NewString(), sincePos: 0, limit: 10},
	}
	pages, held, err := s.getVisibleGroup(ctx, group)
	require.NoError(t, err)
	require.Len(t, pages, 3)
	require.NotEmpty(t, pages[0])
	require.Empty(t, pages[1])
	require.False(t, held[1])
	require.Empty(t, pages[2])
}
