package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	testWriterGrace = 10 * time.Second
	testWriterDelay = time.Second
	testTimeout     = 30 * time.Second
)

type writerHarness struct {
	w    *PresenceWriter
	now  time.Time
	ops  []presenceOp
	fail bool
}

func newWriterHarness() *writerHarness {
	h := &writerHarness{now: time.Unix(1_000_000, 0)}
	h.w = newPresenceWriter(func(_ context.Context, ops []presenceOp) error {
		if h.fail {
			return errors.New("redis down")
		}
		h.ops = append(h.ops, ops...)
		return nil
	}, testWriterGrace, testWriterDelay, func() time.Time { return h.now })
	return h
}

func (h *writerHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// flush returns the ops written by this flush.
func (h *writerHarness) flush() []presenceOp {
	h.ops = nil
	h.w.flush(h.now)
	return h.ops
}

func (h *writerHarness) cycle(user string, held time.Duration) {
	ctx := context.Background()
	_ = h.w.Begin(ctx, user, testTimeout)
	h.advance(held)
	_ = h.w.End(ctx, user)
}

func set(user string) presenceOp     { return presenceOp{userID: user, ttl: testTimeout + testWriterGrace} }
func shorten(user string) presenceOp { return presenceOp{userID: user} }

func TestPresenceWriter_ActiveClientWritesOnlyToRefresh(t *testing.T) {
	h := newWriterHarness()
	h.cycle("u", 50*time.Millisecond)
	assert.Equal(t, []presenceOp{set("u")}, h.flush(), "first long-poll sets the key")

	// A client re-polling every 100ms: its Ends are cancelled by the next Begin, and the
	// key is only rewritten once less than grace/2 of headroom would remain.
	var writes []presenceOp
	for range 100 { // 10s
		h.advance(50 * time.Millisecond)
		h.cycle("u", 50*time.Millisecond)
		writes = append(writes, h.flush()...)
	}
	assert.Len(t, writes, 1, "a refresh every grace/2 (5s), no End")
	for _, op := range writes {
		assert.Equal(t, set("u"), op)
	}
}

func TestPresenceWriter_UserWhoLeavesIsShortenedAfterTheDelay(t *testing.T) {
	h := newWriterHarness()
	h.cycle("u", time.Second)
	assert.Equal(t, []presenceOp{set("u")}, h.flush())

	h.advance(testWriterDelay - time.Millisecond)
	assert.Empty(t, h.flush(), "End is held for the delay")
	h.advance(time.Millisecond)
	assert.Equal(t, []presenceOp{shorten("u")}, h.flush())
	assert.Empty(t, h.flush(), "written once")

	h.cycle("u", time.Second)
	assert.Equal(t, []presenceOp{set("u")}, h.flush(), "after an End the next long-poll sets again")
}

func TestPresenceWriter_OtherDeviceKeepsTheUserPresent(t *testing.T) {
	h := newWriterHarness()
	ctx := context.Background()
	_ = h.w.Begin(ctx, "u", testTimeout) // phone
	_ = h.w.Begin(ctx, "u", testTimeout) // laptop
	_ = h.w.End(ctx, "u")                // phone returns and goes away
	h.flush()
	h.advance(5 * testWriterDelay)
	assert.Empty(t, h.flush(), "the laptop's long-poll is still open")

	_ = h.w.End(ctx, "u")
	h.advance(testWriterDelay)
	assert.Equal(t, []presenceOp{shorten("u")}, h.flush())
}

func TestPresenceWriter_FailedFlushRetries(t *testing.T) {
	h := newWriterHarness()
	h.fail = true
	h.cycle("u", time.Millisecond)
	h.flush() // the SET is lost
	h.fail = false
	h.advance(time.Millisecond)
	_ = h.w.Begin(context.Background(), "u", testTimeout)
	assert.Equal(t, []presenceOp{set("u")}, h.flush(), "a lost SET is written by the next Begin")

	_ = h.w.End(context.Background(), "u")
	h.advance(testWriterDelay)
	h.fail = true
	h.flush() // the End is lost
	h.fail = false
	assert.Equal(t, []presenceOp{shorten("u")}, h.flush(), "a lost End is retried")
}

func TestPresenceWriter_ForgetsIdleUsers(t *testing.T) {
	h := newWriterHarness()
	h.cycle("u", time.Millisecond)
	h.flush()
	h.advance(testWriterDelay)
	h.flush() // End
	h.advance(testWriterGrace)
	h.flush()
	assert.Empty(t, h.w.users)
}

func TestPresenceWriter_CloseEndsEveryone(t *testing.T) {
	var ops []presenceOp
	w := newPresenceWriter(func(_ context.Context, o []presenceOp) error {
		ops = append(ops, o...)
		return nil
	}, testWriterGrace, testWriterDelay, time.Now)
	go w.run()
	_ = w.Begin(context.Background(), "open", testTimeout)
	_ = w.Begin(context.Background(), "returned", testTimeout)
	_ = w.End(context.Background(), "returned")
	w.Close()
	assert.Contains(t, ops, shorten("open"))
	assert.Contains(t, ops, shorten("returned"))
}
