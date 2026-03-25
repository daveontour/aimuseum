// Package appctx defines shared context keys used across packages
// (middleware, crypto, repository) without creating import cycles.
package appctx

import "context"

// contextKey is a private type for this package's context keys.
type contextKey int

const (
	// ContextKeyUserID is the context key for the authenticated user's ID (int64).
	// Injected by middleware.NewAuthMiddleware on every authenticated request.
	// Read via UserIDFromCtx.
	ContextKeyUserID contextKey = iota

	// ContextKeyIsVisitor marks sessions created via visitor key login (bool).
	// Injected alongside ContextKeyUserID by the auth middleware.
	// Read via IsVisitorFromCtx.
	ContextKeyIsVisitor
)

// UserIDFromCtx extracts the authenticated user's ID from the request context.
// Returns 0 when the request is unauthenticated or the middleware is running
// in non-enforcing mode and no valid session was found.
func UserIDFromCtx(ctx context.Context) int64 {
	v, _ := ctx.Value(ContextKeyUserID).(int64)
	return v
}

// IsVisitorFromCtx returns true when the current session was created via visitor
// key login (as opposed to a normal user login or registration).
func IsVisitorFromCtx(ctx context.Context) bool {
	v, _ := ctx.Value(ContextKeyIsVisitor).(bool)
	return v
}
