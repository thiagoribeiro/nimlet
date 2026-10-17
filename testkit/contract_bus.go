//go:build integration

package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// RunEventBusContract verifies Publish/PublishBatch → Subscribe delivery for any EventBus implementation.
func RunEventBusContract(t *testing.T, bus out.EventBus) {
	t.Helper()

	t.Run("publish delivers to subscriber on correct partition", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		userID := "user_bus_test"
		var received string
		done := make(chan struct{})

		for p := 0; p < bus.NumPartitions(); p++ {
			ch, unsub, err := bus.Subscribe(ctx, p)
			require.NoError(t, err)
			go func(ch <-chan string, unsub func()) {
				defer unsub()
				for uid := range ch {
					if uid == userID {
						received = uid
						close(done)
						return
					}
				}
			}(ch, unsub)
		}

		time.Sleep(50 * time.Millisecond)
		require.NoError(t, bus.Publish(ctx, userID))

		select {
		case <-done:
			assert.Equal(t, userID, received)
		case <-ctx.Done():
			t.Fatal("timeout: userID not received by subscriber")
		}
	})

	t.Run("publish_batch delivers all keys", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		keys := []string{"batch_user_1", "batch_user_2", "batch_user_3"}
		received := make(map[string]bool)
		mu := make(chan struct{}, len(keys))

		for p := 0; p < bus.NumPartitions(); p++ {
			ch, unsub, err := bus.Subscribe(ctx, p)
			require.NoError(t, err)
			go func(ch <-chan string, unsub func()) {
				defer unsub()
				for k := range ch {
					for _, want := range keys {
						if k == want {
							mu <- struct{}{}
						}
					}
				}
			}(ch, unsub)
		}

		time.Sleep(50 * time.Millisecond)
		require.NoError(t, bus.PublishBatch(ctx, keys))

		timeout := time.After(4 * time.Second)
		for i := 0; i < len(keys); i++ {
			select {
			case <-mu:
				received[keys[i]] = true
			case <-timeout:
				t.Fatalf("timeout: only received %d/%d batch keys", i, len(keys))
			}
		}
		assert.Len(t, received, len(keys))
	})

	t.Run("publish_batch delivers every key of a large batch", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		const n = 2500 // spans several packed messages
		want := make(map[string]bool, n)
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("big_batch_%d", i)
			want[keys[i]] = true
		}
		got := make(chan string, n)
		for p := 0; p < bus.NumPartitions(); p++ {
			ch, unsub, err := bus.Subscribe(ctx, p)
			require.NoError(t, err)
			go func() {
				defer unsub()
				for k := range ch {
					if want[k] {
						got <- k
					}
				}
			}()
		}
		time.Sleep(50 * time.Millisecond)
		require.NoError(t, bus.PublishBatch(ctx, keys))
		seen := make(map[string]bool, n)
		for len(seen) < n {
			select {
			case k := <-got:
				seen[k] = true
			case <-ctx.Done():
				t.Fatalf("received %d/%d keys", len(seen), n)
			}
		}
	})
}
