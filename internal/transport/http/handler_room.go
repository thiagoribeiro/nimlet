package http

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

func handleCreateRoom(svc in.RoomService) http.HandlerFunc {
	type request struct {
		Name       string `json:"name"`
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
		room, err := svc.CreateRoom(r.Context(), in.CreateRoomCmd{Name: req.Name, JoinRule: joinRule}, actor.principal())
		if err != nil {
			writeError(w, r, err)
			return
		}
		metrics.RoomsCreatedTotal.Inc()
		writeJSON(w, http.StatusCreated, response{
			RoomID:    room.ID,
			Name:      room.Name,
			CreatorID: room.CreatorID,
			JoinRule:  string(room.JoinRule),
		})
	}
}

func handleGetOrCreateDM(svc in.RoomService) http.HandlerFunc {
	type response struct {
		RoomID string `json:"room_id"`
		Name   string `json:"name"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		targetID := chi.URLParam(r, "userId")
		actor := actorFrom(r.Context())
		room, err := svc.GetOrCreateDM(r.Context(), actor.principal(), targetID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, response{
			RoomID: room.ID,
			Name:   room.Name,
		})
	}
}

func handleInviteUser(svc in.RoomService) http.HandlerFunc {
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
		if err := svc.InviteUser(r.Context(), in.InviteCmd{RoomID: roomID, InviteeID: req.UserID}, actor.principal()); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleJoinRoom(svc in.RoomService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomID := chi.URLParam(r, "roomId")
		actor := actorFrom(r.Context())
		if err := svc.JoinRoom(r.Context(), roomID, actor.principal()); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleListRooms(svc in.RoomService) http.HandlerFunc {
	type roomDTO struct {
		RoomID   string `json:"room_id"`
		Name     string `json:"name"`
		JoinRule string `json:"join_rule"`
		State    string `json:"state"`
		Role     string `json:"role"`
	}
	type response struct {
		Rooms []roomDTO `json:"rooms"`
		Next  string    `json:"next,omitempty"` // pass as ?after= for the next page
	}

	return func(w http.ResponseWriter, r *http.Request) {
		actor := actorFrom(r.Context())
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		memberships, next, err := svc.ListRooms(r.Context(), actor.UserID, r.URL.Query().Get("after"), limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		rooms := make([]roomDTO, len(memberships))
		for i, m := range memberships {
			rooms[i] = roomDTO{
				RoomID:   m.Room.ID,
				Name:     m.Room.Name,
				JoinRule: string(m.Room.JoinRule),
				State:    string(m.State),
				Role:     string(m.Role),
			}
		}
		writeJSON(w, http.StatusOK, response{Rooms: rooms, Next: next})
	}
}

// joinRuleFor maps the API's room visibility ("public" or anything else = private) to a
// join rule; shared by the user and AppService room-creation endpoints.
func joinRuleFor(visibility string) domain.JoinRule {
	if visibility == "public" {
		return domain.JoinRulePublic
	}
	return domain.JoinRuleInviteOnly
}
