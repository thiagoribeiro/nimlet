package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

// handleASCreateUser provisions (or re-issues a session for) a Nimlet identity on
// behalf of an AppService's bridged end user. Authenticated via as_token.
func handleASCreateUser(svc in.AuthService) http.HandlerFunc {
	type request struct {
		Username string `json:"username"`
	}
	type response struct {
		UserID       string `json:"user_id"`
		SessionToken string `json:"session_token"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		userID, token, err := svc.EnsureServiceUser(r.Context(), actorFrom(r.Context()).NamespaceID, req.Username)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, response{UserID: userID, SessionToken: token})
	}
}

// handleASCreateRoom creates a room owned by the calling AppService's system user.
// Unlike the public POST /rooms, it accepts an optional stable alias (idempotent) and
// a TTL. Authenticated via as_token.
func handleASCreateRoom(svc in.RoomService) http.HandlerFunc {
	type request struct {
		Name       string `json:"name"`
		Alias      string `json:"alias"`
		TTLSeconds int    `json:"ttl_seconds"`
		Visibility string `json:"visibility"` // "public" or "private" (default "private")
	}
	type response struct {
		RoomID    string `json:"room_id"`
		Name      string `json:"name"`
		CreatorID string `json:"creator_id"`
		JoinRule  string `json:"join_rule"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		joinRule := joinRuleFor(req.Visibility)
		actor := actorFrom(r.Context())
		room, err := svc.CreateRoom(r.Context(), in.CreateRoomCmd{
			Name:       req.Name,
			JoinRule:   joinRule,
			Alias:      req.Alias,
			TTLSeconds: req.TTLSeconds,
		}, actor.principal())
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, response{
			RoomID:    room.ID,
			Name:      room.Name,
			CreatorID: room.CreatorID,
			JoinRule:  string(room.JoinRule),
		})
	}
}

// handleASAddMember directly joins a user to a room (skipping the invite → join
// handshake). The calling AppService's system user must own/admin the room.
// Authenticated via as_token.
func handleASAddMember(svc in.RoomService) http.HandlerFunc {
	type request struct {
		UserID string `json:"user_id"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		roomID := chi.URLParam(r, "roomId")
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		actor := actorFrom(r.Context())
		if err := svc.AddMember(r.Context(), roomID, req.UserID, actor.principal()); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
