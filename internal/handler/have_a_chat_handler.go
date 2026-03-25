package handler

import (
	"encoding/json"
	"net/http"

	"github.com/daveontour/aimuseum/internal/keystore"
	"github.com/daveontour/aimuseum/internal/model"
	"github.com/daveontour/aimuseum/internal/service"
	"github.com/go-chi/chi/v5"
)

// HaveAChatHandler handles the autonomous Claude↔Gemini conversation endpoint.
type HaveAChatHandler struct {
	svc          *service.ChatService
	sessionStore *keystore.SessionMasterStore
}

// NewHaveAChatHandler creates a HaveAChatHandler.
func NewHaveAChatHandler(svc *service.ChatService, sessionStore *keystore.SessionMasterStore) *HaveAChatHandler {
	return &HaveAChatHandler{svc: svc, sessionStore: sessionStore}
}

// RegisterRoutes mounts the have-a-chat endpoint on r.
func (h *HaveAChatHandler) RegisterRoutes(r chi.Router) {
	r.Post("/chat/have-a-chat/turn", h.Turn)
}

// POST /chat/have-a-chat/turn
func (h *HaveAChatHandler) Turn(w http.ResponseWriter, r *http.Request) {
	if !RequireOwnerMasterUnlock(w, r, h.sessionStore) {
		return
	}
	var req model.HaveAChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SpeakingSlot != "a" && req.SpeakingSlot != "b" {
		writeError(w, http.StatusBadRequest, "speaking_slot must be 'a' or 'b'")
		return
	}

	resp, err := h.svc.GenerateHaveAChatTurn(r.Context(), r, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, resp)
}
