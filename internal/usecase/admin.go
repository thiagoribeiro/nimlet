package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// AdminStores groups the stores the admin use case needs.
type AdminStores struct {
	Namespaces  out.NamespaceStore
	Users       out.UserStore
	Sessions    out.SessionStore
	Rooms       out.RoomStore
	Memberships out.MembershipStore
	Events      out.EventStore
	Tx          out.Transactor
	Audit       out.AuditLog
}

type adminService struct {
	st            AdminStores
	operator      string
	correlationID string
}

// NewAdminService builds the CLI's use case. operator identifies who runs it in the audit
// log; correlationID ties together the entries of one CLI invocation.
func NewAdminService(stores AdminStores, operator, correlationID string) in.AdminService {
	return &adminService{st: stores, operator: operator, correlationID: correlationID}
}

// audit records a mutation with its outcome and returns err unchanged. Audit failures are
// logged, not returned: the action itself already happened (or failed) by then.
func (s *adminService) audit(ctx context.Context, command, namespaceID, target string, err error) error {
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	if aerr := s.st.Audit.Record(context.WithoutCancel(ctx), &domain.AuditEntry{
		ID:            uuid.NewString(),
		Operator:      s.operator,
		Command:       command,
		NamespaceID:   namespaceID,
		Target:        target,
		Outcome:       outcome,
		CorrelationID: s.correlationID,
		CreatedAt:     time.Now().UTC(),
	}); aerr != nil {
		slog.Error("admin: audit record failed", "command", command, "error", aerr)
	}
	return err
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), domain.ErrInvalidArgument)
}

// --- namespaces ---

func (s *adminService) namespace(ctx context.Context, slug string) (*domain.Namespace, error) {
	if slug == "" {
		return nil, invalid("namespace is required")
	}
	ns, err := s.st.Namespaces.GetBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("namespace %q: %w", slug, err)
	}
	return ns, nil
}

func (s *adminService) CreateNamespace(ctx context.Context, slug string, allowRegistration bool) (*domain.Namespace, error) {
	ns := &domain.Namespace{
		ID:                uuid.NewString(),
		Slug:              slug,
		Status:            domain.NamespaceActive,
		AllowRegistration: allowRegistration,
		CreatedAt:         time.Now().UTC(),
	}
	var err error
	if !domain.ValidNamespaceSlug(slug) {
		err = invalid("namespace slug %q must match [a-z0-9][a-z0-9-]{0,62}", slug)
	} else {
		err = s.st.Namespaces.Create(ctx, ns)
	}
	if err := s.audit(ctx, "namespace create", ns.ID, slug, err); err != nil {
		return nil, err
	}
	return ns, nil
}

func (s *adminService) ListNamespaces(ctx context.Context) ([]*domain.Namespace, error) {
	return s.st.Namespaces.List(ctx)
}

func (s *adminService) GetNamespace(ctx context.Context, slug string) (*domain.Namespace, error) {
	return s.namespace(ctx, slug)
}

func (s *adminService) SetNamespaceStatus(ctx context.Context, slug string, status domain.NamespaceStatus) (*domain.Namespace, error) {
	ns, err := s.namespace(ctx, slug)
	if err == nil {
		err = s.st.Namespaces.SetStatus(ctx, ns.ID, status)
	}
	command := "namespace enable"
	if status == domain.NamespaceDisabled {
		command = "namespace disable"
	}
	if err := s.audit(ctx, command, nsID(ns), slug, err); err != nil {
		return nil, err
	}
	return s.namespace(ctx, slug)
}

func (s *adminService) SetNamespaceRegistration(ctx context.Context, slug string, allow bool) (*domain.Namespace, error) {
	ns, err := s.namespace(ctx, slug)
	if err == nil {
		err = s.st.Namespaces.SetAllowRegistration(ctx, ns.ID, allow)
	}
	if err := s.audit(ctx, fmt.Sprintf("namespace registration %t", allow), nsID(ns), slug, err); err != nil {
		return nil, err
	}
	return s.namespace(ctx, slug)
}

func (s *adminService) members() membershipOps {
	return membershipOps{tx: s.st.Tx, rooms: s.st.Rooms, memberships: s.st.Memberships}
}

func nsID(ns *domain.Namespace) string {
	if ns == nil {
		return ""
	}
	return ns.ID
}

// --- users ---

var knownUserTypes = map[domain.UserType]bool{
	domain.UserTypeHuman: true, domain.UserTypeAgent: true, domain.UserTypeBot: true,
	domain.UserTypeSystem: true, domain.UserTypeService: true,
}

func (s *adminService) user(ctx context.Context, ns *domain.Namespace, username string) (*domain.User, error) {
	u, _, err := s.st.Users.GetByUsername(ctx, ns.ID, username)
	if err != nil {
		return nil, fmt.Errorf("user %q in namespace %q: %w", username, ns.Slug, err)
	}
	return u, nil
}

func (s *adminService) CreateUser(ctx context.Context, cmd in.AdminCreateUserCmd) (*domain.User, error) {
	ns, err := s.namespace(ctx, cmd.Namespace)
	var u *domain.User
	if err == nil {
		u, err = s.createUser(ctx, ns, cmd)
	}
	if err := s.audit(ctx, "user create", nsID(ns), cmd.Username, err); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *adminService) createUser(ctx context.Context, ns *domain.Namespace, cmd in.AdminCreateUserCmd) (*domain.User, error) {
	if err := errors.Join(
		domain.ValidateUsername(cmd.Username),
		domain.ValidatePassword(cmd.Password),
		domain.ValidateText("display name", cmd.DisplayName, domain.MaxDisplayNameLen),
	); err != nil {
		return nil, err
	}
	userType := cmd.Type
	if userType == "" {
		userType = domain.UserTypeHuman
	}
	if !knownUserTypes[userType] {
		return nil, invalid("unknown user type %q", userType)
	}
	displayName := cmd.DisplayName
	if displayName == "" {
		displayName = cmd.Username
	}
	hash, err := crypto.HashPassword(cmd.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	u := &domain.User{
		ID:          uuid.NewString(),
		NamespaceID: ns.ID,
		Username:    cmd.Username,
		DisplayName: displayName,
		Type:        userType,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.st.Users.Create(ctx, u, hash); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *adminService) ListUsers(ctx context.Context, namespace, afterUsername string, limit int) ([]*domain.User, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return s.st.Users.ListByNamespace(ctx, ns.ID, afterUsername, limit)
}

func (s *adminService) GetUser(ctx context.Context, namespace, username string) (*domain.User, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return s.user(ctx, ns, username)
}

func (s *adminService) SetUserDisabled(ctx context.Context, namespace, username string, disabled bool) error {
	ns, err := s.namespace(ctx, namespace)
	if err == nil {
		var u *domain.User
		if u, err = s.user(ctx, ns, username); err == nil {
			err = s.st.Users.SetDisabled(ctx, u.ID, disabled)
		}
	}
	command := "user enable"
	if disabled {
		command = "user disable"
	}
	return s.audit(ctx, command, nsID(ns), username, err)
}

func (s *adminService) SetUserPassword(ctx context.Context, namespace, username, password string) error {
	ns, err := s.namespace(ctx, namespace)
	if err == nil {
		err = domain.ValidatePassword(password)
	}
	if err == nil {
		var u *domain.User
		if u, err = s.user(ctx, ns, username); err == nil {
			var hash string
			if hash, err = crypto.HashPassword(password); err == nil {
				err = s.st.Users.SetPasswordHash(ctx, u.ID, hash)
			}
		}
	}
	return s.audit(ctx, "user password set", nsID(ns), username, err)
}

func (s *adminService) ListSessions(ctx context.Context, namespace, username string) ([]*domain.Session, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	u, err := s.user(ctx, ns, username)
	if err != nil {
		return nil, err
	}
	return s.st.Sessions.ListByUser(ctx, u.ID)
}

func (s *adminService) RevokeSession(ctx context.Context, namespace, sessionID string) error {
	ns, err := s.namespace(ctx, namespace)
	if err == nil {
		err = s.st.Sessions.DeleteInNamespace(ctx, ns.ID, sessionID)
		if errors.Is(err, domain.ErrNotFound) {
			err = fmt.Errorf("session %q in namespace %q: %w", sessionID, ns.Slug, err)
		}
	}
	return s.audit(ctx, "user session revoke", nsID(ns), sessionID, err)
}

// --- rooms ---

// room loads a room and treats one from another namespace as missing.
func (s *adminService) room(ctx context.Context, ns *domain.Namespace, roomID string) (*domain.Room, error) {
	r, err := s.st.Rooms.GetByID(ctx, roomID)
	if err == nil && r.NamespaceID != ns.ID {
		err = domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room %q in namespace %q: %w", roomID, ns.Slug, err)
	}
	return r, nil
}

func (s *adminService) CreateRoom(ctx context.Context, cmd in.AdminCreateRoomCmd) (*domain.Room, error) {
	ns, err := s.namespace(ctx, cmd.Namespace)
	var room *domain.Room
	if err == nil {
		room, err = s.createRoom(ctx, ns, cmd)
	}
	target := cmd.Name
	if room != nil {
		target = room.ID
	}
	if err := s.audit(ctx, "room create", nsID(ns), target, err); err != nil {
		return nil, err
	}
	return room, nil
}

func (s *adminService) createRoom(ctx context.Context, ns *domain.Namespace, cmd in.AdminCreateRoomCmd) (*domain.Room, error) {
	if cmd.Name == "" {
		return nil, invalid("room name is required")
	}
	if err := validateRoomFields(cmd.Name, cmd.Alias); err != nil {
		return nil, err
	}
	if cmd.Owner == "" {
		return nil, invalid("--owner is required: a room always has an owner from its own namespace")
	}
	joinRule := cmd.JoinRule
	if joinRule == "" {
		joinRule = domain.JoinRuleInviteOnly
	}
	if joinRule != domain.JoinRuleInviteOnly && joinRule != domain.JoinRulePublic {
		return nil, invalid("join rule must be %q or %q", domain.JoinRuleInviteOnly, domain.JoinRulePublic)
	}
	owner, err := s.user(ctx, ns, cmd.Owner)
	if err != nil {
		return nil, err
	}
	if owner.Disabled {
		return nil, invalid("owner %q is disabled", cmd.Owner)
	}
	now := time.Now().UTC()
	room := &domain.Room{
		ID:          uuid.NewString(),
		NamespaceID: ns.ID,
		Name:        cmd.Name,
		CreatorID:   owner.ID,
		CreatedAt:   now,
		MemberCount: 1,
		JoinRule:    joinRule,
		Alias:       cmd.Alias,
	}
	err = s.st.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.st.Rooms.Create(ctx, room); err != nil {
			return err
		}
		return s.st.Memberships.Set(ctx, &domain.Membership{
			NamespaceID: ns.ID, RoomID: room.ID, UserID: owner.ID,
			State: domain.MembershipJoin, Role: domain.RoleOwner, UpdatedAt: now,
		})
	})
	if err != nil {
		return nil, err
	}
	return room, nil
}

func (s *adminService) ListRooms(ctx context.Context, namespace string, limit int) ([]*domain.Room, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return s.st.Rooms.ListByNamespace(ctx, ns.ID, limit)
}

func (s *adminService) GetRoom(ctx context.Context, namespace, roomID string) (*domain.Room, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return s.room(ctx, ns, roomID)
}

// --- memberships ---

var assignableRoles = map[domain.MemberRole]bool{
	domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true,
	domain.RoleGuest: true, domain.RoleAgent: true, domain.RoleBot: true,
}

// memberTarget resolves (namespace, room, user) for a membership command.
func (s *adminService) memberTarget(ctx context.Context, namespace, roomID, username string) (*domain.Namespace, *domain.Room, *domain.User, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, nil, nil, err
	}
	room, err := s.room(ctx, ns, roomID)
	if err != nil {
		return ns, nil, nil, err
	}
	u, err := s.user(ctx, ns, username)
	if err != nil {
		return ns, room, nil, err
	}
	return ns, room, u, nil
}

func (s *adminService) AddMember(ctx context.Context, namespace, roomID, username string, role domain.MemberRole, ifNotExists bool) (*domain.Membership, error) {
	ns, room, u, err := s.memberTarget(ctx, namespace, roomID, username)
	var m *domain.Membership
	if err == nil {
		m, err = s.addMember(ctx, room, u, role, ifNotExists)
	}
	if err := s.audit(ctx, "room member add", nsID(ns), roomID+"/"+username, err); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *adminService) addMember(ctx context.Context, room *domain.Room, u *domain.User, role domain.MemberRole, ifNotExists bool) (*domain.Membership, error) {
	if role == "" {
		role = domain.RoleMember
	}
	if !assignableRoles[role] {
		return nil, invalid("unknown role %q", role)
	}
	_, next, err := s.members().change(ctx, room, u.ID, domain.ActionAdd, role, nil)
	if errors.Is(err, domain.ErrMembershipUnchanged) {
		if !ifNotExists {
			return nil, fmt.Errorf("%q is already a member: %w", u.Username, domain.ErrAlreadyExists)
		}
		return s.st.Memberships.Get(ctx, room.ID, u.ID)
	}
	return next, err
}

func (s *adminService) ListMembers(ctx context.Context, namespace, roomID string) ([]*in.AdminMember, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if _, err := s.room(ctx, ns, roomID); err != nil {
		return nil, err
	}
	ms, err := s.st.Memberships.ListByRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	out := make([]*in.AdminMember, 0, len(ms))
	for _, m := range ms {
		u, err := s.st.Users.GetByID(ctx, m.UserID)
		if err != nil {
			return nil, fmt.Errorf("member %q: %w", m.UserID, err)
		}
		out = append(out, &in.AdminMember{Membership: *m, Username: u.Username})
	}
	return out, nil
}

// otherJoinedOwners counts the room's joined owners other than exceptUserID.
func (s *adminService) otherJoinedOwners(ctx context.Context, roomID, exceptUserID string) (int, error) {
	ms, err := s.st.Memberships.ListByRoom(ctx, roomID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range ms {
		if m.UserID != exceptUserID && m.IsJoined() && m.Role == domain.RoleOwner {
			n++
		}
	}
	return n, nil
}

func (s *adminService) RemoveMember(ctx context.Context, namespace, roomID, username string) error {
	ns, room, u, err := s.memberTarget(ctx, namespace, roomID, username)
	if err == nil {
		err = s.removeMember(ctx, room, u)
	}
	return s.audit(ctx, "room member remove", nsID(ns), roomID+"/"+username, err)
}

func (s *adminService) removeMember(ctx context.Context, room *domain.Room, u *domain.User) error {
	_, _, err := s.members().change(ctx, room, u.ID, domain.ActionRemove, "",
		func(ctx context.Context, cur, _ *domain.Membership) error {
			if cur.IsJoined() && cur.Role == domain.RoleOwner {
				n, err := s.otherJoinedOwners(ctx, room.ID, u.ID)
				if err != nil {
					return err
				}
				if n == 0 {
					return invalid("%q is the room's last owner; make someone else owner first", u.Username)
				}
			}
			// History already fanned out to the member must not stay readable via /sync.
			_, err := s.st.Events.RevokeVisibility(ctx, u.ID, room.ID)
			return err
		})
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%q is not a member: %w", u.Username, err)
	}
	return err
}

func (s *adminService) SetMemberRole(ctx context.Context, namespace, roomID, username string, role domain.MemberRole) (*domain.Membership, error) {
	ns, room, u, err := s.memberTarget(ctx, namespace, roomID, username)
	var m *domain.Membership
	if err == nil {
		m, err = s.setMemberRole(ctx, room, u, role)
	}
	if err := s.audit(ctx, "room member role set", nsID(ns), roomID+"/"+username+"="+string(role), err); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *adminService) setMemberRole(ctx context.Context, room *domain.Room, u *domain.User, role domain.MemberRole) (*domain.Membership, error) {
	if !assignableRoles[role] {
		return nil, invalid("unknown role %q", role)
	}
	m, err := s.st.Memberships.Get(ctx, room.ID, u.ID)
	if err != nil || !m.IsJoined() {
		return nil, fmt.Errorf("%q is not a joined member: %w", u.Username, domain.ErrNotFound)
	}
	if m.Role == domain.RoleOwner && role != domain.RoleOwner {
		n, err := s.otherJoinedOwners(ctx, room.ID, u.ID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, invalid("%q is the room's last owner; make someone else owner first", u.Username)
		}
	}
	m.Role = role
	m.UpdatedAt = time.Now().UTC()
	if err := s.st.Memberships.Set(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}
