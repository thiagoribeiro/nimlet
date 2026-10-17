package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

func handleRegister(svc in.AuthService) http.HandlerFunc {
	type request struct {
		Namespace   string          `json:"namespace"` // optional slug; default namespace when empty
		Username    string          `json:"username"`
		Password    string          `json:"password"`
		DisplayName string          `json:"display_name"`
		DeviceName  string          `json:"device_name"`
		UserType    domain.UserType `json:"user_type"`
	}
	type response struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
		Token    string `json:"access_token"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		result, err := svc.Register(r.Context(), in.RegisterCmd{
			Namespace:   req.Namespace,
			Username:    req.Username,
			Password:    req.Password,
			DisplayName: req.DisplayName,
			DeviceName:  req.DeviceName,
			UserType:    req.UserType,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		metrics.UsersRegisteredTotal.Inc()
		writeJSON(w, http.StatusCreated, response{
			UserID:   result.User.ID,
			DeviceID: result.Device.ID,
			Token:    result.Token,
		})
	}
}

// handleLogin authenticates; perAccount limits attempts per (namespace, username) so a
// password cannot be brute-forced from many IPs.
func handleLogin(svc in.AuthService, perAccount *rateLimiter) http.HandlerFunc {
	type request struct {
		Namespace  string `json:"namespace"` // optional slug selecting the account base; default when empty
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
		DeviceID   string `json:"device_id"` // optional: reuse this device (from a previous login)
	}
	type response struct {
		SessionID string `json:"session_id"`
		UserID    string `json:"user_id"`
		DeviceID  string `json:"device_id"`
		Token     string `json:"access_token"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		if ok, retry := perAccount.allow(req.Namespace+"\x00"+strings.ToLower(req.Username), time.Now()); !ok {
			writeRateLimited(w, retry)
			return
		}
		result, err := svc.Login(r.Context(), in.LoginCmd{
			Namespace:  req.Namespace,
			Username:   req.Username,
			Password:   req.Password,
			DeviceName: req.DeviceName,
			DeviceID:   req.DeviceID,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, response{
			SessionID: result.Session.ID,
			UserID:    result.Session.UserID,
			DeviceID:  result.Session.DeviceID,
			Token:     result.Token,
		})
	}
}

func handleMe(svc in.AuthService) http.HandlerFunc {
	type response struct {
		UserID      string `json:"user_id"`
		DeviceID    string `json:"device_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		actor := actorFrom(r.Context())
		user, err := svc.ResolveUserID(r.Context(), actor.UserID)

		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, response{
			UserID:      user.ID,
			DeviceID:    actor.DeviceID,
			Username:    user.Username,
			DisplayName: user.DisplayName,
		})
	}
}

func handleResolveUsername(svc in.AuthService) http.HandlerFunc {
	type response struct {
		UserID      string `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		user, err := svc.ResolveUsername(r.Context(), actorFrom(r.Context()).NamespaceID, username)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, response{
			UserID:      user.ID,
			Username:    user.Username,
			DisplayName: user.DisplayName,
		})
	}
}

func handleSetPushToken(svc in.AuthService) http.HandlerFunc {
	type request struct {
		PushToken string `json:"push_token"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if !decodeBody(w, r, &req) {
			return
		}
		actor := actorFrom(r.Context())
		if err := svc.SetPushToken(r.Context(), actor.DeviceID, req.PushToken); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleLogout(svc in.AuthService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := actorFrom(r.Context())
		if err := svc.Logout(r.Context(), actor.SessionID); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
