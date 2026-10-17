//go:build integration

package redis_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/redis"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

func testPushQueue(t *testing.T) out.PushQueue {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	client, err := redis.NewClient(addr, "", 0)
	require.NoError(t, err)
	return redis.NewPushQueue(client)
}

// This is the property the whole cross-instance push design depends on: no matter
// how many callers race to Enqueue for the same user at the same time (simulating
// separate server instances handling separate SendEvent calls), exactly one must
// win the claim — otherwise every instance would independently schedule a flush
// and the recipient gets N pushes instead of one.
func TestPushQueue_Enqueue_ExactlyOneClaimWins(t *testing.T) {
	q := testPushQueue(t)
	userID := "user_" + uuid.NewString()
	ctx := context.Background()

	const n = 20
	var claimedCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, err := q.Enqueue(ctx, userID, out.PushQueueItem{
				RoomID: "room1", RoomName: "general",
			})
			require.NoError(t, err)
			if claimed {
				claimedCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(1), claimedCount.Load(), "exactly one caller must win the claim")

	items, err := q.Flush(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, items, n, "the winning flush must see every item enqueued by every caller")
}

// Flush must atomically drain — a second Flush right after must see nothing left.
func TestPushQueue_Flush_ClearsPending(t *testing.T) {
	q := testPushQueue(t)
	userID := "user_" + uuid.NewString()
	ctx := context.Background()

	_, err := q.Enqueue(ctx, userID, out.PushQueueItem{RoomID: "room1", RoomName: "general"})
	require.NoError(t, err)

	first, err := q.Flush(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, first, 1)

	second, err := q.Flush(ctx, userID)
	require.NoError(t, err)
	assert.Empty(t, second)
}

// Flushing a user with nothing pending must be a clean no-op, not an error.
func TestPushQueue_Flush_EmptyIsNoError(t *testing.T) {
	q := testPushQueue(t)
	items, err := q.Flush(context.Background(), "user_"+uuid.NewString())
	require.NoError(t, err)
	assert.Empty(t, items)
}

// Different users must have fully independent claims — one person's messages
// never block or interfere with another's.
func TestPushQueue_Enqueue_IndependentClaimsPerUser(t *testing.T) {
	q := testPushQueue(t)
	ctx := context.Background()
	userA := "user_" + uuid.NewString()
	userB := "user_" + uuid.NewString()

	claimedA, err := q.Enqueue(ctx, userA, out.PushQueueItem{RoomID: "room1", RoomName: "general"})
	require.NoError(t, err)
	claimedB, err := q.Enqueue(ctx, userB, out.PushQueueItem{RoomID: "room1", RoomName: "general"})
	require.NoError(t, err)

	assert.True(t, claimedA)
	assert.True(t, claimedB)
}

// EnqueueBatch keeps Enqueue's semantics per user: the first enqueue in a window claims,
// later ones (batched or not) do not, and every item lands in the right pending list.
func TestPushQueue_EnqueueBatch(t *testing.T) {
	q := testPushQueue(t)
	ctx := context.Background()
	userA := "user_" + uuid.NewString()
	userB := "user_" + uuid.NewString()

	_, err := q.Enqueue(ctx, userA, out.PushQueueItem{RoomID: "room1", RoomName: "general"})
	require.NoError(t, err)

	claimed, err := q.EnqueueBatch(ctx, []string{userA, userB}, out.PushQueueItem{RoomID: "room2", RoomName: "random"})
	require.NoError(t, err)
	assert.Equal(t, []bool{false, true}, claimed)

	itemsA, err := q.Flush(ctx, userA)
	require.NoError(t, err)
	assert.Equal(t, []out.PushQueueItem{{RoomID: "room1", RoomName: "general"}, {RoomID: "room2", RoomName: "random"}}, itemsA)
	itemsB, err := q.Flush(ctx, userB)
	require.NoError(t, err)
	assert.Equal(t, []out.PushQueueItem{{RoomID: "room2", RoomName: "random"}}, itemsB)

	again, err := q.Flush(ctx, userA)
	require.NoError(t, err)
	assert.Empty(t, again, "flush drains the list")
}
