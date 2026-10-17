//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
)

// Sessions and devices must not grow without bound (review P1-12).
func TestSessionAndDeviceGrowth(t *testing.T) {
	env := newTestEnv(t, nil)
	ts := env.ts
	ctx := context.Background()
	count := func(sql string, args ...any) int {
		var n int
		require.NoError(t, env.pool.QueryRow(ctx, sql, args...).Scan(&n))
		return n
	}

	t.Run("login reuses the client's own device", func(t *testing.T) {
		u := registerIn(t, ts, "", "dev_"+uuid.NewString()[:8], "pw")
		other := registerIn(t, ts, "", "dev2_"+uuid.NewString()[:8], "pw")
		var deviceID string
		for i := range 3 {
			resp := apiPost(t, ts, "/login", map[string]any{"username": u.name, "password": "pw", "device_id": deviceID}, "")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			got := decodeJSON(t, resp)["device_id"].(string)
			if i > 0 {
				assert.Equal(t, deviceID, got, "the same device is reused")
			}
			deviceID = got
		}
		assert.Equal(t, 2, count(`SELECT count(*) FROM devices WHERE user_id = $1`, u.userID), "register's device + one reused login device")

		// Another user's device ID is not reusable: a fresh device is created instead.
		var otherDevice string
		require.NoError(t, env.pool.QueryRow(ctx, `SELECT id FROM devices WHERE user_id = $1 LIMIT 1`, other.userID).Scan(&otherDevice))
		resp := apiPost(t, ts, "/login", map[string]any{"username": u.name, "password": "pw", "device_id": otherDevice}, "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEqual(t, otherDevice, decodeJSON(t, resp)["device_id"])
	})

	t.Run("service accounts keep one device and a bounded number of sessions", func(t *testing.T) {
		auth := usecase.NewAuthService(env.users, env.devices, postgres.NewSessionStore(env.pool), env.namespaces,
			postgres.NewTransactor(env.pool), hmacSecret)
		name := "svc_" + uuid.NewString()[:8]
		var userID string
		for range 25 {
			id, _, err := auth.EnsureServiceUser(ctx, "00000000-0000-0000-0000-000000000000", name)
			require.NoError(t, err)
			userID = id
		}
		assert.Equal(t, 1, count(`SELECT count(*) FROM devices WHERE user_id = $1`, userID))
		assert.Equal(t, 20, count(`SELECT count(*) FROM sessions WHERE user_id = $1`, userID))
	})

	t.Run("expired sessions are swept", func(t *testing.T) {
		u := registerIn(t, ts, "", "exp_"+uuid.NewString()[:8], "pw")
		_, err := env.pool.Exec(ctx, `UPDATE sessions SET expires_at = now() - interval '1 day' WHERE user_id = $1`, u.userID)
		require.NoError(t, err)
		n, err := postgres.NewSessionStore(env.pool).DeleteExpired(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, int64(1))
		assert.Zero(t, count(`SELECT count(*) FROM sessions WHERE user_id = $1`, u.userID))
	})
}
