package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const (
	presenceKeyPrefix = "nimlet:presence:"
	// presenceTTL comfortably covers one /sync long-poll (up to maxSyncTimeout=60s in
	// usecase/sync.go) plus the gap before the client reopens the next one.
	presenceTTL = 90 * time.Second
)

type redisPresenceStore struct {
	client *goredis.Client
}

// NewPresenceStore creates a PresenceStore backed by ephemeral Redis keys.
func NewPresenceStore(client *goredis.Client) out.PresenceStore {
	return &redisPresenceStore{client: client}
}

func (s *redisPresenceStore) Touch(ctx context.Context, userID string) error {
	if err := s.client.Set(ctx, presenceKeyPrefix+userID, "1", presenceTTL).Err(); err != nil {
		return fmt.Errorf("redis: touch presence: %w", err)
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
