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

type roomStore struct {
	pool *pgxpool.Pool
}

func NewRoomStore(pool *pgxpool.Pool) out.RoomStore {
	return &roomStore{pool: pool}
}

const roomColumns = `id, namespace_id, name, creator_id, created_at, member_count, join_rule, direct_key, alias, expires_at`

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRoom(row rowScanner) (*domain.Room, error) {
	r := &domain.Room{}
	var directKey, alias *string
	if err := row.Scan(&r.ID, &r.NamespaceID, &r.Name, &r.CreatorID, &r.CreatedAt, &r.MemberCount, &r.JoinRule, &directKey, &alias, &r.ExpiresAt); err != nil {
		return nil, err
	}
	if directKey != nil {
		r.DirectKey = *directKey
	}
	if alias != nil {
		r.Alias = *alias
	}
	return r, nil
}

func (s *roomStore) Create(ctx context.Context, r *domain.Room) error {
	q := querierFrom(ctx, s.pool)
	joinRule := r.JoinRule
	if joinRule == "" {
		joinRule = domain.JoinRuleInviteOnly
	}
	_, err := q.Exec(ctx, `
		INSERT INTO rooms (id, namespace_id, name, creator_id, created_at, member_count, join_rule, direct_key, alias, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.ID, r.NamespaceID, r.Name, r.CreatorID, r.CreatedAt, r.MemberCount, string(joinRule),
		nilIfEmpty(r.DirectKey), nilIfEmpty(r.Alias), r.ExpiresAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("create room: %w", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("postgres: create room: %w", err)
	}
	return nil
}

func (s *roomStore) GetByID(ctx context.Context, id string) (*domain.Room, error) {
	q := querierFrom(ctx, s.pool)
	r, err := scanRoom(q.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get room: %w", err)
	}
	return r, nil
}

func (s *roomStore) GetByDirectKey(ctx context.Context, namespaceID, directKey string) (*domain.Room, error) {
	q := querierFrom(ctx, s.pool)
	r, err := scanRoom(q.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE namespace_id = $1 AND direct_key = $2`, namespaceID, directKey))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get room by direct key: %w", err)
	}
	return r, nil
}

func (s *roomStore) GetByAlias(ctx context.Context, namespaceID, alias string) (*domain.Room, error) {
	q := querierFrom(ctx, s.pool)
	r, err := scanRoom(q.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE namespace_id = $1 AND alias = $2`, namespaceID, alias))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get room by alias: %w", err)
	}
	return r, nil
}

func (s *roomStore) IncrementMemberCount(ctx context.Context, roomID string) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx,
		`UPDATE rooms SET member_count = member_count + 1 WHERE id = $1`, roomID)
	if err != nil {
		return fmt.Errorf("postgres: increment member count: %w", err)
	}
	return nil
}

func (s *roomStore) DecrementMemberCount(ctx context.Context, roomID string) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx,
		`UPDATE rooms SET member_count = GREATEST(member_count - 1, 0) WHERE id = $1`, roomID)
	if err != nil {
		return fmt.Errorf("postgres: decrement member count: %w", err)
	}
	return nil
}

func (s *roomStore) ListByNamespace(ctx context.Context, namespaceID string, limit int) ([]*domain.Room, error) {
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `SELECT `+roomColumns+` FROM rooms
		WHERE namespace_id = $1 AND direct_key IS NULL
		ORDER BY created_at DESC LIMIT $2`, namespaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list rooms: %w", err)
	}
	defer rows.Close()
	var out []*domain.Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan room: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *roomStore) ListByIDs(ctx context.Context, ids []string) ([]*domain.Room, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("postgres: list rooms by ids: %w", err)
	}
	defer rows.Close()

	var out []*domain.Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan room: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteExpired removes every room past its expires_at, deleteBatch rooms per statement so
// a backlog of expired rooms never becomes one huge transaction (long locks, WAL spikes).
// room_memberships cascade via FK; events have no cascade, so they (and their
// visibility/idempotency rows, which are app-managed and have no FK) are deleted explicitly
// by stream_pos before the rooms go.
func (s *roomStore) DeleteExpired(ctx context.Context) (int64, error) {
	const deleteBatch = 50
	q := querierFrom(ctx, s.pool)
	var total int64
	for {
		tag, err := q.Exec(ctx, `
			WITH expired AS (
				SELECT id FROM rooms
				WHERE expires_at IS NOT NULL AND expires_at < now()
				ORDER BY expires_at
				LIMIT $1
			),
			ev AS (
				SELECT stream_pos FROM events WHERE room_id IN (SELECT id FROM expired)
			),
			del_idem AS (
				DELETE FROM event_idempotency WHERE stream_pos IN (SELECT stream_pos FROM ev)
			),
			del_vis AS (
				DELETE FROM event_visibility WHERE stream_pos IN (SELECT stream_pos FROM ev)
			),
			del_ev AS (
				DELETE FROM events WHERE room_id IN (SELECT id FROM expired)
			)
			DELETE FROM rooms WHERE id IN (SELECT id FROM expired)`, deleteBatch)
		if err != nil {
			return total, fmt.Errorf("postgres: delete expired rooms: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < deleteBatch || ctx.Err() != nil {
			return total, nil
		}
	}
}
