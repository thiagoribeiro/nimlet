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

type deviceStore struct {
	pool *pgxpool.Pool
}

func NewDeviceStore(pool *pgxpool.Pool) out.DeviceStore {
	return &deviceStore{pool: pool}
}

func (s *deviceStore) Create(ctx context.Context, d *domain.Device) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO devices (id, user_id, display_name, disabled, created_at)
		VALUES ($1, $2, $3, false, $4)`,
		d.ID, d.UserID, d.DisplayName, d.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: create device: %w", err)
	}
	return nil
}

func (s *deviceStore) GetByID(ctx context.Context, id string) (*domain.Device, error) {
	q := querierFrom(ctx, s.pool)
	d := &domain.Device{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, display_name, disabled, created_at
		FROM devices WHERE id = $1`, id).
		Scan(&d.ID, &d.UserID, &d.DisplayName, &d.Disabled, &d.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get device: %w", err)
	}
	return d, nil
}

func (s *deviceStore) SetPushToken(ctx context.Context, deviceID, pushToken string) error {
	q := querierFrom(ctx, s.pool)
	tag, err := q.Exec(ctx, `
		UPDATE devices SET push_token = $2 WHERE id = $1`,
		deviceID, nullIfEmpty(pushToken),
	)
	if err != nil {
		return fmt.Errorf("postgres: set push token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *deviceStore) ListPushTokensByUserIDs(ctx context.Context, userIDs []string) ([]domain.DevicePushToken, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `
		SELECT id, user_id, push_token
		FROM devices
		WHERE user_id = ANY($1) AND disabled = false AND push_token IS NOT NULL`,
		userIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list push tokens: %w", err)
	}
	defer rows.Close()

	var out []domain.DevicePushToken
	for rows.Next() {
		var t domain.DevicePushToken
		if err := rows.Scan(&t.DeviceID, &t.UserID, &t.PushToken); err != nil {
			return nil, fmt.Errorf("postgres: scan push token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
