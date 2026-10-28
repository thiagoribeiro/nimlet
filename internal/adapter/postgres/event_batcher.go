package postgres

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// BatchConfig controls the grouping of NewBatchedEventStore.
type BatchConfig struct {
	// Writes groups sends into one transaction (group commit).
	Writes GroupLimits
	// Reads groups concurrent /sync queries (GetVisible) into one statement.
	Reads GroupLimits
	// SingleTx writes each batch in one transaction holding the allocation lock until its
	// commit (writeBatchSingleTx) instead of the two-step protocol.
	SingleTx bool
}

// batchWriteTimeout bounds one group's database work; it serves many requests, so it can't
// use any single caller's context.
const batchWriteTimeout = 10 * time.Second

type createReq struct {
	e      *domain.Event
	fanOut bool
	done   chan createRes // buffered: a worker never blocks on a caller that gave up
}

type createRes struct {
	e          *domain.Event
	recipients []string
	err        error
}

// batchedEventStore is the event store with Create and GetVisible run in groups.
//
// Writes: each send costs an advisory-lock round trip for its position and a synchronous
// commit (a WAL flush) for its insert; at a few hundred sends per second those dominate
// Postgres. Write workers take the queued sends and write them with the same two-step
// protocol as eventStore.Create, once per batch: one allocation transaction for all
// positions, then one statement that inserts every event, idempotency record and
// visibility row and clears the in-flight markers, under a single commit. Ordering is
// unchanged: positions are allocated under the lock and stay in flight until that commit.
//
// Reads: a send to a room of N members wakes N long-polls at once, and each would run its
// own GetVisible. Read workers run the waiting ones as one statement (getVisibleGroup).
type batchedEventStore struct {
	*eventStore
	writes   *groupQueue[createReq]  // nil: writes aren't grouped
	reads    *groupQueue[visibleReq] // nil: reads aren't grouped
	singleTx bool
}

// NewBatchedEventStore starts the group workers. Close stops them once the groups in
// flight are done; calls after that (or inside a transaction) take the ungrouped path.
func NewBatchedEventStore(pool *pgxpool.Pool, cfg BatchConfig) interface {
	out.EventStore
	Close()
} {
	s := &batchedEventStore{eventStore: &eventStore{pool: pool}, singleTx: cfg.SingleTx}
	// An ungrouped path must keep its unbounded concurrency, not run through Workers.
	if cfg.Writes.MaxItems > 1 {
		s.writes = newGroupQueue(cfg.Writes, s.write)
	}
	if cfg.Reads.MaxItems > 1 {
		s.reads = newGroupQueue(cfg.Reads, s.readGroup)
	}
	return s
}

func (s *batchedEventStore) Close() {
	if s.writes != nil {
		s.writes.close()
	}
	if s.reads != nil {
		s.reads.close()
	}
}

func (s *batchedEventStore) Create(ctx context.Context, e *domain.Event, fanOut bool) (*domain.Event, []string, error) {
	// Inside a transaction the event must commit with it, not with a batch.
	if _, inTx := querierFrom(ctx, s.pool).(pgx.Tx); inTx || s.writes == nil {
		return s.eventStore.Create(ctx, e, fanOut)
	}
	req := createReq{e: e, fanOut: fanOut, done: make(chan createRes, 1)}
	if handed, err := s.writes.submit(ctx, req); err != nil {
		return nil, nil, err
	} else if !handed {
		return s.eventStore.Create(ctx, e, fanOut)
	}
	// Once handed over the write happens regardless; a caller that gives up early just
	// doesn't learn the result (a retry with the same txn_key returns the stored event).
	select {
	case res := <-req.done:
		return res.e, res.recipients, res.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (s *batchedEventStore) write(batch []createReq) {
	ctx, cancel := context.WithTimeout(context.Background(), batchWriteTimeout)
	defer cancel()

	// A txn_key twice in one batch would conflict with itself in one statement; the later
	// copies go through the single path afterwards and find the stored original.
	seen := make(map[string]struct{}, len(batch))
	var repeats []createReq
	unique := batch[:0:0]
	for _, r := range batch {
		if _, dup := seen[r.e.TxnKey]; dup {
			repeats = append(repeats, r)
			continue
		}
		seen[r.e.TxnKey] = struct{}{}
		unique = append(unique, r)
	}

	if len(unique) == 1 {
		s.writeOne(ctx, unique[0])
	} else if err := s.writeBatch(ctx, unique); err != nil {
		// One bad event (a room deleted meanwhile, ...) must not fail its neighbours:
		// redo each on its own, which also reports the right error to the right caller.
		for _, r := range unique {
			s.writeOne(ctx, r)
		}
	}
	for _, r := range repeats {
		s.writeOne(ctx, r)
	}
}

func (s *batchedEventStore) writeOne(ctx context.Context, r createReq) {
	e, recipients, err := s.eventStore.Create(ctx, r.e, r.fanOut)
	r.done <- createRes{e: e, recipients: recipients, err: err}
}

// writeBatch writes every event of batch or none. On error no caller has been answered.
func (s *batchedEventStore) writeBatch(ctx context.Context, batch []createReq) error {
	if s.singleTx {
		return s.writeBatchSingleTx(ctx, batch)
	}
	rooms := batchRooms(batch)
	positions, seqs, err := s.allocateStreamPositions(ctx, rooms)
	if err != nil {
		return err
	}
	sql, args := insertBatchSQL(batch, positions, seqs, true)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		s.releaseStreamPositions(ctx, positions)
		return fmt.Errorf("postgres: insert event batch: %w", err)
	}
	results, err := collectBatchResults(rows, len(batch))
	if err != nil {
		s.releaseStreamPositions(ctx, positions)
		return err
	}
	s.answer(ctx, batch, positions, seqs, results)
	return nil
}

// writeBatchSingleTx writes a batch in one transaction that takes the allocation lock and
// keeps it until the commit. Commits then happen in position order by construction, so the
// batch needs no stream_inflight markers; readers still honour the markers of the
// unbatched path, which may run alongside. The price: the lock is held through the insert
// and its WAL flush, so batches of all instances insert one at a time.
//
// Two round trips, like the split path: BEGIN + lock + positions + room_seqs, then the
// insert + COMMIT. A COMMIT after a failed insert just rolls back.
func (s *batchedEventStore) writeBatchSingleTx(ctx context.Context, batch []createReq) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: insert event batch: %w", err)
	}
	// A connection left inside a transaction is closed by the pool instead of reused.
	defer conn.Release()

	rooms := batchRooms(batch)
	uniqRooms, incs, counts := roomCounts(rooms)
	alloc := &pgx.Batch{}
	alloc.Queue(`BEGIN`)
	alloc.Queue(`SELECT pg_advisory_xact_lock($1)`, streamAllocLockKey)
	alloc.Queue(`SELECT nextval('events_stream_pos_seq') FROM generate_series(1, $1::int)`, len(rooms))
	alloc.Queue(roomSeqsSQL, uniqRooms, incs)
	var positions []int64
	lastSeq := make(map[string]int64, len(uniqRooms))
	if err := readAllocation(conn.SendBatch(ctx, alloc), &positions, lastSeq); err != nil {
		_, _ = conn.Exec(ctx, `ROLLBACK`)
		return fmt.Errorf("postgres: allocate stream_pos batch: %w", err)
	}
	slices.SortFunc(positions, cmp.Compare[int64])
	seqs := assignRoomSeqs(rooms, counts, lastSeq)

	sql, args := insertBatchSQL(batch, positions, seqs, false)
	write := &pgx.Batch{}
	write.Queue(sql, args...)
	write.Queue(`COMMIT`)
	br := conn.SendBatch(ctx, write)
	rows, err := br.Query()
	var results map[int64]batchOutcome
	if err == nil {
		results, err = collectBatchResults(rows, len(batch))
	}
	if err == nil {
		_, err = br.Exec()
	}
	if cerr := br.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("postgres: insert event batch: %w", err)
	}
	s.answer(ctx, batch, positions, seqs, results)
	return nil
}

func readAllocation(br pgx.BatchResults, positions *[]int64, lastSeq map[string]int64) (err error) {
	defer func() {
		if cerr := br.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err = br.Exec(); err != nil { // BEGIN
		return err
	}
	if _, err = br.Exec(); err != nil { // lock
		return err
	}
	if *positions, err = collectInt64(br); err != nil {
		return err
	}
	return collectRoomSeqs(br, lastSeq)
}

type batchOutcome struct {
	inserted   bool
	recipients []string
}

func batchRooms(batch []createReq) []string {
	rooms := make([]string, len(batch))
	for i, r := range batch {
		rooms[i] = r.e.RoomID
	}
	return rooms
}

// insertBatchSQL is the statement of eventStore.Create over arrays: unnest keeps one row per
// event, and the visibility rows of all small-room events come from one membership join.
// clearInflight also deletes the batch's stream_inflight markers, asserting that none
// expired (split path).
func insertBatchSQL(batch []createReq, positions, seqs []int64, clearInflight bool) (string, []any) {
	n := len(batch)
	txnKeys, ids, roomIDs, senders, types, contents := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	createdAt := make([]time.Time, n)
	fanOut := make([]bool, n)
	for i, r := range batch {
		e := r.e
		txnKeys[i], ids[i], roomIDs[i], senders[i], types[i], contents[i] = e.TxnKey, e.ID, e.RoomID, e.SenderID, e.EventType, string(e.Content)
		createdAt[i], fanOut[i] = e.CreatedAt, r.fanOut
	}
	inflight := `, checked AS (SELECT true AS ok)`
	if clearInflight {
		inflight = `, done AS (
			DELETE FROM stream_inflight
			WHERE pos = ANY($2) AND allocated_at > ` + inflightLiveSince(inflightWriteDeadline) + `
			RETURNING pos
		), checked AS (
			SELECT nimlet_assert((SELECT count(*) FROM done) = cardinality($2::bigint[]),
			                     'stream position allocation expired') AS ok
		)`
	}
	sql := `
		WITH input AS (
			SELECT * FROM unnest($1::text[], $2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[],
			                     $7::text[], $8::timestamptz[], $9::bool[], $10::bigint[])
				AS t(txn_key, stream_pos, id, room_id, sender_id, event_type, content, created_at, fan_out, room_seq)
		), idem AS (
			INSERT INTO event_idempotency (txn_key, stream_pos, event_id, created_at)
			SELECT txn_key, stream_pos, id, $11 FROM input
			ON CONFLICT (txn_key) DO NOTHING
			RETURNING stream_pos
		), ev AS (
			INSERT INTO events (stream_pos, id, room_id, sender_id, event_type, content, txn_key, created_at, room_seq)
			SELECT i.stream_pos, i.id, i.room_id, i.sender_id, i.event_type, i.content::jsonb, i.txn_key, i.created_at, i.room_seq
			FROM input i JOIN idem USING (stream_pos)
			RETURNING stream_pos, room_id
		), vis AS (
			INSERT INTO event_visibility (user_id, stream_pos)
			SELECT m.user_id, ev.stream_pos
			FROM ev
			JOIN input i USING (stream_pos)
			JOIN room_memberships m ON m.room_id = ev.room_id AND m.state = 'join'
			WHERE i.fan_out
			-- No ON CONFLICT, as in eventStore.Create: duplicates are impossible.
			RETURNING user_id, stream_pos
		)` + inflight + `
		SELECT i.stream_pos, idem.stream_pos IS NOT NULL, v.users
		FROM input i
		CROSS JOIN checked
		LEFT JOIN idem USING (stream_pos)
		LEFT JOIN (SELECT stream_pos, array_agg(user_id) AS users FROM vis GROUP BY stream_pos) v USING (stream_pos)`
	return sql, []any{txnKeys, positions, ids, roomIDs, senders, types, contents, createdAt, fanOut, seqs, time.Now().UTC()}
}

// collectBatchResults reads the insert's rows (one per event) and closes them.
func collectBatchResults(rows pgx.Rows, n int) (map[int64]batchOutcome, error) {
	defer rows.Close()
	results := make(map[int64]batchOutcome, n)
	for rows.Next() {
		var pos int64
		var o batchOutcome
		if err := rows.Scan(&pos, &o.inserted, &o.recipients); err != nil {
			return nil, fmt.Errorf("postgres: insert event batch: %w", err)
		}
		results[pos] = o
	}
	if err := rows.Err(); err != nil || len(results) != n {
		return nil, fmt.Errorf("postgres: insert event batch: %d of %d rows: %w", len(results), n, err)
	}
	return results, nil
}

// answer replies to every caller of a committed batch.
func (s *batchedEventStore) answer(ctx context.Context, batch []createReq, positions, seqs []int64, results map[int64]batchOutcome) {
	for i, r := range batch {
		o := results[positions[i]]
		if !o.inserted {
			// txn_key claimed by an earlier send: return the original event.
			orig, err := s.getByTxnKey(ctx, r.e.TxnKey)
			r.done <- createRes{e: orig, err: err}
			continue
		}
		r.e.StreamPos, r.e.RoomSeq = positions[i], seqs[i]
		if o.recipients == nil {
			o.recipients = []string{}
		}
		r.done <- createRes{e: r.e, recipients: o.recipients}
	}
}

// One counter update per room: ON CONFLICT can't touch a row twice in one statement.
const roomSeqsSQL = `
	INSERT INTO room_seqs (room_id, last_seq)
	SELECT * FROM unnest($1::text[], $2::bigint[])
	ON CONFLICT (room_id) DO UPDATE SET last_seq = room_seqs.last_seq + EXCLUDED.last_seq
	RETURNING room_id, last_seq`

// roomCounts returns the distinct rooms of a batch, in first-seen order, with the number of
// events of each (incs, aligned with uniqRooms, and counts, by room).
func roomCounts(rooms []string) (uniqRooms []string, incs []int64, counts map[string]int64) {
	counts = make(map[string]int64)
	for _, r := range rooms {
		if counts[r] == 0 {
			uniqRooms = append(uniqRooms, r)
		}
		counts[r]++
	}
	incs = make([]int64, len(uniqRooms))
	for i, r := range uniqRooms {
		incs[i] = counts[r]
	}
	return uniqRooms, incs, counts
}

// assignRoomSeqs hands out each room's range of room_seqs in batch order. lastSeq is each
// room's counter after the batch. Events of the same room get consecutive room_seqs in
// batch order, the same order as their positions.
func assignRoomSeqs(rooms []string, counts, lastSeq map[string]int64) []int64 {
	next := make(map[string]int64, len(counts))
	for r, last := range lastSeq {
		next[r] = last - counts[r] + 1
	}
	seqs := make([]int64, len(rooms))
	for i, r := range rooms {
		seqs[i] = next[r]
		next[r]++
	}
	return seqs
}

// allocateStreamPositions is allocateStreamPos for a batch: len(rooms) positions in
// ascending order and, per event, its room's next room_seq, in one transaction under the
// allocation lock.
func (s *batchedEventStore) allocateStreamPositions(ctx context.Context, rooms []string) (positions, seqs []int64, err error) {
	uniqRooms, incs, counts := roomCounts(rooms)

	batch := &pgx.Batch{}
	batch.Queue(`BEGIN`)
	// Durability as in allocateStreamPos: the markers need none, and the counters are
	// flushed by the batch's own synchronous commit, later in the WAL.
	batch.Queue(`SET LOCAL synchronous_commit TO off`)
	batch.Queue(`SELECT pg_advisory_xact_lock($1)`, streamAllocLockKey)
	batch.Queue(`INSERT INTO stream_inflight (pos)
		SELECT nextval('events_stream_pos_seq') FROM generate_series(1, $1::int)
		RETURNING pos`, len(rooms))
	batch.Queue(roomSeqsSQL, uniqRooms, incs)
	batch.Queue(`COMMIT`)

	lastSeq := make(map[string]int64, len(uniqRooms))
	br := s.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()
	for i := 0; i < batch.Len(); i++ {
		switch i {
		case 3:
			positions, err = collectInt64(br)
		case 4:
			err = collectRoomSeqs(br, lastSeq)
		default:
			_, err = br.Exec()
		}
		if err != nil {
			// The transaction aborted, so no marker was committed and none needs releasing.
			return nil, nil, fmt.Errorf("postgres: allocate stream_pos batch: %w", err)
		}
	}
	slices.SortFunc(positions, cmp.Compare[int64])
	return positions, assignRoomSeqs(rooms, counts, lastSeq), nil
}

func collectInt64(br pgx.BatchResults) ([]int64, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func collectRoomSeqs(br pgx.BatchResults, into map[string]int64) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var room string
		var last int64
		if err := rows.Scan(&room, &last); err != nil {
			return err
		}
		into[room] = last
	}
	return rows.Err()
}

// releaseStreamPositions drops the in-flight markers of a batch that failed to commit.
func (s *batchedEventStore) releaseStreamPositions(ctx context.Context, positions []int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Best effort, as releaseStreamPos: readers ignore orphans after inflightAbandonAfter.
	if _, err := s.pool.Exec(ctx, `DELETE FROM stream_inflight WHERE pos = ANY($1)`, positions); err != nil {
		slog.Warn("postgres: release stream_pos batch", "positions", len(positions), "error", err)
	}
}
