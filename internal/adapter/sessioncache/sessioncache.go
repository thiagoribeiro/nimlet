// Package sessioncache keeps request authentication off the database for a short while.
//
// Every authenticated request loads its session, device, user and namespace state
// (SessionStore.GetAuthByTokenHash). A /sync long-poll client sends several requests per
// second, so under load that lookup becomes one of the largest costs on Postgres.
// Store serves it from memory for up to a TTL instead. The price is that revoking a session
// or disabling a device, user or namespace takes up to the TTL to reach other instances;
// this instance forgets a session at once when it deletes it.
package sessioncache

import (
	"context"
	"sync"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type entry struct {
	auth    domain.SessionAuth
	expires time.Time
}

// Store wraps a SessionStore with a TTL cache of GetAuthByTokenHash.
type Store struct {
	out.SessionStore
	ttl time.Duration

	mu        sync.Mutex
	byHash    map[string]entry
	nextSweep time.Time
}

// New wraps inner. A ttl <= 0 returns inner unchanged.
func New(inner out.SessionStore, ttl time.Duration) out.SessionStore {
	if ttl <= 0 {
		return inner
	}
	return &Store{SessionStore: inner, ttl: ttl, byHash: make(map[string]entry)}
}

func (s *Store) GetAuthByTokenHash(ctx context.Context, hash string) (*domain.SessionAuth, error) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.byHash[hash]
	s.mu.Unlock()
	if ok && now.Before(e.expires) {
		auth := e.auth // callers get their own copy
		return &auth, nil
	}
	// Errors (not found, database down) are never cached: a new session must work at once,
	// and an outage must not outlive itself.
	auth, err := s.SessionStore.GetAuthByTokenHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if now.After(s.nextSweep) {
		for h, old := range s.byHash {
			if now.After(old.expires) {
				delete(s.byHash, h)
			}
		}
		s.nextSweep = now.Add(10 * s.ttl)
	}
	s.byHash[hash] = entry{auth: *auth, expires: now.Add(s.ttl)}
	s.mu.Unlock()
	return auth, nil
}

func (s *Store) Delete(ctx context.Context, sessionID string) error {
	err := s.SessionStore.Delete(ctx, sessionID)
	s.forget(sessionID)
	return err
}

func (s *Store) DeleteInNamespace(ctx context.Context, namespaceID, sessionID string) error {
	err := s.SessionStore.DeleteInNamespace(ctx, namespaceID, sessionID)
	s.forget(sessionID)
	return err
}

// PruneDevice and DeleteExpired remove sessions this cache can't name; drop it all.

func (s *Store) PruneDevice(ctx context.Context, deviceID string, keep int) error {
	err := s.SessionStore.PruneDevice(ctx, deviceID, keep)
	s.forgetAll()
	return err
}

func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	n, err := s.SessionStore.DeleteExpired(ctx)
	if n > 0 {
		s.forgetAll()
	}
	return n, err
}

func (s *Store) forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, e := range s.byHash {
		if e.auth.Session.ID == sessionID {
			delete(s.byHash, h)
		}
	}
}

func (s *Store) forgetAll() {
	s.mu.Lock()
	clear(s.byHash)
	s.mu.Unlock()
}
