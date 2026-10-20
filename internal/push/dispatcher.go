package push

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// Sender delivers pushes to one device token. Implementations wrap a specific
// provider — e.g. FCM HTTP v1, using a Firebase service-account key.
type Sender interface {
	// Send delivers a digest (namespaces in digest mode).
	Send(ctx context.Context, token string, digest Digest) error
	// SendMessage delivers one message with its preview (namespaces in message mode).
	SendMessage(ctx context.Context, token string, msg MessagePush) error
}

// MessagePush is one message as a namespace in message mode notifies it. Preview is
// short (domain.MaxPreviewRunes) and raw; Kind lets the client label attachments and
// cards in its own language.
type MessagePush struct {
	RoomID     string
	RoomName   string
	EventID    string
	SenderID   string
	SenderName string
	Kind       domain.PreviewKind
	Preview    string
}

// Digest is the per-recipient summary a coalescing window produces. Counts only,
// deliberately never message content — the push exists to wake the recipient up,
// not to carry the conversation; the client always gets the real content from
// /sync once it opens.
type Digest struct {
	Rooms      []RoomDigest
	TotalCount int
}

// RoomDigest is one room's contribution to a Digest. JSON tags matter: this is
// what fcm.Sender embeds (as a JSON string) in the FCM data payload, and the
// mobile client decodes it back — snake_case to match the rest of the API.
type RoomDigest struct {
	RoomID   string `json:"room_id"`
	RoomName string `json:"room_name"`
	Count    int    `json:"count"`
}

// Dispatcher fans newly created events out to batched, per-recipient push
// notifications. Each Dispatch call enqueues into a Redis-backed PushQueue and
// returns immediately (the Redis round-trip itself runs on a background
// goroutine, never the caller's) — the actual flush (presence check, token
// lookup, provider call) happens later, off a per-user timer armed by whichever
// instance's Enqueue call won that user's claim for the current window.
type Dispatcher struct {
	queue          out.PushQueue
	presence       out.PresenceStore
	devices        out.DeviceStore
	sender         Sender
	coalesceWindow time.Duration

	// Message mode (optional, see WithMessageMode).
	namespaces out.NamespaceStore
	users      out.UserStore
	modesMu    sync.Mutex
	modes      map[string]cachedMode
}

type cachedMode struct {
	mode  domain.PushMode
	until time.Time
}

// modeTTL bounds how long a namespace's push mode is cached; a change applies
// within this delay, without a restart.
const modeTTL = 30 * time.Second

// messageSendConcurrency bounds provider calls for one message.
const messageSendConcurrency = 8

func NewDispatcher(queue out.PushQueue, presence out.PresenceStore, devices out.DeviceStore, sender Sender, coalesceWindow time.Duration) *Dispatcher {
	return &Dispatcher{
		queue:          queue,
		presence:       presence,
		devices:        devices,
		sender:         sender,
		coalesceWindow: coalesceWindow,
	}
}

// WithMessageMode lets namespaces choose domain.PushMessage. Without it, every
// namespace gets digests.
func (d *Dispatcher) WithMessageMode(namespaces out.NamespaceStore, users out.UserStore) *Dispatcher {
	d.namespaces, d.users = namespaces, users
	d.modes = map[string]cachedMode{}
	return d
}

// Dispatch notifies every recipientID of event, per its namespace's push mode.
// Non-blocking: lookups and Redis calls run on a spawned goroutine, so a slow
// dependency never delays the caller (the event-send HTTP response).
func (d *Dispatcher) Dispatch(event *domain.Event, recipientIDs []string, roomName string) {
	if len(recipientIDs) == 0 {
		return
	}
	go func() {
		ctx := context.Background()
		if d.mode(ctx, event.NamespaceID) == domain.PushMessage {
			d.sendMessage(ctx, event, recipientIDs, roomName)
			return
		}
		item := out.PushQueueItem{RoomID: event.RoomID, RoomName: roomName}
		d.enqueueAll(ctx, event.ID, recipientIDs, item)
	}()
}

// mode returns the namespace's push mode, cached for modeTTL. Lookup failures fall
// back to digest, which never sends content.
func (d *Dispatcher) mode(ctx context.Context, namespaceID string) domain.PushMode {
	if d.namespaces == nil || namespaceID == "" {
		return domain.PushDigest
	}
	d.modesMu.Lock()
	cached, ok := d.modes[namespaceID]
	d.modesMu.Unlock()
	if ok && time.Now().Before(cached.until) {
		return cached.mode
	}
	mode := domain.PushDigest
	if ns, err := d.namespaces.GetByID(ctx, namespaceID); err != nil {
		slog.Warn("push: namespace lookup failed, using digest", "namespace_id", namespaceID, "error", err)
	} else if ns.PushMode == domain.PushMessage {
		mode = domain.PushMessage
	}
	d.modesMu.Lock()
	d.modes[namespaceID] = cachedMode{mode: mode, until: time.Now().Add(modeTTL)}
	d.modesMu.Unlock()
	return mode
}

// sendMessage pushes one message with its preview, right away, to every recipient
// who is not connected. Events that are not messages (reactions, plugin state,
// commands, agent status) are not pushed in message mode.
func (d *Dispatcher) sendMessage(ctx context.Context, event *domain.Event, recipientIDs []string, roomName string) {
	kind, preview, ok := domain.MessagePreview(event)
	if !ok {
		return
	}
	offline := make([]string, 0, len(recipientIDs))
	for _, userID := range recipientIDs {
		present, err := d.presence.IsPresent(ctx, userID)
		if err != nil {
			// Fail open, as in flush: a redundant push beats a missed one.
			slog.Warn("push: presence check failed, sending anyway", "user_id", userID, "error", err)
		}
		if err != nil || !present {
			offline = append(offline, userID)
		}
	}
	if len(offline) == 0 {
		return
	}
	tokens, err := d.devices.ListPushTokensByUserIDs(ctx, offline)
	if err != nil {
		slog.Error("push: list push tokens", "users", len(offline), "error", err)
		return
	}
	if len(tokens) == 0 {
		return
	}
	msg := MessagePush{
		RoomID: event.RoomID, RoomName: roomName, EventID: event.ID,
		SenderID: event.SenderID, SenderName: d.senderName(ctx, event.SenderID),
		Kind: kind, Preview: preview,
	}
	slots := make(chan struct{}, messageSendConcurrency)
	var wg sync.WaitGroup
	for _, t := range tokens {
		wg.Add(1)
		slots <- struct{}{}
		go func(t domain.DevicePushToken) {
			defer func() { <-slots; wg.Done() }()
			if err := d.sender.SendMessage(ctx, t.PushToken, msg); err != nil {
				slog.Warn("push: message delivery failed", "device_id", t.DeviceID, "user_id", t.UserID, "error", err)
			}
		}(t)
	}
	wg.Wait()
}

func (d *Dispatcher) senderName(ctx context.Context, userID string) string {
	if d.users == nil {
		return ""
	}
	u, err := d.users.GetByID(ctx, userID)
	if err != nil {
		slog.Warn("push: sender lookup failed", "user_id", userID, "error", err)
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func (d *Dispatcher) enqueueAll(ctx context.Context, eventID string, recipientIDs []string, item out.PushQueueItem) {
	claims, err := d.queue.EnqueueBatch(ctx, recipientIDs, item)
	if err != nil {
		slog.Warn("push: enqueue failed", "recipients", len(recipientIDs), "event_id", eventID, "error", err)
		return
	}
	// Recipients this instance claimed share one coalescing window: flush them together, so
	// one timer and one token query serve the whole send instead of one per recipient.
	claimed := make([]string, 0, len(recipientIDs))
	for i, userID := range recipientIDs {
		if claims[i] { // otherwise another instance owns this window's flush for userID
			claimed = append(claimed, userID)
		}
	}
	if len(claimed) == 0 {
		return
	}
	time.AfterFunc(d.coalesceWindow, func() {
		d.flush(context.Background(), claimed)
	})
}

// flush drains each user's pending items and pushes a digest to every device of those who
// are not currently connected.
func (d *Dispatcher) flush(ctx context.Context, userIDs []string) {
	digests := make(map[string]Digest, len(userIDs))
	for _, userID := range userIDs {
		items, err := d.queue.Flush(ctx, userID)
		if err != nil {
			slog.Error("push: flush failed", "user_id", userID, "error", err)
			continue
		}
		if len(items) == 0 {
			continue
		}
		present, err := d.presence.IsPresent(ctx, userID)
		if err != nil {
			// Fail open: an unknown presence state costs a redundant push, never a
			// missed one — see PresenceStore's doc comment.
			slog.Warn("push: presence check failed, sending anyway", "user_id", userID, "error", err)
			present = false
		}
		if !present {
			digests[userID] = buildDigest(items)
		}
	}
	if len(digests) == 0 {
		return
	}

	offline := make([]string, 0, len(digests))
	for userID := range digests {
		offline = append(offline, userID)
	}
	tokens, err := d.devices.ListPushTokensByUserIDs(ctx, offline)
	if err != nil {
		slog.Error("push: list push tokens", "users", len(offline), "error", err)
		return
	}
	for _, t := range tokens {
		if err := d.sender.Send(ctx, t.PushToken, digests[t.UserID]); err != nil {
			slog.Warn("push: delivery failed", "device_id", t.DeviceID, "user_id", t.UserID, "error", err)
		}
	}
}

// buildDigest groups pending items by room, preserving first-seen room order.
func buildDigest(items []out.PushQueueItem) Digest {
	byRoom := make(map[string]*RoomDigest, len(items))
	order := make([]string, 0, len(items))

	for _, it := range items {
		rd, ok := byRoom[it.RoomID]
		if !ok {
			rd = &RoomDigest{RoomID: it.RoomID, RoomName: it.RoomName}
			byRoom[it.RoomID] = rd
			order = append(order, it.RoomID)
		}
		rd.Count++
	}

	rooms := make([]RoomDigest, len(order))
	for i, roomID := range order {
		rooms[i] = *byRoom[roomID]
	}
	return Digest{Rooms: rooms, TotalCount: len(items)}
}

// NopDispatcher is a no-op implementation used until a Sender is configured (see
// Sender's doc comment) — matches appservice.NopDispatcher's role.
type NopDispatcher struct{}

func (NopDispatcher) Dispatch(_ *domain.Event, _ []string, _ string) {}
