package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/daveontour/aimuseum/internal/appctx"
	"github.com/daveontour/aimuseum/internal/service"
)

// UserIDFromCtx extracts the authenticated user's ID injected by AuthMiddleware.
// Returns (0, false) when the request is unauthenticated or the middleware is
// running in non-enforcing mode and no valid session was found.
func UserIDFromCtx(ctx context.Context) (int64, bool) {
	v := appctx.UserIDFromCtx(ctx)
	return v, v != 0
}

// exemptPrefixes lists path prefixes that never require authentication.
// Exact matches and prefix matches are both handled below.
var exemptPrefixes = []string{
	"/static/",
	"/share/",
	"/s/",
	"/visitor/",
	"/admin",  // admin panel has its own session-based auth
}

// exemptExact lists paths that are always accessible without authentication.
var exemptExact = map[string]bool{
	"/health":        true,
	"/auth/login":    true,
	"/auth/register": true,
	"/login":         true,
}

// NewAuthMiddleware returns a middleware that authenticates every request via
// the dm_session cookie and injects the user_id into the request context.
//
// When required is true (AUTH_REQUIRED=true in .env), unauthenticated requests
// to non-exempt paths receive a 401 JSON response.
//
// When required is false (the default during incremental rollout), the
// middleware enriches the context if a valid session is found but passes all
// requests through — preserving single-tenant behaviour while Layers 4–9 are
// being implemented.
//
// Exempt from auth (regardless of required):
//   - GET /health
//   - GET /static/*
//   - POST /auth/login
//   - POST /auth/register
//   - Any /share/* path (future share-token flow)
func NewAuthMiddleware(svc *service.AuthService, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path

			// ── Exempt paths pass straight through ───────────────────────────
			if isExempt(path) {
				next.ServeHTTP(w, r)
				return
			}

			// ── Read session cookie ──────────────────────────────────────────
			var sessionID string
			if c, err := r.Cookie(service.AuthSessionCookieName); err == nil && c != nil {
				sessionID = c.Value
			}

			// ── Authenticate ─────────────────────────────────────────────────
			// Authenticate also slides the session TTL forward on each hit.
			user, isVisitor, err := svc.Authenticate(r.Context(), sessionID)
			if err != nil {
				// DB error — don't leak internals; treat as unauthenticated
				if required {
					writeAuthError(w, http.StatusInternalServerError, "session lookup failed")
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if user == nil {
				if required {
					// Browser navigation to an HTML page gets a redirect instead of JSON 401.
					if looksLikeBrowserNav(r) {
						http.Redirect(w, r, "/login", http.StatusFound)
						return
					}
					writeAuthError(w, http.StatusUnauthorized, "authentication required")
					return
				}
				// Non-enforcing: pass through without user_id in context
				next.ServeHTTP(w, r)
				return
			}

			// ── Inject user_id and visitor flag into context ──────────────────
			ctx := context.WithValue(r.Context(), appctx.ContextKeyUserID, user.ID)
			ctx = context.WithValue(ctx, appctx.ContextKeyIsVisitor, isVisitor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isExempt returns true if the path does not require authentication.
func isExempt(path string) bool {
	if exemptExact[path] {
		return true
	}
	for _, prefix := range exemptPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// looksLikeBrowserNav returns true when the request is a top-level browser
// navigation (Accept header prefers text/html) rather than an API/XHR call.
func looksLikeBrowserNav(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// writeAuthError writes a JSON error response consistent with the rest of the API.
func writeAuthError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  detail,
		"status": status,
	})
}
