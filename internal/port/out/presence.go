package out

import (
	"context"
	"time"
)

// PresenceStore tracks which users currently have a /sync long-poll open, so push
// delivery can skip anyone who's already going to receive the event live.
// It is ephemeral and best-effort by design — same role Redis plays everywhere
// else in this system (never a source of truth, only a latency/volume optimization).
// A lookup failure or a lost/expired key must fail toward "send the push", never
// toward silence: the worst case is a redundant notification, not a missed one.
//
// Presence is per user: while any of their devices is connected, none is pushed.
type PresenceStore interface {
	// Begin records that userID opened a /sync that may block for up to timeout.
	Begin(ctx context.Context, userID string, timeout time.Duration) error

	// End records that the /sync returned (answered, timed out or aborted by the
	// client). Presence lasts a short grace period, covering the gap before a
	// connected client opens its next long-poll.
	End(ctx context.Context, userID string) error

	// IsPresent reports whether userID is connected (or within the grace period).
	IsPresent(ctx context.Context, userID string) (bool, error)
}
