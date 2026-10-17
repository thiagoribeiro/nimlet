package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const defaultPageLimit = 50
const maxPageLimit = 100

// GET /rooms page size: users (and bots) can be in thousands of rooms.
const (
	defaultRoomPage = 200
	maxRoomPage     = 1000
)

type roomService struct {
	rooms       out.RoomStore
	memberships out.MembershipStore
	events      out.EventStore
	bus         out.EventBus
	tx          out.Transactor
	members     membershipOps
	dispatcher  in.AppServiceDispatcher
	push        in.PushDispatcher
}

func NewRoomService(
	rooms out.RoomStore,
	memberships out.MembershipStore,
	events out.EventStore,
	bus out.EventBus,
	tx out.Transactor,
	dispatcher in.AppServiceDispatcher,
	push in.PushDispatcher,
) in.RoomService {
	return &roomService{
		rooms:       rooms,
		memberships: memberships,
		events:      events,
		bus:         bus,
		tx:          tx,
		members:     membershipOps{tx: tx, rooms: rooms, memberships: memberships},
		dispatcher:  dispatcher,
		push:        push,
	}
}

func (s *roomService) CreateRoom(ctx context.Context, cmd in.CreateRoomCmd, actor in.Actor) (*domain.Room, error) {
	if err := validateRoomFields(cmd.Name, cmd.Alias); err != nil {
		return nil, fmt.Errorf("room: create: %w", err)
	}
	// Alias'd create is idempotent: an existing room with this alias is returned as-is.
	if cmd.Alias != "" {
		if existing, err := s.rooms.GetByAlias(ctx, actor.NamespaceID, cmd.Alias); err == nil {
			return existing, nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("room: create: lookup alias: %w", err)
		}
	}

	joinRule := cmd.JoinRule
	if joinRule == "" {
		joinRule = domain.JoinRuleInviteOnly
	}
	room := &domain.Room{
		ID:          uuid.NewString(),
		NamespaceID: actor.NamespaceID,
		Name:        cmd.Name,
		CreatorID:   actor.UserID,
		CreatedAt:   time.Now().UTC(),
		MemberCount: 1, // the creator's own membership below
		JoinRule:    joinRule,
		Alias:       cmd.Alias,
	}
	if cmd.TTLSeconds > 0 {
		exp := time.Now().UTC().Add(time.Duration(cmd.TTLSeconds) * time.Second)
		room.ExpiresAt = &exp
	}
	membership := &domain.Membership{
		NamespaceID: actor.NamespaceID,
		RoomID:      room.ID,
		UserID:      actor.UserID,
		State:       domain.MembershipJoin,
		Role:        domain.RoleOwner,
		UpdatedAt:   time.Now().UTC(),
	}

	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.rooms.Create(ctx, room); err != nil {
			return fmt.Errorf("create room: %w", err)
		}
		return s.memberships.Set(ctx, membership)
	})
	if err != nil {
		// Lost a race on the unique alias — return the room the winner created.
		if cmd.Alias != "" {
			if existing, getErr := s.rooms.GetByAlias(ctx, actor.NamespaceID, cmd.Alias); getErr == nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("room: create: %w", err)
	}
	return room, nil
}

func (s *roomService) AddMember(ctx context.Context, roomID, targetUserID string, actor in.Actor) error {
	if err := requireRoomAdmin(ctx, s.memberships, roomID, actor.UserID); err != nil {
		return fmt.Errorf("room: add member: %w", err)
	}
	// The actor's membership puts the room in the actor's namespace; a target user from any
	// other namespace fails the membership's composite FK → ErrNotFound.
	room, err := roomInNamespace(ctx, s.rooms, roomID, actor.NamespaceID)
	if err != nil {
		return fmt.Errorf("room: add member: %w", err)
	}
	_, _, err = s.members.change(ctx, room, targetUserID, domain.ActionAdd, domain.RoleMember, nil)
	if err != nil && !errors.Is(err, domain.ErrMembershipUnchanged) { // already joined: no-op
		return fmt.Errorf("room: add member: %w", err)
	}
	return nil
}

func (s *roomService) InviteUser(ctx context.Context, cmd in.InviteCmd, actor in.Actor) error {
	if err := requireRoomAdmin(ctx, s.memberships, cmd.RoomID, actor.UserID); err != nil {
		return fmt.Errorf("room: invite: %w", err)
	}
	room, err := roomInNamespace(ctx, s.rooms, cmd.RoomID, actor.NamespaceID)
	if err != nil {
		return fmt.Errorf("room: invite: %w", err)
	}
	// Invitee from another namespace: composite FK → ErrNotFound (see AddMember). An
	// existing member is never touched (ErrAlreadyMember); a pending invite is a no-op.
	_, _, err = s.members.change(ctx, room, cmd.InviteeID, domain.ActionInvite, domain.RoleMember, nil)
	if err != nil && !errors.Is(err, domain.ErrMembershipUnchanged) {
		return fmt.Errorf("room: invite: %w", err)
	}
	return nil
}

func (s *roomService) JoinRoom(ctx context.Context, roomID string, actor in.Actor) error {
	// A room of another namespace is indistinguishable from a missing one — even for public
	// rooms, and before any permission check that would reveal it exists.
	room, err := roomInNamespace(ctx, s.rooms, roomID, actor.NamespaceID)
	if err != nil {
		return fmt.Errorf("room: join: %w", err)
	}
	_, _, err = s.members.change(ctx, room, actor.UserID, domain.ActionJoin, "", nil)
	if err != nil && !errors.Is(err, domain.ErrMembershipUnchanged) { // already joined: idempotent
		return fmt.Errorf("room: join: %w", err)
	}
	return nil
}

// GetOrCreateDM returns the actor's DM room with targetID, creating it on first use. A
// target in another namespace (or none) yields ErrNotFound: the DM key is only looked up
// within the actor's namespace, and creating the target's membership fails its composite FK.
func (s *roomService) GetOrCreateDM(ctx context.Context, actor in.Actor, targetID string) (*domain.Room, error) {
	actorID := actor.UserID
	ids := []string{actorID, targetID}
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	directKey := "direct:" + ids[0] + ":" + ids[1]

	room, err := s.rooms.GetByDirectKey(ctx, actor.NamespaceID, directKey)
	if err == nil {
		// Ensure actorID is joined (target may not be yet — DM auto-adds on first use).
		if joinErr := s.ensureJoined(ctx, room, actorID); joinErr != nil {
			return nil, fmt.Errorf("room: dm: ensure actor joined: %w", joinErr)
		}
		return room, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("room: dm: lookup: %w", err)
	}

	// Create the DM room and join both parties atomically.
	room = &domain.Room{
		ID:          uuid.NewString(),
		NamespaceID: actor.NamespaceID,
		Name:        "direct",
		CreatorID:   actorID,
		CreatedAt:   time.Now().UTC(),
		JoinRule:    domain.JoinRuleDirect,
		DirectKey:   directKey,
	}
	now := time.Now().UTC()
	actorMembership := &domain.Membership{
		NamespaceID: actor.NamespaceID, RoomID: room.ID, UserID: actorID,
		State: domain.MembershipJoin, Role: domain.RoleOwner, UpdatedAt: now,
	}
	targetMembership := &domain.Membership{
		NamespaceID: actor.NamespaceID, RoomID: room.ID, UserID: targetID,
		State: domain.MembershipJoin, Role: domain.RoleMember, UpdatedAt: now,
	}

	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.rooms.Create(ctx, room); err != nil {
			return fmt.Errorf("create dm room: %w", err)
		}
		if err := s.memberships.Set(ctx, actorMembership); err != nil {
			return fmt.Errorf("set actor membership: %w", err)
		}
		if err := s.memberships.Set(ctx, targetMembership); err != nil {
			return fmt.Errorf("set target membership: %w", err)
		}
		if err := s.rooms.IncrementMemberCount(ctx, room.ID); err != nil {
			return err
		}
		return s.rooms.IncrementMemberCount(ctx, room.ID)
	})
	if err != nil {
		return nil, fmt.Errorf("room: dm: create: %w", err)
	}
	room.MemberCount = 2
	return room, nil
}

func (s *roomService) ensureJoined(ctx context.Context, room *domain.Room, userID string) error {
	_, _, err := s.members.change(ctx, room, userID, domain.ActionJoin, "", nil)
	if errors.Is(err, domain.ErrMembershipUnchanged) {
		return nil
	}
	return err
}

func (s *roomService) SendEvent(ctx context.Context, cmd in.SendEventCmd, actorID, deviceID string) (*domain.Event, error) {
	m, err := s.memberships.Get(ctx, cmd.RoomID, actorID)
	if err != nil {
		return nil, fmt.Errorf("room: send: get membership: %w", err)
	}
	if !m.CanPublish() {
		return nil, domain.ErrPermissionDenied
	}

	room, err := s.rooms.GetByID(ctx, cmd.RoomID)
	if err != nil {
		return nil, fmt.Errorf("room: send: get room: %w", err)
	}

	event := &domain.Event{
		ID:        uuid.NewString(),
		RoomID:    cmd.RoomID,
		SenderID:  actorID,
		EventType: cmd.EventType,
		Content:   cmd.Content,
		TxnKey:    deviceID + ":" + cmd.TxnID,
		CreatedAt: time.Now().UTC(),
		// Not stored; routes AppService delivery to the room's namespace only.
		NamespaceID: room.NamespaceID,
	}

	if room.IsLarge() {
		// Large room: no per-user visibility rows; one room-level wake-up.
		// Push is intentionally skipped here too — listing every joined member just to
		// push-notify them would reintroduce the O(N) fan-out this path exists to avoid.
		created, _, err := s.events.Create(ctx, event, false)
		if err != nil {
			return nil, fmt.Errorf("room: send: %w", err)
		}
		if created.ID != event.ID {
			return created, nil // idempotent replay: the original send already notified
		}
		go func() { _ = s.bus.Publish(context.WithoutCancel(ctx), "room:"+cmd.RoomID) }()
		s.dispatcher.Dispatch(created)
		return created, nil
	}

	// Small room: the store writes one visibility row per joined member and returns them.
	created, members, err := s.events.Create(ctx, event, true)
	if err != nil {
		return nil, fmt.Errorf("room: send: %w", err)
	}
	if created.ID != event.ID {
		// Idempotent replay (txn already used): the original send already woke and
		// notified everyone; re-notifying would duplicate pushes and AppService deliveries.
		return created, nil
	}

	// Publish wake-ups post-commit, fire-and-forget.
	// If Redis is unavailable, clients will timeout and re-poll — acceptable degradation.
	go s.publishWakeups(context.WithoutCancel(ctx), members)
	s.dispatcher.Dispatch(created)
	if !created.AgentActivity() {
		recipients := make([]string, 0, len(members))
		for _, id := range members {
			if id != actorID {
				recipients = append(recipients, id)
			}
		}
		s.push.Dispatch(created, recipients, room.Name)
	}

	return created, nil
}

func (s *roomService) GetMessages(ctx context.Context, cmd in.GetMessagesCmd, actorID string) ([]*domain.Event, error) {
	m, err := s.memberships.Get(ctx, cmd.RoomID, actorID)
	if err != nil {
		return nil, fmt.Errorf("room: messages: %w", err)
	}
	if !m.IsJoined() {
		return nil, domain.ErrRoomNotJoined
	}

	limit := cmd.Limit
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}

	events, err := s.events.GetByRoom(ctx, cmd.RoomID, cmd.From, cmd.Dir, limit)
	if err != nil {
		return nil, fmt.Errorf("room: messages: %w", err)
	}
	return events, nil
}

// ListRooms pages through the actor's rooms by room ID. next is the after-value of the
// following page, empty on the last one.
func (s *roomService) ListRooms(ctx context.Context, actorID, afterRoomID string, limit int) ([]*in.RoomMembership, string, error) {
	if limit <= 0 || limit > maxRoomPage {
		limit = defaultRoomPage
	}
	// One extra row tells whether another page exists without a COUNT.
	memberships, err := s.memberships.ListByUser(ctx, actorID, afterRoomID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("room: list rooms: list memberships: %w", err)
	}
	next := ""
	if len(memberships) > limit {
		memberships = memberships[:limit]
		next = memberships[limit-1].RoomID
	}
	if len(memberships) == 0 {
		return nil, "", nil
	}

	ids := make([]string, len(memberships))
	for i, m := range memberships {
		ids[i] = m.RoomID
	}
	rooms, err := s.rooms.ListByIDs(ctx, ids)
	if err != nil {
		return nil, "", fmt.Errorf("room: list rooms: list rooms: %w", err)
	}
	roomsByID := make(map[string]*domain.Room, len(rooms))
	for _, r := range rooms {
		roomsByID[r.ID] = r
	}

	result := make([]*in.RoomMembership, 0, len(memberships))
	for _, m := range memberships {
		room, ok := roomsByID[m.RoomID]
		if !ok {
			continue // room was deleted or otherwise missing; skip rather than fail the whole list
		}
		result = append(result, &in.RoomMembership{Room: room, State: m.State, Role: m.Role})
	}
	return result, next, nil
}

// publishWakeups delivers wake-up notifications via a single Redis Pipeline round-trip.
// Errors are logged, not returned: the events are already committed, and on Redis
// unavailability clients time out and re-poll.
func (s *roomService) publishWakeups(ctx context.Context, keys []string) {
	if err := s.bus.PublishBatch(ctx, keys); err != nil {
		slog.WarnContext(ctx, "room: publish wake-ups", "keys", len(keys), "error", err)
	}
}

func validateRoomFields(name, alias string) error {
	return errors.Join(
		domain.ValidateText("name", name, domain.MaxRoomNameLen),
		domain.ValidateText("alias", alias, domain.MaxAliasLen),
	)
}
