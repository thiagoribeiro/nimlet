//go:build integration

package redis_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/redis"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const testGrace = 300 * time.Millisecond

func testPresence(t *testing.T) out.PresenceStore {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	client, err := redis.NewClient(addr, "", 0)
	require.NoError(t, err)
	return redis.NewPresenceStoreWithGrace(client, testGrace)
}

func present(t *testing.T, s out.PresenceStore, user string) bool {
	t.Helper()
	ok, err := s.IsPresent(context.Background(), user)
	require.NoError(t, err)
	return ok
}

func TestPresence_EndsAGracePeriodAfterTheSyncReturns(t *testing.T) {
	s, ctx, user := testPresence(t), context.Background(), uuid.NewString()
	assert.False(t, present(t, s, user))

	require.NoError(t, s.Begin(ctx, user, time.Minute))
	time.Sleep(2 * testGrace)
	assert.True(t, present(t, s, user), "an open long-poll keeps the user present")

	require.NoError(t, s.End(ctx, user))
	assert.True(t, present(t, s, user), "present during the grace period")
	time.Sleep(2 * testGrace)
	assert.False(t, present(t, s, user), "absent after the grace period")
}

func TestPresence_NextLongPollRestoresTheFullWindow(t *testing.T) {
	s, ctx, user := testPresence(t), context.Background(), uuid.NewString()
	require.NoError(t, s.Begin(ctx, user, time.Minute))
	require.NoError(t, s.End(ctx, user))
	require.NoError(t, s.Begin(ctx, user, time.Minute))
	time.Sleep(2 * testGrace)
	assert.True(t, present(t, s, user))
}

func TestPresence_EndWithoutBeginIsANoop(t *testing.T) {
	s, ctx, user := testPresence(t), context.Background(), uuid.NewString()
	require.NoError(t, s.End(ctx, user))
	assert.False(t, present(t, s, user))
}
