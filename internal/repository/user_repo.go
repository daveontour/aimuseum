package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User represents a row from the users table.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	DisplayName  string
	IsActive     bool
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

// AuthSession represents a row from the sessions table.
type AuthSession struct {
	ID        string
	UserID    int64
	ExpiresAt time.Time
	IsVisitor bool
}

// UserRepo handles all database operations for users and auth sessions.
type UserRepo struct {
	pool *pgxpool.Pool
}

// NewUserRepo creates a UserRepo.
func NewUserRepo(pool *pgxpool.Pool) *UserRepo {
	return &UserRepo{pool: pool}
}

// Create inserts a new user and returns the created record.
func (r *UserRepo) Create(ctx context.Context, email, passwordHash, displayName string) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name)
		 VALUES ($1, $2, NULLIF($3, ''))
		 RETURNING id, email, password_hash, COALESCE(display_name, ''), is_active, created_at`,
		email, passwordHash, displayName,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.IsActive, &u.CreatedAt)
	return &u, err
}

// FindByEmail returns the user with the given email, or nil if not found.
func (r *UserRepo) FindByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, COALESCE(display_name, ''), is_active, created_at
		 FROM users WHERE email = $1`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.IsActive, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

// FindByID returns the user with the given ID, or nil if not found.
func (r *UserRepo) FindByID(ctx context.Context, id int64) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, COALESCE(display_name, ''), is_active, created_at
		 FROM users WHERE id = $1`,
		id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.IsActive, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

// UpdatePasswordHash replaces the password hash for a user.
func (r *UserRepo) UpdatePasswordHash(ctx context.Context, id int64, hash string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2`,
		hash, id,
	)
	return err
}

// TouchLastLogin sets last_login_at = NOW() for the given user.
func (r *UserRepo) TouchLastLogin(ctx context.Context, id int64) {
	// Best-effort — ignore error so login still succeeds if this fails.
	_, _ = r.pool.Exec(ctx, `UPDATE users SET last_login_at = NOW() WHERE id = $1`, id)
}

// EmailExists reports whether an email address is already registered.
func (r *UserRepo) EmailExists(ctx context.Context, email string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, email,
	).Scan(&exists)
	return exists, err
}

// ── Sessions ──────────────────────────────────────────────────────────────────

// CreateSession inserts a new authenticated session.
// isVisitor should be true for sessions created via visitor key login.
func (r *UserRepo) CreateSession(ctx context.Context, id string, userID int64, expiresAt time.Time, isVisitor bool) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at, is_visitor) VALUES ($1, $2, $3, $4)`,
		id, userID, expiresAt, isVisitor,
	)
	return err
}

// FindSession returns a non-expired session by ID, or nil if not found/expired.
func (r *UserRepo) FindSession(ctx context.Context, id string) (*AuthSession, error) {
	var s AuthSession
	err := r.pool.QueryRow(ctx,
		`SELECT id, user_id, expires_at, is_visitor FROM sessions
		 WHERE id = $1 AND expires_at > NOW()`,
		id,
	).Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.IsVisitor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// ExtendSession pushes expires_at forward.
func (r *UserRepo) ExtendSession(ctx context.Context, id string, newExpiry time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE sessions SET expires_at = $1 WHERE id = $2`,
		newExpiry, id,
	)
	return err
}

// DeleteSession removes a session (used on logout).
func (r *UserRepo) DeleteSession(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

// PurgeExpiredSessions deletes all sessions past their expiry.  Called
// periodically from the background cleanup goroutine in AuthService.
func (r *UserRepo) PurgeExpiredSessions(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= NOW()`)
	return err
}

// ListAll returns every user ordered by created_at ascending.
func (r *UserRepo) ListAll(ctx context.Context) ([]*User, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, email, password_hash, COALESCE(display_name, ''), is_active, created_at
		 FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.IsActive, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, &u)
	}
	return users, rows.Err()
}

// Delete removes a user by ID; all associated data cascades via FK.
func (r *UserRepo) Delete(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return err
}
