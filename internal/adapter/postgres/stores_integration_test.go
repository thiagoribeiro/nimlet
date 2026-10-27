//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/app"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
	"github.com/thiagoribeiro/nimlet/testkit"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		os.Exit(m.Run()) // will skip inside testPool
	}

	// app.Migrate serialises with other test packages migrating the same database.
	if err := app.Migrate(context.Background(), dsn, "up"); err != nil {
		fmt.Fprintf(os.Stderr, "migration: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func testPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	pool, err := postgres.Open(context.Background(), dsn, 10)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestUserStore_Contract(t *testing.T) {
	testkit.RunUserStoreContract(t, postgres.NewUserStore(testPool(t)))
}

func TestEventStore_Contract(t *testing.T) {
	pool := testPool(t)
	testkit.RunEventStoreContract(t, postgres.NewEventStore(pool), pool)
}

func TestDeviceStore_Contract(t *testing.T) {
	pool := testPool(t)
	testkit.RunDeviceStoreContract(t, postgres.NewDeviceStore(pool), pool)
}

func mustCreateTestUser(ctx context.Context, t *testing.T, users interface {
	Create(ctx context.Context, u *domain.User, passwordHash string) error
}) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, users.Create(ctx, &domain.User{
		NamespaceID: domain.DefaultNamespaceID,
		ID:          id, Username: "storetest_" + id[:8], DisplayName: "Store Test", Type: domain.UserTypeHuman, CreatedAt: time.Now().UTC(),
	}, "$argon2id$test"))
	return id
}

func TestRoomStore_ListByIDs(t *testing.T) {
	pool := testPool(t)
	rooms := postgres.NewRoomStore(pool)
	users := postgres.NewUserStore(pool)
	ctx := context.Background()

	creatorID := mustCreateTestUser(ctx, t, users)
	r1 := &domain.Room{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Name: "room one", CreatorID: creatorID, CreatedAt: time.Now().UTC().Truncate(time.Millisecond), JoinRule: domain.JoinRuleInviteOnly}
	r2 := &domain.Room{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Name: "room two", CreatorID: creatorID, CreatedAt: time.Now().UTC().Truncate(time.Millisecond), JoinRule: domain.JoinRulePublic}
	require.NoError(t, rooms.Create(ctx, r1))
	require.NoError(t, rooms.Create(ctx, r2))

	got, err := rooms.ListByIDs(ctx, []string{r1.ID, r2.ID, "nonexistent-id"})
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i, r := range got {
		ids[i] = r.ID
	}
	assert.ElementsMatch(t, []string{r1.ID, r2.ID}, ids)

	empty, err := rooms.ListByIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestMembershipStore_ListByUser(t *testing.T) {
	pool := testPool(t)
	rooms := postgres.NewRoomStore(pool)
	memberships := postgres.NewMembershipStore(pool)
	users := postgres.NewUserStore(pool)
	ctx := context.Background()

	userID := mustCreateTestUser(ctx, t, users)
	otherUserID := mustCreateTestUser(ctx, t, users)
	r1 := &domain.Room{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Name: "joined room", CreatorID: userID, CreatedAt: time.Now().UTC(), JoinRule: domain.JoinRuleInviteOnly}
	r2 := &domain.Room{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Name: "invited room", CreatorID: otherUserID, CreatedAt: time.Now().UTC(), JoinRule: domain.JoinRuleInviteOnly}
	require.NoError(t, rooms.Create(ctx, r1))
	require.NoError(t, rooms.Create(ctx, r2))

	require.NoError(t, memberships.Set(ctx, &domain.Membership{NamespaceID: domain.DefaultNamespaceID, RoomID: r1.ID, UserID: userID, State: domain.MembershipJoin, Role: domain.RoleOwner, UpdatedAt: time.Now().UTC()}))
	require.NoError(t, memberships.Set(ctx, &domain.Membership{NamespaceID: domain.DefaultNamespaceID, RoomID: r2.ID, UserID: userID, State: domain.MembershipInvite, Role: domain.RoleMember, UpdatedAt: time.Now().UTC()}))

	got, err := memberships.ListByUser(ctx, userID, "", 100)
	require.NoError(t, err)
	require.Len(t, got, 2)

	byRoom := make(map[string]*domain.Membership, len(got))
	for _, m := range got {
		byRoom[m.RoomID] = m
	}
	assert.Equal(t, domain.MembershipJoin, byRoom[r1.ID].State)
	assert.Equal(t, domain.MembershipInvite, byRoom[r2.ID].State)
}

// TestEventStore_ReadCeiling pins the in-flight ordering rule deterministically: a position
// allocated but not yet committed holds back every later event, and an abandoned allocation
// stops holding them back after inflightAbandonAfter.
func TestEventStore_ReadCeiling(t *testing.T) {
	pool := testPool(t)
	store := postgres.NewEventStore(pool)
	users := postgres.NewUserStore(pool)
	rooms := postgres.NewRoomStore(pool)
	ctx := context.Background()

	reader := mustCreateTestUser(ctx, t, users)
	room := &domain.Room{NamespaceID: domain.DefaultNamespaceID, ID: uuid.NewString(), Name: "ceiling", CreatorID: reader, CreatedAt: time.Now().UTC(), JoinRule: domain.JoinRuleInviteOnly}
	require.NoError(t, rooms.Create(ctx, room))
	testkit.SeedMembership(pool, room.ID, reader, "join")
	send := func() *domain.Event {
		e, _, err := store.Create(ctx, &domain.Event{
			ID: uuid.NewString(), RoomID: room.ID, SenderID: reader, EventType: "m.room.message",
			Content: []byte(`{}`), TxnKey: uuid.NewString(), CreatedAt: time.Now().UTC(),
		}, true)
		require.NoError(t, err)
		return e
	}

	before := send()
	// A slow writer holds the next position.
	var slowPos int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO stream_inflight (pos) VALUES (nextval('events_stream_pos_seq')) RETURNING pos`).Scan(&slowPos))
	after := send()
	require.Greater(t, after.StreamPos, slowPos)

	got, err := store.GetVisible(ctx, reader, before.StreamPos, 10)
	require.NoError(t, err)
	assert.Empty(t, got, "events above an in-flight position must be held back")

	// The slow writer is abandoned: once it is old enough, readers move on.
	_, err = pool.Exec(ctx, `UPDATE stream_inflight SET allocated_at = now() - interval '1 minute' WHERE pos = $1`, slowPos)
	require.NoError(t, err)
	got, err = store.GetVisible(ctx, reader, before.StreamPos, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, after.ID, got[0].ID)

	n, err := postgres.SweepStreamInflight(ctx, pool)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))
}

func TestSessionStore_GetAuthByTokenHash(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	users := postgres.NewUserStore(pool)
	devices := postgres.NewDeviceStore(pool)
	sessions := postgres.NewSessionStore(pool)

	userID := mustCreateTestUser(ctx, t, users)
	dev := &domain.Device{ID: uuid.NewString(), UserID: userID, DisplayName: "d", CreatedAt: time.Now().UTC()}
	require.NoError(t, devices.Create(ctx, dev))
	sess := &domain.Session{
		NamespaceID: domain.DefaultNamespaceID,
		ID:          uuid.NewString(), DeviceID: dev.ID, UserID: userID, TokenHash: "hash-" + uuid.NewString(),
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond), CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	require.NoError(t, sessions.Create(ctx, sess))

	got, err := sessions.GetAuthByTokenHash(ctx, sess.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.Session.ID)
	assert.Equal(t, userID, got.Session.UserID)
	assert.False(t, got.DeviceDisabled)

	_, err = pool.Exec(ctx, `UPDATE devices SET disabled = true WHERE id = $1`, dev.ID)
	require.NoError(t, err)
	got, err = sessions.GetAuthByTokenHash(ctx, sess.TokenHash)
	require.NoError(t, err)
	assert.True(t, got.DeviceDisabled)

	_, err = sessions.GetAuthByTokenHash(ctx, "no-such-hash")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestNamespaceStore(t *testing.T) {
	pool := testPool(t)
	store := postgres.NewNamespaceStore(pool)
	ctx := context.Background()

	def, err := store.GetBySlug(ctx, domain.DefaultNamespaceSlug)
	require.NoError(t, err)
	assert.Equal(t, domain.DefaultNamespaceID, def.ID)
	assert.True(t, def.AllowRegistration, "migrated default keeps public registration")

	ns := &domain.Namespace{ID: uuid.NewString(), Slug: "store-" + uuid.NewString()[:8], Status: domain.NamespaceActive, CreatedAt: time.Now().UTC()}
	require.NoError(t, store.Create(ctx, ns))
	dup := *ns
	dup.ID = uuid.NewString()
	assert.ErrorIs(t, store.Create(ctx, &dup), domain.ErrAlreadyExists)

	require.NoError(t, store.SetStatus(ctx, ns.ID, domain.NamespaceDisabled))
	got, err := store.GetByID(ctx, ns.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.NamespaceDisabled, got.Status)
	assert.NotNil(t, got.DisabledAt)

	require.NoError(t, store.SetStatus(ctx, ns.ID, domain.NamespaceActive))
	require.NoError(t, store.SetAllowRegistration(ctx, ns.ID, true))
	got, err = store.GetBySlug(ctx, ns.Slug)
	require.NoError(t, err)
	assert.Equal(t, domain.NamespaceActive, got.Status)
	assert.Nil(t, got.DisabledAt)
	assert.True(t, got.AllowRegistration)
	assert.Equal(t, domain.PushDigest, got.PushMode, "new namespaces default to digest pushes")
	require.NoError(t, store.SetPushMode(ctx, ns.ID, domain.PushMessage))
	got, err = store.GetByID(ctx, ns.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.PushMessage, got.PushMode)
	assert.ErrorIs(t, store.SetPushMode(ctx, uuid.NewString(), domain.PushMessage), domain.ErrNotFound)

	assert.ErrorIs(t, store.SetStatus(ctx, uuid.NewString(), domain.NamespaceDisabled), domain.ErrNotFound)

	// Same username in two namespaces: allowed, and lookups stay in their namespace.
	users := postgres.NewUserStore(pool)
	name := "same_" + uuid.NewString()[:8]
	uDef := &domain.User{ID: uuid.NewString(), NamespaceID: domain.DefaultNamespaceID, Username: name, Type: domain.UserTypeHuman, CreatedAt: time.Now().UTC()}
	uNS := &domain.User{ID: uuid.NewString(), NamespaceID: ns.ID, Username: name, Type: domain.UserTypeHuman, CreatedAt: time.Now().UTC()}
	require.NoError(t, users.Create(ctx, uDef, "h1"))
	require.NoError(t, users.Create(ctx, uNS, "h2"))
	gotU, hash, err := users.GetByUsername(ctx, ns.ID, name)
	require.NoError(t, err)
	assert.Equal(t, uNS.ID, gotU.ID)
	assert.Equal(t, "h2", hash)
}

// Partition maintenance keeps blocks created ahead of the stream, so events never fall into
// the DEFAULT partitions (review P0-11). Advances the shared sequence; harmless for other
// tests, which never assume absolute positions.
func TestEnsureEventPartitions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const block = 10_000_000

	var highest int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT max(substring(relname FROM 'events_p(\d+)$')::bigint)
		FROM pg_class WHERE relname ~ '^events_p\d+$'`).Scan(&highest))
	// Jump to just below the end of the last existing block.
	_, err := pool.Exec(ctx, `SELECT setval('events_stream_pos_seq', $1)`, highest*block-5)
	require.NoError(t, err)

	created, defaultInUse, err := postgres.EnsureEventPartitions(ctx, pool)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, created, 1, "the next block is created before the stream reaches it")
	assert.False(t, defaultInUse)

	// Writes across the old boundary land in real partitions, never in DEFAULT.
	users := postgres.NewUserStore(pool)
	rooms := postgres.NewRoomStore(pool)
	store := postgres.NewEventStore(pool)
	sender := mustCreateTestUser(ctx, t, users)
	room := &domain.Room{ID: uuid.NewString(), NamespaceID: domain.DefaultNamespaceID, Name: "p", CreatorID: sender, CreatedAt: time.Now().UTC(), JoinRule: domain.JoinRuleInviteOnly}
	require.NoError(t, rooms.Create(ctx, room))
	testkit.SeedMembership(pool, room.ID, sender, "join")
	for range 10 {
		_, _, err := store.Create(ctx, &domain.Event{ID: uuid.NewString(), RoomID: room.ID, SenderID: sender,
			EventType: "m.text", Content: []byte(`{}`), TxnKey: uuid.NewString(), CreatedAt: time.Now().UTC()}, true)
		require.NoError(t, err)
	}
	_, defaultInUse, err = postgres.EnsureEventPartitions(ctx, pool)
	require.NoError(t, err)
	assert.False(t, defaultInUse, "events crossed into a new block without touching DEFAULT")

	again, _, err := postgres.EnsureEventPartitions(ctx, pool)
	require.NoError(t, err)
	assert.Zero(t, again, "idempotent")
}

// batchModes runs a test against both write protocols of the batched store.
func batchModes(t *testing.T, run func(t *testing.T, singleTx bool)) {
	for _, single := range []bool{false, true} {
		name := "split"
		if single {
			name = "single"
		}
		t.Run(name, func(t *testing.T) { run(t, single) })
	}
}

// The batched store must honour the whole event store contract; MaxWait makes the
// contract's concurrent sends and reads actually share groups.
func TestBatchedEventStore_Contract(t *testing.T) {
	batchModes(t, func(t *testing.T, single bool) {
		pool := testPool(t)
		store := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
			Writes:   postgres.GroupLimits{MaxItems: 16, MaxWait: 2 * time.Millisecond, Workers: 2},
			Reads:    postgres.GroupLimits{MaxItems: 16, MaxWait: 2 * time.Millisecond, Workers: 2},
			SingleTx: single,
		})
		t.Cleanup(store.Close)
		testkit.RunEventStoreContract(t, store, pool)
	})
}

// Single-transaction batches commit without stream_inflight markers, while unbatched sends
// still use them. A reader tailing the stream with its cursor must never see a position
// before a lower one has become visible: it would skip that one for good.
func TestBatchedEventStore_MixedWritersKeepOrder(t *testing.T) {
	pool := testPool(t)
	plain := postgres.NewEventStore(pool)
	batched := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
		Writes:   postgres.GroupLimits{MaxItems: 16, MaxWait: time.Millisecond, Workers: 4},
		SingleTx: true,
	})
	t.Cleanup(batched.Close)
	ctx := context.Background()

	user, room := uuid.NewString(), uuid.NewString()
	testkit.SeedUser(pool, user)
	testkit.SeedRoom(pool, room, user)
	testkit.SeedMembership(pool, room, user, "join")
	device := uuid.NewString()
	start, err := plain.CurrentStreamPos(ctx)
	require.NoError(t, err)

	const writers, perWriter = 8, 40
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var store out.EventStore = batched
			if w%2 == 0 {
				store = plain
			}
			for i := range perWriter {
				e := &domain.Event{
					ID: uuid.NewString(), RoomID: room, SenderID: user, EventType: "m.room.message",
					Content: []byte(`{"body":"x"}`), TxnKey: fmt.Sprintf("%s:%d-%d", device, w, i), CreatedAt: time.Now().UTC(),
				}
				_, _, err := store.Create(ctx, e, true)
				assert.NoError(t, err)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var seen []int64
	since := start
	tail := func() {
		events, err := plain.GetVisible(ctx, user, since, 100)
		require.NoError(t, err)
		for _, e := range events {
			seen = append(seen, e.StreamPos)
			since = e.StreamPos
		}
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		tail()
	}
	tail()

	all, err := plain.GetVisible(ctx, user, start, writers*perWriter+10)
	require.NoError(t, err)
	require.Len(t, all, writers*perWriter)
	want := make([]int64, len(all))
	for i, e := range all {
		want[i] = e.StreamPos
	}
	assert.Equal(t, want, seen, "the tailing reader skipped or reordered positions")
}

// A batch mixing good sends, a send to a missing room and a txn_key repeated within the
// batch: the bad send fails alone, the repeat returns the original, the rest commit with
// consecutive room_seqs.
func TestBatchedEventStore_IsolatesFailures(t *testing.T) {
	batchModes(t, testBatchIsolatesFailures)
}

func testBatchIsolatesFailures(t *testing.T, single bool) {
	pool := testPool(t)
	store := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
		Writes:   postgres.GroupLimits{MaxItems: 64, MaxWait: 20 * time.Millisecond, Workers: 1},
		SingleTx: single,
	})
	t.Cleanup(store.Close)
	ctx := context.Background()

	user, room := uuid.NewString(), uuid.NewString()
	testkit.SeedUser(pool, user)
	testkit.SeedRoom(pool, room, user)
	testkit.SeedMembership(pool, room, user, "join")
	device := uuid.NewString() // txn keys must be new on every run of the test
	newEvent := func(roomID, txn string) *domain.Event {
		return &domain.Event{
			ID: uuid.NewString(), RoomID: roomID, SenderID: user, EventType: "m.room.message",
			Content: []byte(`{"body":"x"}`), TxnKey: device + ":" + txn, CreatedAt: time.Now().UTC(),
		}
	}

	type result struct {
		e   *domain.Event
		err error
	}
	const good = 20
	results := make([]result, good+2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var e *domain.Event
			switch i {
			case good: // missing room
				e = newEvent(uuid.NewString(), uuid.NewString())
			case good + 1: // same txn as send 0
				e = newEvent(room, "txn-0")
			default:
				e = newEvent(room, fmt.Sprintf("txn-%d", i))
			}
			got, _, err := store.Create(ctx, e, true)
			results[i] = result{got, err}
		}()
	}
	wg.Wait()

	require.ErrorIs(t, results[good].err, domain.ErrNotFound)
	require.NoError(t, results[good+1].err)
	require.NoError(t, results[0].err)
	assert.Equal(t, results[0].e.ID, results[good+1].e.ID, "a repeated txn_key returns the original event")

	seqs := make(map[int64]bool)
	for _, r := range results[:good] {
		require.NoError(t, r.err)
		seqs[r.e.RoomSeq] = true
	}
	for s := int64(1); s <= good; s++ {
		assert.True(t, seqs[s], "room_seq %d missing", s)
	}
	var stored int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE room_id = $1`, room).Scan(&stored))
	assert.Equal(t, good, stored)
}

// Grouped reads must return, per user, exactly what an ungrouped GetVisible returns: small
// rooms through visibility rows, large rooms straight from events, any cursor and limit.
func TestBatchedEventStore_GroupedReadsMatch(t *testing.T) {
	pool := testPool(t)
	plain := postgres.NewEventStore(pool)
	grouped := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
		Reads: postgres.GroupLimits{MaxItems: 64, MaxWait: 20 * time.Millisecond, Workers: 1},
	})
	t.Cleanup(grouped.Close)
	ctx := context.Background()

	owner, small, large := uuid.NewString(), uuid.NewString(), uuid.NewString()
	testkit.SeedUser(pool, owner)
	testkit.SeedRoom(pool, small, owner)
	testkit.SeedLargeRoom(pool, large, owner)
	users := make([]string, 12)
	for i := range users {
		users[i] = uuid.NewString()
		testkit.SeedUser(pool, users[i])
		testkit.SeedMembership(pool, small, users[i], "join")
		if i%2 == 0 {
			testkit.SeedMembership(pool, large, users[i], "join")
		}
	}
	device := uuid.NewString()
	send := func(room string, fanOut bool) {
		_, _, err := plain.Create(ctx, &domain.Event{
			ID: uuid.NewString(), RoomID: room, SenderID: owner, EventType: "m.room.message",
			Content: []byte(`{"body":"x"}`), TxnKey: device + ":" + uuid.NewString(), CreatedAt: time.Now().UTC(),
		}, fanOut)
		require.NoError(t, err)
	}
	for i := range 40 {
		send(small, true)
		if i%3 == 0 {
			send(large, false)
		}
	}
	all, err := plain.GetVisible(ctx, users[0], 0, 1000)
	require.NoError(t, err)
	require.Greater(t, len(all), 40)

	type query struct {
		user  string
		since int64
		limit int
	}
	var queries []query
	for i, u := range users {
		queries = append(queries,
			query{u, 0, 100},
			query{u, all[i*2].StreamPos, 5},
			query{u, all[len(all)-1].StreamPos, 100}) // caught up: empty
	}
	ids := func(es []*domain.Event) []string {
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = e.ID
		}
		return out
	}
	got := make([][]string, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			es, err := grouped.GetVisible(ctx, q.user, q.since, q.limit)
			if err != nil {
				t.Errorf("grouped GetVisible: %v", err)
			}
			got[i] = ids(es)
		}()
	}
	wg.Wait()
	for i, q := range queries {
		want, err := plain.GetVisible(ctx, q.user, q.since, q.limit)
		require.NoError(t, err)
		assert.Equal(t, ids(want), got[i], "query %d (user %d, since %d, limit %d)", i, i/3, q.since, q.limit)
	}
}

// A reader whose only new event is held back by an in-flight write must wait for that write
// instead of returning empty: the event's wake-up is already spent, and the write it waits on
// only wakes its own room, so an empty answer would leave the reader asleep until timeout.
func TestEventStore_WaitsPastCeiling(t *testing.T) {
	pool := testPool(t)
	batched := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
		Reads: postgres.GroupLimits{MaxItems: 8, Workers: 1},
	})
	t.Cleanup(batched.Close)
	for name, store := range map[string]interface {
		Create(context.Context, *domain.Event, bool) (*domain.Event, []string, error)
		GetVisible(context.Context, string, int64, int) ([]*domain.Event, error)
	}{"plain": postgres.NewEventStore(pool), "grouped": batched} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			reader, room := uuid.NewString(), uuid.NewString()
			testkit.SeedUser(pool, reader)
			testkit.SeedRoom(pool, room, reader)
			testkit.SeedMembership(pool, room, reader, "join")
			send := func() *domain.Event {
				e, _, err := store.Create(ctx, &domain.Event{
					ID: uuid.NewString(), RoomID: room, SenderID: reader, EventType: "m.room.message",
					Content: []byte(`{}`), TxnKey: uuid.NewString(), CreatedAt: time.Now().UTC(),
				}, true)
				require.NoError(t, err)
				return e
			}
			before := send()
			var slowPos int64
			require.NoError(t, pool.QueryRow(ctx,
				`INSERT INTO stream_inflight (pos) VALUES (nextval('events_stream_pos_seq')) RETURNING pos`).Scan(&slowPos))
			after := send()

			go func() {
				time.Sleep(50 * time.Millisecond)
				_, _ = pool.Exec(ctx, `DELETE FROM stream_inflight WHERE pos = $1`, slowPos)
			}()
			start := time.Now()
			got, err := store.GetVisible(ctx, reader, before.StreamPos, 10)
			require.NoError(t, err)
			require.Len(t, got, 1, "the held-back event is returned once the write completes")
			assert.Equal(t, after.ID, got[0].ID)
			assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
		})
	}
}
