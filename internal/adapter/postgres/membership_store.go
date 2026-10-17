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

type membershipStore struct {
	pool *pgxpool.Pool
}

func NewMembershipStore(pool *pgxpool.Pool) out.MembershipStore {
	return &membershipStore{pool: pool}
}

const membershipColumns = `namespace_id, room_id, user_id, state, role, updated_at`

func scanMembership(row rowScanner) (*domain.Membership, error) {
	m := &domain.Membership{}
	if err := row.Scan(&m.NamespaceID, &m.RoomID, &m.UserID, &m.State, &m.Role, &m.UpdatedAt); err != nil {
		return nil, err
	}
	return m, nil
}

func scanMemberships(rows pgx.Rows) ([]*domain.Membership, error) {
	defer rows.Close()
	var out []*domain.Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan membership: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *membershipStore) Set(ctx context.Context, m *domain.Membership) error {
	q := querierFrom(ctx, s.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (room_id, user_id) DO UPDATE
			SET state = EXCLUDED.state,
			    role  = EXCLUDED.role,
			    updated_at = EXCLUDED.updated_at`,
		m.NamespaceID, m.RoomID, m.UserID, string(m.State), string(m.Role), m.UpdatedAt,
	)
	if err != nil {
		// The composite FKs reject a room or user from another namespace (or a missing
		// one): to the caller that is simply "no such user/room here".
		if isForeignKeyViolation(err) {
			return fmt.Errorf("set membership: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("postgres: set membership: %w", err)
	}
	return nil
}

func (s *membershipStore) Transition(ctx context.Context, namespaceID, roomID, userID string,
	next func(cur *domain.Membership) (*domain.Membership, error)) (*domain.Membership, error) {
	q := querierFrom(ctx, s.pool)
	// Two passes at most: if a concurrent transaction inserts the row between our read and
	// our insert, the insert does nothing and we decide again against the row it created.
	for range 2 {
		cur, err := scanMembership(q.QueryRow(ctx, `
			SELECT `+membershipColumns+`
			FROM room_memberships
			WHERE room_id = $1 AND user_id = $2
			FOR UPDATE`, roomID, userID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			cur = nil
		case err != nil:
			return nil, fmt.Errorf("postgres: lock membership: %w", err)
		}

		m, err := next(cur)
		if err != nil {
			return cur, err
		}
		if cur != nil {
			if _, err := q.Exec(ctx, `
				UPDATE room_memberships SET state = $3, role = $4, updated_at = $5
				WHERE room_id = $1 AND user_id = $2`,
				roomID, userID, string(m.State), string(m.Role), m.UpdatedAt); err != nil {
				return cur, fmt.Errorf("postgres: update membership: %w", err)
			}
			return cur, nil
		}
		tag, err := q.Exec(ctx, `
			INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (room_id, user_id) DO NOTHING`,
			namespaceID, roomID, userID, string(m.State), string(m.Role), m.UpdatedAt)
		if err != nil {
			if isForeignKeyViolation(err) {
				return nil, fmt.Errorf("membership transition: %w", domain.ErrNotFound)
			}
			return nil, fmt.Errorf("postgres: insert membership: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("postgres: membership transition: concurrent updates did not settle")
}

func (s *membershipStore) Get(ctx context.Context, roomID, userID string) (*domain.Membership, error) {
	m, err := scanMembership(querierFrom(ctx, s.pool).QueryRow(ctx, `
		SELECT `+membershipColumns+` FROM room_memberships
		WHERE room_id = $1 AND user_id = $2`, roomID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get membership: %w", err)
	}
	return m, nil
}

func (s *membershipStore) ListLargeRooms(ctx context.Context, userID string) ([]string, error) {
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `
		SELECT r.id FROM rooms r
		JOIN room_memberships m ON m.room_id = r.id
		WHERE m.user_id = $1 AND m.state = 'join' AND r.member_count >= `+largeRoomThresholdSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list large rooms: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: scan large room: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *membershipStore) ListByUser(ctx context.Context, userID, afterRoomID string, limit int) ([]*domain.Membership, error) {
	rows, err := querierFrom(ctx, s.pool).Query(ctx, `
		SELECT `+membershipColumns+` FROM room_memberships
		WHERE user_id = $1 AND room_id > $2
		ORDER BY room_id LIMIT $3`, userID, afterRoomID, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list memberships by user: %w", err)
	}
	return scanMemberships(rows)
}

func (s *membershipStore) ListByRoom(ctx context.Context, roomID string) ([]*domain.Membership, error) {
	rows, err := querierFrom(ctx, s.pool).Query(ctx, `
		SELECT `+membershipColumns+` FROM room_memberships
		WHERE room_id = $1 ORDER BY updated_at`, roomID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list room memberships: %w", err)
	}
	return scanMemberships(rows)
}
