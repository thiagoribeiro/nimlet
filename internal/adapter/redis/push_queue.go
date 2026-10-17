package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const (
	pushPendingKeyPrefix = "nimlet:push:pending:"
	pushClaimKeyPrefix   = "nimlet:push:claim:"
	// pushClaimWindow must be >= push.Dispatcher's coalescing window — the claim only
	// needs to survive long enough for the claiming instance's flush timer to fire.
	pushClaimWindow = 10 * time.Second
)

type redisPushQueue struct {
	client *goredis.Client
}

// NewPushQueue creates a PushQueue backed by a Redis list (pending items) and an
// ephemeral claim key (SET NX) that arbitrates which instance flushes a given
// user's batch.
func NewPushQueue(client *goredis.Client) out.PushQueue {
	return &redisPushQueue{client: client}
}

func (q *redisPushQueue) Enqueue(ctx context.Context, userID string, item out.PushQueueItem) (bool, error) {
	encoded, err := json.Marshal(item)
	if err != nil {
		return false, fmt.Errorf("redis: encode push item: %w", err)
	}
	if err := q.client.RPush(ctx, pushPendingKeyPrefix+userID, encoded).Err(); err != nil {
		return false, fmt.Errorf("redis: enqueue push item: %w", err)
	}

	claimed, err := q.client.SetNX(ctx, pushClaimKeyPrefix+userID, "1", pushClaimWindow).Result()
	if err != nil {
		return false, fmt.Errorf("redis: claim push flush: %w", err)
	}
	return claimed, nil
}

func (q *redisPushQueue) EnqueueBatch(ctx context.Context, userIDs []string, item out.PushQueueItem) ([]bool, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("redis: encode push item: %w", err)
	}
	pipe := q.client.Pipeline()
	claims := make([]*goredis.BoolCmd, len(userIDs))
	for i, userID := range userIDs {
		pipe.RPush(ctx, pushPendingKeyPrefix+userID, encoded)
		claims[i] = pipe.SetNX(ctx, pushClaimKeyPrefix+userID, "1", pushClaimWindow)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis: enqueue push batch: %w", err)
	}
	claimed := make([]bool, len(userIDs))
	for i, c := range claims {
		claimed[i] = c.Val()
	}
	return claimed, nil
}

func (q *redisPushQueue) Flush(ctx context.Context, userID string) ([]out.PushQueueItem, error) {
	key := pushPendingKeyPrefix + userID
	// LRANGE + DEL in one MULTI: atomic (nothing enqueued in between is lost) and one round trip.
	var lrange *goredis.StringSliceCmd
	if _, err := q.client.TxPipelined(ctx, func(p goredis.Pipeliner) error {
		lrange = p.LRange(ctx, key, 0, -1)
		p.Del(ctx, key)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("redis: drain pending push items: %w", err)
	}
	raw := lrange.Val()
	if len(raw) == 0 {
		return nil, nil
	}

	items := make([]out.PushQueueItem, 0, len(raw))
	for _, r := range raw {
		var item out.PushQueueItem
		if err := json.Unmarshal([]byte(r), &item); err != nil {
			continue // skip malformed entries rather than failing the whole flush
		}
		items = append(items, item)
	}
	return items, nil
}
