package handler

import (
	"net/http"

	"github.com/daveontour/aimuseum/internal/keystore"
)

// RequireOwnerMasterUnlock writes 403 and returns false unless this browser session was
// unlocked with the owner master key (not a visitor keyring seat). Use on endpoints
// that modify or delete archive data.
func RequireOwnerMasterUnlock(w http.ResponseWriter, r *http.Request, sessionStore *keystore.SessionMasterStore) bool {
	if sessionStore == nil {
		writeError(w, http.StatusForbidden, "owner master key unlock required in this browser session to modify or delete data")
		return false
	}
	unlocked, master := sessionStore.SessionStatus(r)
	if !unlocked || !master {
		writeError(w, http.StatusForbidden, "owner master key unlock required — visitor sessions cannot modify or delete data")
		return false
	}
	return true
}
