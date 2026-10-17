package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type transactor struct {
	pool *pgxpool.Pool
}

func NewTransactor(pool *pgxpool.Pool) out.Transactor {
	return &transactor{pool: pool}
}

func (t *transactor) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}

	txCtx := withTxQuerier(ctx, tx)
	if err := fn(txCtx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			slog.Error("tx rollback failed", "error", rbErr)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit tx: %w", err)
	}
	return nil
}
