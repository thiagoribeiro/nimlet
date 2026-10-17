package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
)

const sessionTTL = 90 * 24 * time.Hour // 90 days

// serviceSessionsKept bounds the live sessions of one service account: EnsureServiceUser mints
// one per call, and older ones are revoked beyond this many.
const serviceSessionsKept = 20

// dummyPasswordHash is verified when the account does not exist, so a failed login costs
// the same Argon2 work whether or not the username is known.
var dummyPasswordHash = func() string {
	h, err := crypto.HashPassword("nimlet-dummy-password")
	if err != nil {
		panic(err)
	}
	return h
}()

type authService struct {
	users      out.UserStore
	devices    out.DeviceStore
	sessions   out.SessionStore
	namespaces out.NamespaceStore
	tx         out.Transactor
	secret     []byte // HMAC secret for session tokens
}

func NewAuthService(
	users out.UserStore,
	devices out.DeviceStore,
	sessions out.SessionStore,
	namespaces out.NamespaceStore,
	tx out.Transactor,
	hmacSecret []byte,
) in.AuthService {
	return &authService{
		users:      users,
		devices:    devices,
		sessions:   sessions,
		namespaces: namespaces,
		tx:         tx,
		secret:     hmacSecret,
	}
}

// selectNamespace resolves the namespace slug a /login or /register names ("" = default).
func (s *authService) selectNamespace(ctx context.Context, slug string) (*domain.Namespace, error) {
	if slug == "" {
		slug = domain.DefaultNamespaceSlug
	}
	return s.namespaces.GetBySlug(ctx, slug)
}

// validateNewAccount checks the client-supplied fields of a new account.
func validateNewAccount(username, password, displayName, deviceName string) error {
	return errors.Join(
		domain.ValidateUsername(username),
		domain.ValidatePassword(password),
		domain.ValidateText("display_name", displayName, domain.MaxDisplayNameLen),
		domain.ValidateText("device_name", deviceName, domain.MaxDeviceNameLen),
	)
}

func (s *authService) Register(ctx context.Context, cmd in.RegisterCmd) (*in.RegisterResult, error) {
	ns, err := s.selectNamespace(ctx, cmd.Namespace)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Same answer as a namespace that exists but is closed: no enumeration.
			return nil, domain.ErrRegistrationClosed
		}
		return nil, fmt.Errorf("auth: register: %w", err)
	}
	if !ns.IsActive() || !ns.AllowRegistration {
		return nil, domain.ErrRegistrationClosed
	}

	// Clients never declare what kind of account they are (the API contract): public
	// registration only creates humans. Agents, bots and service accounts are created by
	// operators (nimlet user create --type) or AppServices.
	if cmd.UserType != "" && cmd.UserType != domain.UserTypeHuman {
		return nil, fmt.Errorf("auth: register: user_type %q is not self-registrable: %w", cmd.UserType, domain.ErrInvalidArgument)
	}
	userType := domain.UserTypeHuman
	if err := validateNewAccount(cmd.Username, cmd.Password, cmd.DisplayName, cmd.DeviceName); err != nil {
		return nil, fmt.Errorf("auth: register: %w", err)
	}

	passwordHash, err := crypto.HashPassword(cmd.Password)
	if err != nil {
		return nil, fmt.Errorf("auth: hash password: %w", err)
	}

	user := &domain.User{
		ID:          uuid.NewString(),
		NamespaceID: ns.ID,
		Username:    cmd.Username,
		DisplayName: cmd.DisplayName,
		Type:        userType,
		CreatedAt:   time.Now().UTC(),
	}
	device := &domain.Device{
		ID:          uuid.NewString(),
		UserID:      user.ID,
		DisplayName: cmd.DeviceName,
		CreatedAt:   time.Now().UTC(),
	}

	var token string
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.users.Create(ctx, user, passwordHash); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		if err := s.devices.Create(ctx, device); err != nil {
			return fmt.Errorf("create device: %w", err)
		}
		var sessionErr error
		_, token, sessionErr = s.createSession(ctx, ns.ID, user.ID, device.ID)
		return sessionErr
	})
	if err != nil {
		return nil, fmt.Errorf("auth: register: %w", err)
	}

	return &in.RegisterResult{User: user, Device: device, Token: token}, nil
}

// Login authenticates (namespace, username, password). An unknown namespace, unknown user
// and wrong password all fail identically — same error, and a password hash is verified
// either way so timing does not tell them apart either. Account and namespace state are
// only revealed to a caller who proved the password.
func (s *authService) Login(ctx context.Context, cmd in.LoginCmd) (*in.LoginResult, error) {
	// An oversized password can never match (registration caps it) — reject it before
	// spending Argon2 time on it.
	if domain.ValidatePassword(cmd.Password) != nil {
		return nil, domain.ErrInvalidPassword
	}
	if err := domain.ValidateText("device_name", cmd.DeviceName, domain.MaxDeviceNameLen); err != nil {
		return nil, fmt.Errorf("auth: login: %w", err)
	}
	var user *domain.User
	passwordHash := dummyPasswordHash
	ns, err := s.selectNamespace(ctx, cmd.Namespace)
	switch {
	case err == nil:
		var hash string
		user, hash, err = s.users.GetByUsername(ctx, ns.ID, cmd.Username)
		if err == nil {
			passwordHash = hash
		} else if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("auth: login: %w", err)
		}
	case !errors.Is(err, domain.ErrNotFound):
		return nil, fmt.Errorf("auth: login: %w", err)
	}

	ok, err := crypto.VerifyPassword(cmd.Password, passwordHash)
	if err != nil {
		return nil, fmt.Errorf("auth: verify password: %w", err)
	}
	if !ok || user == nil {
		return nil, domain.ErrInvalidPassword
	}
	if user.Disabled {
		return nil, domain.ErrUserDisabled
	}
	if !ns.IsActive() {
		return nil, domain.ErrNamespaceDisabled
	}

	device := s.reusableDevice(ctx, cmd.DeviceID, user.ID)
	reuse := device != nil
	if !reuse {
		device = &domain.Device{
			ID:          uuid.NewString(),
			UserID:      user.ID,
			DisplayName: cmd.DeviceName,
			CreatedAt:   time.Now().UTC(),
		}
	}

	var token string
	var session *domain.Session
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if !reuse {
			if err := s.devices.Create(ctx, device); err != nil {
				return fmt.Errorf("create device: %w", err)
			}
		}
		var err error
		session, token, err = s.createSession(ctx, ns.ID, user.ID, device.ID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("auth: login: %w", err)
	}

	return &in.LoginResult{Session: session, Token: token}, nil
}

func (s *authService) ResolveUsername(ctx context.Context, namespaceID, username string) (*domain.User, error) {
	user, _, err := s.users.GetByUsername(ctx, namespaceID, username)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve username: %w", err)
	}
	return user, nil
}

func (s *authService) ResolveUserID(ctx context.Context, userID string) (*domain.User, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve user id: %w", err)
	}
	return user, nil
}

func (s *authService) SetPushToken(ctx context.Context, deviceID, pushToken string) error {
	if err := s.devices.SetPushToken(ctx, deviceID, pushToken); err != nil {
		return fmt.Errorf("auth: set push token: %w", err)
	}
	return nil
}

func (s *authService) Logout(ctx context.Context, sessionID string) error {
	if err := s.sessions.Delete(ctx, sessionID); err != nil {
		return fmt.Errorf("auth: logout: %w", err)
	}
	return nil
}

func (s *authService) EnsureServiceUser(ctx context.Context, namespaceID, username string) (string, string, error) {
	if err := domain.ValidateUsername(username); err != nil {
		return "", "", fmt.Errorf("auth: ensure service user: %w", err)
	}

	user, _, err := s.users.GetByUsername(ctx, namespaceID, username)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return "", "", fmt.Errorf("auth: ensure service user: lookup: %w", err)
	}
	// An AppService may only act as the service accounts it provisions — never mint a
	// session for a human (or any other) account that happens to have this username.
	if user != nil && user.Type != domain.UserTypeService {
		return "", "", fmt.Errorf("auth: ensure service user %q: %w", username, domain.ErrPermissionDenied)
	}

	var token string
	txErr := s.tx.WithTx(ctx, func(ctx context.Context) error {
		if user == nil {
			randPassword, genErr := crypto.GenerateID()
			if genErr != nil {
				return genErr
			}
			passwordHash, hashErr := crypto.HashPassword(randPassword)
			if hashErr != nil {
				return fmt.Errorf("hash password: %w", hashErr)
			}
			user = &domain.User{
				ID:          uuid.NewString(),
				NamespaceID: namespaceID,
				Username:    username,
				DisplayName: username,
				Type:        domain.UserTypeService,
				CreatedAt:   time.Now().UTC(),
			}
			if createErr := s.users.Create(ctx, user, passwordHash); createErr != nil {
				return fmt.Errorf("create user: %w", createErr)
			}
		}

		// One stable device per service account (not one per call), keeping only its newest
		// sessions: bridges may call this per message, which used to grow devices and sessions
		// without bound.
		deviceID := "svc-device-" + user.ID
		if _, err := s.devices.GetByID(ctx, deviceID); errors.Is(err, domain.ErrNotFound) {
			if err := s.devices.Create(ctx, &domain.Device{
				ID: deviceID, UserID: user.ID, DisplayName: "appservice", CreatedAt: time.Now().UTC(),
			}); err != nil {
				return fmt.Errorf("create device: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("get device: %w", err)
		}

		var sessErr error
		if _, token, sessErr = s.createSession(ctx, namespaceID, user.ID, deviceID); sessErr != nil {
			return sessErr
		}
		return s.sessions.PruneDevice(ctx, deviceID, serviceSessionsKept)
	})
	if txErr != nil {
		return "", "", fmt.Errorf("auth: ensure service user: %w", txErr)
	}
	return user.ID, token, nil
}

// reusableDevice returns the device a login asked to reuse, if it is this user's and enabled.
// Anything else (unknown, someone else's, disabled) silently gets a new device instead.
func (s *authService) reusableDevice(ctx context.Context, deviceID, userID string) *domain.Device {
	if deviceID == "" {
		return nil
	}
	d, err := s.devices.GetByID(ctx, deviceID)
	if err != nil || d.UserID != userID || d.Disabled {
		return nil
	}
	return d
}

func (s *authService) createSession(ctx context.Context, namespaceID, userID, deviceID string) (*domain.Session, string, error) {
	sessionID := uuid.NewString()
	token, err := crypto.NewSessionToken(sessionID, s.secret)
	if err != nil {
		return nil, "", err
	}
	session := &domain.Session{
		ID:          sessionID,
		NamespaceID: namespaceID,
		DeviceID:    deviceID,
		UserID:      userID,
		TokenHash:   crypto.TokenHash(token),
		ExpiresAt:   time.Now().UTC().Add(sessionTTL),
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return nil, "", fmt.Errorf("create session: %w", err)
	}
	return session, token, nil
}
