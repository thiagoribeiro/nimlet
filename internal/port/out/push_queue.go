package out

import "context"

// PushQueueItem is one pending notification-worthy event for a user, waiting to be
// folded into their next batched push digest.
type PushQueueItem struct {
	RoomID   string
	RoomName string
}

// PushQueue holds the cross-instance state push batching needs: a per-user pending
// list, plus a claim lock so exactly one instance owns flushing a given person's
// batch in a given coalescing window, no matter how many different instances
// enqueued into it (each instance only sees the requests its own /rooms/.../send
// calls happen to land on).
type PushQueue interface {
	// Enqueue appends item to userID's pending list. claimed reports whether this
	// call won the right to flush userID's batch after the coalescing window —
	// callers that get claimed=false must not schedule a flush themselves; whoever
	// got claimed=true will read (and clear) everything enqueued for userID,
	// including items added after this call returns but before the flush runs.
	Enqueue(ctx context.Context, userID string, item PushQueueItem) (claimed bool, err error)

	// EnqueueBatch is Enqueue for many users in one round trip; claimed[i] belongs to
	// userIDs[i]. Same claim semantics as Enqueue.
	EnqueueBatch(ctx context.Context, userIDs []string, item PushQueueItem) (claimed []bool, err error)

	// Flush atomically drains and returns every item currently pending for userID.
	Flush(ctx context.Context, userID string) ([]PushQueueItem, error)
}
