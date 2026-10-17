package domain

import (
	"errors"
	"time"
)

type MembershipState string

const (
	MembershipInvite MembershipState = "invite"
	MembershipJoin   MembershipState = "join"
	MembershipLeave  MembershipState = "leave"
	MembershipBan    MembershipState = "ban"
)

type MemberRole string

const (
	RoleOwner  MemberRole = "owner"
	RoleAdmin  MemberRole = "admin"
	RoleMember MemberRole = "member"
	RoleGuest  MemberRole = "guest"
	RoleAgent  MemberRole = "agent"
	RoleBot    MemberRole = "bot"
)

type Membership struct {
	NamespaceID string // always the room's (and the user's) namespace
	RoomID      string
	UserID      string
	State       MembershipState
	Role        MemberRole
	UpdatedAt   time.Time
}

func (m *Membership) IsJoined() bool {
	return m.State == MembershipJoin
}

func (m *Membership) CanPublish() bool {
	switch m.Role {
	case RoleOwner, RoleAdmin, RoleMember, RoleAgent, RoleBot:
		return m.IsJoined()
	default:
		return false
	}
}

// MembershipAction is something that changes a membership.
type MembershipAction int

const (
	ActionJoin   MembershipAction = iota // the user joins by themselves
	ActionInvite                         // a room admin invites the user
	ActionAdd                            // an admin (AppService, CLI) joins the user directly
	ActionRemove                         // an admin removes the user
)

// ErrMembershipUnchanged means the membership is already where the action would put it.
// Callers decide whether that is success (idempotent join) or a conflict (CLI add).
var ErrMembershipUnchanged = errors.New("membership unchanged")

// NextMembership is the single source of truth for membership transitions. Given the
// current membership (nil = none), it returns the state and role after action, or:
//   - ErrMembershipUnchanged when nothing would change;
//   - ErrPermissionDenied when the action is not allowed from the current state;
//   - ErrAlreadyMember when inviting someone already joined;
//   - ErrNotFound when removing a non-member.
//
// Rules: a ban is final (nothing but an explicit unban, which does not exist yet, lifts
// it); invites never touch an existing member's state or role; leaving an invite-only room
// means a new invite is needed to come back; a direct-message room only ever admits its two
// participants and never gains members through invites or adds.
func NextMembership(cur *Membership, action MembershipAction, room *Room, userID string, role MemberRole) (MembershipState, MemberRole, error) {
	if role == "" {
		role = RoleMember
	}
	var state MembershipState
	if cur != nil {
		state = cur.State
	}
	if state == MembershipBan && action != ActionRemove {
		return "", "", ErrPermissionDenied
	}
	if room.IsDirect() && (action == ActionInvite || action == ActionAdd || !room.IsParticipant(userID)) {
		return "", "", ErrPermissionDenied
	}
	openToJoin := room.JoinRule == JoinRulePublic || room.JoinRule == JoinRuleDirect

	switch action {
	case ActionJoin:
		switch state {
		case MembershipJoin:
			return "", "", ErrMembershipUnchanged
		case MembershipInvite:
			return MembershipJoin, cur.Role, nil // keep the role the inviter chose
		case "", MembershipLeave:
			if !openToJoin {
				return "", "", ErrPermissionDenied
			}
			return MembershipJoin, RoleMember, nil // rejoining never restores an old role
		}
	case ActionInvite:
		switch state {
		case MembershipJoin:
			return "", "", ErrAlreadyMember
		case MembershipInvite:
			return "", "", ErrMembershipUnchanged
		case "", MembershipLeave:
			return MembershipInvite, role, nil
		}
	case ActionAdd:
		switch state {
		case MembershipJoin:
			return "", "", ErrMembershipUnchanged
		case "", MembershipInvite, MembershipLeave:
			return MembershipJoin, role, nil
		}
	case ActionRemove:
		switch state {
		case MembershipJoin, MembershipInvite:
			return MembershipLeave, cur.Role, nil
		default:
			return "", "", ErrNotFound
		}
	}
	return "", "", ErrPermissionDenied
}

// MemberCountDelta is how rooms.member_count moves when a membership goes from cur
// (nil = none) to next.
func MemberCountDelta(cur *Membership, next MembershipState) int {
	was := cur != nil && cur.State == MembershipJoin
	is := next == MembershipJoin
	switch {
	case is && !was:
		return 1
	case was && !is:
		return -1
	}
	return 0
}
