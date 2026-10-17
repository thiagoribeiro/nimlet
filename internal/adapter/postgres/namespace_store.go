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

type namespaceStore struct {
	pool *pgxpool.Pool
}

func NewNamespaceStore(pool *pgxpool.Pool) out.NamespaceStore {
	return &namespaceStore{pool: pool}
}

const namespaceColumns = `id, slug, status, allow_registration, created_at, disabled_at`

func scanNamespace(row rowScanner) (*domain.Namespace, error) {
	n := &domain.Namespace{}
	if err := row.Scan(&n.ID, &n.Slug, &n.Status, &n.AllowRegistration, &n.CreatedAt, &n.DisabledAt); err != nil {
		return nil, err
	}
	return n, nil
}

func (s *namespaceStore) Create(ctx context.Context, n *domain.Namespace) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO namespaces (id, slug, status, allow_registration, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		n.ID, n.Slug, string(n.Status), n.AllowRegistration, n.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("create namespace %q: %w", n.Slug, domain.ErrAlreadyExists)
		}
		if isInsufficientPrivilege(err) {
			return fmt.Errorf("create namespace: database role lacks the privilege: %w", domain.ErrPermissionDenied)
		}
		return fmt.Errorf("postgres: create namespace: %w", err)
	}
	return nil
}

func (s *namespaceStore) get(ctx context.Context, where string, arg string) (*domain.Namespace, error) {
	q := querierFrom(ctx, s.pool)
	n, err := scanNamespace(q.QueryRow(ctx, `SELECT `+namespaceColumns+` FROM namespaces WHERE `+where+` = $1`, arg))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get namespace: %w", err)
	}
	return n, nil
}

func (s *namespaceStore) GetByID(ctx context.Context, id string) (*domain.Namespace, error) {
	return s.get(ctx, "id", id)
}

func (s *namespaceStore) GetBySlug(ctx context.Context, slug string) (*domain.Namespace, error) {
	return s.get(ctx, "slug", slug)
}

func (s *namespaceStore) List(ctx context.Context) ([]*domain.Namespace, error) {
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `SELECT `+namespaceColumns+` FROM namespaces ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list namespaces: %w", err)
	}
	defer rows.Close()
	var out []*domain.Namespace
	for rows.Next() {
		n, err := scanNamespace(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan namespace: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *namespaceStore) SetStatus(ctx context.Context, id string, status domain.NamespaceStatus) error {
	q := querierFrom(ctx, s.pool)
	tag, err := q.Exec(ctx, `
		UPDATE namespaces
		SET status = $2,
		    disabled_at = CASE WHEN $2 = 'disabled' THEN coalesce(disabled_at, now()) END
		WHERE id = $1`, id, string(status))
	if err != nil {
		if isInsufficientPrivilege(err) {
			return fmt.Errorf("set namespace status: database role lacks the privilege: %w", domain.ErrPermissionDenied)
		}
		return fmt.Errorf("postgres: set namespace status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *namespaceStore) SetAllowRegistration(ctx context.Context, id string, allow bool) error {
	q := querierFrom(ctx, s.pool)
	tag, err := q.Exec(ctx, `UPDATE namespaces SET allow_registration = $2 WHERE id = $1`, id, allow)
	if err != nil {
		return fmt.Errorf("postgres: set namespace registration: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
