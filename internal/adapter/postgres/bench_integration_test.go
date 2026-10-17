//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Benchmark fixture sizes. The fixture is built once per test binary with bulk SQL and
// shared by every benchmark; results in docs/benchmarks.md assume these values.
const (
	benchUsers             = 5000
	benchSmallRooms        = 40
	benchSmallRoomMembers  = 50
	benchSmallRoomEvents   = 500 // per small room → 20k events, 1M visibility rows
	benchLargeRooms        = 50
	benchLargeRoomEvents   = 1000 // per large room → 50k events, no visibility rows
	benchLargeRoomMemberCt = 1000
)

type benchWorld struct {
	pool *pgxpool.Pool
	// readers[k] is a user joined to all small rooms' worth of history plus k large rooms.
	readers map[int]string
	// smallRooms[n] is a room with exactly n joined members (members listed in roomMembers).
	smallRooms  map[int]string
	roomMembers map[string][]string
	largeRoom   string // write target for Create benchmarks; no reader is a member
	senderID    string
	token       string
}

var (
	benchOnce  sync.Once
	benchState *benchWorld
	benchErr   error
)

func benchFixture(b *testing.B) *benchWorld {
	b.Helper()
	pool := testPool(b)
	benchOnce.Do(func() { benchState, benchErr = buildBenchWorld(context.Background(), pool) })
	if benchErr != nil {
		b.Fatalf("bench fixture: %v", benchErr)
	}
	benchState.pool = pool
	return benchState
}

func buildBenchWorld(ctx context.Context, pool *pgxpool.Pool) (*benchWorld, error) {
	w := &benchWorld{
		readers:     map[int]string{},
		smallRooms:  map[int]string{},
		roomMembers: map[string][]string{},
	}
	prefix := "bench_" + uuid.NewString()[:8] + "_"

	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}

	// Users: prefix || n, ids are deterministic within this run.
	if err := exec(`
		INSERT INTO users (id, namespace_id, username, display_name, type, password_hash)
		SELECT $1 || n, '00000000-0000-0000-0000-000000000000', $1 || n, 'Bench ' || n, 'human', '$argon2id$bench'
		FROM generate_series(0, $2 - 1) n`, prefix, benchUsers); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	user := func(n int) string { return fmt.Sprintf("%s%d", prefix, n) }
	w.senderID = user(0)

	// Small rooms with history: room i has members [i*members, (i+1)*members), plus the
	// readers (users 4900..4902) so they see every small room's history.
	readerBase := benchUsers - 100
	for k, idx := range []int{0, 5, 50} {
		w.readers[idx] = user(readerBase + k)
	}
	smallIDs := make([]string, benchSmallRooms)
	for i := range smallIDs {
		smallIDs[i] = prefix + fmt.Sprintf("small_%d", i)
	}
	if err := exec(`
		INSERT INTO rooms (id, namespace_id, name, creator_id, member_count)
		SELECT r, '00000000-0000-0000-0000-000000000000', r, $2, $3 FROM unnest($1::text[]) r`, smallIDs, w.senderID, benchSmallRoomMembers+3); err != nil {
		return nil, fmt.Errorf("small rooms: %w", err)
	}
	if err := exec(`
		INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role)
		SELECT '00000000-0000-0000-0000-000000000000', $1 || 'small_' || r, $1 || (r * $2 + m), 'join', 'member'
		FROM generate_series(0, $3 - 1) r, generate_series(0, $2 - 1) m`,
		prefix, benchSmallRoomMembers, benchSmallRooms); err != nil {
		return nil, fmt.Errorf("small memberships: %w", err)
	}
	readerIDs := []string{w.readers[0], w.readers[5], w.readers[50]}
	if err := exec(`
		INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role)
		SELECT '00000000-0000-0000-0000-000000000000', r, u, 'join', 'member' FROM unnest($1::text[]) r, unnest($2::text[]) u`,
		smallIDs, readerIDs); err != nil {
		return nil, fmt.Errorf("reader memberships: %w", err)
	}
	if err := exec(`
		INSERT INTO events (stream_pos, id, room_id, sender_id, event_type, content, txn_key)
		SELECT nextval('events_stream_pos_seq'), gen_random_uuid()::text, r, $2,
		       'm.room.message', '{"body":"bench history message"}', gen_random_uuid()::text
		FROM unnest($1::text[]) r, generate_series(1, $3)`,
		smallIDs, w.senderID, benchSmallRoomEvents); err != nil {
		return nil, fmt.Errorf("small events: %w", err)
	}
	if err := exec(`
		INSERT INTO event_visibility (user_id, stream_pos)
		SELECT m.user_id, e.stream_pos
		FROM events e JOIN room_memberships m ON m.room_id = e.room_id AND m.state = 'join'
		WHERE e.room_id = ANY($1)`, smallIDs); err != nil {
		return nil, fmt.Errorf("visibility: %w", err)
	}

	// Large rooms: member_count is set directly; only the readers have membership rows.
	largeIDs := make([]string, benchLargeRooms)
	for i := range largeIDs {
		largeIDs[i] = prefix + fmt.Sprintf("large_%d", i)
	}
	w.largeRoom = prefix + "large_write"
	if err := exec(`
		INSERT INTO rooms (id, namespace_id, name, creator_id, member_count)
		SELECT r, '00000000-0000-0000-0000-000000000000', r, $2, $3 FROM unnest($1::text[]) r`, append(largeIDs, w.largeRoom), w.senderID, benchLargeRoomMemberCt); err != nil {
		return nil, fmt.Errorf("large rooms: %w", err)
	}
	for _, k := range []int{5, 50} {
		if err := exec(`
			INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role)
			SELECT '00000000-0000-0000-0000-000000000000', r, $2, 'join', 'member' FROM unnest($1::text[]) r`, largeIDs[:k], w.readers[k]); err != nil {
			return nil, fmt.Errorf("large memberships: %w", err)
		}
	}
	if err := exec(`
		INSERT INTO events (stream_pos, id, room_id, sender_id, event_type, content, txn_key)
		SELECT nextval('events_stream_pos_seq'), gen_random_uuid()::text, r, $2,
		       'm.room.message', '{"body":"bench large-room message"}', gen_random_uuid()::text
		FROM generate_series(1, $3), unnest($1::text[]) r`,
		largeIDs, w.senderID, benchLargeRoomEvents); err != nil {
		return nil, fmt.Errorf("large events: %w", err)
	}

	// Fan-out rooms for Create benchmarks: exactly n members each, no history.
	next := 0
	for _, n := range []int{10, 200} {
		roomID := prefix + fmt.Sprintf("fanout_%d", n)
		if err := exec(`INSERT INTO rooms (id, namespace_id, name, creator_id, member_count) VALUES ($1, '00000000-0000-0000-0000-000000000000', $1, $2, $3)`,
			roomID, w.senderID, n); err != nil {
			return nil, fmt.Errorf("fanout room: %w", err)
		}
		members := make([]string, n)
		for i := range members {
			members[i] = user(next)
			next++
		}
		if err := exec(`
			INSERT INTO room_memberships (namespace_id, room_id, user_id, state, role)
			SELECT '00000000-0000-0000-0000-000000000000', $1, u, 'join', 'member' FROM unnest($2::text[]) u`, roomID, members); err != nil {
			return nil, fmt.Errorf("fanout memberships: %w", err)
		}
		w.smallRooms[n] = roomID
		w.roomMembers[roomID] = members
	}

	// A session for the auth lookup benchmark.
	deviceID := prefix + "device"
	if err := exec(`INSERT INTO devices (id, user_id, display_name) VALUES ($1, $2, 'bench')`,
		deviceID, w.senderID); err != nil {
		return nil, fmt.Errorf("device: %w", err)
	}
	sessionID := uuid.NewString()
	token, err := crypto.NewSessionToken(sessionID, []byte(benchSecret))
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	w.token = token
	if err := exec(`
		INSERT INTO sessions (id, namespace_id, device_id, user_id, token_hash, expires_at)
		VALUES ($1, '00000000-0000-0000-0000-000000000000', $2, $3, $4, $5)`,
		sessionID, deviceID, w.senderID, crypto.TokenHash(w.token), time.Now().Add(time.Hour)); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}

	if _, err := pool.Exec(ctx, `ANALYZE`); err != nil {
		return nil, fmt.Errorf("analyze: %w", err)
	}
	return w, nil
}

const benchSecret = "bench-secret-at-least-32-characters!!"

func BenchmarkEventStore_Create(b *testing.B) {
	w := benchFixture(b)
	store := postgres.NewEventStore(w.pool)
	ctx := context.Background()

	// fanOut writes one visibility row per joined member, read from room_memberships.
	run := func(b *testing.B, roomID string, fanOut bool) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_, _, err := store.Create(ctx, &domain.Event{
				ID: uuid.NewString(), RoomID: roomID, SenderID: w.senderID,
				EventType: "m.room.message", Content: []byte(`{"body":"bench"}`),
				TxnKey: uuid.NewString(), CreatedAt: time.Now().UTC(),
			}, fanOut)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	for _, n := range []int{10, 200} {
		roomID := w.smallRooms[n]
		b.Run(fmt.Sprintf("small_members=%d", n), func(b *testing.B) { run(b, roomID, true) })
	}
	b.Run("large", func(b *testing.B) { run(b, w.largeRoom, false) })
}

func BenchmarkEventStore_GetVisible(b *testing.B) {
	w := benchFixture(b)
	events := postgres.NewEventStore(w.pool)
	ctx := context.Background()

	for _, k := range []int{0, 5, 50} {
		userID := w.readers[k]
		for _, mode := range []string{"initial", "incremental"} {
			b.Run(fmt.Sprintf("large_rooms=%d/%s", k, mode), func(b *testing.B) {
				// incremental: a client that is caught up except for the last ~50 positions.
				var since int64
				if mode == "incremental" {
					cur, err := events.CurrentStreamPos(ctx)
					if err != nil {
						b.Fatal(err)
					}
					since = cur - 50
				}
				b.ReportAllocs()
				for b.Loop() {
					if _, err := events.GetVisible(ctx, userID, since, 100); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkMembershipStore_ListLargeRooms(b *testing.B) {
	w := benchFixture(b)
	memberships := postgres.NewMembershipStore(w.pool)
	ctx := context.Background()
	for _, k := range []int{0, 50} {
		b.Run(fmt.Sprintf("large_rooms=%d", k), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := memberships.ListLargeRooms(ctx, w.readers[k]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAuthLookup measures the per-request DB work of the auth middleware.
func BenchmarkAuthLookup(b *testing.B) {
	w := benchFixture(b)
	sessions := postgres.NewSessionStore(w.pool)
	ctx := context.Background()
	hash := crypto.TokenHash(w.token)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := sessions.GetAuthByTokenHash(ctx, hash); err != nil {
			b.Fatal(err)
		}
	}
}
