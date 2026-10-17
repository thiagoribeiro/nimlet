//go:build integration

package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// RunDeviceStoreContract verifies the device store contract for any adapter, including
// the push-token registration/fan-out methods the push feature depends on.
// pool is used only for FK fixture seeding (users) and seeding device states the store's
// own Create can't produce (e.g. disabled), not for the store under test.
func RunDeviceStoreContract(t *testing.T, store out.DeviceStore, pool *pgxpool.Pool) {
	t.Helper()

	t.Run("create and get by id", func(t *testing.T) {
		ctx := context.Background()
		userID := NewID()
		SeedUser(pool, userID)

		d := &domain.Device{
			ID:          NewID(),
			UserID:      userID,
			DisplayName: "Test Device",
			CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
		}
		require.NoError(t, store.Create(ctx, d))

		got, err := store.GetByID(ctx, d.ID)
		require.NoError(t, err)
		assert.Equal(t, d.ID, got.ID)
		assert.Equal(t, d.UserID, got.UserID)
		assert.False(t, got.Disabled)
	})

	t.Run("get by id not found", func(t *testing.T) {
		_, err := store.GetByID(context.Background(), "nonexistent-id")
		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("set push token on nonexistent device returns ErrNotFound", func(t *testing.T) {
		err := store.SetPushToken(context.Background(), "nonexistent-id", "tok")
		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("set push token then list by user", func(t *testing.T) {
		ctx := context.Background()
		userID := NewID()
		SeedUser(pool, userID)

		d := &domain.Device{ID: NewID(), UserID: userID, DisplayName: "Push Device", CreatedAt: time.Now().UTC()}
		require.NoError(t, store.Create(ctx, d))

		// No token registered yet: excluded from the fan-out list.
		tokens, err := store.ListPushTokensByUserIDs(ctx, []string{userID})
		require.NoError(t, err)
		assert.Empty(t, tokens)

		require.NoError(t, store.SetPushToken(ctx, d.ID, "fcm-token-1"))

		tokens, err = store.ListPushTokensByUserIDs(ctx, []string{userID})
		require.NoError(t, err)
		require.Len(t, tokens, 1)
		assert.Equal(t, d.ID, tokens[0].DeviceID)
		assert.Equal(t, userID, tokens[0].UserID)
		assert.Equal(t, "fcm-token-1", tokens[0].PushToken)

		// Clearing the token (empty string) removes it from the fan-out list again.
		require.NoError(t, store.SetPushToken(ctx, d.ID, ""))
		tokens, err = store.ListPushTokensByUserIDs(ctx, []string{userID})
		require.NoError(t, err)
		assert.Empty(t, tokens)
	})

	t.Run("list push tokens excludes disabled devices", func(t *testing.T) {
		ctx := context.Background()
		userID := NewID()
		SeedUser(pool, userID)

		activeID, disabledID := NewID(), NewID()
		SeedDevice(pool, activeID, userID, false)
		SeedDevice(pool, disabledID, userID, true)
		require.NoError(t, store.SetPushToken(ctx, activeID, "tok-active"))
		require.NoError(t, store.SetPushToken(ctx, disabledID, "tok-disabled"))

		tokens, err := store.ListPushTokensByUserIDs(ctx, []string{userID})
		require.NoError(t, err)
		require.Len(t, tokens, 1)
		assert.Equal(t, activeID, tokens[0].DeviceID)
	})

	t.Run("list push tokens spans multiple users and skips empty input", func(t *testing.T) {
		ctx := context.Background()
		userA, userB := NewID(), NewID()
		SeedUser(pool, userA)
		SeedUser(pool, userB)
		devA, devB := NewID(), NewID()
		SeedDevice(pool, devA, userA, false)
		SeedDevice(pool, devB, userB, false)
		require.NoError(t, store.SetPushToken(ctx, devA, "tok-a"))
		require.NoError(t, store.SetPushToken(ctx, devB, "tok-b"))

		tokens, err := store.ListPushTokensByUserIDs(ctx, []string{userA, userB})
		require.NoError(t, err)
		require.Len(t, tokens, 2)

		empty, err := store.ListPushTokensByUserIDs(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, empty)
	})
}
