package domain

import (
	"strings"
	"time"
)

// LargeRoomThreshold is the member count from which a room stops getting per-member
// event_visibility rows and is synced by room instead (see README, read/write model).
const LargeRoomThreshold = 1000

type JoinRule string

const (
	JoinRuleInviteOnly JoinRule = "invite_only"
	JoinRulePublic     JoinRule = "public"
	JoinRuleDirect     JoinRule = "direct"
)

type Room struct {
	ID          string
	NamespaceID string
	Name        string
	CreatorID   string
	CreatedAt   time.Time
	MemberCount int
	JoinRule    JoinRule
	DirectKey   string     // non-empty only for DM rooms
	Alias       string     // optional stable external reference; empty when unset
	ExpiresAt   *time.Time // optional TTL; nil = never expires
}

func (r *Room) IsLarge() bool  { return r.MemberCount >= LargeRoomThreshold }
func (r *Room) IsDirect() bool { return r.DirectKey != "" }

// IsParticipant reports whether userID is one of the two people a direct-message room is
// for (its DirectKey is "direct:<idA>:<idB>"). Always true for non-direct rooms.
func (r *Room) IsParticipant(userID string) bool {
	if !r.IsDirect() {
		return true
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(r.DirectKey, "direct:"), ":")
	return ok && (userID == a || userID == b)
}
