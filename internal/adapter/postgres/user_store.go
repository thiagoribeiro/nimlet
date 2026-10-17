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

type userStore struct {
	pool *pgxpool.Pool
}

func NewUserStore(pool *pgxpool.Pool) out.UserStore {
	return &userStore{pool: pool}
}

func (s *userStore) Create(ctx context.Context, u *domain.User, passwordHash string) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO users (id, namespace_id, username, display_name, type, password_hash, disabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, false, $7)`,
		u.ID, u.NamespaceID, u.Username, u.DisplayName, string(u.Type), passwordHash, u.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("create user %q: %w", u.Username, domain.ErrAlreadyExists)
		}
		return fmt.Errorf("postgres: create user: %w", err)
	}
	return nil
}

func (s *userStore) GetByID(ctx context.Context, id string) (*domain.User, error) {
	q := querierFrom(ctx, s.pool)
	u := &domain.User{}
	err := q.QueryRow(ctx, `
		SELECT id, namespace_id, username, display_name, type, disabled, created_at
		FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.NamespaceID, &u.Username, &u.DisplayName, &u.Type, &u.Disabled, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get user by id: %w", err)
	}
	return u, nil
}

func (s *userStore) GetByUsername(ctx context.Context, namespaceID, username string) (*domain.User, string, error) {
	q := querierFrom(ctx, s.pool)
	u := &domain.User{}
	var passwordHash string
	err := q.QueryRow(ctx, `
		SELECT id, namespace_id, username, display_name, type, disabled, created_at, password_hash
		FROM users WHERE namespace_id = $1 AND username = $2`, namespaceID, username).
		Scan(&u.ID, &u.NamespaceID, &u.Username, &u.DisplayName, &u.Type, &u.Disabled, &u.CreatedAt, &passwordHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", domain.ErrNotFound
		}
		return nil, "", fmt.Errorf("postgres: get user by username: %w", err)
	}
	return u, passwordHash, nil
}

func (s *userStore) ListByNamespace(ctx context.Context, namespaceID, afterUsername string, limit int) ([]*domain.User, error) {
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `
		SELECT id, namespace_id, username, display_name, type, disabled, created_at
		FROM users
		WHERE namespace_id = $1 AND username > $2
		ORDER BY username
		LIMIT $3`, namespaceID, afterUsername, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list users: %w", err)
	}
	defer rows.Close()
	var out []*domain.User
	for rows.Next() {
		u := &domain.User{}
		if err := rows.Scan(&u.ID, &u.NamespaceID, &u.Username, &u.DisplayName, &u.Type, &u.Disabled, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *userStore) SetDisabled(ctx context.Context, userID string, disabled bool) error {
	return s.update(ctx, `UPDATE users SET disabled = $2 WHERE id = $1`, userID, disabled)
}

func (s *userStore) SetPasswordHash(ctx context.Context, userID, passwordHash string) error {
	return s.update(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, passwordHash)
}

func (s *userStore) update(ctx context.Context, sql string, args ...any) error {
	tag, err := querierFrom(ctx, s.pool).Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("postgres: update user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
