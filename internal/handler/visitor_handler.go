package handler

import (
	"encoding/json"
	"net/http"

	"github.com/daveontour/aimuseum/internal/keystore"
	"github.com/daveontour/aimuseum/internal/service"
	"github.com/go-chi/chi/v5"
)

// VisitorHandler provides unauthenticated endpoints that let a visitor discover
// an archive by subject name or email and log in with a visitor key.
type VisitorHandler struct {
	svc          *service.VisitorService
	authSvc      *service.AuthService
	sessionStore *keystore.SessionMasterStore
	secure       bool
}

// NewVisitorHandler creates a VisitorHandler.
func NewVisitorHandler(
	svc *service.VisitorService,
	authSvc *service.AuthService,
	sessionStore *keystore.SessionMasterStore,
	secure bool,
) *VisitorHandler {
	return &VisitorHandler{svc: svc, authSvc: authSvc, sessionStore: sessionStore, secure: secure}
}

// RegisterRoutes mounts visitor endpoints.  Both are exempt from auth middleware
// (see exemptPrefixes in internal/middleware/auth.go).
func (h *VisitorHandler) RegisterRoutes(r chi.Router) {
	r.Get("/visitor/hints", h.GetHints)
	r.Post("/visitor/login", h.Login)
}

// GET /visitor/hints?identifier=<email>
// Returns hint strings for the archive owner identified by email address.
// Always responds 200 with { "hints": [] } even when the email is unknown,
// to avoid leaking account existence.
func (h *VisitorHandler) GetHints(w http.ResponseWriter, r *http.Request) {
	identifier := r.URL.Query().Get("identifier")
	hints, err := h.svc.GetHintsByEmail(r.Context(), identifier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not retrieve hints")
		return
	}
	writeJSON(w, map[string]any{"hints": hints})
}

// POST /visitor/login
// Body: { "identifier": "<name or email>", "key": "<visitor key>" }
// Verifies the visitor key against the named archive's keyring, creates a
// dm_session cookie scoped to that archive's owner, and stores the key in the
// RAM keystore so encrypted data is accessible.
func (h *VisitorHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifier string `json:"identifier"`
		Key        string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	userID, err := h.svc.ResolveUserID(r.Context(), req.Identifier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "archive lookup failed")
		return
	}
	if userID == -1 {
		writeError(w, http.StatusNotFound, "archive not found")
		return
	}

	ok, err := h.svc.VerifyVisitorKey(r.Context(), userID, req.Key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key verification failed")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid visitor key")
		return
	}

	// For multi-tenant archives (userID > 0) create a DB-backed session so
	// the auth middleware scopes all subsequent requests to this user's data.
	if userID > 0 {
		sessionID, err := h.authSvc.CreateShareSession(r.Context(), userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session creation failed")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     service.AuthSessionCookieName,
			Value:    sessionID,
			Path:     "/",
			MaxAge:   service.CookieMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   h.secure,
		})
	}

	// Store the visitor key in RAM so the keyring unlock layer can decrypt
	// sensitive data for this session (mirrors session_handler.go behaviour).
	if err := h.sessionStore.Put(w, r, req.Key, false); err != nil {
		writeError(w, http.StatusInternalServerError, "session store error")
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}
