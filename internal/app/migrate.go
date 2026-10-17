package app

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/thiagoribeiro/nimlet/migrations"
)

// migrationLockKey is the Postgres advisory lock that serialises migration runs.
const migrationLockKey int64 = 0x6e696d6c65740002 // "nimlet" + 2

// Migrate runs a goose command (up, down, status, version, reset, ...) against the
// embedded Postgres migrations. Runs are serialised with an advisory lock, so several
// instances (or test processes) starting at once never apply the same migration twice.
func Migrate(ctx context.Context, dsn, command string, args ...string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrations: open: %w", err)
	}
	defer db.Close()

	// Session-level lock held on a dedicated connection for the whole run; goose uses
	// other connections from the same pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrations: connect: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("migrations: lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	goose.SetBaseFS(migrations.PostgresFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrations: set dialect: %w", err)
	}
	if err := goose.RunContext(ctx, command, db, "postgres", args...); err != nil {
		return fmt.Errorf("migrations: %s: %w", command, err)
	}
	return nil
}
