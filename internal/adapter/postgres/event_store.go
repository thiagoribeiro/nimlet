package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type eventStore struct {
	pool *pgxpool.Pool
}

func NewEventStore(pool *pgxpool.Pool) out.EventStore {
	return &eventStore{pool: pool}
}

// Stream-position allocation (see migration 00014_stream_inflight.sql).
const (
	// streamAllocLockKey serialises allocations so they become visible in stream_pos order.
	streamAllocLockKey int64 = 0x6e696d6c65740001 // "nimlet" + 1
	// inflightWriteDeadline: a send whose allocation is older than this aborts instead of
	// committing, so readers can safely stop waiting for it after inflightAbandonAfter.
	inflightWriteDeadline = "5 seconds"
	// inflightAbandonAfter: readers ignore allocations older than this (crashed writer).
	inflightAbandonAfter = "30 seconds"
)

// largeRoomThresholdSQL inlines domain.LargeRoomThreshold into queries as a literal so the
// planner can use the partial index idx_rooms_large (WHERE member_count >= 1000).
var largeRoomThresholdSQL = strconv.Itoa(domain.LargeRoomThreshold)

// readCeilingCTE yields the highest stream_pos a reader may return or advance its cursor to:
// just below the lowest live in-flight allocation. Evaluated in the reader's own snapshot.
const readCeilingCTE = `ceiling AS (
		SELECT COALESCE(min(pos) - 1, 9223372036854775807) AS pos
		FROM stream_inflight
		WHERE allocated_at > clock_timestamp() - interval '` + inflightAbandonAfter + `')`

// Create inserts an event, its idempotency record and its visibility rows.
//
// Ordering guarantee: /sync clients follow a cursor (the last stream_pos they saw), so an
// event must never become visible after an event with a higher stream_pos. Two steps:
//
//  1. allocateStreamPos: nextval() + an in-flight marker, in a short transaction holding an
//     advisory lock, so allocations are visible in position order.
//  2. One statement inserts event_idempotency, events and event_visibility (from the room's
//     joined members) and deletes the marker, so all of it commits atomically in a single
//     round trip. If txn_key was already
//     claimed (idempotent re-send) nothing is inserted, the position becomes a harmless gap
//     and the original event is returned.
//
// Readers cap results below the lowest in-flight position (readCeilingCTE).
func (s *eventStore) Create(ctx context.Context, e *domain.Event, fanOut bool) (*domain.Event, []string, error) {
	streamPos, roomSeq, err := s.allocateStreamPos(ctx, e.RoomID)
	if err != nil {
		return nil, nil, err
	}

	q := querierFrom(ctx, s.pool)
	var inserted bool
	var recipients []string
	// Visibility rows come straight from room_memberships inside the statement: the member
	// list never travels to the application and back, and it is exactly the membership
	// snapshot the event is written against (no join can slip in between a list and the insert).
	err = q.QueryRow(ctx, `
		WITH idem AS (
			INSERT INTO event_idempotency (txn_key, stream_pos, event_id, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (txn_key) DO NOTHING
			RETURNING stream_pos
		), ev AS (
			INSERT INTO events (stream_pos, id, room_id, sender_id, event_type, content, txn_key, created_at, room_seq)
			SELECT stream_pos, $3, $5, $6, $7, $8, $1, $9, $11 FROM idem
			RETURNING stream_pos
		), vis AS (
			INSERT INTO event_visibility (user_id, stream_pos)
			SELECT m.user_id, ev.stream_pos
			FROM ev
			JOIN room_memberships m ON m.room_id = $5 AND m.state = 'join'
			WHERE $10
			ON CONFLICT DO NOTHING
			RETURNING user_id
		), done AS (
			DELETE FROM stream_inflight
			WHERE pos = $2 AND allocated_at > clock_timestamp() - interval '`+inflightWriteDeadline+`'
			RETURNING pos
		)
		SELECT nimlet_assert(EXISTS (SELECT 1 FROM done), 'stream position allocation expired'),
		       EXISTS (SELECT 1 FROM idem),
		       ARRAY(SELECT user_id FROM vis)`,
		e.TxnKey, streamPos, e.ID, time.Now().UTC(),
		e.RoomID, e.SenderID, e.EventType, []byte(e.Content), e.CreatedAt,
		fanOut, roomSeq,
	).Scan(new(bool), &inserted, &recipients)
	if err != nil {
		s.releaseStreamPos(ctx, streamPos)
		return nil, nil, fmt.Errorf("postgres: insert event: %w", err)
	}
	if !inserted {
		// txn_key already claimed by an earlier send: return the original event.
		orig, err := s.getByTxnKey(ctx, e.TxnKey)
		return orig, nil, err
	}
	e.StreamPos = streamPos
	e.RoomSeq = roomSeq
	return e, recipients, nil
}

// allocateStreamPos reserves the next stream position and the room's next room_seq, and
// marks the position in flight. It always runs on its own connection and commits
// immediately, even when ctx carries a transaction: the marker must be visible to readers
// before the event can be. Both numbers come from the same serialised transaction, so within
// a room room_seq follows stream_pos order exactly.
func (s *eventStore) allocateStreamPos(ctx context.Context, roomID string) (pos, roomSeq int64, err error) {
	batch := &pgx.Batch{}
	batch.Queue(`BEGIN`)
	// The marker needs no durability: a crash discards every in-flight send with it. The
	// room counter is flushed with the event's own (synchronous) commit, which comes later
	// in the WAL; an increment lost in a crash was never used by a committed event.
	batch.Queue(`SET LOCAL synchronous_commit TO off`)
	batch.Queue(`SELECT pg_advisory_xact_lock($1)`, streamAllocLockKey)
	batch.Queue(`INSERT INTO stream_inflight (pos) VALUES (nextval('events_stream_pos_seq')) RETURNING pos`)
	batch.Queue(`
		INSERT INTO room_seqs (room_id, last_seq) VALUES ($1, 1)
		ON CONFLICT (room_id) DO UPDATE SET last_seq = room_seqs.last_seq + 1
		RETURNING last_seq`, roomID)
	batch.Queue(`COMMIT`)

	br := s.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()
	for i := 0; i < batch.Len(); i++ {
		switch i {
		case 3:
			err = br.QueryRow().Scan(&pos)
		case 4:
			err = br.QueryRow().Scan(&roomSeq)
			if isForeignKeyViolation(err) {
				return 0, 0, fmt.Errorf("allocate stream_pos: room %q: %w", roomID, domain.ErrNotFound)
			}
		default:
			_, err = br.Exec()
		}
		if err != nil {
			return 0, 0, fmt.Errorf("postgres: allocate stream_pos: %w", err)
		}
	}
	return pos, roomSeq, nil
}

// releaseStreamPos drops an in-flight marker after a failed write so readers stop waiting
// for it. Best effort: an orphaned marker is ignored after inflightAbandonAfter anyway.
func (s *eventStore) releaseStreamPos(ctx context.Context, pos int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, `DELETE FROM stream_inflight WHERE pos = $1`, pos); err != nil {
		slog.Warn("postgres: release stream_pos", "pos", pos, "error", err)
	}
}

// SweepStreamInflight deletes in-flight markers abandoned by crashed writers. Readers
// already ignore them; this only keeps the table small.
func SweepStreamInflight(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM stream_inflight
		WHERE allocated_at < clock_timestamp() - interval '`+inflightAbandonAfter+`'`)
	if err != nil {
		return 0, fmt.Errorf("postgres: sweep stream_inflight: %w", err)
	}
	return tag.RowsAffected(), nil
}

// GetVisible is the hot path for /sync: the next page of events for userID after sincePos.
//
// Small rooms and DMs come from event_visibility (PK user_id, stream_pos); large rooms have
// no per-user rows, so they are read straight from events per joined large room (idx
// room_id, stream_pos). The large-room list is resolved here, in the same snapshot as the
// events: a list fetched by an earlier query could miss a just-joined room while the cursor
// moves past its events.
//
// Each source is an ordered index scan limited before merging, so the cost is
// O(limit × sources), not O(history) — and the plan is the same whatever $since is, which
// keeps Postgres' generic plan for this prepared statement fast. (The previous shape, one
// ORDER BY over an unbounded UNION ALL, degraded 10× once the generic plan kicked in; see
// docs/benchmarks.md.) UNION (not ALL) dedupes events of a room that crossed the
// large-room threshold and so appear in both sources. Results never exceed the read ceiling.
func (s *eventStore) GetVisible(ctx context.Context, userID string, sincePos int64, limit int) ([]*domain.Event, error) {
	q := querierFrom(ctx, s.pool)
	rows, err := q.Query(ctx, `
		WITH `+readCeilingCTE+`,
		page AS (
			(SELECT v.stream_pos
			 FROM event_visibility v
			 WHERE v.user_id = $1 AND v.stream_pos > $2
			   AND v.stream_pos <= (SELECT pos FROM ceiling)
			 ORDER BY v.stream_pos
			 LIMIT $3)
			UNION
			(SELECT x.stream_pos
			 FROM room_memberships m
			 JOIN rooms r ON r.id = m.room_id
			 CROSS JOIN LATERAL (
				SELECT e.stream_pos FROM events e
				WHERE e.room_id = m.room_id AND e.stream_pos > $2
				  AND e.stream_pos <= (SELECT pos FROM ceiling)
				ORDER BY e.stream_pos
				LIMIT $3) x
			 WHERE m.user_id = $1 AND m.state = 'join' AND r.member_count >= `+largeRoomThresholdSQL+`)
			ORDER BY 1
			LIMIT $3
		)
		SELECT e.id, e.stream_pos, e.room_seq, e.room_id, e.sender_id, u.display_name,
		       e.event_type, e.content, e.txn_key, e.created_at
		FROM page p
		JOIN events e ON e.stream_pos = p.stream_pos
		JOIN users  u ON u.id = e.sender_id
		ORDER BY e.stream_pos`,
		userID, sincePos, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: get visible events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// GetByRoom pages a room's history by room_seq: after fromSeq (dir "f") or before it
// (dir "b"). Forward pages respect the read ceiling like /sync, so paging forward never
// passes an event that is still being written.
func (s *eventStore) GetByRoom(ctx context.Context, roomID string, fromSeq int64, dir string, limit int) ([]*domain.Event, error) {
	q := querierFrom(ctx, s.pool)

	var query string
	if dir == "b" {
		query = `
			SELECT e.id, e.stream_pos, e.room_seq, e.room_id, e.sender_id, u.display_name,
			       e.event_type, e.content, e.txn_key, e.created_at
			FROM events e
			JOIN users u ON u.id = e.sender_id
			WHERE e.room_id = $1 AND e.room_seq < $2
			ORDER BY e.room_seq DESC
			LIMIT $3`
	} else {
		query = `
			SELECT e.id, e.stream_pos, e.room_seq, e.room_id, e.sender_id, u.display_name,
			       e.event_type, e.content, e.txn_key, e.created_at
			FROM events e
			JOIN users u ON u.id = e.sender_id
			WHERE e.room_id = $1 AND e.room_seq > $2
			  AND e.stream_pos <= (WITH ` + readCeilingCTE + ` SELECT pos FROM ceiling)
			ORDER BY e.room_seq ASC
			LIMIT $3`
	}

	rows, err := q.Query(ctx, query, roomID, fromSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: get events by room: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// CurrentStreamPos reads the last_value of the sequence directly — O(1),
// no full-table scan, partition-transparent.
func (s *eventStore) CurrentStreamPos(ctx context.Context) (int64, error) {
	q := querierFrom(ctx, s.pool)
	var pos int64
	// last_value is the most recently generated value (0 if never called).
	// is_called=false means the sequence was reset and last_value is the start value.
	var isCalled bool
	if err := q.QueryRow(ctx,
		`SELECT last_value, is_called FROM events_stream_pos_seq`,
	).Scan(&pos, &isCalled); err != nil {
		return 0, fmt.Errorf("postgres: current stream pos: %w", err)
	}
	if !isCalled {
		return 0, nil
	}
	return pos, nil
}

func (s *eventStore) getByTxnKey(ctx context.Context, txnKey string) (*domain.Event, error) {
	q := querierFrom(ctx, s.pool)

	// Fetch the stream_pos from the idempotency table, then load the full event.
	var streamPos int64
	var eventID string
	err := q.QueryRow(ctx,
		`SELECT stream_pos, event_id FROM event_idempotency WHERE txn_key = $1`,
		txnKey,
	).Scan(&streamPos, &eventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get idempotency record: %w", err)
	}

	e := &domain.Event{}
	err = q.QueryRow(ctx, `
		SELECT id, stream_pos, room_seq, room_id, sender_id, event_type, content, txn_key, created_at
		FROM events WHERE stream_pos = $1`, streamPos).
		Scan(&e.ID, &e.StreamPos, &e.RoomSeq, &e.RoomID, &e.SenderID, &e.EventType, &e.Content, &e.TxnKey, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get event by stream_pos: %w", err)
	}
	return e, nil
}

func scanEvents(rows pgx.Rows) ([]*domain.Event, error) {
	var events []*domain.Event
	for rows.Next() {
		e := &domain.Event{}
		if err := rows.Scan(
			&e.ID, &e.StreamPos, &e.RoomSeq, &e.RoomID, &e.SenderID, &e.SenderDisplayName,
			&e.EventType, &e.Content, &e.TxnKey, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("postgres: scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *eventStore) RevokeVisibility(ctx context.Context, userID, roomID string) (int64, error) {
	q := querierFrom(ctx, s.pool)
	tag, err := q.Exec(ctx, `
		DELETE FROM event_visibility v
		USING events e
		WHERE v.user_id = $1 AND v.stream_pos = e.stream_pos AND e.room_id = $2`, userID, roomID)
	if err != nil {
		return 0, fmt.Errorf("postgres: revoke visibility: %w", err)
	}
	return tag.RowsAffected(), nil
}

// EnsureEventPartitions keeps stream_pos partitions created ahead of the current position
// (see migration 00018) and reports whether anything has landed in the DEFAULT partitions,
// which means the range ran out before maintenance caught up and needs the operator runbook.
func EnsureEventPartitions(ctx context.Context, pool *pgxpool.Pool) (created int, defaultInUse bool, err error) {
	if err := pool.QueryRow(ctx, `SELECT ensure_event_partitions()`).Scan(&created); err != nil {
		return 0, false, fmt.Errorf("postgres: ensure event partitions: %w", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM events_default)
		    OR EXISTS (SELECT 1 FROM event_visibility_default)`).Scan(&defaultInUse); err != nil {
		return created, false, fmt.Errorf("postgres: check default partitions: %w", err)
	}
	return created, defaultInUse, nil
}
