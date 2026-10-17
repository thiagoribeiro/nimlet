//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/app"
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn != "" {
		if err := app.Migrate(context.Background(), dsn, "up"); err != nil {
			panic(err)
		}
		os.Setenv("NIMLET_ADMIN_DB_DSN", dsn)
		os.Setenv("NIMLET_ADMIN_OPERATOR", "cli-test")
	}
	os.Exit(m.Run())
}

// nimlet runs the CLI in-process and returns stdout and the exit code.
func nimlet(t *testing.T, stdin string, argv ...string) (string, int) {
	t.Helper()
	if os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	var out, errOut bytes.Buffer
	cmd := newRootCmd()
	cmd.SetArgs(argv)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	code := report(&errOut, cmd.ExecuteContext(context.Background()))
	if code != 0 {
		t.Logf("nimlet %s → %d: %s", strings.Join(argv, " "), code, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), code
}

func mustRun(t *testing.T, stdin string, argv ...string) string {
	t.Helper()
	out, code := nimlet(t, stdin, argv...)
	require.Equal(t, 0, code, "nimlet %v", argv)
	return out
}

func TestAdminCLI(t *testing.T) {
	ns := "cli-" + uuid.NewString()[:8]
	other := "cli-other-" + uuid.NewString()[:8]
	mustRun(t, "", "namespace", "create", ns)
	mustRun(t, "", "namespace", "create", other)

	t.Run("namespace create is not repeatable and validates slugs", func(t *testing.T) {
		_, code := nimlet(t, "", "namespace", "create", ns)
		assert.Equal(t, exitConflict, code)
		_, code = nimlet(t, "", "namespace", "create", "Bad_Slug")
		assert.Equal(t, exitUsage, code)
	})

	aliceID := mustRun(t, "alice-password\n", "user", "create", "alice", "-n", ns, "--password-stdin", "-q")
	bobID := mustRun(t, "bob-password\n", "user", "create", "bob", "-n", ns, "--password-stdin", "-q")
	carolOtherID := mustRun(t, "carol-password\n", "user", "create", "carol", "-n", other, "--password-stdin", "-q")
	require.NotEmpty(t, aliceID)

	t.Run("usage errors", func(t *testing.T) {
		_, code := nimlet(t, "", "user", "list") // no namespace
		assert.Equal(t, exitUsage, code)
		_, code = nimlet(t, "", "user", "create", "dave", "-n", ns, "--password-stdin") // empty stdin
		assert.Equal(t, exitUsage, code)
		_, code = nimlet(t, "", "room", "create", "x", "-n", ns) // no owner
		assert.Equal(t, exitUsage, code)
		_, code = nimlet(t, "", "user", "show", "alice", "-n", ns, "--output", "yaml")
		assert.Equal(t, exitUsage, code)
	})

	t.Run("user sync-cursor converts a numeric cursor for that user only", func(t *testing.T) {
		secret := "cli-test-secret-at-least-32-chars!!"
		t.Setenv("NIMLET_AUTH_HMAC_SECRET", "")
		_, code := nimlet(t, "", "user", "sync-cursor", "alice", "-n", ns, "--at", "243")
		assert.Equal(t, exitUsage, code, "needs the server secret")
		t.Setenv("NIMLET_AUTH_HMAC_SECRET", secret)
		_, code = nimlet(t, "", "user", "sync-cursor", "alice", "-n", ns)
		assert.Equal(t, exitUsage, code, "--at is required")
		_, code = nimlet(t, "", "user", "sync-cursor", "carol", "-n", ns, "--at", "243")
		assert.Equal(t, exitNotFound, code, "user of another namespace")

		token := mustRun(t, "", "user", "sync-cursor", "alice", "-n", ns, "--at", "243")
		codec, err := cursor.New([]byte(secret))
		require.NoError(t, err)
		pos, err := codec.Decode(cursor.Sync, aliceID, token)
		require.NoError(t, err)
		assert.Equal(t, int64(243), pos)
		_, err = codec.Decode(cursor.Sync, bobID, token)
		assert.ErrorIs(t, err, cursor.ErrInvalid)
	})

	t.Run("a CLI-created user can log in to its namespace only", func(t *testing.T) {
		pool, err := postgres.Open(context.Background(), os.Getenv("TEST_POSTGRES_DSN"), 2)
		require.NoError(t, err)
		defer pool.Close()
		auth := usecase.NewAuthService(postgres.NewUserStore(pool), postgres.NewDeviceStore(pool),
			postgres.NewSessionStore(pool), postgres.NewNamespaceStore(pool), postgres.NewTransactor(pool), []byte("cli-test-secret-at-least-32-chars!!"))
		res, err := auth.Login(context.Background(), in.LoginCmd{Namespace: ns, Username: "alice", Password: "alice-password"})
		require.NoError(t, err)
		assert.Equal(t, aliceID, res.Session.UserID)
		_, err = auth.Login(context.Background(), in.LoginCmd{Namespace: other, Username: "alice", Password: "alice-password"})
		assert.ErrorIs(t, err, domain.ErrInvalidPassword)

		sessions := mustRun(t, "", "user", "session", "list", "alice", "-n", ns, "-q")
		assert.Equal(t, res.Session.ID, sessions)
		_, code := nimlet(t, "", "user", "session", "revoke", res.Session.ID, "-n", other)
		assert.Equal(t, exitNotFound, code, "a session cannot be revoked through another namespace")
		mustRun(t, "", "user", "session", "revoke", res.Session.ID, "-n", ns)
		assert.Empty(t, mustRun(t, "", "user", "session", "list", "alice", "-n", ns, "-q"))
	})

	roomID := mustRun(t, "", "room", "create", "general", "-n", ns, "--owner", "alice", "-q")

	memberCount := func() float64 {
		var r map[string]any
		require.NoError(t, json.Unmarshal([]byte(mustRun(t, "", "room", "show", roomID, "-n", ns, "-o", "json")), &r))
		return r["member_count"].(float64)
	}

	t.Run("membership lifecycle keeps member_count consistent", func(t *testing.T) {
		assert.Equal(t, 1.0, memberCount())
		mustRun(t, "", "room", "member", "add", roomID, "bob", "-n", ns)
		assert.Equal(t, 2.0, memberCount())

		_, code := nimlet(t, "", "room", "member", "add", roomID, "bob", "-n", ns)
		assert.Equal(t, exitConflict, code)
		mustRun(t, "", "room", "member", "add", roomID, "bob", "-n", ns, "--if-not-exists")
		assert.Equal(t, 2.0, memberCount(), "--if-not-exists must not double count")

		var members []map[string]any
		require.NoError(t, json.Unmarshal([]byte(mustRun(t, "", "room", "member", "list", roomID, "-n", ns, "-o", "json")), &members))
		require.Len(t, members, 2)

		_, code = nimlet(t, "", "room", "member", "remove", roomID, "alice", "-n", ns)
		assert.Equal(t, exitUsage, code, "last owner cannot be removed")
		_, code = nimlet(t, "", "room", "member", "role", "set", roomID, "alice", "member", "-n", ns)
		assert.Equal(t, exitUsage, code, "last owner cannot be demoted")
		_, code = nimlet(t, "", "room", "member", "role", "set", roomID, "bob", "emperor", "-n", ns)
		assert.Equal(t, exitUsage, code)
	})

	t.Run("names resolve inside the given namespace only", func(t *testing.T) {
		_, code := nimlet(t, "", "room", "member", "add", roomID, "carol", "-n", ns)
		assert.Equal(t, exitNotFound, code, "carol exists only in the other namespace")
		_, code = nimlet(t, "", "room", "show", roomID, "-n", other)
		assert.Equal(t, exitNotFound, code)
		_, code = nimlet(t, "", "room", "member", "add", roomID, "carol", "-n", other)
		assert.Equal(t, exitNotFound, code, "the room does not exist in the other namespace")
		assert.NotEmpty(t, carolOtherID)
	})

	t.Run("removing a member revokes their access to the room's history", func(t *testing.T) {
		pool, err := postgres.Open(context.Background(), os.Getenv("TEST_POSTGRES_DSN"), 2)
		require.NoError(t, err)
		defer pool.Close()
		events := postgres.NewEventStore(pool)
		e, _, err := events.Create(context.Background(), &domain.Event{
			ID: uuid.NewString(), RoomID: roomID, SenderID: aliceID, EventType: "m.room.message",
			Content: []byte(`{"body":"before removal"}`), TxnKey: uuid.NewString(), CreatedAt: time.Now().UTC(),
		}, true) // alice and bob are the room's joined members
		require.NoError(t, err)
		visible, err := events.GetVisible(context.Background(), bobID, e.StreamPos-1, 10)
		require.NoError(t, err)
		require.Len(t, visible, 1)

		mustRun(t, "", "room", "member", "remove", roomID, "bob", "-n", ns)
		assert.Equal(t, 1.0, memberCount())
		visible, err = events.GetVisible(context.Background(), bobID, 0, 100)
		require.NoError(t, err)
		for _, v := range visible {
			assert.NotEqual(t, roomID, v.RoomID, "removed member still sees the room's history")
		}
		_, code := nimlet(t, "", "room", "member", "remove", roomID, "bob", "-n", ns)
		assert.Equal(t, exitNotFound, code)
	})

	t.Run("disable needs confirmation and is audited without secrets", func(t *testing.T) {
		_, code := nimlet(t, "", "namespace", "disable", ns)
		assert.Equal(t, exitUsage, code)
		mustRun(t, "", "namespace", "disable", ns, "--confirm", ns)
		var n map[string]any
		require.NoError(t, json.Unmarshal([]byte(mustRun(t, "", "namespace", "show", ns, "-o", "json")), &n))
		assert.Equal(t, "disabled", n["status"])
		mustRun(t, "", "namespace", "enable", ns)

		pool, err := postgres.Open(context.Background(), os.Getenv("TEST_POSTGRES_DSN"), 2)
		require.NoError(t, err)
		defer pool.Close()
		var commands, leaked int
		require.NoError(t, pool.QueryRow(context.Background(), `
			SELECT count(*), count(*) FILTER (WHERE target LIKE '%password%' OR outcome LIKE '%password-%')
			FROM admin_audit a JOIN namespaces n ON n.id = a.namespace_id
			WHERE n.slug = $1 AND a.operator = 'cli-test'`, ns).Scan(&commands, &leaked))
		assert.GreaterOrEqual(t, commands, 10)
		assert.Zero(t, leaked)
	})
}
