package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrVisitorSessionLLMNotUpdated is returned when PATCH targets a missing, expired, or non-visitor session.
var ErrVisitorSessionLLMNotUpdated = errors.New("session not found, expired, or not a visitor session")

// sessionLLMJSON is the JSON shape stored in sessions.visitor_llm_overrides.
type sessionLLMJSON struct {
	GeminiAPIKey    string `json:"gemini_api_key,omitempty"`
	AnthropicAPIKey string `json:"anthropic_api_key,omitempty"`
	GeminiModel     string `json:"gemini_model,omitempty"`
	ClaudeModel     string `json:"claude_model,omitempty"`
	TavilyAPIKey    string `json:"tavily_api_key,omitempty"`
}

func sessionLLMFromStored(s UserLLMStored) sessionLLMJSON {
	return sessionLLMJSON{
		GeminiAPIKey:    s.GeminiAPIKey,
		AnthropicAPIKey: s.AnthropicAPIKey,
		GeminiModel:     s.GeminiModel,
		ClaudeModel:     s.ClaudeModel,
		TavilyAPIKey:    s.TavilyAPIKey,
	}
}

func (j sessionLLMJSON) toStored() UserLLMStored {
	return UserLLMStored{
		GeminiAPIKey:    j.GeminiAPIKey,
		AnthropicAPIKey: j.AnthropicAPIKey,
		GeminiModel:     j.GeminiModel,
		ClaudeModel:     j.ClaudeModel,
		TavilyAPIKey:    j.TavilyAPIKey,
	}
}

// GetSessionVisitorLLM loads visitor_llm_overrides for an active visitor session.
// Returns empty values when the session is missing, expired, not visitor, or has no JSON.
func (r *UserRepo) GetSessionVisitorLLM(ctx context.Context, sessionID string) (*UserLLMStored, error) {
	if sessionID == "" {
		return &UserLLMStored{}, nil
	}
	var raw []byte
	err := r.pool.QueryRow(ctx, `
		SELECT visitor_llm_overrides
		FROM sessions
		WHERE id = $1 AND expires_at > NOW() AND is_visitor = TRUE`,
		sessionID,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return &UserLLMStored{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return &UserLLMStored{}, nil
	}
	var j sessionLLMJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return &UserLLMStored{}, nil
	}
	out := j.toStored()
	return &out, nil
}

// PatchSessionVisitorLLM merges patch into visitor_llm_overrides for the current visitor session only.
func (r *UserRepo) PatchSessionVisitorLLM(ctx context.Context, sessionID string, p UserLLMPatch) error {
	if sessionID == "" {
		return ErrVisitorSessionLLMNotUpdated
	}
	cur, err := r.GetSessionVisitorLLM(ctx, sessionID)
	if err != nil {
		return err
	}
	next := mergeLLMStoredWithPatch(*cur, p)
	j := sessionLLMFromStored(next)
	payload, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE sessions
		SET visitor_llm_overrides = $2::jsonb
		WHERE id = $1 AND expires_at > NOW() AND is_visitor = TRUE`,
		sessionID, string(payload),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrVisitorSessionLLMNotUpdated
	}
	return nil
}
