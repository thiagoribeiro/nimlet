package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

type auditLog struct {
	pool *pgxpool.Pool
}

func NewAuditLog(pool *pgxpool.Pool) out.AuditLog {
	return &auditLog{pool: pool}
}

func (a *auditLog) Record(ctx context.Context, e *domain.AuditEntry) error {
	_, err := a.pool.Exec(ctx, `
		INSERT INTO admin_audit (id, operator, command, namespace_id, target, outcome, correlation_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ID, e.Operator, e.Command, nilIfEmpty(e.NamespaceID), e.Target, e.Outcome, e.CorrelationID, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: record audit: %w", err)
	}
	return nil
}
