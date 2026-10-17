package domain

import (
	"regexp"
	"time"
)

// DefaultNamespaceID is the namespace every pre-namespace row was migrated into, and the
// one /login and /register select when a client names none.
const (
	DefaultNamespaceID   = "00000000-0000-0000-0000-000000000000"
	DefaultNamespaceSlug = "default"
)

type NamespaceStatus string

const (
	NamespaceActive   NamespaceStatus = "active"
	NamespaceDisabled NamespaceStatus = "disabled"
)

// Namespace is an isolation boundary: users, rooms, memberships, sessions, media and
// AppServices of one namespace never grant access to another's. Slug is the stable
// human-readable name (also the /login selector); ID is the immutable internal key.
type Namespace struct {
	ID                string
	Slug              string
	Status            NamespaceStatus
	AllowRegistration bool // public POST /register is accepted for this namespace
	CreatedAt         time.Time
	DisabledAt        *time.Time
}

func (n *Namespace) IsActive() bool { return n.Status == NamespaceActive }

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidNamespaceSlug mirrors the namespaces.slug CHECK constraint.
func ValidNamespaceSlug(slug string) bool { return slugPattern.MatchString(slug) }
