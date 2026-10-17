package domain

import "time"

// AuditEntry is one administrative action, recorded whatever its outcome.
type AuditEntry struct {
	ID            string
	Operator      string // who ran it: OS user@host, or NIMLET_ADMIN_OPERATOR
	Command       string // e.g. "user create"
	NamespaceID   string // empty for instance-level commands
	Target        string // what it acted on (slug, username, room ID, ...)
	Outcome       string // "ok" or the error text
	CorrelationID string
	CreatedAt     time.Time
}
