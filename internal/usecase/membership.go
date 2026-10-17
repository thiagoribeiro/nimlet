package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// membershipOps is the one place memberships change (besides a new room's first members),
// shared by the room and admin use cases so their rules cannot drift apart.
type membershipOps struct {
	tx          out.Transactor
	rooms       out.RoomStore
	memberships out.MembershipStore
}

// change applies action to userID's membership of room in one transaction. The row is
// locked while domain.NextMembership decides, and rooms.member_count moves only when the
// joined state really changes — concurrent joins count once. within, if set, runs in the
// same transaction after the decision (e.g. to enforce extra rules or revoke history);
// its error rolls everything back. Returns the membership before and after.
func (o membershipOps) change(ctx context.Context, room *domain.Room, userID string,
	action domain.MembershipAction, role domain.MemberRole,
	within func(ctx context.Context, cur, next *domain.Membership) error,
) (cur, next *domain.Membership, err error) {
	err = o.tx.WithTx(ctx, func(ctx context.Context) error {
		var decideErr error
		cur, err = o.memberships.Transition(ctx, room.NamespaceID, room.ID, userID,
			func(c *domain.Membership) (*domain.Membership, error) {
				state, r, err := domain.NextMembership(c, action, room, userID, role)
				if err != nil {
					decideErr = err
					return nil, err
				}
				next = &domain.Membership{
					NamespaceID: room.NamespaceID, RoomID: room.ID, UserID: userID,
					State: state, Role: r, UpdatedAt: time.Now().UTC(),
				}
				return next, nil
			})
		if err != nil {
			if decideErr != nil {
				return decideErr
			}
			return err
		}
		if within != nil {
			if err := within(ctx, cur, next); err != nil {
				return err
			}
		}
		switch domain.MemberCountDelta(cur, next.State) {
		case 1:
			return o.rooms.IncrementMemberCount(ctx, room.ID)
		case -1:
			return o.rooms.DecrementMemberCount(ctx, room.ID)
		}
		return nil
	})
	return cur, next, err
}

// requireRoomAdmin checks that userID is a joined owner or admin of roomID. A non-member
// gets ErrRoomNotJoined, never a hint about the room's other members.
func requireRoomAdmin(ctx context.Context, memberships out.MembershipStore, roomID, userID string) error {
	m, err := memberships.Get(ctx, roomID, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrRoomNotJoined
		}
		return fmt.Errorf("get actor membership: %w", err)
	}
	if !m.IsJoined() {
		return domain.ErrRoomNotJoined
	}
	if m.Role != domain.RoleOwner && m.Role != domain.RoleAdmin {
		return domain.ErrPermissionDenied
	}
	return nil
}

// roomInNamespace loads a room, reporting one of another namespace as missing.
func roomInNamespace(ctx context.Context, rooms out.RoomStore, roomID, namespaceID string) (*domain.Room, error) {
	room, err := rooms.GetByID(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if room.NamespaceID != namespaceID {
		return nil, domain.ErrNotFound
	}
	return room, nil
}
