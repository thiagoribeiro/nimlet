package usecase_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
	"github.com/thiagoribeiro/nimlet/testkit"
)

// actor is a user of the default namespace, the namespace MockRoomStore's rooms live in.
func actor(userID string) in.Actor {
	return in.Actor{NamespaceID: domain.DefaultNamespaceID, UserID: userID}
}

func newRoomSvc(rooms *testkit.MockRoomStore, memberships *testkit.MockMembershipStore, events *testkit.MockEventStore, bus *testkit.MockEventBus) in.RoomService {
	return usecase.NewRoomService(rooms, memberships, events, bus, &testkit.MockTransactor{}, &testkit.MockAppServiceDispatcher{}, &testkit.MockPushDispatcher{})
}

func joinedMembership(roomID, userID string, role domain.MemberRole) *domain.Membership {
	return &domain.Membership{
		RoomID: roomID, UserID: userID,
		State: domain.MembershipJoin, Role: role,
		UpdatedAt: time.Now(),
	}
}

// --- CreateRoom ---

func TestRoom_CreateRoom_SetsOwnerMembership(t *testing.T) {
	var savedMembership *domain.Membership

	svc := newRoomSvc(
		&testkit.MockRoomStore{
			CreateFn: func(_ context.Context, _ *domain.Room) error { return nil },
		},
		&testkit.MockMembershipStore{
			SetFn: func(_ context.Context, m *domain.Membership) error {
				savedMembership = m
				return nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	room, err := svc.CreateRoom(context.Background(), in.CreateRoomCmd{Name: "general"}, actor("user1"))

	require.NoError(t, err)
	assert.Equal(t, "general", room.Name)
	assert.Equal(t, "user1", room.CreatorID)
	require.NotNil(t, savedMembership)
	assert.Equal(t, domain.RoleOwner, savedMembership.Role)
	assert.Equal(t, domain.MembershipJoin, savedMembership.State)
}

// --- InviteUser ---

func TestRoom_InviteUser_ByOwner_Succeeds(t *testing.T) {
	roomID := uuid.NewString()
	var savedInvite *domain.Membership

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				if userID == "user1" {
					return joinedMembership(roomID, userID, domain.RoleOwner), nil
				}
				return nil, domain.ErrNotFound // the invitee has no membership yet
			},
			SetFn: func(_ context.Context, m *domain.Membership) error {
				savedInvite = m
				return nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	err := svc.InviteUser(context.Background(), in.InviteCmd{RoomID: roomID, InviteeID: "user2"}, actor("user1"))

	require.NoError(t, err)
	require.NotNil(t, savedInvite)
	assert.Equal(t, domain.MembershipInvite, savedInvite.State)
	assert.Equal(t, "user2", savedInvite.UserID)
}

func TestRoom_InviteUser_ByMember_ReturnsPermissionDenied(t *testing.T) {
	roomID := uuid.NewString()

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return joinedMembership(roomID, userID, domain.RoleMember), nil // not owner/admin
			},
			SetFn: func(_ context.Context, _ *domain.Membership) error { return nil },
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	err := svc.InviteUser(context.Background(), in.InviteCmd{RoomID: roomID, InviteeID: "user2"}, actor("user1"))

	require.ErrorIs(t, err, domain.ErrPermissionDenied)
}

// --- JoinRoom ---

func TestRoom_JoinRoom_FromInvite_Succeeds(t *testing.T) {
	roomID := uuid.NewString()
	var updatedMembership *domain.Membership

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return &domain.Membership{
					RoomID: roomID, UserID: userID,
					State: domain.MembershipInvite, Role: domain.RoleMember,
				}, nil
			},
			SetFn: func(_ context.Context, m *domain.Membership) error {
				updatedMembership = m
				return nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	require.NoError(t, svc.JoinRoom(context.Background(), roomID, actor("user2")))
	require.NotNil(t, updatedMembership)
	assert.Equal(t, domain.MembershipJoin, updatedMembership.State)
}

func TestRoom_JoinRoom_AlreadyJoined_IsIdempotent(t *testing.T) {
	roomID := uuid.NewString()
	setCalled := false

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return joinedMembership(roomID, userID, domain.RoleMember), nil
			},
			SetFn: func(_ context.Context, _ *domain.Membership) error {
				setCalled = true
				return nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	require.NoError(t, svc.JoinRoom(context.Background(), roomID, actor("user2")))
	assert.False(t, setCalled, "Set must not be called when already joined")
}

// --- SendEvent ---

func TestRoom_SendEvent_Success_PublishesWakeup(t *testing.T) {
	roomID := uuid.NewString()
	var mu sync.Mutex
	published := make([]string, 0)

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return joinedMembership(roomID, userID, domain.RoleMember), nil
			},
		},
		&testkit.MockEventStore{
			CreateFn: func(_ context.Context, e *domain.Event, fanOut bool) (*domain.Event, []string, error) {
				require.True(t, fanOut, "small rooms fan out")
				e.StreamPos = 1
				return e, []string{"user1", "user2"}, nil // the joined members the store wrote rows for
			},
		},
		&testkit.MockEventBus{
			PublishFn: func(_ context.Context, userID string) error {
				mu.Lock()
				published = append(published, userID)
				mu.Unlock()
				return nil
			},
		},
	)

	event, err := svc.SendEvent(context.Background(), in.SendEventCmd{
		RoomID:    roomID,
		EventType: "m.room.message",
		TxnID:     "txn1",
		Content:   json.RawMessage(`{"body":"hello"}`),
	}, "user1", "device1")

	require.NoError(t, err)
	assert.Equal(t, int64(1), event.StreamPos)

	// Give the fire-and-forget goroutine time to publish.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), published...)
	mu.Unlock()
	assert.ElementsMatch(t, []string{"user1", "user2"}, got)
}

func TestRoom_SendEvent_NotJoined_ReturnsPermissionDenied(t *testing.T) {
	roomID := uuid.NewString()

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return &domain.Membership{
					RoomID: roomID, UserID: userID,
					State: domain.MembershipInvite, Role: domain.RoleMember,
				}, nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	_, err := svc.SendEvent(context.Background(), in.SendEventCmd{
		RoomID: roomID, EventType: "m.room.message", TxnID: "t1",
		Content: json.RawMessage(`{}`),
	}, "user1", "device1")

	require.ErrorIs(t, err, domain.ErrPermissionDenied)
}

func TestRoom_SendEvent_IdempotentReturn(t *testing.T) {
	roomID := uuid.NewString()
	existing := &domain.Event{ID: "original-evt", StreamPos: 42}
	callCount := 0

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return joinedMembership(roomID, userID, domain.RoleMember), nil
			},
		},
		&testkit.MockEventStore{
			CreateFn: func(_ context.Context, _ *domain.Event, _ bool) (*domain.Event, []string, error) {
				callCount++
				return existing, nil, nil // always returns same event (idempotent)
			},
		},
		&testkit.MockEventBus{},
	)

	cmd := in.SendEventCmd{RoomID: roomID, EventType: "m.room.message", TxnID: "same-txn", Content: json.RawMessage(`{}`)}

	e1, _ := svc.SendEvent(context.Background(), cmd, "user1", "device1")
	e2, _ := svc.SendEvent(context.Background(), cmd, "user1", "device1")

	assert.Equal(t, e1.ID, e2.ID)
	assert.Equal(t, e1.StreamPos, e2.StreamPos)
}

// --- GetMessages ---

func TestRoom_GetMessages_NotJoined_ReturnsErrRoomNotJoined(t *testing.T) {
	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, _ string) (*domain.Membership, error) {
				return &domain.Membership{State: domain.MembershipInvite}, nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	_, err := svc.GetMessages(context.Background(), in.GetMessagesCmd{RoomID: "room1"}, "user1")

	require.ErrorIs(t, err, domain.ErrRoomNotJoined)
}

// --- ListRooms ---

func TestRoom_ListRooms_JoinsMembershipsWithRooms(t *testing.T) {
	room1 := &domain.Room{ID: "room1", Name: "General"}
	room2 := &domain.Room{ID: "room2", Name: "Random"}

	svc := newRoomSvc(
		&testkit.MockRoomStore{
			ListByIDsFn: func(_ context.Context, ids []string) ([]*domain.Room, error) {
				assert.ElementsMatch(t, []string{"room1", "room2"}, ids)
				return []*domain.Room{room1, room2}, nil
			},
		},
		&testkit.MockMembershipStore{
			ListByUserFn: func(_ context.Context, userID string) ([]*domain.Membership, error) {
				assert.Equal(t, "user1", userID)
				return []*domain.Membership{
					{RoomID: "room1", UserID: "user1", State: domain.MembershipJoin, Role: domain.RoleOwner},
					{RoomID: "room2", UserID: "user1", State: domain.MembershipInvite, Role: domain.RoleMember},
				}, nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	result, _, err := svc.ListRooms(context.Background(), "user1", "", 0)

	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, room1, result[0].Room)
	assert.Equal(t, domain.MembershipJoin, result[0].State)
	assert.Equal(t, domain.RoleOwner, result[0].Role)
	assert.Equal(t, room2, result[1].Room)
	assert.Equal(t, domain.MembershipInvite, result[1].State)
}

func TestRoom_ListRooms_NoMemberships_ReturnsEmpty(t *testing.T) {
	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			ListByUserFn: func(_ context.Context, _ string) ([]*domain.Membership, error) {
				return nil, nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	result, _, err := svc.ListRooms(context.Background(), "user1", "", 0)

	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestRoom_ListRooms_SkipsRoomsMissingFromRoomStore(t *testing.T) {
	room1 := &domain.Room{ID: "room1", Name: "General"}

	svc := newRoomSvc(
		&testkit.MockRoomStore{
			// room2 was deleted after the membership row was written; ListByIDs simply omits it.
			ListByIDsFn: func(_ context.Context, _ []string) ([]*domain.Room, error) {
				return []*domain.Room{room1}, nil
			},
		},
		&testkit.MockMembershipStore{
			ListByUserFn: func(_ context.Context, _ string) ([]*domain.Membership, error) {
				return []*domain.Membership{
					{RoomID: "room1", UserID: "user1", State: domain.MembershipJoin, Role: domain.RoleOwner},
					{RoomID: "room2", UserID: "user1", State: domain.MembershipJoin, Role: domain.RoleMember},
				}, nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	result, _, err := svc.ListRooms(context.Background(), "user1", "", 0)

	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, room1, result[0].Room)
}

// --- CreateRoom: alias + TTL ---

func TestRoom_CreateRoom_ExistingAlias_ReturnsExisting(t *testing.T) {
	existing := &domain.Room{ID: "room-existing", Name: "merchant_1", Alias: "merchant:1"}
	createCalled := false

	svc := newRoomSvc(
		&testkit.MockRoomStore{
			GetByAliasFn: func(_ context.Context, _, alias string) (*domain.Room, error) {
				assert.Equal(t, "merchant:1", alias)
				return existing, nil
			},
			CreateFn: func(_ context.Context, _ *domain.Room) error { createCalled = true; return nil },
		},
		&testkit.MockMembershipStore{SetFn: func(_ context.Context, _ *domain.Membership) error { return nil }},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	room, err := svc.CreateRoom(context.Background(), in.CreateRoomCmd{Name: "merchant_1", Alias: "merchant:1"}, actor("as-user"))

	require.NoError(t, err)
	assert.Equal(t, existing, room)
	assert.False(t, createCalled, "should not create a second room for an existing alias")
}

func TestRoom_CreateRoom_TTL_SetsExpiresAt(t *testing.T) {
	var saved *domain.Room

	svc := newRoomSvc(
		&testkit.MockRoomStore{
			GetByAliasFn: func(_ context.Context, _, _ string) (*domain.Room, error) { return nil, domain.ErrNotFound },
			CreateFn:     func(_ context.Context, r *domain.Room) error { saved = r; return nil },
		},
		&testkit.MockMembershipStore{SetFn: func(_ context.Context, _ *domain.Membership) error { return nil }},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	_, err := svc.CreateRoom(context.Background(), in.CreateRoomCmd{Name: "order_1", TTLSeconds: 3600}, actor("as-user"))

	require.NoError(t, err)
	require.NotNil(t, saved)
	require.NotNil(t, saved.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *saved.ExpiresAt, 5*time.Second)
}

// --- AddMember ---

func TestRoom_AddMember_ByOwner_JoinsAndBumpsCount(t *testing.T) {
	roomID := uuid.NewString()
	var saved *domain.Membership
	incremented := false

	svc := newRoomSvc(
		&testkit.MockRoomStore{
			IncrementMemberCountFn: func(_ context.Context, _ string) error { incremented = true; return nil },
		},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				if userID == "as-user" {
					return joinedMembership(roomID, userID, domain.RoleOwner), nil
				}
				return nil, domain.ErrNotFound
			},
			SetFn: func(_ context.Context, m *domain.Membership) error { saved = m; return nil },
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	err := svc.AddMember(context.Background(), roomID, "customer-9", actor("as-user"))

	require.NoError(t, err)
	require.NotNil(t, saved)
	assert.Equal(t, "customer-9", saved.UserID)
	assert.Equal(t, domain.MembershipJoin, saved.State)
	assert.Equal(t, domain.RoleMember, saved.Role)
	assert.True(t, incremented)
}

func TestRoom_AddMember_ByNonOwner_ReturnsPermissionDenied(t *testing.T) {
	roomID := uuid.NewString()

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
				return joinedMembership(roomID, userID, domain.RoleMember), nil
			},
			SetFn: func(_ context.Context, _ *domain.Membership) error { return nil },
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	err := svc.AddMember(context.Background(), roomID, "customer-9", actor("as-user"))

	require.ErrorIs(t, err, domain.ErrPermissionDenied)
}

func TestRoom_AddMember_AlreadyJoined_IsIdempotent(t *testing.T) {
	roomID := uuid.NewString()
	setCalled := false

	svc := newRoomSvc(
		&testkit.MockRoomStore{},
		&testkit.MockMembershipStore{
			GetFn: func(_ context.Context, _, _ string) (*domain.Membership, error) {
				return joinedMembership(roomID, "any", domain.RoleOwner), nil
			},
			SetFn: func(_ context.Context, _ *domain.Membership) error { setCalled = true; return nil },
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	err := svc.AddMember(context.Background(), roomID, "customer-9", actor("as-user"))

	require.NoError(t, err)
	assert.False(t, setCalled, "already-joined target should be a no-op")
}

// A room of another namespace behaves as nonexistent — even a public one, and before any
// permission check could reveal that it exists.
func TestRoom_JoinRoom_OtherNamespace_NotFound(t *testing.T) {
	roomID := uuid.NewString()
	svc := newRoomSvc(
		&testkit.MockRoomStore{GetByIDFn: func(_ context.Context, id string) (*domain.Room, error) {
			return &domain.Room{ID: id, NamespaceID: "ns-other", JoinRule: domain.JoinRulePublic}, nil
		}},
		&testkit.MockMembershipStore{
			SetFn: func(_ context.Context, _ *domain.Membership) error {
				t.Fatal("membership must not be written")
				return nil
			},
		},
		&testkit.MockEventStore{},
		&testkit.MockEventBus{},
	)

	err := svc.JoinRoom(context.Background(), roomID, actor("user2"))
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// membershipsOf builds a MockMembershipStore over a fixed state map, recording writes.
func membershipsOf(roomID string, state map[string]*domain.Membership, written *[]*domain.Membership) *testkit.MockMembershipStore {
	return &testkit.MockMembershipStore{
		GetFn: func(_ context.Context, _, userID string) (*domain.Membership, error) {
			if m, ok := state[userID]; ok {
				return m, nil
			}
			return nil, domain.ErrNotFound
		},
		SetFn: func(_ context.Context, m *domain.Membership) error {
			*written = append(*written, m)
			return nil
		},
	}
}

// Regression (review P0-2): inviting an existing member used to overwrite their state and
// role, demoting an owner to an invited member.
func TestRoom_InviteUser_CannotDemoteExistingMember(t *testing.T) {
	roomID := uuid.NewString()
	var written []*domain.Membership
	svc := newRoomSvc(&testkit.MockRoomStore{}, membershipsOf(roomID, map[string]*domain.Membership{
		"admin": joinedMembership(roomID, "admin", domain.RoleAdmin),
		"owner": joinedMembership(roomID, "owner", domain.RoleOwner),
	}, &written), &testkit.MockEventStore{}, &testkit.MockEventBus{})

	err := svc.InviteUser(context.Background(), in.InviteCmd{RoomID: roomID, InviteeID: "owner"}, actor("admin"))
	require.ErrorIs(t, err, domain.ErrAlreadyMember)
	assert.Empty(t, written, "the owner's membership must not be rewritten")
}

// Regression (review P0-1): any non-joined membership used to be treated as an invite, so a
// member who left (or was removed) or was banned could rejoin an invite-only room.
func TestRoom_JoinRoom_LeftOrBannedCannotRejoinInviteOnly(t *testing.T) {
	for _, state := range []domain.MembershipState{domain.MembershipLeave, domain.MembershipBan} {
		t.Run(string(state), func(t *testing.T) {
			roomID := uuid.NewString()
			var written []*domain.Membership
			svc := newRoomSvc(&testkit.MockRoomStore{
				GetByIDFn: func(_ context.Context, id string) (*domain.Room, error) {
					return &domain.Room{ID: id, NamespaceID: domain.DefaultNamespaceID, JoinRule: domain.JoinRuleInviteOnly}, nil
				},
			}, membershipsOf(roomID, map[string]*domain.Membership{
				"u": {RoomID: roomID, UserID: "u", State: state, Role: domain.RoleMember},
			}, &written), &testkit.MockEventStore{}, &testkit.MockEventBus{})

			err := svc.JoinRoom(context.Background(), roomID, actor("u"))
			require.ErrorIs(t, err, domain.ErrPermissionDenied)
			assert.Empty(t, written)
		})
	}
}

// Regression (review): any user who knew a DM room's ID could join it and read it.
func TestRoom_JoinRoom_StrangerCannotJoinDM(t *testing.T) {
	roomID := uuid.NewString()
	var written []*domain.Membership
	svc := newRoomSvc(&testkit.MockRoomStore{
		GetByIDFn: func(_ context.Context, id string) (*domain.Room, error) {
			return &domain.Room{ID: id, NamespaceID: domain.DefaultNamespaceID, JoinRule: domain.JoinRuleDirect, DirectKey: "direct:alice:bob"}, nil
		},
	}, membershipsOf(roomID, nil, &written), &testkit.MockEventStore{}, &testkit.MockEventBus{})

	err := svc.JoinRoom(context.Background(), roomID, actor("mallory"))
	require.ErrorIs(t, err, domain.ErrPermissionDenied)
	assert.Empty(t, written)
}
