//go:build integration

package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// RunUserStoreContract runs the canonical contract tests for any out.UserStore implementation.
// This same suite is run against every adapter (postgres, future sqlite, etc.).
func RunUserStoreContract(t *testing.T, store out.UserStore) {
	t.Helper()

	t.Run("create and get by id", func(t *testing.T) {
		ctx := context.Background()
		u := &domain.User{
			NamespaceID: domain.DefaultNamespaceID,
			ID:          uuid.NewString(),
			Username:    "contract_" + uuid.NewString()[:8],
			DisplayName: "Test User",
			Type:        domain.UserTypeHuman,
			CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
		}
		require.NoError(t, store.Create(ctx, u, "$argon2id$test"))

		got, err := store.GetByID(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, u.ID, got.ID)
		assert.Equal(t, u.Username, got.Username)
		assert.Equal(t, u.Type, got.Type)
	})

	t.Run("get by username returns password hash", func(t *testing.T) {
		ctx := context.Background()
		u := &domain.User{
			NamespaceID: domain.DefaultNamespaceID,
			ID:          uuid.NewString(),
			Username:    "contract_" + uuid.NewString()[:8],
			DisplayName: "Hash User",
			Type:        domain.UserTypeHuman,
			CreatedAt:   time.Now().UTC(),
		}
		hash := "$argon2id$v=19$m=65536,t=1,p=4$abc$def"
		require.NoError(t, store.Create(ctx, u, hash))

		got, gotHash, err := store.GetByUsername(ctx, domain.DefaultNamespaceID, u.Username)
		require.NoError(t, err)
		assert.Equal(t, u.ID, got.ID)
		assert.Equal(t, hash, gotHash)
	})

	t.Run("duplicate username returns ErrAlreadyExists", func(t *testing.T) {
		ctx := context.Background()
		username := "dup_" + uuid.NewString()[:8]
		u1 := &domain.User{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Username: username, Type: domain.UserTypeHuman, CreatedAt: time.Now().UTC()}
		u2 := &domain.User{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Username: username, Type: domain.UserTypeHuman, CreatedAt: time.Now().UTC()}
		require.NoError(t, store.Create(ctx, u1, "hash1"))
		err := store.Create(ctx, u2, "hash2")
		require.ErrorIs(t, err, domain.ErrAlreadyExists)
	})

	t.Run("get by id not found", func(t *testing.T) {
		_, err := store.GetByID(context.Background(), "nonexistent-id")
		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}
