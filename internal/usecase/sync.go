package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
	nimsync "github.com/thiagoribeiro/nimlet/internal/sync"
)

const (
	defaultSyncTimeout = 30 * time.Second
	maxSyncTimeout     = 60 * time.Second
	syncQueryLimit     = 100

	// largeRoomCacheTTL bounds how stale the large-room list used for *wake-up
	// registration* may be. Staleness only delays a wake-up (the next one, or the
	// long-poll timeout, still delivers); which events a sync returns never depends on
	// it, because EventStore.GetVisible resolves large rooms itself.
	largeRoomCacheTTL = 10 * time.Second
)

type syncService struct {
	events      out.EventStore
	memberships out.MembershipStore
	registry    *nimsync.WaiterRegistry
	presence    out.PresenceStore
	largeRooms  *ttlCache[[]string]
}

func NewSyncService(events out.EventStore, memberships out.MembershipStore, registry *nimsync.WaiterRegistry, presence out.PresenceStore) in.SyncService {
	return &syncService{
		events:      events,
		memberships: memberships,
		registry:    registry,
		presence:    presence,
		largeRooms:  newTTLCache[[]string](largeRoomCacheTTL),
	}
}

// Sync returns events after cmd.Since, long-polling up to the timeout when there are none.
//
// The waiter is registered *before* the first query: a send that commits between the
// query and the registration would otherwise publish its wake-up to nobody and the client
// would sit out the whole timeout. With the buffered(1) channel already in place, such a
// wake-up is kept and the select below returns at once.
func (s *syncService) Sync(ctx context.Context, cmd in.SyncCmd) (*in.SyncResult, error) {
	timeout := time.Duration(cmd.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultSyncTimeout
	}
	if timeout > maxSyncTimeout {
		timeout = maxSyncTimeout
	}

	// Best-effort: presence only gates whether push delivery skips this user, so a
	// failure here must never fail the /sync request itself. End runs even when the
	// client aborts (backgrounded app), so presence ends a grace period later.
	if err := s.presence.Begin(ctx, cmd.UserID, timeout); err != nil {
		slog.WarnContext(ctx, "sync: begin presence", "user_id", cmd.UserID, "error", err)
	}
	defer func() {
		if err := s.presence.End(context.WithoutCancel(ctx), cmd.UserID); err != nil {
			slog.WarnContext(ctx, "sync: end presence", "user_id", cmd.UserID, "error", err)
		}
	}()

	largeRoomIDs, err := s.largeRoomsFor(ctx, cmd.UserID)
	if err != nil {
		return nil, err
	}
	notifCh := s.registry.Register(cmd.UserID, largeRoomIDs)
	defer s.registry.Unregister(cmd.UserID, largeRoomIDs, notifCh)

	events, err := s.events.GetVisible(ctx, cmd.UserID, cmd.Since, syncQueryLimit)
	if err != nil {
		return nil, fmt.Errorf("sync: query: %w", err)
	}
	if len(events) > 0 {
		return result(cmd.Since, events), nil
	}

	// The deadline bounds the wait only. The re-query below uses the request context: a
	// wake-up that lands just as the wait times out must not run its query on an already
	// expired context (that turned a normal wake-up into a 500).
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-notifCh:
	case <-s.registry.Closed():
		// Shutting down: no more wake-ups will come. Answer now (re-querying, in case
		// something landed since the first query) so the client re-polls another instance.
	case <-wait.Done():
		// Timeout or client disconnect — return empty result with current cursor.
		// Client opens another /sync immediately; this is the normal keep-alive cycle.
		return result(cmd.Since, events), nil
	}
	// Re-query after wake-up: the notification only guarantees that
	// something changed, not what. False positives (shared partition) are fine.
	events, err = s.events.GetVisible(ctx, cmd.UserID, cmd.Since, syncQueryLimit)
	if err != nil {
		return nil, fmt.Errorf("sync: re-query after notify: %w", err)
	}
	return result(cmd.Since, events), nil
}

func result(since int64, events []*domain.Event) *in.SyncResult {
	next := since
	if len(events) > 0 {
		next = events[len(events)-1].StreamPos
	}
	return &in.SyncResult{NextBatch: next, Events: events}
}

func (s *syncService) largeRoomsFor(ctx context.Context, userID string) ([]string, error) {
	if ids, ok := s.largeRooms.get(userID); ok {
		return ids, nil
	}
	ids, err := s.memberships.ListLargeRooms(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("sync: list large rooms: %w", err)
	}
	s.largeRooms.put(userID, ids)
	return ids, nil
}

// ttlCache is a small in-process map whose entries expire; expired entries are swept on
// insert. Used for state that may be briefly stale (wake-up registration, presence refresh).
type ttlCache[V any] struct {
	ttl       time.Duration
	mu        sync.Mutex
	entries   map[string]ttlEntry[V]
	nextSweep time.Time
}

type ttlEntry[V any] struct {
	v       V
	expires time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, entries: make(map[string]ttlEntry[V])}
}

func (c *ttlCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[V]) put(key string, v V) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.After(c.nextSweep) {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		c.nextSweep = now.Add(c.ttl)
	}
	c.entries[key] = ttlEntry[V]{v: v, expires: now.Add(c.ttl)}
}
