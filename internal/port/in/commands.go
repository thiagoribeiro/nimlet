package in

import (
	"context"
	"encoding/json"
	"io"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Actor is the authenticated principal a use case acts for. NamespaceID always comes from
// the session (or the AppService registration) — never from request fields — and scopes
// every name the use case resolves.
type Actor struct {
	NamespaceID string
	UserID      string
	DeviceID    string
}

// AuthService handles user identity and session lifecycle.
type AuthService interface {
	Register(ctx context.Context, cmd RegisterCmd) (*RegisterResult, error)
	Login(ctx context.Context, cmd LoginCmd) (*LoginResult, error)
	Logout(ctx context.Context, sessionID string) error
	// ResolveUsername looks up a user by username (no password hash returned) — used to turn
	// a username into a user_id for invites, since clients only ever see usernames in the UI.
	ResolveUsername(ctx context.Context, namespaceID, username string) (*domain.User, error)
	// ResolveUserID looks up a user by ID — backs GET /me (actor.UserID comes from the
	// session, but the session doesn't carry username/display_name).
	ResolveUserID(ctx context.Context, userID string) (*domain.User, error)
	// EnsureServiceUser get-or-creates a "service"-type user for the given username in
	// namespaceID and always mints a fresh session, returning (userID, accessToken). Used by
	// an AppService to provision a Nimlet identity for one of its bridged end users. It
	// never hands out a session for a non-service user (ErrPermissionDenied).
	EnsureServiceUser(ctx context.Context, namespaceID, username string) (userID string, token string, err error)
	// SetPushToken registers the FCM push token for the caller's own device. deviceID
	// always comes from the authenticated session, never a client-supplied value.
	SetPushToken(ctx context.Context, deviceID, pushToken string) error
}

// RoomService handles rooms, membership, and event publishing.
//
// Methods that create rooms or memberships take the full Actor: the new rows live in the
// actor's namespace, and users/rooms of any other namespace behave as nonexistent
// (ErrNotFound). The others only touch rooms the actor is already a member of, which by
// construction share its namespace.
type RoomService interface {
	CreateRoom(ctx context.Context, cmd CreateRoomCmd, actor Actor) (*domain.Room, error)
	GetOrCreateDM(ctx context.Context, actor Actor, targetID string) (*domain.Room, error)
	InviteUser(ctx context.Context, cmd InviteCmd, actor Actor) error
	JoinRoom(ctx context.Context, roomID string, actor Actor) error
	// AddMember directly joins targetUserID to roomID with the "member" role, skipping the
	// invite → join handshake. The actor must be a joined owner/admin of the room. Used by
	// AppServices to provision room membership for their bridged users.
	AddMember(ctx context.Context, roomID, targetUserID string, actor Actor) error
	SendEvent(ctx context.Context, cmd SendEventCmd, actorID, deviceID string) (*domain.Event, error)
	GetMessages(ctx context.Context, cmd GetMessagesCmd, actorID string) ([]*domain.Event, error)
	// ListRooms returns every room actorID has a membership in (any state), newest room first
	// is not guaranteed — ordering is left to the caller. Powers the client's room sidebar.
	ListRooms(ctx context.Context, actorID, afterRoomID string, limit int) (rooms []*RoomMembership, next string, err error)
}

// SyncService handles the long-poll /sync endpoint.
type SyncService interface {
	Sync(ctx context.Context, cmd SyncCmd) (*SyncResult, error)
}

// AppServiceDispatcher routes newly created events to registered AppServices.
// Implementations must be non-blocking (drop on full buffer, never block the caller).
type AppServiceDispatcher interface {
	Dispatch(event *domain.Event)
}

// PushDispatcher routes newly created events to mobile push notifications for
// recipientIDs (every joined member of the room except the sender). roomName is
// passed through for the notification digest, since domain.Event only carries the
// room ID. Implementations must be non-blocking (never block the caller, even on a
// slow/unavailable delivery backend) and are responsible for their own
// presence/offline filtering and batching.
type PushDispatcher interface {
	Dispatch(event *domain.Event, recipientIDs []string, roomName string)
}

// MediaService handles file uploads and downloads.
type MediaService interface {
	// Upload stores r as a new file owned by the actor, in the actor's namespace.
	Upload(ctx context.Context, actor Actor, contentType string, r io.Reader) (*UploadResult, error)
	// Download returns a file if it belongs to the actor's namespace; ErrNotFound otherwise
	// (including when it exists elsewhere). The caller closes the reader.
	Download(ctx context.Context, actor Actor, mediaID string) (body io.ReadCloser, contentType string, size int64, err error)
}

// AdminService backs the nimlet CLI: namespace, user, room and membership administration.
// Its callers are operators with direct database access, not end users — there is no HTTP
// admin API. Every name (username, room) is resolved inside the namespace given by slug, so an
// operator acting on one namespace can never touch another's data by naming it. Every
// mutation is audited, whatever its outcome.
type AdminService interface {
	CreateNamespace(ctx context.Context, slug string, allowRegistration bool) (*domain.Namespace, error)
	ListNamespaces(ctx context.Context) ([]*domain.Namespace, error)
	GetNamespace(ctx context.Context, slug string) (*domain.Namespace, error)
	SetNamespaceStatus(ctx context.Context, slug string, status domain.NamespaceStatus) (*domain.Namespace, error)
	SetNamespaceRegistration(ctx context.Context, slug string, allow bool) (*domain.Namespace, error)

	CreateUser(ctx context.Context, cmd AdminCreateUserCmd) (*domain.User, error)
	ListUsers(ctx context.Context, namespace, afterUsername string, limit int) ([]*domain.User, error)
	GetUser(ctx context.Context, namespace, username string) (*domain.User, error)
	SetUserDisabled(ctx context.Context, namespace, username string, disabled bool) error
	SetUserPassword(ctx context.Context, namespace, username, password string) error
	ListSessions(ctx context.Context, namespace, username string) ([]*domain.Session, error)
	RevokeSession(ctx context.Context, namespace, sessionID string) error

	CreateRoom(ctx context.Context, cmd AdminCreateRoomCmd) (*domain.Room, error)
	ListRooms(ctx context.Context, namespace string, limit int) ([]*domain.Room, error)
	GetRoom(ctx context.Context, namespace, roomID string) (*domain.Room, error)
	// AddMember joins username to the room directly (not an invite), with the same effects
	// as a join. An existing joined member is ErrAlreadyExists unless ifNotExists.
	AddMember(ctx context.Context, namespace, roomID, username string, role domain.MemberRole, ifNotExists bool) (*domain.Membership, error)
	ListMembers(ctx context.Context, namespace, roomID string) ([]*AdminMember, error)
	// RemoveMember moves the member to "leave" and revokes their access to the room's
	// history. The last owner cannot be removed.
	RemoveMember(ctx context.Context, namespace, roomID, username string) error
	SetMemberRole(ctx context.Context, namespace, roomID, username string, role domain.MemberRole) (*domain.Membership, error)
}

type AdminCreateUserCmd struct {
	Namespace   string
	Username    string
	DisplayName string
	Type        domain.UserType
	Password    string
}

type AdminCreateRoomCmd struct {
	Namespace string
	Name      string
	Owner     string // username of an existing user of the namespace
	JoinRule  domain.JoinRule
	Alias     string
}

// AdminMember is a membership with the member's username, for listings.
type AdminMember struct {
	domain.Membership
	Username string
}

// --- Command and result types ---

type RegisterCmd struct {
	// Namespace is the slug of the namespace to register in ("" = default). It only selects
	// where the account is created; the namespace must allow public registration.
	Namespace   string
	Username    string
	Password    string
	DisplayName string
	DeviceName  string
	UserType    domain.UserType
}

type RegisterResult struct {
	User   *domain.User
	Device *domain.Device
	Token  string
}

type LoginCmd struct {
	// Namespace is the slug whose accounts Username is looked up in ("" = default). A
	// selector, not an authority: the password is verified for that namespace's account.
	Namespace  string
	Username   string
	Password   string
	DeviceName string
	// DeviceID, if it names an enabled device of this user, is reused instead of creating a
	// new device per login (clients should keep the device_id the first login returned).
	DeviceID string
}

type LoginResult struct {
	Session *domain.Session
	Token   string
}

type CreateRoomCmd struct {
	Name     string
	JoinRule domain.JoinRule
	// Alias is an optional stable external reference. If a room with this alias already
	// exists, CreateRoom returns it instead of creating a new one (idempotent).
	Alias string
	// TTLSeconds, when > 0, sets the room's expires_at; the background sweeper reclaims it.
	TTLSeconds int
}

type InviteCmd struct {
	RoomID    string
	InviteeID string
}

type SendEventCmd struct {
	RoomID    string
	EventType string
	TxnID     string
	Content   json.RawMessage
}

type GetMessagesCmd struct {
	RoomID string
	From   int64
	Dir    string // "f" forward, "b" backward
	Limit  int
}

type SyncCmd struct {
	UserID    string
	Since     int64
	TimeoutMs int64
}

type SyncResult struct {
	NextBatch int64
	Events    []*domain.Event
}

type UploadResult struct {
	MediaID    string
	ContentURI string // mxc://<server name>/<mediaID>
}

// RoomMembership pairs a room with the actor's own membership state/role in it — the shape
// ListRooms returns, since a room list without knowing "am I invited or joined, what's my
// role" isn't enough to render a sidebar.
type RoomMembership struct {
	Room  *domain.Room
	State domain.MembershipState
	Role  domain.MemberRole
}
