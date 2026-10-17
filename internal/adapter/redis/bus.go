package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const (
	channelPrefix     = "nimlet:sync:"
	defaultChanBuffer = keysPerMessage // room for one full packed message
	forwardWait       = 100 * time.Millisecond

	// A message carries up to keysPerMessage wake-up keys separated by keySeparator. Keys
	// are user IDs or "room:"+roomID (UUIDs), which never contain a newline.
	keySeparator   = "\n"
	keysPerMessage = 1000
)

type redisBus struct {
	client     *goredis.Client
	partitions int
	channels   []string // channels[p] = channelPrefix + p; precomputed, publish is per recipient
	chanBuffer int
	dropped    atomic.Int64 // total messages dropped due to full channel buffer
}

// NewBus creates an EventBus backed by Redis Pub/Sub.
// partitions should be 256 for production; each partition maps to one channel.
func NewBus(client *goredis.Client, partitions int) out.EventBus {
	channels := make([]string, partitions)
	for p := range channels {
		channels[p] = channelPrefix + strconv.Itoa(p)
	}
	return &redisBus{
		client:     client,
		partitions: partitions,
		channels:   channels,
		chanBuffer: defaultChanBuffer,
	}
}

func (b *redisBus) NumPartitions() int { return b.partitions }

// Publish sends a single key to its partition channel.
// Use PublishBatch when sending to multiple keys to reduce round-trips.
func (b *redisBus) Publish(ctx context.Context, key string) error {
	partition := hashPartition(key, b.partitions)
	if err := b.client.Publish(ctx, b.channels[partition], key).Err(); err != nil {
		return fmt.Errorf("redis: publish partition %d: %w", partition, err)
	}
	return nil
}

// PublishBatch packs keys into as few messages as possible (keysPerMessage per message,
// newline-separated) and publishes them in one round trip. Every instance subscribes to
// every partition, so one message per key — the old behaviour — cost members × instances
// Redis deliveries per send; now it is ⌈members/keysPerMessage⌉ × instances.
func (b *redisBus) PublishBatch(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	pipe := b.client.Pipeline()
	for start := 0; start < len(keys); start += keysPerMessage {
		chunk := keys[start:min(start+keysPerMessage, len(keys))]
		pipe.Publish(ctx, b.channels[hashPartition(chunk[0], b.partitions)], strings.Join(chunk, keySeparator))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: publish batch: %w", err)
	}
	return nil
}

// Subscribe returns a channel that delivers keys for the given partition.
// A goroutine reads from Redis and forwards to the buffered channel.
// The cancel func unsubscribes and stops the goroutine.
func (b *redisBus) Subscribe(ctx context.Context, partition int) (<-chan string, func(), error) {
	sub := b.client.Subscribe(ctx, b.channels[partition])

	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, nil, fmt.Errorf("redis: subscribe partition %d: %w", partition, err)
	}

	ch := make(chan string, b.chanBuffer)

	go func() {
		defer close(ch)
		msgCh := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				for key := range strings.SplitSeq(msg.Payload, keySeparator) {
					b.forward(ctx, ch, key)
				}
			}
		}
	}()

	return ch, func() { _ = sub.Close() }, nil
}

// forward hands key to the subscriber. One packed message can carry up to keysPerMessage
// keys at once, so a full buffer is waited on briefly (the consumer is the in-process
// dispatcher, which drains fast) before the key is dropped. A drop is not data loss: the
// client wakes on its long-poll timeout and re-syncs. Drops are counted for alerting.
func (b *redisBus) forward(ctx context.Context, ch chan<- string, key string) {
	select {
	case ch <- key:
		return
	default:
	}
	timer := time.NewTimer(forwardWait)
	defer timer.Stop()
	select {
	case ch <- key:
	case <-timer.C:
		b.dropped.Add(1)
	case <-ctx.Done():
	}
}

// DroppedNotifications returns the total number of dropped notifications since startup.
func (b *redisBus) DroppedNotifications() int64 { return b.dropped.Load() }

// hashPartition maps a key to a partition using inline FNV-1a — zero allocation.
func hashPartition(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h) % n
}
