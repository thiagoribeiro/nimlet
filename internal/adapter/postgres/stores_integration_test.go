//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/app"
	"github.com/thiagoribeiro/nimlet/internal/domain"
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
