package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	appcrypto "github.com/daveontour/aimuseum/internal/crypto"
	"github.com/daveontour/aimuseum/internal/repository"
)

const (
	// authSessionCookieName is the HttpOnly cookie that carries the auth session ID.
	// Separate from dm_keyring_sid which carries the keyring unlock password.
	AuthSessionCookieName = "dm_session"

	authSessionTTL     = 24 * time.Hour
	minPasswordLength  = 12
	sessionCleanupFreq = 30 * time.Minute
)

// ErrInvalidCredentials is returned when email/password does not match.
var ErrInvalidCredentials = errors.New("invalid email or password")

// ErrEmailTaken is returned when registering with an already-used email.
var ErrEmailTaken = errors.New("email address is already registered")

// ErrWeakPassword is returned when the password does not meet length requirements.
var ErrWeakPassword = fmt.Errorf("password must be at least %d characters", minPasswordLength)

// ErrUserNotFound is returned when the session points to a deleted/inactive user.
var ErrUserNotFound = errors.New("user not found")

// AuthService handles user registration, login, session management, and
// password changes.  It is intentionally separate from the keyring-unlock
// SessionMasterStore (which stores the keyring decrypt password in RAM).
type AuthService struct {
	users    *repository.UserRepo
	secure   bool // Secure flag for Set-Cookie (true behind HTTPS)
}

// NewAuthService creates an AuthService.  secure should be true when the app
// is served over HTTPS.
func NewAuthService(users *repository.UserRepo, secure bool) *AuthService {
	svc := &AuthService{users: users, secure: secure}
	go svc.cleanupLoop()
	return svc
}

// ── Registration ─────────────────────────────────────────────────────────────

// Register creates a new user account.  Returns ErrEmailTaken if the email is
// already in use, ErrWeakPassword if the password is too short.
func (s *AuthService) Register(ctx context.Context, email, password, displayName string) (*repository.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)

	if len(password) < minPasswordLength {
		return nil, ErrWeakPassword
	}

	exists, err := s.users.EmailExists(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("check email: %w", err)
	}
	if exists {
		return nil, ErrEmailTaken
	}

	hash, err := appcrypto.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.users.Create(ctx, email, hash, displayName)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// ── Login ────────────────────────────────────────────────────────────────────

// Login validates credentials and creates a new session.  Returns the session
// ID (to be stored in the cookie) and the authenticated user.
// Returns ErrInvalidCredentials on any mismatch to prevent user enumeration.
func (s *AuthService) Login(ctx context.Context, email, password string) (sessionID string, user *repository.User, err error) {
	email = strings.ToLower(strings.TrimSpace(email))

	u, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		return "", nil, fmt.Errorf("find user: %w", err)
	}

	// Always run VerifyPassword even when the user is not found — constant
	// time behaviour prevents user-enumeration via response timing.
	dummyHash := "$argon2id$v=19$m=65536,t=3,p=4$dGhpcyBpcyBub3QgcmVhbA$dGhpcyBpcyBub3QgcmVhbA"
	checkHash := dummyHash
	if u != nil {
		checkHash = u.PasswordHash
	}
	ok, verifyErr := appcrypto.VerifyPassword(password, checkHash)
	if verifyErr != nil {
		return "", nil, fmt.Errorf("verify password: %w", verifyErr)
	}
	if !ok || u == nil || !u.IsActive {
		return "", nil, ErrInvalidCredentials
	}

	sid, err := randomSessionID()
	if err != nil {
		return "", nil, fmt.Errorf("generate session: %w", err)
	}

	if err := s.users.CreateSession(ctx, sid, u.ID, time.Now().Add(authSessionTTL), false); err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}

	s.users.TouchLastLogin(ctx, u.ID)
	return sid, u, nil
}

// ── Session lookup ───────────────────────────────────────────────────────────

// Authenticate looks up the session ID from the cookie value and returns the
// associated user.  Returns (nil, nil) when the session is missing or expired —
// the caller should treat this as unauthenticated rather than an error.
// Authenticate looks up the session and returns the associated user and whether
// the session is a visitor session (created via visitor key login).
// Returns (nil, false, nil) when the session is absent or expired.
func (s *AuthService) Authenticate(ctx context.Context, sessionID string) (*repository.User, bool, error) {
	if sessionID == "" {
		return nil, false, nil
	}
	sess, err := s.users.FindSession(ctx, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("find session: %w", err)
	}
	if sess == nil {
		return nil, false, nil
	}

	// Slide the expiry forward on activity.
	_ = s.users.ExtendSession(ctx, sessionID, time.Now().Add(authSessionTTL))

	user, err := s.users.FindByID(ctx, sess.UserID)
	if err != nil {
		return nil, false, fmt.Errorf("find user: %w", err)
	}
	if user == nil || !user.IsActive {
		return nil, false, nil
	}
	return user, sess.IsVisitor, nil
}

// ── Logout ───────────────────────────────────────────────────────────────────

// Logout deletes the session from the database.  The caller is responsible for
// expiring the cookie on the HTTP response.
func (s *AuthService) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.users.DeleteSession(ctx, sessionID)
}

// ── Change password ──────────────────────────────────────────────────────────

// ChangePassword verifies currentPassword then stores a new hash.
// Returns ErrInvalidCredentials when currentPassword is wrong.
func (s *AuthService) ChangePassword(ctx context.Context, userID int64, currentPassword, newPassword string) error {
	if len(newPassword) < minPasswordLength {
		return ErrWeakPassword
	}

	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("find user: %w", err)
	}
	if user == nil {
		return ErrUserNotFound
	}

	ok, err := appcrypto.VerifyPassword(currentPassword, user.PasswordHash)
	if err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}

	newHash, err := appcrypto.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	return s.users.UpdatePasswordHash(ctx, userID, newHash)
}

// ── Share sessions ───────────────────────────────────────────────────────────

// CreateShareSession creates a DB session scoped to ownerUserID for a share visitor.
// Unlike Login, no password verification is performed — the share token validates the visitor.
func (s *AuthService) CreateShareSession(ctx context.Context, ownerUserID int64) (string, error) {
	sid, err := randomSessionID()
	if err != nil {
		return "", fmt.Errorf("generate share session: %w", err)
	}
	if err := s.users.CreateSession(ctx, sid, ownerUserID, time.Now().Add(authSessionTTL), true); err != nil {
		return "", fmt.Errorf("create share session: %w", err)
	}
	return sid, nil
}

// ── Cookie helpers ───────────────────────────────────────────────────────────

// CookieMaxAge is the Set-Cookie Max-Age for a new auth session.
const CookieMaxAge = int(authSessionTTL / time.Second)

// ── Internals ────────────────────────────────────────────────────────────────

func randomSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *AuthService) cleanupLoop() {
	t := time.NewTicker(sessionCleanupFreq)
	defer t.Stop()
	for range t.C {
		if err := s.users.PurgeExpiredSessions(context.Background()); err != nil {
			slog.Warn("session cleanup failed", "err", err)
		}
	}
}
