package sync

import "sync"

// waiterShard holds one bucket of user and room waiters, each protected by its own mutex.
// Sharding across 256 buckets reduces lock contention by ~256× under high concurrency.
type waiterShard struct {
	mu    sync.Mutex
	users map[string]waiterSet // userID → active /sync channels
	rooms map[string]waiterSet // roomID → active /sync channels (large rooms)
}

// waiterSet makes Unregister O(1); a large room can hold thousands of waiters and every
// long-poll cycle registers and unregisters once.
type waiterSet map[chan struct{}]struct{}

// WaiterRegistry maps user IDs (and large room IDs) to pending /sync goroutines.
// Each waiting goroutine holds a buffered(1) channel; Notify/NotifyRoom sends to all.
// Buffer=1 is critical: prevents losing a notification that fires between the
// failed fast-path query and the goroutine blocking on select.
type WaiterRegistry struct {
	shards [256]waiterShard

	closeOnce sync.Once
	closed    chan struct{}
}

func NewWaiterRegistry() *WaiterRegistry {
	r := &WaiterRegistry{closed: make(chan struct{})}
	for i := range r.shards {
		r.shards[i].users = make(map[string]waiterSet)
		r.shards[i].rooms = make(map[string]waiterSet)
	}
	return r
}

func shardIndex(key string) uint8 {
	// FNV-1a inline — no allocation, same distribution as the Redis hash function.
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return uint8(h)
}

// Register allocates a wake-up channel for userID (and optionally registers it
// for each large roomID so that room notifications also wake this goroutine).
// Caller must defer Unregister to avoid leaks.
func (r *WaiterRegistry) Register(userID string, roomIDs []string) chan struct{} {
	ch := make(chan struct{}, 1)

	// Register under user shard.
	us := &r.shards[shardIndex(userID)]
	us.mu.Lock()
	addChan(us.users, userID, ch)
	us.mu.Unlock()

	// Register under each large-room shard.
	for _, rid := range roomIDs {
		rs := &r.shards[shardIndex(rid)]
		rs.mu.Lock()
		addChan(rs.rooms, rid, ch)
		rs.mu.Unlock()
	}

	return ch
}

// Unregister removes the channel from user and room buckets.
func (r *WaiterRegistry) Unregister(userID string, roomIDs []string, ch chan struct{}) {
	us := &r.shards[shardIndex(userID)]
	us.mu.Lock()
	removeChan(us.users, userID, ch)
	us.mu.Unlock()

	for _, rid := range roomIDs {
		rs := &r.shards[shardIndex(rid)]
		rs.mu.Lock()
		removeChan(rs.rooms, rid, ch)
		rs.mu.Unlock()
	}
}

// Notify wakes up all goroutines waiting for userID.
func (r *WaiterRegistry) Notify(userID string) {
	s := &r.shards[shardIndex(userID)]
	s.mu.Lock()
	sendAll(s.users[userID])
	s.mu.Unlock()
}

// NotifyRoom wakes up all goroutines that registered interest in roomID.
// Used by large-room notifications where a single "room:" event replaces N per-user publishes.
func (r *WaiterRegistry) NotifyRoom(roomID string) {
	s := &r.shards[shardIndex(roomID)]
	s.mu.Lock()
	sendAll(s.rooms[roomID])
	s.mu.Unlock()
}

// Close releases every current and future waiter (see Closed). Called on shutdown: once the
// dispatcher stops, no wake-up reaches this instance, and a long-poll left waiting would
// hold its client for the whole timeout instead of letting it re-poll another instance.
func (r *WaiterRegistry) Close() {
	r.closeOnce.Do(func() { close(r.closed) })
}

// Closed is closed by Close.
func (r *WaiterRegistry) Closed() <-chan struct{} {
	return r.closed
}

// WaiterCount returns the number of active user waiters (for observability).
func (r *WaiterRegistry) WaiterCount(userID string) int {
	s := &r.shards[shardIndex(userID)]
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users[userID])
}

// sendAll never blocks (each channel is buffered(1) and an already-pending wake-up is
// enough), so it is safe to call with the shard lock held — no snapshot copy needed.
func sendAll(chans waiterSet) {
	for ch := range chans {
		select {
		case ch <- struct{}{}:
		default: // already notified; channel is buffered(1), goroutine will re-query
		}
	}
}

func addChan(m map[string]waiterSet, key string, ch chan struct{}) {
	set := m[key]
	if set == nil {
		set = make(waiterSet, 1)
		m[key] = set
	}
	set[ch] = struct{}{}
}

func removeChan(m map[string]waiterSet, key string, ch chan struct{}) {
	set := m[key]
	delete(set, ch)
	if len(set) == 0 {
		delete(m, key)
	}
}
