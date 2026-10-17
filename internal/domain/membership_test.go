package domain

import (
	"errors"
	"testing"
)

func TestNextMembership(t *testing.T) {
	inviteOnly := &Room{JoinRule: JoinRuleInviteOnly}
	public := &Room{JoinRule: JoinRulePublic}
	dm := &Room{JoinRule: JoinRuleDirect, DirectKey: "direct:alice:bob"}
	m := func(s MembershipState, r MemberRole) *Membership { return &Membership{State: s, Role: r} }

	tests := []struct {
		name      string
		cur       *Membership
		action    MembershipAction
		room      *Room
		user      string
		wantState MembershipState
		wantRole  MemberRole
		wantErr   error
	}{
		// join
		{"join invite-only without invite", nil, ActionJoin, inviteOnly, "u", "", "", ErrPermissionDenied},
		{"join public", nil, ActionJoin, public, "u", MembershipJoin, RoleMember, nil},
		{"accept invite keeps role", m(MembershipInvite, RoleAdmin), ActionJoin, inviteOnly, "u", MembershipJoin, RoleAdmin, nil},
		{"rejoin invite-only after leave", m(MembershipLeave, RoleMember), ActionJoin, inviteOnly, "u", "", "", ErrPermissionDenied},
		{"rejoin public after leave drops old role", m(MembershipLeave, RoleOwner), ActionJoin, public, "u", MembershipJoin, RoleMember, nil},
		{"banned cannot join public", m(MembershipBan, RoleMember), ActionJoin, public, "u", "", "", ErrPermissionDenied},
		{"join twice", m(MembershipJoin, RoleMember), ActionJoin, public, "u", "", "", ErrMembershipUnchanged},
		{"stranger joins DM", nil, ActionJoin, dm, "mallory", "", "", ErrPermissionDenied},
		{"participant rejoins DM", m(MembershipLeave, RoleMember), ActionJoin, dm, "bob", MembershipJoin, RoleMember, nil},
		// invite
		{"invite new", nil, ActionInvite, inviteOnly, "u", MembershipInvite, RoleMember, nil},
		{"invite cannot demote a member", m(MembershipJoin, RoleOwner), ActionInvite, inviteOnly, "u", "", "", ErrAlreadyMember},
		{"invite cannot unban", m(MembershipBan, RoleMember), ActionInvite, inviteOnly, "u", "", "", ErrPermissionDenied},
		{"re-invite after leave", m(MembershipLeave, RoleOwner), ActionInvite, inviteOnly, "u", MembershipInvite, RoleMember, nil},
		{"invite into DM", nil, ActionInvite, dm, "carol", "", "", ErrPermissionDenied},
		// add
		{"add new", nil, ActionAdd, inviteOnly, "u", MembershipJoin, RoleMember, nil},
		{"add existing member", m(MembershipJoin, RoleOwner), ActionAdd, inviteOnly, "u", "", "", ErrMembershipUnchanged},
		{"add cannot unban", m(MembershipBan, RoleMember), ActionAdd, inviteOnly, "u", "", "", ErrPermissionDenied},
		{"add into DM", nil, ActionAdd, dm, "alice", "", "", ErrPermissionDenied},
		// remove
		{"remove member", m(MembershipJoin, RoleAdmin), ActionRemove, inviteOnly, "u", MembershipLeave, RoleAdmin, nil},
		{"remove invite", m(MembershipInvite, RoleMember), ActionRemove, inviteOnly, "u", MembershipLeave, RoleMember, nil},
		{"remove non-member", nil, ActionRemove, inviteOnly, "u", "", "", ErrNotFound},
		{"remove already left", m(MembershipLeave, RoleMember), ActionRemove, inviteOnly, "u", "", "", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, role, err := NextMembership(tt.cur, tt.action, tt.room, tt.user, "")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if state != tt.wantState || role != tt.wantRole {
				t.Fatalf("got (%s, %s), want (%s, %s)", state, role, tt.wantState, tt.wantRole)
			}
		})
	}
}

func TestMemberCountDelta(t *testing.T) {
	join := &Membership{State: MembershipJoin}
	invite := &Membership{State: MembershipInvite}
	for _, tt := range []struct {
		cur  *Membership
		next MembershipState
		want int
	}{
		{nil, MembershipJoin, 1}, {invite, MembershipJoin, 1}, {join, MembershipLeave, -1},
		{invite, MembershipLeave, 0}, {nil, MembershipInvite, 0},
	} {
		if got := MemberCountDelta(tt.cur, tt.next); got != tt.want {
			t.Errorf("MemberCountDelta(%v, %s) = %d, want %d", tt.cur, tt.next, got, tt.want)
		}
	}
}

func TestRoomIsParticipant(t *testing.T) {
	dm := &Room{DirectKey: "direct:a1:b2"}
	if !dm.IsParticipant("a1") || !dm.IsParticipant("b2") || dm.IsParticipant("c3") || dm.IsParticipant("direct") {
		t.Fatal("IsParticipant wrong for DM")
	}
	if !(&Room{}).IsParticipant("anyone") {
		t.Fatal("non-DM rooms admit anyone")
	}
}
