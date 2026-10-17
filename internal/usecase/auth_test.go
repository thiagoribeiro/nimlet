package usecase_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
	"github.com/thiagoribeiro/nimlet/testkit"
)

var testHMACSecret = []byte("test-secret-at-least-32-chars!!!")

func newAuthSvc(users *testkit.MockUserStore, devices *testkit.MockDeviceStore, sessions *testkit.MockSessionStore) in.AuthService {
	return usecase.NewAuthService(users, devices, sessions, testkit.NewMockNamespaceStore(), &testkit.MockTransactor{}, testHMACSecret)
}

func mustHashPassword(pw string) string {
	h, err := crypto.HashPassword(pw)
	if err != nil {
		panic(err)
	}
	return h
}

func TestAuth_Register_Success(t *testing.T) {
	var savedUser *domain.User
	var savedSession *domain.Session

	svc := newAuthSvc(
		&testkit.MockUserStore{
			CreateFn: func(_ context.Context, u *domain.User, _ string) error {
				savedUser = u
				return nil
			},
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error { return nil },
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, s *domain.Session) error {
				savedSession = s
				return nil
			},
		},
	)

	result, err := svc.Register(context.Background(), in.RegisterCmd{
		Username:    "alice",
		Password:    "hunter2",
		DisplayName: "Alice",
		DeviceName:  "iPhone",
		UserType:    domain.UserTypeHuman,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "alice", savedUser.Username)
	assert.NotEmpty(t, savedSession.TokenHash)
	assert.Contains(t, result.Token, ".", "HMAC token must contain separator")
}

func TestAuth_Register_DuplicateUsername_ReturnsErrAlreadyExists(t *testing.T) {
	svc := newAuthSvc(
		&testkit.MockUserStore{
			CreateFn: func(_ context.Context, _ *domain.User, _ string) error {
				return domain.ErrAlreadyExists
			},
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error { return nil },
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, _ *domain.Session) error { return nil },
		},
	)

	_, err := svc.Register(context.Background(), in.RegisterCmd{
		Username: "alice", Password: "pw", DisplayName: "A", DeviceName: "d",
	})

	require.ErrorIs(t, err, domain.ErrAlreadyExists)
}

func TestAuth_Login_Success(t *testing.T) {
	hash := mustHashPassword("correct-password")
	user := &domain.User{ID: uuid.NewString(), Username: "bob", Type: domain.UserTypeHuman}

	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
				return user, hash, nil
			},
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error { return nil },
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, _ *domain.Session) error { return nil },
		},
	)

	result, err := svc.Login(context.Background(), in.LoginCmd{
		Username: "bob", Password: "correct-password", DeviceName: "laptop",
	})

	require.NoError(t, err)
	assert.NotEmpty(t, result.Token)
}

func TestAuth_Login_WrongPassword_ReturnsErrInvalidPassword(t *testing.T) {
	hash := mustHashPassword("correct-password")
	user := &domain.User{ID: uuid.NewString(), Username: "bob", Type: domain.UserTypeHuman}

	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
				return user, hash, nil
			},
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error { return nil },
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, _ *domain.Session) error { return nil },
		},
	)

	_, err := svc.Login(context.Background(), in.LoginCmd{
		Username: "bob", Password: "wrong-password", DeviceName: "d",
	})

	require.ErrorIs(t, err, domain.ErrInvalidPassword)
}

func TestAuth_Login_DisabledUser_ReturnsErrUserDisabled(t *testing.T) {
	user := &domain.User{
		ID: uuid.NewString(), Username: "disabled", Type: domain.UserTypeHuman, Disabled: true,
	}
	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
				return user, mustHashPassword("pw"), nil
			},
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error { return nil },
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, _ *domain.Session) error { return nil },
		},
	)

	_, err := svc.Login(context.Background(), in.LoginCmd{
		Username: "disabled", Password: "pw", DeviceName: "d",
	})

	require.ErrorIs(t, err, domain.ErrUserDisabled)
}

func TestAuth_Logout_DeletesSession(t *testing.T) {
	var deletedID string
	svc := newAuthSvc(
		&testkit.MockUserStore{},
		&testkit.MockDeviceStore{},
		&testkit.MockSessionStore{
			DeleteFn: func(_ context.Context, id string) error {
				deletedID = id
				return nil
			},
		},
	)

	require.NoError(t, svc.Logout(context.Background(), "session-abc"))
	assert.Equal(t, "session-abc", deletedID)
}

func TestAuth_TransactionRollback_OnCreateDeviceFailure(t *testing.T) {
	var rolledBack bool
	tx := &testkit.MockTransactor{
		WithTxFn: func(ctx context.Context, fn func(context.Context) error) error {
			err := fn(ctx)
			if err != nil {
				rolledBack = true
			}
			return err
		},
	}
	svc := usecase.NewAuthService(
		&testkit.MockUserStore{
			CreateFn: func(_ context.Context, _ *domain.User, _ string) error { return nil },
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error {
				return assert.AnError
			},
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, _ *domain.Session) error { return nil },
		},
		testkit.NewMockNamespaceStore(),
		tx,
		testHMACSecret,
	)

	_, err := svc.Register(context.Background(), in.RegisterCmd{
		Username: "x", Password: "pw", DisplayName: "X", DeviceName: "d",
	})

	require.Error(t, err)
	assert.True(t, rolledBack)
}

// --- ResolveUsername / ResolveUserID ---

func TestAuth_ResolveUsername_Success(t *testing.T) {
	user := &domain.User{ID: uuid.NewString(), Username: "bob", DisplayName: "Bob", Type: domain.UserTypeHuman}
	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, username string) (*domain.User, string, error) {
				assert.Equal(t, "bob", username)
				return user, "irrelevant-hash", nil
			},
		},
		&testkit.MockDeviceStore{}, &testkit.MockSessionStore{},
	)

	got, err := svc.ResolveUsername(context.Background(), domain.DefaultNamespaceID, "bob")

	require.NoError(t, err)
	assert.Equal(t, user, got)
}

func TestAuth_ResolveUsername_NotFound(t *testing.T) {
	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
				return nil, "", domain.ErrNotFound
			},
		},
		&testkit.MockDeviceStore{}, &testkit.MockSessionStore{},
	)

	_, err := svc.ResolveUsername(context.Background(), domain.DefaultNamespaceID, "ghost")

	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAuth_ResolveUserID_Success(t *testing.T) {
	user := &domain.User{ID: "user1", Username: "bob", DisplayName: "Bob", Type: domain.UserTypeHuman}
	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByIDFn: func(_ context.Context, id string) (*domain.User, error) {
				assert.Equal(t, "user1", id)
				return user, nil
			},
		},
		&testkit.MockDeviceStore{}, &testkit.MockSessionStore{},
	)

	got, err := svc.ResolveUserID(context.Background(), "user1")

	require.NoError(t, err)
	assert.Equal(t, user, got)
}

// --- EnsureServiceUser ---

func TestAuth_EnsureServiceUser_NewUser_CreatesAndReturnsToken(t *testing.T) {
	var createdUser *domain.User
	var createdSession *domain.Session

	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
				return nil, "", domain.ErrNotFound
			},
			CreateFn: func(_ context.Context, u *domain.User, hash string) error {
				assert.NotEmpty(t, hash)
				createdUser = u
				return nil
			},
		},
		&testkit.MockDeviceStore{
			CreateFn: func(_ context.Context, _ *domain.Device) error { return nil },
		},
		&testkit.MockSessionStore{
			CreateFn: func(_ context.Context, s *domain.Session) error { createdSession = s; return nil },
		},
	)

	userID, token, err := svc.EnsureServiceUser(context.Background(), domain.DefaultNamespaceID, "customer_42")

	require.NoError(t, err)
	require.NotNil(t, createdUser)
	assert.Equal(t, "customer_42", createdUser.Username)
	assert.Equal(t, domain.UserTypeService, createdUser.Type)
	assert.Equal(t, createdUser.ID, userID)
	assert.NotEmpty(t, token)
	require.NotNil(t, createdSession)
	assert.Equal(t, userID, createdSession.UserID)
}

func TestAuth_EnsureServiceUser_ExistingUser_ReusesUserMintsFreshSession(t *testing.T) {
	existing := &domain.User{ID: "u-existing", Username: "vendor_7", Type: domain.UserTypeService}
	createCalled := false

	svc := newAuthSvc(
		&testkit.MockUserStore{
			GetByUsernameFn: func(_ context.Context, _, name string) (*domain.User, string, error) {
				assert.Equal(t, "vendor_7", name)
				return existing, "hash", nil
			},
			CreateFn: func(_ context.Context, _ *domain.User, _ string) error { createCalled = true; return nil },
		},
		&testkit.MockDeviceStore{CreateFn: func(_ context.Context, _ *domain.Device) error { return nil }},
		&testkit.MockSessionStore{CreateFn: func(_ context.Context, _ *domain.Session) error { return nil }},
	)

	userID, token, err := svc.EnsureServiceUser(context.Background(), domain.DefaultNamespaceID, "vendor_7")

	require.NoError(t, err)
	assert.Equal(t, "u-existing", userID)
	assert.NotEmpty(t, token)
	assert.False(t, createCalled, "existing user must not be re-created")
}

func TestAuth_EnsureServiceUser_EmptyUsername_ReturnsError(t *testing.T) {
	svc := newAuthSvc(&testkit.MockUserStore{}, &testkit.MockDeviceStore{}, &testkit.MockSessionStore{})
	_, _, err := svc.EnsureServiceUser(context.Background(), domain.DefaultNamespaceID, "")
	require.Error(t, err)
}

// --- Namespaces ---

var acme = &domain.Namespace{ID: "ns-acme", Slug: "acme", Status: domain.NamespaceActive}

func newNSAuthSvc(users *testkit.MockUserStore, sessions *testkit.MockSessionStore, extra ...*domain.Namespace) in.AuthService {
	if sessions == nil {
		sessions = &testkit.MockSessionStore{CreateFn: func(_ context.Context, _ *domain.Session) error { return nil }}
	}
	return usecase.NewAuthService(users,
		&testkit.MockDeviceStore{CreateFn: func(_ context.Context, _ *domain.Device) error { return nil }},
		sessions, testkit.NewMockNamespaceStore(extra...), &testkit.MockTransactor{}, testHMACSecret)
}

// The /login namespace selects which account base the username is looked up in, and the
// session is bound to that namespace.
func TestAuth_Login_SelectsNamespace(t *testing.T) {
	hash := mustHashPassword("pw")
	var lookedUpIn string
	var session *domain.Session
	svc := newNSAuthSvc(
		&testkit.MockUserStore{GetByUsernameFn: func(_ context.Context, ns, _ string) (*domain.User, string, error) {
			lookedUpIn = ns
			return &domain.User{ID: "u1", NamespaceID: ns, Username: "alice", Type: domain.UserTypeHuman}, hash, nil
		}},
		&testkit.MockSessionStore{CreateFn: func(_ context.Context, s *domain.Session) error { session = s; return nil }},
		acme,
	)

	_, err := svc.Login(context.Background(), in.LoginCmd{Namespace: "acme", Username: "alice", Password: "pw"})
	require.NoError(t, err)
	assert.Equal(t, acme.ID, lookedUpIn)
	assert.Equal(t, acme.ID, session.NamespaceID)
}

// Unknown namespace, unknown user and wrong password are indistinguishable.
func TestAuth_Login_FailuresDoNotRevealExistence(t *testing.T) {
	hash := mustHashPassword("pw")
	users := &testkit.MockUserStore{GetByUsernameFn: func(_ context.Context, _, name string) (*domain.User, string, error) {
		if name != "alice" {
			return nil, "", domain.ErrNotFound
		}
		return &domain.User{ID: "u1", Username: "alice", Type: domain.UserTypeHuman}, hash, nil
	}}
	svc := newNSAuthSvc(users, nil, acme)

	for name, cmd := range map[string]in.LoginCmd{
		"unknown namespace": {Namespace: "nope", Username: "alice", Password: "pw"},
		"unknown user":      {Namespace: "acme", Username: "bob", Password: "pw"},
		"wrong password":    {Namespace: "acme", Username: "alice", Password: "nope"},
	} {
		_, err := svc.Login(context.Background(), cmd)
		assert.ErrorIs(t, err, domain.ErrInvalidPassword, name)
	}
}

func TestAuth_Login_DisabledNamespace(t *testing.T) {
	hash := mustHashPassword("pw")
	disabled := &domain.Namespace{ID: "ns-off", Slug: "off", Status: domain.NamespaceDisabled}
	svc := newNSAuthSvc(&testkit.MockUserStore{GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
		return &domain.User{ID: "u1", Username: "alice", Type: domain.UserTypeHuman}, hash, nil
	}}, nil, disabled)

	_, err := svc.Login(context.Background(), in.LoginCmd{Namespace: "off", Username: "alice", Password: "pw"})
	assert.ErrorIs(t, err, domain.ErrNamespaceDisabled)
}

func TestAuth_Register_NamespacePolicy(t *testing.T) {
	open := &domain.Namespace{ID: "ns-open", Slug: "open", Status: domain.NamespaceActive, AllowRegistration: true}
	var created *domain.User
	users := &testkit.MockUserStore{CreateFn: func(_ context.Context, u *domain.User, _ string) error { created = u; return nil }}
	svc := newNSAuthSvc(users, nil, acme, open)

	_, err := svc.Register(context.Background(), in.RegisterCmd{Namespace: "open", Username: "a", Password: "pw"})
	require.NoError(t, err)
	assert.Equal(t, open.ID, created.NamespaceID)

	for name, cmd := range map[string]in.RegisterCmd{
		"registration off": {Namespace: "acme", Username: "a", Password: "pw"},
		"unknown":          {Namespace: "nope", Username: "a", Password: "pw"},
	} {
		_, err := svc.Register(context.Background(), cmd)
		assert.ErrorIs(t, err, domain.ErrRegistrationClosed, name)
	}

	_, err = svc.Register(context.Background(), in.RegisterCmd{Username: "a", Password: "pw", UserType: domain.UserTypeService})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "service accounts are not self-registrable")
}

// An AppService must never obtain a session for a human account by naming its username.
func TestAuth_EnsureServiceUser_RefusesNonServiceAccount(t *testing.T) {
	svc := newNSAuthSvc(&testkit.MockUserStore{GetByUsernameFn: func(_ context.Context, _, _ string) (*domain.User, string, error) {
		return &domain.User{ID: "u-human", Username: "alice", Type: domain.UserTypeHuman}, "hash", nil
	}}, nil)

	_, _, err := svc.EnsureServiceUser(context.Background(), domain.DefaultNamespaceID, "alice")
	assert.ErrorIs(t, err, domain.ErrPermissionDenied)
}
