package usecase_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	nimsync "github.com/thiagoribeiro/nimlet/internal/sync"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
	"github.com/thiagoribeiro/nimlet/testkit"
)

func makeEvent(streamPos int64) *domain.Event {
	return &domain.Event{
		ID:        "evt1",
		StreamPos: streamPos,
		RoomID:    "room1",
		SenderID:  "user2",
		EventType: "m.room.message",
		Content:   json.RawMessage(`{"body":"hello"}`),
		CreatedAt: time.Now().UTC(),
	}
}

func noLargeRooms() *testkit.MockMembershipStore {
	return &testkit.MockMembershipStore{}
}

// Fast path: events already exist at query time → return immediately without blocking.
func TestSync_FastPath_ReturnsImmediately(t *testing.T) {
	events := []*domain.Event{makeEvent(5), makeEvent(6)}
	store := &testkit.MockEventStore{
		GetVisibleFn: func(_ context.Context, userID string, sincePos int64, _ int) ([]*domain.Event, error) {
			return events, nil
		},
	}

	registry := nimsync.NewWaiterRegistry()
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})

	start := time.Now()
	result, err := svc.Sync(context.Background(), in.SyncCmd{
		UserID: "user1", Since: 0, TimeoutMs: 30_000,
	})

	require.NoError(t, err)
	assert.Len(t, result.Events, 2)
	assert.Equal(t, int64(6), result.NextBatch)
	assert.Less(t, time.Since(start), 200*time.Millisecond, "fast path must not block")
}

// Slow path (wake-up): no events initially; a goroutine notifies the waiter;
// Sync re-queries and returns the new events.
func TestSync_SlowPath_WakesOnNotification(t *testing.T) {
	callCount := 0
	store := &testkit.MockEventStore{
		GetVisibleFn: func(_ context.Context, _ string, _ int64, _ int) ([]*domain.Event, error) {
			callCount++
			if callCount == 1 {
				return nil, nil // first call: nothing yet
			}
			return []*domain.Event{makeEvent(1)}, nil // after wake-up
		},
	}

	registry := nimsync.NewWaiterRegistry()
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})

	// Fire notification from another goroutine after a short delay.
	go func() {
		time.Sleep(60 * time.Millisecond)
		registry.Notify("user1")
	}()

	result, err := svc.Sync(context.Background(), in.SyncCmd{
		UserID: "user1", Since: 0, TimeoutMs: 5_000,
	})

	require.NoError(t, err)
	require.Len(t, result.Events, 1)
	assert.Equal(t, int64(1), result.NextBatch)
	assert.Equal(t, 2, callCount, "must re-query after notification")
}

// Slow path (timeout): no events and no notification within the timeout window.
// Must return an empty result with the original since cursor, not an error.
func TestSync_SlowPath_TimeoutReturnsEmpty(t *testing.T) {
	store := &testkit.MockEventStore{
		GetVisibleFn: func(_ context.Context, _ string, _ int64, _ int) ([]*domain.Event, error) {
			return nil, nil
		},
	}

	registry := nimsync.NewWaiterRegistry()
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})

	start := time.Now()
	result, err := svc.Sync(context.Background(), in.SyncCmd{
		UserID: "user1", Since: 42, TimeoutMs: 100, // tiny timeout for test speed
	})

	require.NoError(t, err)
	assert.Empty(t, result.Events)
	assert.Equal(t, int64(42), result.NextBatch, "cursor must not advance on timeout")
	assert.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond)
}

// Client disconnect: context cancelled while waiting — must return cleanly.
func TestSync_SlowPath_ContextCancelledReturnsEmpty(t *testing.T) {
	store := &testkit.MockEventStore{
		GetVisibleFn: func(_ context.Context, _ string, _ int64, _ int) ([]*domain.Event, error) {
			return nil, nil
		},
	}

	registry := nimsync.NewWaiterRegistry()
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := svc.Sync(ctx, in.SyncCmd{
		UserID: "user1", Since: 0, TimeoutMs: 30_000,
	})

	require.NoError(t, err)
	assert.Empty(t, result.Events)
}

// Multiple concurrent waiters for the same user all wake up.
func TestSync_MultipleWaiters_AllWake(t *testing.T) {
	registry := nimsync.NewWaiterRegistry()

	const n = 5
	results := make(chan *in.SyncResult, n)

	for i := 0; i < n; i++ {
		go func() {
			// Each goroutine needs its own store that returns empty on first call
			// and events on re-query after wake-up.
			called := false
			localStore := &testkit.MockEventStore{
				GetVisibleFn: func(_ context.Context, _ string, _ int64, _ int) ([]*domain.Event, error) {
					if !called {
						called = true
						return nil, nil
					}
					return []*domain.Event{makeEvent(1)}, nil
				},
			}
			localSvc := usecase.NewSyncService(localStore, noLargeRooms(), registry, &testkit.MockPresenceStore{})
			r, _ := localSvc.Sync(context.Background(), in.SyncCmd{
				UserID: "user_multi", Since: 0, TimeoutMs: 5_000,
			})
			results <- r
		}()
	}

	time.Sleep(80 * time.Millisecond)
	registry.Notify("user_multi")

	for i := 0; i < n; i++ {
		select {
		case r := <-results:
			assert.Len(t, r.Events, 1)
		case <-time.After(3 * time.Second):
			t.Fatalf("waiter %d did not wake up", i)
		}
	}
}

// False positive from a shared partition must not return wrong events.
// The re-query after a false-positive wake-up returns empty → behaves like timeout.
func TestSync_FalsePositiveWakeup_ReturnsEmpty(t *testing.T) {
	queryCalls := 0
	store := &testkit.MockEventStore{
		GetVisibleFn: func(_ context.Context, _ string, _ int64, _ int) ([]*domain.Event, error) {
			queryCalls++
			return nil, nil // always empty — simulates notification for another user on same partition
		},
	}

	registry := nimsync.NewWaiterRegistry()
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})

	go func() {
		time.Sleep(40 * time.Millisecond)
		registry.Notify("user1") // wake-up for user1 but no new events
	}()

	result, err := svc.Sync(context.Background(), in.SyncCmd{
		UserID: "user1", Since: 0, TimeoutMs: 200,
	})

	require.NoError(t, err)
	assert.Empty(t, result.Events)
	// Must have re-queried at least twice: once on entry, once after false positive
	assert.GreaterOrEqual(t, queryCalls, 2)
}

// A send that commits while the first query runs (after it read, before the long-poll
// starts) must still wake this sync. Regression: the waiter used to be registered after
// the query, so that wake-up was published to nobody and the client sat out the timeout.
func TestSync_WakeDuringFirstQuery_IsNotLost(t *testing.T) {
	registry := nimsync.NewWaiterRegistry()
	calls := 0
	store := &testkit.MockEventStore{
		GetVisibleFn: func(_ context.Context, userID string, _ int64, _ int) ([]*domain.Event, error) {
			calls++
			if calls == 1 {
				registry.Notify(userID) // the concurrent send's wake-up lands mid-query
				return nil, nil
			}
			return []*domain.Event{makeEvent(7)}, nil
		},
	}
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})

	start := time.Now()
	result, err := svc.Sync(context.Background(), in.SyncCmd{UserID: "user1", TimeoutMs: 5_000})
	require.NoError(t, err)
	assert.Len(t, result.Events, 1)
	assert.Less(t, time.Since(start), time.Second, "wake-up during the first query was lost")
}

// Presence is refreshed at most once per presenceRefresh, not on every /sync.
func TestSync_PresenceTouchIsThrottled(t *testing.T) {
	touches := 0
	presence := &testkit.MockPresenceStore{TouchFn: func(context.Context, string) error { touches++; return nil }}
	store := &testkit.MockEventStore{GetVisibleFn: func(context.Context, string, int64, int) ([]*domain.Event, error) {
		return []*domain.Event{makeEvent(1)}, nil
	}}
	svc := usecase.NewSyncService(store, noLargeRooms(), nimsync.NewWaiterRegistry(), presence)
	for range 5 {
		_, err := svc.Sync(context.Background(), in.SyncCmd{UserID: "u1"})
		require.NoError(t, err)
	}
	_, err := svc.Sync(context.Background(), in.SyncCmd{UserID: "u2"})
	require.NoError(t, err)
	assert.Equal(t, 2, touches, "once per user within the refresh window")
}

// A wake-up shortly before the wait deadline must still re-query successfully even if the
// deadline passes during that query (regression: the re-query ran on the wait context,
// failed with "context deadline exceeded" and the client got a 500).
func TestSync_ReQueryOutlivesWaitDeadline(t *testing.T) {
	registry := nimsync.NewWaiterRegistry()
	calls := 0
	store := &testkit.MockEventStore{
		GetVisibleFn: func(ctx context.Context, userID string, _ int64, _ int) ([]*domain.Event, error) {
			calls++
			if calls == 1 {
				go func() { time.Sleep(10 * time.Millisecond); registry.Notify(userID) }()
				return nil, nil
			}
			time.Sleep(60 * time.Millisecond) // a slow query that outlives the 50ms wait
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []*domain.Event{makeEvent(3)}, nil
		},
	}
	svc := usecase.NewSyncService(store, noLargeRooms(), registry, &testkit.MockPresenceStore{})
	res, err := svc.Sync(context.Background(), in.SyncCmd{UserID: "u", TimeoutMs: 50})
	require.NoError(t, err)
	assert.Len(t, res.Events, 1)
}
