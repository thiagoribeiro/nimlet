package push_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
	"github.com/thiagoribeiro/nimlet/internal/push"
	"github.com/thiagoribeiro/nimlet/testkit"
)

const testWindow = 20 * time.Millisecond

type fakeSender struct {
	mu   sync.Mutex
	sent []push.Digest
}

func (f *fakeSender) Send(_ context.Context, _ string, digest push.Digest) error {
	f.mu.Lock()
	f.sent = append(f.sent, digest)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) digestsSent() []push.Digest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]push.Digest(nil), f.sent...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func testEvent(roomID string) *domain.Event {
	return &domain.Event{ID: "evt1", RoomID: roomID, SenderID: "sender1", EventType: "m.room.message"}
}

// A claimed Enqueue schedules exactly one flush after the coalescing window, and
// the resulting digest is built from whatever Flush returns.
func TestDispatcher_ClaimedRecipient_FlushesAndSends(t *testing.T) {
	queue := &testkit.MockPushQueue{
		EnqueueFn: func(_ context.Context, _ string, _ out.PushQueueItem) (bool, error) { return true, nil },
		FlushFn: func(_ context.Context, userID string) ([]out.PushQueueItem, error) {
			assert.Equal(t, "user1", userID)
			return []out.PushQueueItem{
				{RoomID: "room1", RoomName: "general"},
				{RoomID: "room1", RoomName: "general"},
				{RoomID: "room2", RoomName: "random"},
			}, nil
		},
	}
	devices := &testkit.MockDeviceStore{
		ListPushTokensByUserIDsFn: func(_ context.Context, userIDs []string) ([]domain.DevicePushToken, error) {
			assert.Equal(t, []string{"user1"}, userIDs)
			return []domain.DevicePushToken{{DeviceID: "dev1", UserID: "user1", PushToken: "tok1"}}, nil
		},
	}
	presence := &testkit.MockPresenceStore{IsPresentFn: func(_ context.Context, _ string) (bool, error) { return false, nil }}
	sender := &fakeSender{}

	d := push.NewDispatcher(queue, presence, devices, sender, testWindow)
	d.Dispatch(testEvent("room1"), []string{"user1"}, "general")

	waitFor(t, func() bool { return len(sender.digestsSent()) == 1 })
	digest := sender.digestsSent()[0]
	assert.Equal(t, 3, digest.TotalCount)
	require.Len(t, digest.Rooms, 2)
	assert.Equal(t, push.RoomDigest{RoomID: "room1", RoomName: "general", Count: 2}, digest.Rooms[0])
	assert.Equal(t, push.RoomDigest{RoomID: "room2", RoomName: "random", Count: 1}, digest.Rooms[1])
}

// claimed=false must never schedule a flush — another instance owns this window.
func TestDispatcher_NotClaimed_NeverFlushes(t *testing.T) {
	var flushCalled bool
	queue := &testkit.MockPushQueue{
		EnqueueFn: func(_ context.Context, _ string, _ out.PushQueueItem) (bool, error) { return false, nil },
		FlushFn: func(_ context.Context, _ string) ([]out.PushQueueItem, error) {
			flushCalled = true
			return nil, nil
		},
	}
	sender := &fakeSender{}
	d := push.NewDispatcher(queue, &testkit.MockPresenceStore{}, &testkit.MockDeviceStore{}, sender, testWindow)
	d.Dispatch(testEvent("room1"), []string{"user1"}, "general")

	time.Sleep(3 * testWindow)
	assert.False(t, flushCalled)
	assert.Empty(t, sender.digestsSent())
}

// If the recipient is present by flush time, the batch is discarded silently.
func TestDispatcher_PresentAtFlush_DiscardsBatch(t *testing.T) {
	queue := &testkit.MockPushQueue{
		FlushFn: func(_ context.Context, _ string) ([]out.PushQueueItem, error) {
			return []out.PushQueueItem{{RoomID: "room1", RoomName: "general"}}, nil
		},
	}
	devices := &testkit.MockDeviceStore{
		ListPushTokensByUserIDsFn: func(_ context.Context, _ []string) ([]domain.DevicePushToken, error) {
			t.Fatal("should not fetch tokens when the recipient is present")
			return nil, nil
		},
	}
	presence := &testkit.MockPresenceStore{IsPresentFn: func(_ context.Context, _ string) (bool, error) { return true, nil }}
	sender := &fakeSender{}

	d := push.NewDispatcher(queue, presence, devices, sender, testWindow)
	d.Dispatch(testEvent("room1"), []string{"user1"}, "general")

	time.Sleep(3 * testWindow)
	assert.Empty(t, sender.digestsSent())
}

// A presence lookup error must fail open (treated as offline) — a redundant push is
// acceptable, a silently dropped one is not.
func TestDispatcher_PresenceErrorFailsOpen(t *testing.T) {
	queue := &testkit.MockPushQueue{
		FlushFn: func(_ context.Context, _ string) ([]out.PushQueueItem, error) {
			return []out.PushQueueItem{{RoomID: "room1", RoomName: "general"}}, nil
		},
	}
	devices := &testkit.MockDeviceStore{
		ListPushTokensByUserIDsFn: func(_ context.Context, _ []string) ([]domain.DevicePushToken, error) {
			return []domain.DevicePushToken{{DeviceID: "dev1", UserID: "user1", PushToken: "tok1"}}, nil
		},
	}
	presence := &testkit.MockPresenceStore{
		IsPresentFn: func(_ context.Context, _ string) (bool, error) { return false, errors.New("redis unavailable") },
	}
	sender := &fakeSender{}

	d := push.NewDispatcher(queue, presence, devices, sender, testWindow)
	d.Dispatch(testEvent("room1"), []string{"user1"}, "general")

	waitFor(t, func() bool { return len(sender.digestsSent()) == 1 })
}

// An empty flush (nothing pending — e.g. already drained by a concurrent flush)
// must not touch presence or the device store at all.
func TestDispatcher_EmptyFlush_NoOp(t *testing.T) {
	queue := &testkit.MockPushQueue{
		FlushFn: func(_ context.Context, _ string) ([]out.PushQueueItem, error) { return nil, nil },
	}
	presence := &testkit.MockPresenceStore{
		IsPresentFn: func(_ context.Context, _ string) (bool, error) {
			t.Fatal("should not check presence on an empty flush")
			return false, nil
		},
	}
	d := push.NewDispatcher(queue, presence, &testkit.MockDeviceStore{}, &fakeSender{}, testWindow)
	d.Dispatch(testEvent("room1"), []string{"user1"}, "general")

	time.Sleep(3 * testWindow)
}

// No recipients must not touch the queue at all.
func TestDispatcher_NoRecipients_NoOp(t *testing.T) {
	queue := &testkit.MockPushQueue{
		EnqueueFn: func(_ context.Context, _ string, _ out.PushQueueItem) (bool, error) {
			t.Fatal("should not enqueue with zero recipients")
			return false, nil
		},
	}
	d := push.NewDispatcher(queue, &testkit.MockPresenceStore{}, &testkit.MockDeviceStore{}, &fakeSender{}, testWindow)
	d.Dispatch(testEvent("room1"), nil, "general")

	time.Sleep(3 * testWindow)
}

// All recipients of one send are flushed together: one token query for the offline ones,
// and each device gets its own owner's digest.
func TestDispatcher_BatchesRecipientsOfOneSend(t *testing.T) {
	queue := &testkit.MockPushQueue{
		FlushFn: func(_ context.Context, userID string) ([]out.PushQueueItem, error) {
			return []out.PushQueueItem{{RoomID: "room-of-" + userID, RoomName: "r"}}, nil
		},
	}
	presence := &testkit.MockPresenceStore{IsPresentFn: func(_ context.Context, userID string) (bool, error) {
		return userID == "online", nil
	}}
	var mu sync.Mutex
	var queries [][]string
	devices := &testkit.MockDeviceStore{
		ListPushTokensByUserIDsFn: func(_ context.Context, userIDs []string) ([]domain.DevicePushToken, error) {
			mu.Lock()
			queries = append(queries, append([]string(nil), userIDs...))
			mu.Unlock()
			out := make([]domain.DevicePushToken, 0, len(userIDs))
			for _, u := range userIDs {
				out = append(out, domain.DevicePushToken{DeviceID: "dev-" + u, UserID: u, PushToken: "tok-" + u})
			}
			return out, nil
		},
	}
	sender := &fakeSender{}
	d := push.NewDispatcher(queue, presence, devices, sender, testWindow)
	d.Dispatch(&domain.Event{ID: "e", RoomID: "room"}, []string{"a", "b", "online"}, "r")

	waitFor(t, func() bool { return len(sender.digestsSent()) == 2 })
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, queries, 1, "one token query for the whole send")
	assert.ElementsMatch(t, []string{"a", "b"}, queries[0], "only offline recipients")
	rooms := []string{}
	for _, dg := range sender.digestsSent() {
		rooms = append(rooms, dg.Rooms[0].RoomID)
	}
	assert.ElementsMatch(t, []string{"room-of-a", "room-of-b"}, rooms, "each device gets its owner's digest")
}
