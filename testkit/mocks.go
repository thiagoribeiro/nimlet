package testkit

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

// --- MockUserStore ---

type MockUserStore struct {
	CreateFn        func(ctx context.Context, u *domain.User, passwordHash string) error
	GetByIDFn       func(ctx context.Context, id string) (*domain.User, error)
	GetByUsernameFn func(ctx context.Context, namespaceID, username string) (*domain.User, string, error)
}

func (m *MockUserStore) Create(ctx context.Context, u *domain.User, passwordHash string) error {
	return m.CreateFn(ctx, u, passwordHash)
}
func (m *MockUserStore) GetByID(ctx context.Context, id string) (*domain.User, error) {
	return m.GetByIDFn(ctx, id)
}
func (m *MockUserStore) GetByUsername(ctx context.Context, namespaceID, username string) (*domain.User, string, error) {
	return m.GetByUsernameFn(ctx, namespaceID, username)
}

// --- MockDeviceStore ---

type MockDeviceStore struct {
	CreateFn                  func(ctx context.Context, d *domain.Device) error
	GetByIDFn                 func(ctx context.Context, id string) (*domain.Device, error)
	SetPushTokenFn            func(ctx context.Context, deviceID, pushToken string) error
	ListPushTokensByUserIDsFn func(ctx context.Context, userIDs []string) ([]domain.DevicePushToken, error)
}

func (m *MockDeviceStore) Create(ctx context.Context, d *domain.Device) error {
	return m.CreateFn(ctx, d)
}
func (m *MockDeviceStore) GetByID(ctx context.Context, id string) (*domain.Device, error) {
	if m.GetByIDFn == nil {
		return nil, domain.ErrNotFound // behaves like an empty store
	}
	return m.GetByIDFn(ctx, id)
}
func (m *MockDeviceStore) SetPushToken(ctx context.Context, deviceID, pushToken string) error {
	if m.SetPushTokenFn != nil {
		return m.SetPushTokenFn(ctx, deviceID, pushToken)
	}
	return nil
}
func (m *MockDeviceStore) ListPushTokensByUserIDs(ctx context.Context, userIDs []string) ([]domain.DevicePushToken, error) {
	if m.ListPushTokensByUserIDsFn != nil {
		return m.ListPushTokensByUserIDsFn(ctx, userIDs)
	}
	return nil, nil
}

// --- MockPresenceStore ---

// MockPresenceStore defaults to "always absent" so push-path tests don't need to wire
// it up unless they specifically care about presence gating.
type MockPresenceStore struct {
	TouchFn     func(ctx context.Context, userID string) error
	IsPresentFn func(ctx context.Context, userID string) (bool, error)
}

func (m *MockPresenceStore) Touch(ctx context.Context, userID string) error {
	if m.TouchFn != nil {
		return m.TouchFn(ctx, userID)
	}
	return nil
}
func (m *MockPresenceStore) IsPresent(ctx context.Context, userID string) (bool, error) {
	if m.IsPresentFn != nil {
		return m.IsPresentFn(ctx, userID)
	}
	return false, nil
}

// --- MockSessionStore ---

type MockSessionStore struct {
	CreateFn             func(ctx context.Context, s *domain.Session) error
	GetAuthByTokenHashFn func(ctx context.Context, hash string) (*domain.SessionAuth, error)
	DeleteFn             func(ctx context.Context, sessionID string) error
}

func (m *MockSessionStore) Create(ctx context.Context, s *domain.Session) error {
	return m.CreateFn(ctx, s)
}
func (m *MockSessionStore) GetAuthByTokenHash(ctx context.Context, hash string) (*domain.SessionAuth, error) {
	return m.GetAuthByTokenHashFn(ctx, hash)
}
func (m *MockSessionStore) Delete(ctx context.Context, sessionID string) error {
	return m.DeleteFn(ctx, sessionID)
}

// --- MockRoomStore ---

type MockRoomStore struct {
	CreateFn               func(ctx context.Context, r *domain.Room) error
	GetByIDFn              func(ctx context.Context, id string) (*domain.Room, error)
	GetByDirectKeyFn       func(ctx context.Context, namespaceID, directKey string) (*domain.Room, error)
	GetByAliasFn           func(ctx context.Context, namespaceID, alias string) (*domain.Room, error)
	IncrementMemberCountFn func(ctx context.Context, roomID string) error
	ListByIDsFn            func(ctx context.Context, ids []string) ([]*domain.Room, error)
	DeleteExpiredFn        func(ctx context.Context) (int64, error)
}

func (m *MockRoomStore) Create(ctx context.Context, r *domain.Room) error {
	return m.CreateFn(ctx, r)
}
func (m *MockRoomStore) GetByID(ctx context.Context, id string) (*domain.Room, error) {
	if m.GetByIDFn != nil {
		return m.GetByIDFn(ctx, id)
	}
	return &domain.Room{ID: id, NamespaceID: domain.DefaultNamespaceID}, nil
}
func (m *MockRoomStore) GetByDirectKey(ctx context.Context, namespaceID, directKey string) (*domain.Room, error) {
	if m.GetByDirectKeyFn != nil {
		return m.GetByDirectKeyFn(ctx, namespaceID, directKey)
	}
	return nil, domain.ErrNotFound
}
func (m *MockRoomStore) GetByAlias(ctx context.Context, namespaceID, alias string) (*domain.Room, error) {
	if m.GetByAliasFn != nil {
		return m.GetByAliasFn(ctx, namespaceID, alias)
	}
	return nil, domain.ErrNotFound
}
func (m *MockRoomStore) DeleteExpired(ctx context.Context) (int64, error) {
	if m.DeleteExpiredFn != nil {
		return m.DeleteExpiredFn(ctx)
	}
	return 0, nil
}
func (m *MockRoomStore) IncrementMemberCount(ctx context.Context, roomID string) error {
	if m.IncrementMemberCountFn != nil {
		return m.IncrementMemberCountFn(ctx, roomID)
	}
	return nil
}
func (m *MockRoomStore) ListByIDs(ctx context.Context, ids []string) ([]*domain.Room, error) {
	if m.ListByIDsFn != nil {
		return m.ListByIDsFn(ctx, ids)
	}
	return nil, nil
}

// --- MockMembershipStore ---

type MockMembershipStore struct {
	SetFn            func(ctx context.Context, m *domain.Membership) error
	GetFn            func(ctx context.Context, roomID, userID string) (*domain.Membership, error)
	ListLargeRoomsFn func(ctx context.Context, userID string) ([]string, error)
	ListByUserFn     func(ctx context.Context, userID string) ([]*domain.Membership, error)
}

func (m *MockMembershipStore) Set(ctx context.Context, ms *domain.Membership) error {
	return m.SetFn(ctx, ms)
}

// Transition emulates the store on top of GetFn/SetFn (no locking), so unit tests keep
// stubbing those two.
func (m *MockMembershipStore) Transition(ctx context.Context, _, roomID, userID string,
	next func(cur *domain.Membership) (*domain.Membership, error)) (*domain.Membership, error) {
	var cur *domain.Membership
	if m.GetFn != nil {
		c, err := m.GetFn(ctx, roomID, userID)
		switch {
		case err == nil:
			cur = c
		case !errors.Is(err, domain.ErrNotFound):
			return nil, err
		}
	}
	nm, err := next(cur)
	if err != nil {
		return cur, err
	}
	if m.SetFn != nil {
		if err := m.SetFn(ctx, nm); err != nil {
			return cur, err
		}
	}
	return cur, nil
}

func (m *MockMembershipStore) Get(ctx context.Context, roomID, userID string) (*domain.Membership, error) {
	return m.GetFn(ctx, roomID, userID)
}
func (m *MockMembershipStore) ListLargeRooms(ctx context.Context, userID string) ([]string, error) {
	if m.ListLargeRoomsFn != nil {
		return m.ListLargeRoomsFn(ctx, userID)
	}
	return nil, nil
}

// ListByUser ignores paging (returns everything ListByUserFn returns); the use case trims.
func (m *MockMembershipStore) ListByUser(ctx context.Context, userID, _ string, _ int) ([]*domain.Membership, error) {
	if m.ListByUserFn != nil {
		return m.ListByUserFn(ctx, userID)
	}
	return nil, nil
}

// --- MockEventStore ---

type MockEventStore struct {
	CreateFn           func(ctx context.Context, e *domain.Event, fanOut bool) (*domain.Event, []string, error)
	GetVisibleFn       func(ctx context.Context, userID string, sincePos int64, limit int) ([]*domain.Event, error)
	GetByRoomFn        func(ctx context.Context, roomID string, fromPos int64, dir string, limit int) ([]*domain.Event, error)
	CurrentStreamPosFn func(ctx context.Context) (int64, error)
}

func (m *MockEventStore) Create(ctx context.Context, e *domain.Event, fanOut bool) (*domain.Event, []string, error) {
	return m.CreateFn(ctx, e, fanOut)
}
func (m *MockEventStore) GetVisible(ctx context.Context, userID string, sincePos int64, limit int) ([]*domain.Event, error) {
	return m.GetVisibleFn(ctx, userID, sincePos, limit)
}
func (m *MockEventStore) GetByRoom(ctx context.Context, roomID string, fromPos int64, dir string, limit int) ([]*domain.Event, error) {
	return m.GetByRoomFn(ctx, roomID, fromPos, dir, limit)
}
func (m *MockEventStore) CurrentStreamPos(ctx context.Context) (int64, error) {
	return m.CurrentStreamPosFn(ctx)
}

// --- MockEventBus ---

type MockEventBus struct {
	PublishFn      func(ctx context.Context, key string) error
	PublishBatchFn func(ctx context.Context, keys []string) error
	SubscribeFn    func(ctx context.Context, partition int) (<-chan string, func(), error)
	PartitionsFn   func() int
}

func (m *MockEventBus) Publish(ctx context.Context, key string) error {
	if m.PublishFn != nil {
		return m.PublishFn(ctx, key)
	}
	return nil
}
func (m *MockEventBus) PublishBatch(ctx context.Context, keys []string) error {
	if m.PublishBatchFn != nil {
		return m.PublishBatchFn(ctx, keys)
	}
	// Default: delegate to Publish one-by-one (for tests that only set PublishFn).
	for _, k := range keys {
		if err := m.Publish(ctx, k); err != nil {
			return err
		}
	}
	return nil
}
func (m *MockEventBus) Subscribe(ctx context.Context, partition int) (<-chan string, func(), error) {
	if m.SubscribeFn != nil {
		return m.SubscribeFn(ctx, partition)
	}
	ch := make(chan string)
	return ch, func() { close(ch) }, nil
}
func (m *MockEventBus) NumPartitions() int {
	if m.PartitionsFn != nil {
		return m.PartitionsFn()
	}
	return 1
}

// --- MockTransactor ---

// MockTransactor calls fn(ctx) directly — a transparent passthrough.
// Tests that need to simulate tx failures can override WithTxFn.
type MockTransactor struct {
	WithTxFn func(ctx context.Context, fn func(ctx context.Context) error) error
}

func (m *MockTransactor) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if m.WithTxFn != nil {
		return m.WithTxFn(ctx, fn)
	}
	return fn(ctx)
}

// --- MockAppServiceDispatcher ---

type MockAppServiceDispatcher struct {
	Dispatched []*domain.Event
}

func (m *MockAppServiceDispatcher) Dispatch(e *domain.Event) {
	m.Dispatched = append(m.Dispatched, e)
}

// --- MockPushDispatcher ---

type MockPushDispatcher struct {
	Dispatched []*domain.Event
	Recipients [][]string
	RoomNames  []string
}

func (m *MockPushDispatcher) Dispatch(e *domain.Event, recipientIDs []string, roomName string) {
	m.Dispatched = append(m.Dispatched, e)
	m.Recipients = append(m.Recipients, recipientIDs)
	m.RoomNames = append(m.RoomNames, roomName)
}

// --- MockPushQueue ---

// MockPushQueue defaults Enqueue to always-claimed and Flush to empty, so tests
// that don't care about the queue's exact semantics can ignore it entirely.
type MockPushQueue struct {
	EnqueueFn func(ctx context.Context, userID string, item out.PushQueueItem) (bool, error)
	FlushFn   func(ctx context.Context, userID string) ([]out.PushQueueItem, error)
}

func (m *MockPushQueue) Enqueue(ctx context.Context, userID string, item out.PushQueueItem) (bool, error) {
	if m.EnqueueFn != nil {
		return m.EnqueueFn(ctx, userID, item)
	}
	return true, nil
}

// EnqueueBatch delegates to Enqueue per user, so tests can keep stubbing EnqueueFn.
func (m *MockPushQueue) EnqueueBatch(ctx context.Context, userIDs []string, item out.PushQueueItem) ([]bool, error) {
	claimed := make([]bool, len(userIDs))
	for i, u := range userIDs {
		c, err := m.Enqueue(ctx, u, item)
		if err != nil {
			return nil, err
		}
		claimed[i] = c
	}
	return claimed, nil
}
func (m *MockPushQueue) Flush(ctx context.Context, userID string) ([]out.PushQueueItem, error) {
	if m.FlushFn != nil {
		return m.FlushFn(ctx, userID)
	}
	return nil, nil
}

// --- MockMediaStore ---

type MockMediaStore struct {
	SaveFn func(ctx context.Context, mediaID, ownerID, contentType string, r io.Reader) error
	GetFn  func(ctx context.Context, mediaID string) (io.ReadCloser, string, int64, error)
}

func (m *MockMediaStore) Save(ctx context.Context, mediaID, ownerID, contentType string, r io.Reader) error {
	if m.SaveFn != nil {
		return m.SaveFn(ctx, mediaID, ownerID, contentType, r)
	}
	return nil
}
func (m *MockMediaStore) Get(ctx context.Context, mediaID string) (io.ReadCloser, string, int64, error) {
	if m.GetFn != nil {
		return m.GetFn(ctx, mediaID)
	}
	return nil, "", 0, domain.ErrNotFound
}

// --- MockNamespaceStore ---

// MockNamespaceStore is an in-memory NamespaceStore seeded with the default namespace
// (public registration on), so auth tests behave like a fresh install by default.
type MockNamespaceStore struct {
	mu     sync.Mutex
	bySlug map[string]*domain.Namespace
}

func NewMockNamespaceStore(extra ...*domain.Namespace) *MockNamespaceStore {
	m := &MockNamespaceStore{bySlug: map[string]*domain.Namespace{
		domain.DefaultNamespaceSlug: {
			ID: domain.DefaultNamespaceID, Slug: domain.DefaultNamespaceSlug,
			Status: domain.NamespaceActive, AllowRegistration: true,
		},
	}}
	for _, n := range extra {
		m.bySlug[n.Slug] = n
	}
	return m
}

func (m *MockNamespaceStore) Create(_ context.Context, n *domain.Namespace) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.bySlug[n.Slug]; ok {
		return domain.ErrAlreadyExists
	}
	m.bySlug[n.Slug] = n
	return nil
}

func (m *MockNamespaceStore) GetByID(_ context.Context, id string) (*domain.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.bySlug {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m *MockNamespaceStore) GetBySlug(_ context.Context, slug string) (*domain.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.bySlug[slug]; ok {
		return n, nil
	}
	return nil, domain.ErrNotFound
}

func (m *MockNamespaceStore) List(_ context.Context) ([]*domain.Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Namespace, 0, len(m.bySlug))
	for _, n := range m.bySlug {
		out = append(out, n)
	}
	return out, nil
}

func (m *MockNamespaceStore) SetStatus(ctx context.Context, id string, status domain.NamespaceStatus) error {
	n, err := m.GetByID(ctx, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	n.Status = status
	m.mu.Unlock()
	return nil
}

func (m *MockNamespaceStore) SetAllowRegistration(ctx context.Context, id string, allow bool) error {
	n, err := m.GetByID(ctx, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	n.AllowRegistration = allow
	m.mu.Unlock()
	return nil
}

// --- MockMediaIndex ---

type MockMediaIndex struct {
	mu    sync.Mutex
	media map[string]*domain.Media
}

func NewMockMediaIndex() *MockMediaIndex {
	return &MockMediaIndex{media: map[string]*domain.Media{}}
}

func (m *MockMediaIndex) Create(_ context.Context, md *domain.Media) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.media[md.ID] = md
	return nil
}

func (m *MockMediaIndex) Get(_ context.Context, id string) (*domain.Media, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if md, ok := m.media[id]; ok {
		return md, nil
	}
	return nil, domain.ErrNotFound
}

// --- admin methods (unused by unit tests; real behaviour is covered by integration tests) ---

func (m *MockUserStore) ListByNamespace(context.Context, string, string, int) ([]*domain.User, error) {
	return nil, nil
}
func (m *MockUserStore) SetDisabled(context.Context, string, bool) error       { return nil }
func (m *MockUserStore) SetPasswordHash(context.Context, string, string) error { return nil }
func (m *MockSessionStore) ListByUser(context.Context, string) ([]*domain.Session, error) {
	return nil, nil
}
func (m *MockSessionStore) DeleteInNamespace(context.Context, string, string) error { return nil }
func (m *MockRoomStore) DecrementMemberCount(context.Context, string) error         { return nil }
func (m *MockRoomStore) ListByNamespace(context.Context, string, int) ([]*domain.Room, error) {
	return nil, nil
}
func (m *MockMembershipStore) ListByRoom(context.Context, string) ([]*domain.Membership, error) {
	return nil, nil
}
func (m *MockEventStore) RevokeVisibility(context.Context, string, string) (int64, error) {
	return 0, nil
}
func (m *MockSessionStore) DeleteExpired(context.Context) (int64, error)   { return 0, nil }
func (m *MockSessionStore) PruneDevice(context.Context, string, int) error { return nil }
