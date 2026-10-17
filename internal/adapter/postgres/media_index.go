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

type mediaIndex struct {
	pool *pgxpool.Pool
}

func NewMediaIndex(pool *pgxpool.Pool) out.MediaIndex {
	return &mediaIndex{pool: pool}
}

func (s *mediaIndex) Create(ctx context.Context, m *domain.Media) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO media (id, namespace_id, owner_user_id, content_type, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		m.ID, m.NamespaceID, m.OwnerUserID, m.ContentType, m.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: create media: %w", err)
	}
	return nil
}

func (s *mediaIndex) Get(ctx context.Context, mediaID string) (*domain.Media, error) {
	q := querierFrom(ctx, s.pool)
	m := &domain.Media{}
	err := q.QueryRow(ctx, `
		SELECT id, namespace_id, owner_user_id, content_type, created_at
		FROM media WHERE id = $1`, mediaID).
		Scan(&m.ID, &m.NamespaceID, &m.OwnerUserID, &m.ContentType, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get media: %w", err)
	}
	return m, nil
}
