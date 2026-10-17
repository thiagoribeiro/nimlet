//go:build integration

package redis_test

import (
	"os"
	"testing"

	"github.com/thiagoribeiro/nimlet/internal/adapter/redis"
	"github.com/thiagoribeiro/nimlet/testkit"
)

func TestRedisBus_Contract(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	client, err := redis.NewClient(addr, "", 0)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	testkit.RunEventBusContract(t, redis.NewBus(client, 4)) // 4 partitions is enough for tests
}
