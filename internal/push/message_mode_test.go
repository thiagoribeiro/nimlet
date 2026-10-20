package push_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
	"github.com/thiagoribeiro/nimlet/internal/push"
	"github.com/thiagoribeiro/nimlet/testkit"
)

func messageEvent(id, eventType string, content any) *domain.Event {
	b, _ := json.Marshal(content)
	return &domain.Event{ID: id, RoomID: "room1", SenderID: "alice", EventType: eventType, Content: b, NamespaceID: "ns-msg"}
}

type messageFixture struct {
	sender     *fakeSender
	namespaces *testkit.MockNamespaceStore
	enqueued   chan string
	d          *push.Dispatcher
}

func newMessageFixture(t *testing.T, present map[string]bool) *messageFixture {
	t.Helper()
	f := &messageFixture{sender: &fakeSender{}, enqueued: make(chan string, 10)}
	f.namespaces = testkit.NewMockNamespaceStore(&domain.Namespace{ID: "ns-msg", Slug: "msg", Status: domain.NamespaceActive, PushMode: domain.PushMessage})
	queue := &testkit.MockPushQueue{EnqueueFn: func(_ context.Context, userID string, _ out.PushQueueItem) (bool, error) {
		f.enqueued <- userID
		return false, nil
	}}
	presence := &testkit.MockPresenceStore{IsPresentFn: func(_ context.Context, userID string) (bool, error) { return present[userID], nil }}
	devices := &testkit.MockDeviceStore{ListPushTokensByUserIDsFn: func(_ context.Context, ids []string) ([]domain.DevicePushToken, error) {
		tokens := make([]domain.DevicePushToken, len(ids))
		for i, id := range ids {
			tokens[i] = domain.DevicePushToken{DeviceID: "dev-" + id, UserID: id, PushToken: "tok-" + id}
		}
		return tokens, nil
	}}
	users := &testkit.MockUserStore{GetByIDFn: func(_ context.Context, id string) (*domain.User, error) {
		return &domain.User{ID: id, Username: id, DisplayName: "Alice"}, nil
	}}
	f.d = push.NewDispatcher(queue, presence, devices, f.sender, testWindow).WithMessageMode(f.namespaces, users)
	return f
}

func TestMessageMode_OnePushPerMessageWithPreview(t *testing.T) {
	f := newMessageFixture(t, map[string]bool{"carol": true})
	f.d.Dispatch(messageEvent("e1", "m.room.message", map[string]string{"msgtype": "m.text", "body": "oi, tudo bem?"}), []string{"bob", "carol"}, "Família")
	f.d.Dispatch(messageEvent("e2", "m.room.message", map[string]string{"msgtype": "m.text", "body": "vamos?"}), []string{"bob", "carol"}, "Família")
	waitFor(t, func() bool { return len(f.sender.messagesSent()) == 2 })
	time.Sleep(3 * testWindow)

	sent := f.sender.messagesSent()
	require.Len(t, sent, 2, "carol is connected and must not be pushed")
	byEvent := map[string]sentMessage{}
	for _, s := range sent {
		byEvent[s.msg.EventID] = s
	}
	assert.Equal(t, "tok-bob", byEvent["e1"].token)
	assert.Equal(t, push.MessagePush{RoomID: "room1", RoomName: "Família", EventID: "e1", SenderID: "alice", SenderName: "Alice", Kind: domain.PreviewText, Preview: "oi, tudo bem?"}, byEvent["e1"].msg)
	assert.Equal(t, "vamos?", byEvent["e2"].msg.Preview)
	assert.Empty(t, f.sender.digestsSent())
	assert.Len(t, f.enqueued, 0, "message mode must not use the digest queue")
}

func TestMessageMode_NonMessageEventsAreNotPushed(t *testing.T) {
	f := newMessageFixture(t, nil)
	f.d.Dispatch(messageEvent("s1", "m.plugin.state.updated", map[string]any{"plugin": "checklist"}), []string{"bob"}, "Família")
	f.d.Dispatch(messageEvent("r1", "m.room.reaction", map[string]any{"relates_to": "e1", "key": "👍"}), []string{"bob"}, "Família")
	time.Sleep(5 * testWindow)
	assert.Empty(t, f.sender.messagesSent())
	assert.Empty(t, f.sender.digestsSent())
	assert.Len(t, f.enqueued, 0)
}

func TestMessageMode_DigestNamespacesKeepTheDigest(t *testing.T) {
	f := newMessageFixture(t, nil)
	e := messageEvent("e1", "m.room.message", map[string]string{"body": "oi"})
	e.NamespaceID = domain.DefaultNamespaceID // default namespace keeps digest
	f.d.Dispatch(e, []string{"bob"}, "general")
	select {
	case userID := <-f.enqueued:
		assert.Equal(t, "bob", userID)
	case <-time.After(time.Second):
		t.Fatal("digest namespace did not enqueue")
	}
	assert.Empty(t, f.sender.messagesSent())
}

func TestMessageMode_ModeIsCached(t *testing.T) {
	f := newMessageFixture(t, nil)
	f.d.Dispatch(messageEvent("e1", "m.room.message", map[string]string{"body": "um"}), []string{"bob"}, "Família")
	waitFor(t, func() bool { return len(f.sender.messagesSent()) == 1 })
	// Switching to digest takes effect only after the cache TTL: the next message still
	// goes out as a message push.
	require.NoError(t, f.namespaces.SetPushMode(context.Background(), "ns-msg", domain.PushDigest))
	f.d.Dispatch(messageEvent("e2", "m.room.message", map[string]string{"body": "dois"}), []string{"bob"}, "Família")
	waitFor(t, func() bool { return len(f.sender.messagesSent()) == 2 })
}
