package out

import (
	"context"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// NamespaceStore persists namespaces. Only administrative code (the nimlet CLI) creates or
// changes them; the server reads them to resolve /login selectors and AppService config.
type NamespaceStore interface {
	Create(ctx context.Context, ns *domain.Namespace) error // ErrAlreadyExists on slug clash
	GetByID(ctx context.Context, id string) (*domain.Namespace, error)
	GetBySlug(ctx context.Context, slug string) (*domain.Namespace, error)
	List(ctx context.Context) ([]*domain.Namespace, error)
	SetStatus(ctx context.Context, id string, status domain.NamespaceStatus) error
	SetAllowRegistration(ctx context.Context, id string, allow bool) error
}

// MediaIndex records which namespace and user each upload belongs to, so downloads can
// be authorised. The bytes themselves live in a MediaStore.
type MediaIndex interface {
	Create(ctx context.Context, m *domain.Media) error
	// Get returns ErrNotFound for media uploaded before namespaces existed.
	Get(ctx context.Context, mediaID string) (*domain.Media, error)
}
