package out

import (
	"context"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// AuditLog records administrative actions. Entries never contain secrets.
type AuditLog interface {
	Record(ctx context.Context, e *domain.AuditEntry) error
}
