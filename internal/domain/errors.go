package domain

import "errors"

var (
	ErrNotFound         = errors.New("not found")
	ErrAlreadyExists    = errors.New("already exists")
	ErrPermissionDenied = errors.New("permission denied")
	ErrInvalidToken     = errors.New("invalid token")
	ErrSessionExpired   = errors.New("session expired")
	ErrUserDisabled     = errors.New("user disabled")
	ErrDeviceDisabled   = errors.New("device disabled")
	ErrRoomNotJoined    = errors.New("user has not joined the room")
	ErrAlreadyMember    = errors.New("already a member")
	ErrDuplicateTxn     = errors.New("duplicate transaction")
	ErrInvalidPassword  = errors.New("invalid password")
	// ErrNamespaceDisabled: the namespace exists but is disabled (no login, writes or sync).
	ErrNamespaceDisabled = errors.New("namespace disabled")
	// ErrRegistrationClosed: public registration is not enabled for the namespace.
	ErrRegistrationClosed = errors.New("registration closed")
	// ErrInvalidArgument: the request is malformed or asks for something not allowed.
	ErrInvalidArgument = errors.New("invalid argument")
)
