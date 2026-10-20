package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const presenceKeyPrefix = "nimlet:presence:"

// PresenceGrace is how long a user stays present after their /sync returns: long
// enough for a connected client to open the next long-poll, short enough that a
// user who closed the app is pushed within seconds.
const PresenceGrace = 10 * time.Second

type redisPresenceStore struct {
	client *goredis.Client
	grace  time.Duration
}

// NewPresenceStore creates a PresenceStore backed by one ephemeral Redis key per user.
func NewPresenceStore(client *goredis.Client) out.PresenceStore {
	return NewPresenceStoreWithGrace(client, PresenceGrace)
}

// NewPresenceStoreWithGrace is NewPresenceStore with a custom grace period (tests).
func NewPresenceStoreWithGrace(client *goredis.Client, grace time.Duration) out.PresenceStore {
	return &redisPresenceStore{client: client, grace: grace}
}

func (s *redisPresenceStore) Begin(ctx context.Context, userID string, timeout time.Duration) error {
	if err := s.client.Set(ctx, presenceKeyPrefix+userID, "1", timeout+s.grace).Err(); err != nil {
		return fmt.Errorf("redis: begin presence: %w", err)
	}
	return nil
}

// End shortens the key to the grace period. With two devices connected, one ending
// also shortens the other's presence until its next long-poll: that can cost one
// redundant push, never a missed one.
func (s *redisPresenceStore) End(ctx context.Context, userID string) error {
	if err := s.client.PExpire(ctx, presenceKeyPrefix+userID, s.grace).Err(); err != nil {
		return fmt.Errorf("redis: end presence: %w", err)
	}
	return nil
}

func (s *redisPresenceStore) IsPresent(ctx context.Context, userID string) (bool, error) {
	n, err := s.client.Exists(ctx, presenceKeyPrefix+userID).Result()
	if err != nil {
		return false, fmt.Errorf("redis: check presence: %w", err)
	}
	return n > 0, nil
}
