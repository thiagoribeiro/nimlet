package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

type visibleReq struct {
	userID   string
	sincePos int64
	limit    int
	done     chan visibleRes // buffered: a worker never blocks on a caller that gave up
}

type visibleRes struct {
	events []*domain.Event
	held   bool // see waitPastCeiling
	err    error
}

// GetVisible runs in a group with the other /sync queries waiting at the same moment.
func (s *batchedEventStore) GetVisible(ctx context.Context, userID string, sincePos int64, limit int) ([]*domain.Event, error) {
	if _, inTx := querierFrom(ctx, s.pool).(pgx.Tx); inTx || s.reads == nil {
		return s.eventStore.GetVisible(ctx, userID, sincePos, limit)
	}
	return waitPastCeiling(ctx, func() ([]*domain.Event, bool, error) {
		req := visibleReq{userID: userID, sincePos: sincePos, limit: limit, done: make(chan visibleRes, 1)}
		if handed, err := s.reads.submit(ctx, req); err != nil {
			return nil, false, err
		} else if !handed {
			return s.getVisibleOnce(ctx, userID, sincePos, limit)
		}
		select {
		case res := <-req.done:
			return res.events, res.held, res.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	})
}

func (s *batchedEventStore) readGroup(group []visibleReq) {
	ctx, cancel := context.WithTimeout(context.Background(), batchWriteTimeout)
	defer cancel()
	if len(group) > 1 {
		pages, held, err := s.getVisibleGroup(ctx, group)
		if err == nil {
			for i, r := range group {
				r.done <- visibleRes{events: pages[i], held: held[i]}
			}
			return
		}
		// Not expected: the fallback below is correct but runs the group one query at a
		// time on this worker, which throttles every /sync behind it.
		slog.Error("postgres: grouped GetVisible failed, running queries one by one", "group", len(group), "error", err)
	}
	// One request, or the group failed: run each on its own so every caller gets its own
	// answer (and its own error).
	for _, r := range group {
		events, held, err := s.getVisibleOnce(ctx, r.userID, r.sincePos, r.limit)
		r.done <- visibleRes{events: events, held: held, err: err}
	}
}

// getVisibleGroup is GetVisible for several (user, since) pairs in one statement: the same
// per-user page under a LATERAL join, in one snapshot with one read ceiling. That shares
// what dominates a caught-up /sync query, the per-statement work: snapshot, plan, and the
// locks on every events / event_visibility partition it may touch.
func (s *batchedEventStore) getVisibleGroup(ctx context.Context, group []visibleReq) ([][]*domain.Event, []bool, error) {
	users := make([]string, len(group))
	since := make([]int64, len(group))
	limits := make([]int32, len(group))
	for i, r := range group {
		users[i], since[i], limits[i] = r.userID, r.sincePos, int32(r.limit)
	}
	rows, err := s.pool.Query(ctx, `
		WITH `+readCeilingCTE+`,
		req AS (
			SELECT * FROM unnest($1::text[], $2::bigint[], $3::int[]) WITH ORDINALITY AS r(user_id, since, lim, idx)
		),
		page AS (
			SELECT r.idx, x.stream_pos
			FROM req r
			CROSS JOIN LATERAL (
				(SELECT v.stream_pos
				 FROM event_visibility v
				 WHERE v.user_id = r.user_id AND v.stream_pos > r.since
				   AND v.stream_pos <= (SELECT pos FROM ceiling)
				 ORDER BY v.stream_pos
				 LIMIT r.lim)
				UNION
				(SELECT y.stream_pos
				 FROM room_memberships m
				 JOIN rooms rm ON rm.id = m.room_id
				 CROSS JOIN LATERAL (
					SELECT e.stream_pos FROM events e
					WHERE e.room_id = m.room_id AND e.stream_pos > r.since
					  AND e.stream_pos <= (SELECT pos FROM ceiling)
					ORDER BY e.stream_pos
					LIMIT r.lim) y
				 WHERE m.user_id = r.user_id AND m.state = 'join' AND rm.member_count >= `+largeRoomThresholdSQL+`)
				ORDER BY 1
				LIMIT r.lim
			) x
		),
		held AS (
			SELECT r.idx, NOT EXISTS (SELECT 1 FROM page p WHERE p.idx = r.idx) AND `+heldSQL("r.user_id")+` AS held
			FROM req r
		)
		-- Three kinds of rows: each request's held flag (0), each page entry as (request,
		-- position) (1), and every distinct event once (2). Members of a room woken together
		-- share one copy of its events instead of each fetching, sending and decoding theirs.
		SELECT 0 AS kind, h.idx, h.held, 0::bigint AS pos, `+zeroEventColumns+`
		FROM held h
		UNION ALL
		SELECT 1, p.idx, false, p.stream_pos, `+zeroEventColumns+`
		FROM page p
		UNION ALL
		SELECT 2, 0, false, e.stream_pos, e.id, e.stream_pos, e.room_seq, e.room_id, e.sender_id,
		       u.display_name, e.event_type, e.content, e.txn_key, e.created_at
		FROM (SELECT DISTINCT stream_pos FROM page) d
		JOIN events e ON e.stream_pos = d.stream_pos
		JOIN users  u ON u.id = e.sender_id
		ORDER BY 1, 2, 4`,
		users, since, limits,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: get visible events (group): %w", err)
	}
	defer rows.Close()
	type entry struct{ idx, pos int64 }
	var entries []entry // in (request, position) order
	events := make(map[int64]*domain.Event)
	held := make([]bool, len(group))
	for rows.Next() {
		var kind, idx, pos int64
		var h bool
		e := &domain.Event{}
		if err := rows.Scan(
			&kind, &idx, &h, &pos, &e.ID, &e.StreamPos, &e.RoomSeq, &e.RoomID, &e.SenderID, &e.SenderDisplayName,
			&e.EventType, &e.Content, &e.TxnKey, &e.CreatedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("postgres: scan event (group): %w", err)
		}
		switch kind {
		case 0:
			held[idx-1] = h
		case 1:
			entries = append(entries, entry{idx, pos})
		case 2:
			events[pos] = e
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Pages share *domain.Event values between users: callers only read them.
	pages := make([][]*domain.Event, len(group))
	for _, en := range entries {
		e, ok := events[en.pos]
		if !ok {
			return nil, nil, fmt.Errorf("postgres: get visible events (group): event %d missing", en.pos)
		}
		pages[en.idx-1] = append(pages[en.idx-1], e)
	}
	return pages, held, nil
}
