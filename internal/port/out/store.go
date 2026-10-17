package out

import (
	"context"
	"io"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Transactor allows use cases to span multiple store operations in one DB transaction.
// The transaction is propagated via context; each store checks for it before using the pool.
type Transactor interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type UserStore interface {
	Create(ctx context.Context, u *domain.User, passwordHash string) error
	GetByID(ctx context.Context, id string) (*domain.User, error)
	// GetByUsername resolves a username within one namespace: user + passwordHash.
	GetByUsername(ctx context.Context, namespaceID, username string) (*domain.User, string, error)
	// ListByNamespace pages through a namespace's users ordered by username, after
	// afterUsername ("" = from the start).
	ListByNamespace(ctx context.Context, namespaceID, afterUsername string, limit int) ([]*domain.User, error)
	SetDisabled(ctx context.Context, userID string, disabled bool) error
	SetPasswordHash(ctx context.Context, userID, passwordHash string) error
}

type DeviceStore interface {
	Create(ctx context.Context, d *domain.Device) error
	GetByID(ctx context.Context, id string) (*domain.Device, error)
	// SetPushToken registers (or clears, if pushToken is "") the FCM token for deviceID.
	SetPushToken(ctx context.Context, deviceID, pushToken string) error
	// ListPushTokensByUserIDs returns one row per non-disabled device with a registered
	// push token, for every userID in the list — the fan-out list a push dispatch needs.
	ListPushTokensByUserIDs(ctx context.Context, userIDs []string) ([]domain.DevicePushToken, error)
}

type SessionStore interface {
	Create(ctx context.Context, s *domain.Session) error
	// GetAuthByTokenHash loads a session plus the device, user and namespace state request
	// authentication checks, in one query.
	GetAuthByTokenHash(ctx context.Context, hash string) (*domain.SessionAuth, error)
	Delete(ctx context.Context, sessionID string) error
	// ListByUser returns userID's sessions, newest first (token hashes are not returned).
	ListByUser(ctx context.Context, userID string) ([]*domain.Session, error)
	// DeleteInNamespace deletes a session only if it belongs to namespaceID (ErrNotFound otherwise).
	DeleteInNamespace(ctx context.Context, namespaceID, sessionID string) error
	// DeleteExpired removes sessions past expires_at; returns how many.
	DeleteExpired(ctx context.Context) (int64, error)
	// PruneDevice keeps only the newest keep sessions of deviceID.
	PruneDevice(ctx context.Context, deviceID string, keep int) error
}

type RoomStore interface {
	Create(ctx context.Context, r *domain.Room) error
	GetByID(ctx context.Context, id string) (*domain.Room, error)
	GetByDirectKey(ctx context.Context, namespaceID, directKey string) (*domain.Room, error)
	// GetByAlias returns the namespace's room with the given non-empty alias, or ErrNotFound.
	GetByAlias(ctx context.Context, namespaceID, alias string) (*domain.Room, error)
	IncrementMemberCount(ctx context.Context, roomID string) error
	DecrementMemberCount(ctx context.Context, roomID string) error
	// ListByNamespace pages through a namespace's rooms ordered by creation, newest first.
	ListByNamespace(ctx context.Context, namespaceID string, limit int) ([]*domain.Room, error)
	// ListByIDs returns rooms for the given IDs (missing IDs are simply omitted, not an error).
	ListByIDs(ctx context.Context, ids []string) ([]*domain.Room, error)
	// DeleteExpired removes every room whose expires_at has passed, along with its
	// memberships, events, and visibility/idempotency rows. Returns the count deleted.
	DeleteExpired(ctx context.Context) (int64, error)
}

type MembershipStore interface {
	// Set writes a membership unconditionally. Only for rows whose current state cannot
	// matter (a brand-new room's first members, an admin role change); every other change
	// goes through Transition.
	Set(ctx context.Context, m *domain.Membership) error
	// Transition locks (roomID, userID)'s membership, passes it to next (nil = none) and
	// writes what next returns; an error from next aborts without writing. It returns the
	// membership as it was before. Must run inside Transactor.WithTx so the row lock holds
	// until commit: concurrent transitions of the same membership then serialise instead of
	// both acting on the same stale state.
	Transition(ctx context.Context, namespaceID, roomID, userID string,
		next func(cur *domain.Membership) (*domain.Membership, error)) (*domain.Membership, error)
	Get(ctx context.Context, roomID, userID string) (*domain.Membership, error)
	// ListByRoom returns every membership of roomID, in any state.
	ListByRoom(ctx context.Context, roomID string) ([]*domain.Membership, error)
	// ListLargeRooms returns IDs of rooms where userID is joined and member_count >= threshold.
	ListLargeRooms(ctx context.Context, userID string) ([]string, error)
	// ListByUser returns every membership (any state) for userID — used to list "my rooms".
	// ListByUser pages through userID's memberships (any state) ordered by room ID, after
	// afterRoomID ("" = from the start).
	ListByUser(ctx context.Context, userID, afterRoomID string, limit int) ([]*domain.Membership, error)
}

type EventStore interface {
	// Create stores an event. With fanOut, every member joined to the room at that moment
	// gets a per-user visibility row in the same atomic statement, and their user IDs are
	// returned (for wake-ups and push); large rooms pass fanOut=false. Re-sending a claimed
	// txn_key returns the original event and no recipients.
	Create(ctx context.Context, e *domain.Event, fanOut bool) (*domain.Event, []string, error)

	// GetVisible returns events visible to userID with stream_pos > sincePos, in stream_pos
	// order: per-user visibility rows plus every large room userID has joined (resolved by
	// the store in the same snapshot). It never returns a position that could still be
	// followed by a lower one committing later. This is the critical hot-path query for /sync.
	GetVisible(ctx context.Context, userID string, sincePos int64, limit int) ([]*domain.Event, error)

	// GetByRoom pages a room's history by room_seq (after fromSeq for dir "f", before it for "b").
	GetByRoom(ctx context.Context, roomID string, fromSeq int64, dir string, limit int) ([]*domain.Event, error)
	// RevokeVisibility deletes userID's per-user visibility rows for roomID, so a removed
	// member stops seeing that room's history through /sync. Returns the rows removed.
	RevokeVisibility(ctx context.Context, userID, roomID string) (int64, error)

	// CurrentStreamPos returns the highest stream_pos globally (0 if empty).
	CurrentStreamPos(ctx context.Context) (int64, error)
}

type MediaStore interface {
	Save(ctx context.Context, mediaID, ownerID, contentType string, r io.Reader) error
	// Get returns an io.ReadCloser for zero-copy streaming to the client.
	Get(ctx context.Context, mediaID string) (rc io.ReadCloser, contentType string, size int64, err error)
}
