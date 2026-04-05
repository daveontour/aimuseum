package database

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrateBilling creates tables for the billing database (LLM usage events only).
func MigrateBilling(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire billing connection: %w", err)
	}
	defer conn.Release()

	for _, stmt := range billingSchemaDDL() {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			preview := stmt
			if len(preview) > 100 {
				preview = preview[:100] + "..."
			}
			return fmt.Errorf("billing migration failed (%s): %w", preview, err)
		}
	}
	slog.Info("billing database migration complete")
	return nil
}

func billingSchemaDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS llm_usage_events (
			id                BIGSERIAL PRIMARY KEY,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			provider          VARCHAR(32) NOT NULL,
			user_id           BIGINT,
			is_visitor        BOOLEAN NOT NULL DEFAULT FALSE,
			input_tokens      INT NOT NULL,
			output_tokens     INT NOT NULL,
			model_name        TEXT,
			user_email        TEXT,
			user_first_name   TEXT,
			user_family_name  TEXT,
			used_server_llm_key BOOLEAN
		)`,
		`CREATE INDEX IF NOT EXISTS idx_llm_usage_events_created_at ON llm_usage_events (created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_llm_usage_events_user_created ON llm_usage_events (user_id, created_at)`,
		`ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS user_email TEXT`,
		`ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS user_first_name TEXT`,
		`ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS user_family_name TEXT`,
		`ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS used_server_llm_key BOOLEAN`,
		`ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS succeeded BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS error_message TEXT`,
	}
}
