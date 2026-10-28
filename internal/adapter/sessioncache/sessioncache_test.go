package sessioncache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type fakeSessions struct {
	out.SessionStore // methods the tests don't call
	lookups          int
	err              error
	userDisabled     bool
}

func (f *fakeSessions) GetAuthByTokenHash(_ context.Context, hash string) (*domain.SessionAuth, error) {
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	return &domain.SessionAuth{Session: domain.Session{ID: "sess-" + hash}, UserDisabled: f.userDisabled}, nil
}

func (f *fakeSessions) Delete(context.Context, string) error { return nil }

func TestCachesWithinTTL(t *testing.T) {
	ctx := context.Background()
	inner := &fakeSessions{}
	s := New(inner, time.Hour)

	a, err := s.GetAuthByTokenHash(ctx, "h")
	require.NoError(t, err)
	a.UserDisabled = true // a caller mutating its copy must not leak into the cache
	b, err := s.GetAuthByTokenHash(ctx, "h")
	require.NoError(t, err)
	assert.False(t, b.UserDisabled)
	assert.Equal(t, 1, inner.lookups)
}

func TestExpiresAfterTTL(t *testing.T) {
	ctx := context.Background()
	inner := &fakeSessions{}
	s := New(inner, 20*time.Millisecond)

	_, _ = s.GetAuthByTokenHash(ctx, "h")
	inner.userDisabled = true
	time.Sleep(30 * time.Millisecond)
	a, err := s.GetAuthByTokenHash(ctx, "h")
	require.NoError(t, err)
	assert.True(t, a.UserDisabled, "state changes are seen once the TTL has passed")
	assert.Equal(t, 2, inner.lookups)
}

func TestDeleteForgetsAtOnce(t *testing.T) {
	ctx := context.Background()
	inner := &fakeSessions{}
	s := New(inner, time.Hour)

	_, _ = s.GetAuthByTokenHash(ctx, "h")
	require.NoError(t, s.Delete(ctx, "sess-h"))
	inner.err = domain.ErrNotFound
	_, err := s.GetAuthByTokenHash(ctx, "h")
	assert.ErrorIs(t, err, domain.ErrNotFound, "a logout on this instance takes effect immediately")
}

func TestErrorsAreNotCached(t *testing.T) {
	ctx := context.Background()
	inner := &fakeSessions{err: errors.New("db down")}
	s := New(inner, time.Hour)

	_, err := s.GetAuthByTokenHash(ctx, "h")
	require.Error(t, err)
	inner.err = nil
	_, err = s.GetAuthByTokenHash(ctx, "h")
	require.NoError(t, err)
	assert.Equal(t, 2, inner.lookups)
}

func TestZeroTTLDisables(t *testing.T) {
	inner := &fakeSessions{}
	assert.Same(t, out.SessionStore(inner), New(inner, 0))
}
