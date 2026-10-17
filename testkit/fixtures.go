//go:build integration

package testkit

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Seed* helpers insert rows directly via SQL, bypassing the store layer, all in the
// default namespace.

// SeedUser inserts a minimal user row directly via SQL, bypassing the store layer.
// Used in contract tests to satisfy FK constraints without depending on UserStore.
func SeedUser(pool *pgxpool.Pool, userID string) {
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO users (id, namespace_id, username, display_name, type, password_hash, disabled, created_at)
		VALUES ($1, '00000000-0000-0000-0000-000000000000', $2, 'Test', 'human', '$hash', false, $3)
		ON CONFLICT DO NOTHING`,
		userID, "user_"+userID[:8], time.Now().UTC(),
	)
}

// SeedDevice inserts a minimal device row directly via SQL, bypassing the store layer —
// used by contract tests that need a device in a specific state (e.g. disabled) the
// store's own Create method can't produce.
func SeedDevice(pool *pgxpool.Pool, deviceID, userID string, disabled bool) {
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO devices (id, user_id, display_name, disabled, created_at)
		VALUES ($1, $2, 'Test Device', $3, $4)
		ON CONFLICT DO NOTHING`,
		deviceID, userID, disabled, time.Now().UTC(),
	)
}

// SeedRoom inserts a minimal room row directly via SQL.
func SeedRoom(pool *pgxpool.Pool, roomID, creatorID string) {
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO rooms (id, namespace_id, name, creator_id, created_at)
		VALUES ($1, '00000000-0000-0000-0000-000000000000', $2, $3, $4)
		ON CONFLICT DO NOTHING`,
		roomID, "room_"+roomID[:8], creatorID, time.Now().UTC(),
	)
}

// SeedLargeRoom inserts a room whose member_count is already past the large-room threshold,
// so stores treat it as large without seeding a thousand memberships.
func SeedLargeRoom(pool *pgxpool.Pool, roomID, creatorID string) {
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO rooms (id, namespace_id, name, creator_id, created_at, member_count)
		VALUES ($1, '00000000-0000-0000-0000-000000000000', $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		roomID, "room_"+roomID[:8], creatorID, time.Now().UTC(), domain.LargeRoomThreshold,
	)
}

// SeedMembership inserts or updates a membership row directly via SQL.
func SeedMembership(pool *pgxpool.Pool, roomID, userID, state string) {
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role, updated_at)
		VALUES ('00000000-0000-0000-0000-000000000000', $1, $2, $3, 'member', $4)
		ON CONFLICT (room_id, user_id) DO UPDATE SET state = EXCLUDED.state`,
		roomID, userID, state, time.Now().UTC(),
	)
}

// NewID returns a short unique ID suitable for seeding.
func NewID() string {
	return uuid.NewString()
}
