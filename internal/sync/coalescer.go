package sync

import (
	"strings"
	"sync"
	"time"
)

// coalescer batches keys arriving within a time window and deduplicates them
// before flushing to the WaiterRegistry. Keys are either plain userIDs or
// "room:"+roomID for large-room notifications.
// This prevents thundering-herd when a busy room generates many notifications
// for the same partition in quick succession.
type coalescer struct {
	window   time.Duration
	registry *WaiterRegistry

	mu      sync.Mutex
	pending map[string]struct{}
	timer   *time.Timer
}

func newCoalescer(window time.Duration, registry *WaiterRegistry) *coalescer {
	return &coalescer{
		window:   window,
		registry: registry,
		pending:  make(map[string]struct{}, 64),
	}
}

// add queues a key for notification. The first call starts the batch timer.
func (c *coalescer) add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pending[key] = struct{}{}
	if c.timer == nil {
		c.timer = time.AfterFunc(c.window, c.flush)
	}
}

func (c *coalescer) flush() {
	c.mu.Lock()
	pending := c.pending
	// Reuse map capacity when it's stable-sized; replace when it grew very large
	// to avoid a permanent memory anchor.
	if len(c.pending) > 10_000 {
		c.pending = make(map[string]struct{}, 256)
	} else {
		c.pending = make(map[string]struct{}, len(pending))
	}
	c.timer = nil
	c.mu.Unlock()

	for key := range pending {
		if roomID, ok := strings.CutPrefix(key, "room:"); ok {
			c.registry.NotifyRoom(roomID)
		} else {
			c.registry.Notify(key)
		}
	}
}
