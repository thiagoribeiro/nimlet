package domain

import "time"

type Session struct {
	ID          string
	NamespaceID string // namespace of the user; fixed for the session's lifetime
	DeviceID    string
	UserID      string
	TokenHash   string // HMAC-SHA256, never stored raw
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// SessionAuth is a session plus the state of everything request authentication depends on,
// loaded together in one query.
type SessionAuth struct {
	Session           Session
	DeviceDisabled    bool
	UserDisabled      bool
	NamespaceDisabled bool
}

func (s *Session) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}
