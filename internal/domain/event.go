package domain

import (
	"encoding/json"
	"time"
)

type Event struct {
	ID                string
	StreamPos         int64 // global monotonic counter, assigned by DB; never leaves the server
	RoomSeq           int64 // per-room order (monotonic, may have gaps); safe to expose
	RoomID            string
	SenderID          string
	SenderDisplayName string          // populated on reads; empty on write path
	EventType         string          // e.g. "m.room.message", "m.agent.command"
	Content           json.RawMessage // arbitrary JSON payload, immutable
	TxnKey            string          // "<device_id>:<txn_id>", unique idempotency key
	CreatedAt         time.Time
	// NamespaceID is the room's namespace, filled in on the write path (not persisted:
	// events are reached through rooms) so AppService delivery can filter by namespace.
	NamespaceID string
}

// EventVisibility links a user to an event they can see.
// Inserted atomically alongside the event (same transaction).
type EventVisibility struct {
	UserID    string
	StreamPos int64
}
