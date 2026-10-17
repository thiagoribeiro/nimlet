package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"

	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
)

// ErrDependency marks failures to reach a dependency (the database), so the CLI can report
// them with their own exit code.
var ErrDependency = errors.New("dependency unavailable")

// OpenAdmin connects the admin use case to the database named by dsn. It needs nothing
// else — no HMAC secret, Redis or media store — and never runs migrations implicitly.
// correlationID tags this invocation's audit entries. Call the returned close func when done.
func OpenAdmin(ctx context.Context, dsn, correlationID string) (in.AdminService, func(), error) {
	pool, err := postgres.Open(ctx, dsn, 4)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	svc := usecase.NewAdminService(usecase.AdminStores{
		Namespaces:  postgres.NewNamespaceStore(pool),
		Users:       postgres.NewUserStore(pool),
		Sessions:    postgres.NewSessionStore(pool),
		Rooms:       postgres.NewRoomStore(pool),
		Memberships: postgres.NewMembershipStore(pool),
		Events:      postgres.NewEventStore(pool),
		Tx:          postgres.NewTransactor(pool),
		Audit:       postgres.NewAuditLog(pool),
	}, Operator(), correlationID)
	return svc, pool.Close, nil
}

// Operator is the identity recorded in the audit log: NIMLET_ADMIN_OPERATOR when set (e.g.
// a Kubernetes Job's service account), otherwise the OS user and host.
func Operator() string {
	if op := os.Getenv("NIMLET_ADMIN_OPERATOR"); op != "" {
		return op
	}
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return name + "@" + host
}
