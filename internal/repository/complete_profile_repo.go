package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CompleteProfileRepo accesses the complete_profiles table.
type CompleteProfileRepo struct {
	pool *pgxpool.Pool
}

// NewCompleteProfileRepo creates a CompleteProfileRepo.
func NewCompleteProfileRepo(pool *pgxpool.Pool) *CompleteProfileRepo {
	return &CompleteProfileRepo{pool: pool}
}

// ListNames returns all contact names that have complete profiles.
func (r *CompleteProfileRepo) ListNames(ctx context.Context) ([]string, error) {
	uid := uidFromCtx(ctx)
	q := `SELECT name FROM complete_profiles WHERE name IS NOT NULL AND name != ''`
	args := []any{}
	q, args = addUIDFilter(q, args, uid)
	q += " ORDER BY name"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list complete profile names: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan name: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// GetByName returns the profile for a contact by name. Returns nil, nil if not found.
func (r *CompleteProfileRepo) GetByName(ctx context.Context, name string) (*string, error) {
	uid := uidFromCtx(ctx)
	q := `SELECT profile FROM complete_profiles WHERE name = $1`
	args := []any{name}
	q, args = addUIDFilter(q, args, uid)
	var profile *string
	err := r.pool.QueryRow(ctx, q, args...).Scan(&profile)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get complete profile by name: %w", err)
	}
	return profile, nil
}

// Upsert creates or updates a complete profile by name.
// The table has no unique constraint on name; we update existing rows or insert if none.
func (r *CompleteProfileRepo) Upsert(ctx context.Context, name, profile string) error {
	uid := uidFromCtx(ctx)
	q := `UPDATE complete_profiles SET profile = $2, updated_at = NOW() WHERE name = $1`
	args := []any{name, profile}
	q, args = addUIDFilter(q, args, uid)
	res, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update complete profile: %w", err)
	}
	if res.RowsAffected() > 0 {
		return nil
	}
	_, err = r.pool.Exec(ctx,
		`INSERT INTO complete_profiles (name, profile, user_id) VALUES ($1, $2, $3)`,
		name, profile, uidVal(uid),
	)
	if err != nil {
		return fmt.Errorf("insert complete profile: %w", err)
	}
	return nil
}

// DeleteByName deletes a complete profile by name. Returns true if a row was deleted.
func (r *CompleteProfileRepo) DeleteByName(ctx context.Context, name string) (bool, error) {
	uid := uidFromCtx(ctx)
	q := `DELETE FROM complete_profiles WHERE name = $1`
	args := []any{name}
	q, args = addUIDFilter(q, args, uid)
	res, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("delete complete profile: %w", err)
	}
	return res.RowsAffected() > 0, nil
}
