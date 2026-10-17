package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type sessionStore struct {
	pool *pgxpool.Pool
}

func NewSessionStore(pool *pgxpool.Pool) out.SessionStore {
	return &sessionStore{pool: pool}
}

func (s *sessionStore) Create(ctx context.Context, sess *domain.Session) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO sessions (id, namespace_id, device_id, user_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sess.ID, sess.NamespaceID, sess.DeviceID, sess.UserID, sess.TokenHash, sess.ExpiresAt, sess.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: create session: %w", err)
	}
	return nil
}

func (s *sessionStore) GetAuthByTokenHash(ctx context.Context, hash string) (*domain.SessionAuth, error) {
	q := querierFrom(ctx, s.pool)
	a := &domain.SessionAuth{}
	sess := &a.Session
	err := q.QueryRow(ctx, `
		SELECT s.id, s.namespace_id, s.device_id, s.user_id, s.token_hash, s.expires_at, s.created_at,
		       d.disabled, u.disabled, n.status <> 'active'
		FROM sessions s
		JOIN devices    d ON d.id = s.device_id
		JOIN users      u ON u.id = s.user_id
		JOIN namespaces n ON n.id = s.namespace_id
		WHERE s.token_hash = $1`, hash).
		Scan(&sess.ID, &sess.NamespaceID, &sess.DeviceID, &sess.UserID, &sess.TokenHash, &sess.ExpiresAt, &sess.CreatedAt,
			&a.DeviceDisabled, &a.UserDisabled, &a.NamespaceDisabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get session: %w", err)
	}
	return a, nil
}

func (s *sessionStore) Delete(ctx context.Context, sessionID string) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("postgres: delete session: %w", err)
	}
	return nil
}

func (s *sessionStore) ListByUser(ctx context.Context, userID string) ([]*domain.Session, error) {
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `
		SELECT id, namespace_id, device_id, user_id, expires_at, created_at
		FROM sessions WHERE user_id = $1
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list sessions: %w", err)
	}
	defer rows.Close()
	var out []*domain.Session
	for rows.Next() {
		sess := &domain.Session{}
		if err := rows.Scan(&sess.ID, &sess.NamespaceID, &sess.DeviceID, &sess.UserID, &sess.ExpiresAt, &sess.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *sessionStore) DeleteInNamespace(ctx context.Context, namespaceID, sessionID string) error {
	q := querierFrom(ctx, s.pool)
	tag, err := q.Exec(ctx, `DELETE FROM sessions WHERE id = $1 AND namespace_id = $2`, sessionID, namespaceID)
	if err != nil {
		return fmt.Errorf("postgres: delete session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *sessionStore) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := querierFrom(ctx, s.pool).Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("postgres: delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *sessionStore) PruneDevice(ctx context.Context, deviceID string, keep int) error {
	_, err := querierFrom(ctx, s.pool).Exec(ctx, `
		DELETE FROM sessions
		WHERE device_id = $1 AND id NOT IN (
			SELECT id FROM sessions WHERE device_id = $1 ORDER BY created_at DESC LIMIT $2)`,
		deviceID, keep)
	if err != nil {
		return fmt.Errorf("postgres: prune device sessions: %w", err)
	}
	return nil
}
