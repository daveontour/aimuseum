package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/daveontour/aimuseum/internal/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChatRepo accesses the chat_conversations and chat_turns tables.
type ChatRepo struct {
	pool *pgxpool.Pool
}

// NewChatRepo creates a ChatRepo.
func NewChatRepo(pool *pgxpool.Pool) *ChatRepo {
	return &ChatRepo{pool: pool}
}

// CreateConversation inserts a new chat_conversations row.
func (r *ChatRepo) CreateConversation(ctx context.Context, title, voice string) (*model.ChatConversation, error) {
	uid := uidFromCtx(ctx)
	var c model.ChatConversation
	err := r.pool.QueryRow(ctx,
		`INSERT INTO chat_conversations (title, voice, user_id)
		 VALUES ($1, $2, $3)
		 RETURNING id, title, voice, created_at, updated_at, last_message_at`,
		title, voice, uidVal(uid),
	).Scan(&c.ID, &c.Title, &c.Voice, &c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt)
	if err != nil {
		return nil, fmt.Errorf("CreateConversation: %w", err)
	}
	return &c, nil
}

// GetConversation returns a single conversation by ID, or nil if not found.
func (r *ChatRepo) GetConversation(ctx context.Context, id int64) (*model.ChatConversation, error) {
	uid := uidFromCtx(ctx)
	q := `SELECT id, title, voice, created_at, updated_at, last_message_at
	      FROM chat_conversations WHERE id = $1`
	args := []any{id}
	q, args = addUIDFilter(q, args, uid)
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("GetConversation %d: %w", id, err)
	}
	defer rows.Close()
	if rows.Next() {
		var c model.ChatConversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Voice, &c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt); err != nil {
			return nil, err
		}
		return &c, nil
	}
	return nil, rows.Err()
}

// ListConversations returns conversations ordered by most recent activity.
func (r *ChatRepo) ListConversations(ctx context.Context, limit *int) ([]*model.ChatConversation, error) {
	uid := uidFromCtx(ctx)
	q := `SELECT id, title, voice, created_at, updated_at, last_message_at
	      FROM chat_conversations WHERE TRUE`
	args := []any{}
	q, args = addUIDFilter(q, args, uid)
	q += " ORDER BY COALESCE(last_message_at, created_at) DESC"
	if limit != nil {
		args = append(args, *limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListConversations: %w", err)
	}
	defer rows.Close()
	var out []*model.ChatConversation
	for rows.Next() {
		var c model.ChatConversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Voice, &c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// TurnCount returns the number of turns for a conversation.
func (r *ChatRepo) TurnCount(ctx context.Context, conversationID int64) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM chat_turns WHERE conversation_id = $1`, conversationID,
	).Scan(&n)
	return n, err
}

// TurnCountsBatch returns a map of conversation_id → turn count for all given IDs in one query.
func (r *ChatRepo) TurnCountsBatch(ctx context.Context, ids []int64) (map[int64]int64, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT conversation_id, COUNT(*) FROM chat_turns WHERE conversation_id = ANY($1) GROUP BY conversation_id`,
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("TurnCountsBatch: %w", err)
	}
	defer rows.Close()
	counts := make(map[int64]int64, len(ids))
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("TurnCountsBatch scan: %w", err)
		}
		counts[id] = n
	}
	return counts, rows.Err()
}

// UpdateConversation modifies title and/or voice.
func (r *ChatRepo) UpdateConversation(ctx context.Context, id int64, title, voice *string) (*model.ChatConversation, error) {
	uid := uidFromCtx(ctx)
	q := `UPDATE chat_conversations
	      SET title = COALESCE($1, title), voice = COALESCE($2, voice), updated_at = NOW()
	      WHERE id = $3`
	args := []any{title, voice, id}
	q, args = addUIDFilter(q, args, uid)
	q += ` RETURNING id, title, voice, created_at, updated_at, last_message_at`
	var c model.ChatConversation
	err := r.pool.QueryRow(ctx, q, args...).
		Scan(&c.ID, &c.Title, &c.Voice, &c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("UpdateConversation %d: %w", id, err)
	}
	return &c, nil
}

// DeleteConversation removes a conversation (cascade deletes turns).
func (r *ChatRepo) DeleteConversation(ctx context.Context, id int64) error {
	uid := uidFromCtx(ctx)
	q := `DELETE FROM chat_conversations WHERE id = $1`
	args := []any{id}
	q, args = addUIDFilter(q, args, uid)
	_, err := r.pool.Exec(ctx, q, args...)
	return err
}

// GetTurns returns the last N turns for a conversation, in chronological order.
func (r *ChatRepo) GetTurns(ctx context.Context, conversationID int64, limit int) ([]*model.ChatTurn, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, conversation_id, turn_number, user_input, response_text, voice, temperature, created_at
		 FROM (
		   SELECT id, conversation_id, turn_number, user_input, response_text, voice, temperature, created_at
		   FROM chat_turns WHERE conversation_id = $1
		   ORDER BY turn_number DESC LIMIT $2
		 ) sub ORDER BY turn_number ASC`,
		conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("GetTurns: %w", err)
	}
	defer rows.Close()
	var out []*model.ChatTurn
	for rows.Next() {
		var t model.ChatTurn
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.TurnNumber, &t.UserInput, &t.ResponseText,
			&t.Voice, &t.Temperature, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// SaveTurn inserts a new chat_turns row and updates last_message_at in a single transaction.
func (r *ChatRepo) SaveTurn(ctx context.Context, conversationID int64, userInput, responseText, voice string, temperature float64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SaveTurn begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx,
		`INSERT INTO chat_turns (conversation_id, turn_number, user_input, response_text, voice, temperature)
		 VALUES ($1,
		   COALESCE((SELECT MAX(turn_number) FROM chat_turns WHERE conversation_id = $1), 0) + 1,
		   $2, $3, $4, $5)`,
		conversationID, userInput, responseText, voice, temperature,
	)
	if err != nil {
		return fmt.Errorf("SaveTurn insert: %w", err)
	}
	_, err = tx.Exec(ctx,
		`UPDATE chat_conversations SET last_message_at = $1 WHERE id = $2`,
		time.Now(), conversationID)
	if err != nil {
		return fmt.Errorf("SaveTurn update: %w", err)
	}
	return tx.Commit(ctx)
}
