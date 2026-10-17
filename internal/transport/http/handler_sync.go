package http

import (
	"net/http"
	"strconv"

	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/wire"
)

func handleSync(svc in.SyncService, cursors *cursor.Codec) http.HandlerFunc {
	type response struct {
		NextBatch string       `json:"next_batch"` // opaque; pass back as ?since=
		Events    []wire.Event `json:"events"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		metrics.ActiveSyncConnections.Inc()
		defer metrics.ActiveSyncConnections.Dec()

		actor := actorFrom(r.Context())
		token := r.URL.Query().Get("since")
		if token == "0" {
			token = "" // what pre-opaque clients send on their first sync: "from the start"
		}
		since, err := cursors.Decode(cursor.Sync, actor.UserID, token)
		if err != nil {
			writeError(w, r, err)
			return
		}
		timeoutMs, _ := strconv.ParseInt(r.URL.Query().Get("timeout"), 10, 64)

		result, err := svc.Sync(r.Context(), in.SyncCmd{
			UserID:    actor.UserID,
			Since:     since,
			TimeoutMs: timeoutMs,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}

		// Always 200, even on timeout — client uses next_batch to detect no new events.
		writeJSON(w, http.StatusOK, response{
			NextBatch: cursors.Encode(cursor.Sync, actor.UserID, result.NextBatch),
			Events:    wire.FromEvents(result.Events),
		})
	}
}
