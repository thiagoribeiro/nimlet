package out

import "context"

// PresenceStore tracks which users currently have an active /sync long-poll open,
// so push delivery can skip anyone who's already going to receive the event live.
// It is ephemeral and best-effort by design — same role Redis plays everywhere
// else in this system (never a source of truth, only a latency/volume optimization).
// A lookup failure or a lost/expired key must fail toward "send the push", never
// toward silence: the worst case is a redundant notification, not a missed one.
type PresenceStore interface {
	// Touch records that userID has an active /sync connection right now, refreshing
	// any existing TTL. Called once per /sync call, before blocking.
	Touch(ctx context.Context, userID string) error

	// IsPresent reports whether userID has touched presence recently enough to still
	// be considered actively connected.
	IsPresent(ctx context.Context, userID string) (bool, error)
}
