package out

import "context"

// EventBus is the notification channel between event producers and /sync waiters.
// It uses partitioned channels to limit the number of subscriptions.
// Partition key: FNV32a(key) % NumPartitions — key is a userID or "room:"+roomID.
type EventBus interface {
	// Publish notifies subscribers of the partition owning key.
	// key is either a plain userID (small room fan-out) or "room:"+roomID (large rooms).
	Publish(ctx context.Context, key string) error

	// PublishBatch sends multiple keys in one round trip; implementations may pack several
	// keys into one message on any partition, so subscribers must listen on every partition
	// (as sync.Dispatcher does). Preferred over N×Publish for small-room fan-out.
	PublishBatch(ctx context.Context, keys []string) error

	// Subscribe returns a channel that receives keys needing wake-up for the given partition.
	// The returned cancel func must be called to release resources.
	Subscribe(ctx context.Context, partition int) (ch <-chan string, cancel func(), err error)

	// NumPartitions returns the total number of partitions.
	NumPartitions() int
}
