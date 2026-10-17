// Package wire holds the JSON shapes Nimlet sends to clients and AppServices, so both see
// the same event format.
package wire

import (
	"encoding/json"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Event is an event as delivered outside the server. It carries room_seq — the event's
// position within its room, for ordering, dedupe and paging — but never the global stream
// position, which would reveal other namespaces' traffic (see internal/cursor).
type Event struct {
	ID                string          `json:"event_id"`
	RoomID            string          `json:"room_id"`
	RoomSeq           int64           `json:"room_seq"`
	SenderID          string          `json:"sender_id"`
	SenderDisplayName string          `json:"sender_display_name,omitempty"`
	EventType         string          `json:"type"`
	Content           json.RawMessage `json:"content"`
	CreatedAt         int64           `json:"origin_server_ts"` // milliseconds
}

func FromEvent(e *domain.Event) Event {
	return Event{
		ID:                e.ID,
		RoomID:            e.RoomID,
		RoomSeq:           e.RoomSeq,
		SenderID:          e.SenderID,
		SenderDisplayName: e.SenderDisplayName,
		EventType:         e.EventType,
		Content:           e.Content,
		CreatedAt:         e.CreatedAt.UnixMilli(),
	}
}

func FromEvents(events []*domain.Event) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[i] = FromEvent(e)
	}
	return out
}
