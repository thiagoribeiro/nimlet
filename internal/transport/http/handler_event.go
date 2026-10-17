package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/wire"
)

func handleSendEvent(svc in.RoomService) http.HandlerFunc {
	type response struct {
		EventID string `json:"event_id"`
		RoomSeq int64  `json:"room_seq"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		roomID := chi.URLParam(r, "roomId")
		eventType := chi.URLParam(r, "eventType")
		txnID := chi.URLParam(r, "txnId")
		if err := errors.Join(
			domain.ValidateToken("event type", eventType, domain.MaxEventTypeLen),
			domain.ValidateToken("txn id", txnID, domain.MaxTxnIDLen),
		); err != nil {
			writeError(w, r, err)
			return
		}

		// The content is opaque and stored as sent: read it once (bounded by limitBody) and
		// validate in place instead of decoding into a value and copying it back out.
		content, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !isJSONObject(content) {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: "event content must be a JSON object"})
			return
		}

		actor := actorFrom(r.Context())
		event, err := svc.SendEvent(r.Context(), in.SendEventCmd{
			RoomID:    roomID,
			EventType: eventType,
			TxnID:     txnID,
			Content:   json.RawMessage(content),
		}, actor.UserID, actor.DeviceID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		metrics.EventsSentTotal.Inc()
		writeJSON(w, http.StatusOK, response{EventID: event.ID, RoomSeq: event.RoomSeq})
	}
}

func handleGetMessages(svc in.RoomService) http.HandlerFunc {
	type response struct {
		Start  int64        `json:"start"` // room_seq this page started from
		End    int64        `json:"end"`   // room_seq of its last event: pass as ?from= to continue
		Events []wire.Event `json:"chunk"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		roomID := chi.URLParam(r, "roomId")
		actor := actorFrom(r.Context())

		dir := r.URL.Query().Get("dir")
		if dir != "b" {
			dir = "f"
		}
		var from int64
		if v := r.URL.Query().Get("from"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: "from must be a room_seq"})
				return
			}
			from = n
		} else if dir == "b" {
			from = math.MaxInt64 // backwards without from: start at the newest event
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

		events, err := svc.GetMessages(r.Context(), in.GetMessagesCmd{
			RoomID: roomID,
			From:   from,
			Dir:    dir,
			Limit:  limit,
		}, actor.UserID)
		if err != nil {
			writeError(w, r, err)
			return
		}

		start, end := from, from
		if from == math.MaxInt64 {
			// Keep the open-ended sentinel out of responses: JS cannot represent it exactly.
			start, end = 0, 0
			if len(events) > 0 {
				start = events[0].RoomSeq + 1
			}
		}
		if len(events) > 0 {
			end = events[len(events)-1].RoomSeq
		}
		writeJSON(w, http.StatusOK, response{Start: start, End: end, Events: wire.FromEvents(events)})
	}
}

// isJSONObject reports whether b is valid JSON whose top-level value is an object.
func isJSONObject(b []byte) bool {
	return json.Valid(b) && bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte("{"))
}
