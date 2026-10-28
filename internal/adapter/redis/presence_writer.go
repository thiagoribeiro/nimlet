package redis

import (
	"context"
	"log/slog"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// PresenceEndDelay is how long a returned /sync waits before its End reaches Redis. A
// connected client opens its next long-poll well within it, which cancels the End.
const PresenceEndDelay = time.Second

// presenceOp is one write to a user's presence key: a SET with ttl, or (ttl 0) shortening
// the key to the grace period.
type presenceOp struct {
	userID string
	ttl    time.Duration
}

// presenceSink applies a group of presence writes in one round trip.
type presenceSink func(ctx context.Context, ops []presenceOp) error

// PresenceWriter is the PresenceStore /sync uses: Begin and End return at once, and a
// background loop writes their net effect to Redis, pipelined.
//
// Two /syncs per user per wake-up used to cost a SET and a PEXPIRE each. Here:
//   - Begin writes only when the key it last wrote would expire too soon for this long-poll
//     (less than half the grace period to spare).
//   - End is held for PresenceEndDelay and dropped if the user's next /sync begins on this
//     instance meanwhile, so only users who actually left shorten their key.
//
// Being late is safe by the PresenceStore contract: a Begin that lands a few milliseconds
// late can cost one redundant push, and an End delayed by PresenceEndDelay keeps a user who
// left present for that much longer than the grace period. Open /syncs are counted per
// user, so one device returning no longer shortens another one's presence on this instance.
type PresenceWriter struct {
	sink  presenceSink
	read  *redisPresenceStore
	grace time.Duration
	delay time.Duration
	now   func() time.Time

	mu    sync.Mutex
	users map[string]*presenceUser

	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	lastWarn time.Time
}

type presenceUser struct {
	active  int           // /syncs open on this instance
	expires time.Time     // expiry of the key as this instance last wrote it; zero: unknown
	set     time.Duration // pending SET ttl; zero: none
	endAt   time.Time     // when the pending End is due; zero: none
}

// NewPresenceWriter returns the PresenceStore for /sync, backed by one Redis key per user.
// Close it after the HTTP server has shut down, so the Ends of the last /syncs reach Redis.
func NewPresenceWriter(client *goredis.Client) *PresenceWriter {
	store := &redisPresenceStore{client: client, grace: PresenceGrace}
	w := newPresenceWriter(store.apply, PresenceGrace, PresenceEndDelay, time.Now)
	w.read = store
	go w.run()
	return w
}

// newPresenceWriter builds a writer without its loop; tests drive flush themselves.
func newPresenceWriter(sink presenceSink, grace, delay time.Duration, now func() time.Time) *PresenceWriter {
	return &PresenceWriter{
		sink: sink, grace: grace, delay: delay, now: now,
		users: make(map[string]*presenceUser),
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Begin never fails: the write happens in the background.
func (w *PresenceWriter) Begin(_ context.Context, userID string, timeout time.Duration) error {
	now := w.now()
	ttl := timeout + w.grace
	w.mu.Lock()
	u := w.users[userID]
	if u == nil {
		u = &presenceUser{}
		w.users[userID] = u
	}
	u.active++
	u.endAt = time.Time{}
	write := u.expires.Before(now.Add(ttl - w.grace/2))
	if write {
		u.set = max(u.set, ttl)
	}
	w.mu.Unlock()
	if write {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// End never fails: it is applied PresenceEndDelay later unless the user begins again.
func (w *PresenceWriter) End(_ context.Context, userID string) error {
	now := w.now()
	w.mu.Lock()
	if u := w.users[userID]; u != nil && u.active > 0 {
		u.active--
		if u.active == 0 {
			u.endAt = now.Add(w.delay)
		}
	}
	w.mu.Unlock()
	return nil
}

// IsPresent reads Redis, which holds every instance's presence.
func (w *PresenceWriter) IsPresent(ctx context.Context, userID string) (bool, error) {
	return w.read.IsPresent(ctx, userID)
}

// Close applies the pending writes, ending the presence of every user still open here.
func (w *PresenceWriter) Close() {
	close(w.stop)
	<-w.done
}

func (w *PresenceWriter) run() {
	defer close(w.done)
	tick := time.NewTicker(w.delay / 4)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			now := w.now()
			w.mu.Lock()
			for _, u := range w.users {
				u.active, u.endAt = 0, now
			}
			w.mu.Unlock()
			w.flush(now)
			return
		case <-w.kick:
		case <-tick.C:
		}
		w.flush(w.now())
	}
}

// flush writes the pending SETs and the Ends due at now. Under load, Begins pile up while
// a flush is in flight, so the next one carries them all in one pipeline.
func (w *PresenceWriter) flush(now time.Time) {
	var ops []presenceOp
	w.mu.Lock()
	for id, u := range w.users {
		if u.set > 0 {
			ops = append(ops, presenceOp{userID: id, ttl: u.set})
			u.expires, u.set = now.Add(u.set), 0
		}
		if !u.endAt.IsZero() && !now.Before(u.endAt) {
			ops = append(ops, presenceOp{userID: id})
			u.expires, u.endAt = now.Add(w.grace), time.Time{}
		}
		if u.active == 0 && u.endAt.IsZero() && u.set == 0 && !u.expires.After(now) {
			delete(w.users, id)
		}
	}
	w.mu.Unlock()
	if len(ops) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.sink(ctx, ops); err != nil {
		w.failed(now, ops, err)
	}
}

// failed forgets what a failed flush claimed to have written, so the next Begin writes
// again, and retries its Ends: a lost End would keep a user who left silent for the whole
// long-poll timeout.
func (w *PresenceWriter) failed(now time.Time, ops []presenceOp, err error) {
	w.mu.Lock()
	for _, op := range ops {
		u := w.users[op.userID]
		if u == nil {
			continue
		}
		u.expires = time.Time{}
		if op.ttl == 0 && u.active == 0 && u.endAt.IsZero() {
			u.endAt = now
		}
	}
	warn := now.Sub(w.lastWarn) >= time.Second
	if warn {
		w.lastWarn = now
	}
	w.mu.Unlock()
	if warn {
		slog.Warn("redis: presence flush failed", "ops", len(ops), "error", err)
	}
}

// apply writes ops in one pipeline.
func (s *redisPresenceStore) apply(ctx context.Context, ops []presenceOp) error {
	_, err := s.client.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for _, op := range ops {
			if op.ttl > 0 {
				p.Set(ctx, presenceKeyPrefix+op.userID, "1", op.ttl)
			} else {
				p.PExpire(ctx, presenceKeyPrefix+op.userID, s.grace)
			}
		}
		return nil
	})
	return err
}
