package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateMultitenancy applies Layer 1 of the multi-tenancy migration.
//
// Specifically it:
//   - Creates the users, sessions, archive_shares, and audit_log tables.
//   - Adds a nullable user_id FK column (REFERENCES users) to every data table.
//   - Creates an index on user_id for each data table.
//   - Adds user_id (nullable) to app_configuration and updates its unique constraint.
//   - Updates the emails, import_control_last_run, and subject_configuration
//     unique constraints so they are scoped per user.
//   - Enables PostgreSQL Row-Level Security on every data table with a policy
//     that filters by user_id when app.current_user_id is set in the session.
//
// The function is fully idempotent — safe to run on an already-migrated database.
//
// NOTE: RLS is enabled with ENABLE (not FORCE), so the database owner/table-owner
// role bypasses it. Enforcement activates when the application is configured to
// connect as a non-owner role (Layer 10 — security hardening).
func migrateMultitenancy(ctx context.Context, conn *pgxpool.Conn) error {
	for i, stmt := range multitenancyDDL() {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			preview := stmt
			if len(preview) > 150 {
				preview = preview[:150] + "..."
			}
			return fmt.Errorf("multitenancy migration statement %d failed: %w\nSQL: %s", i+1, err, preview)
		}
	}
	return nil
}

// multitenancyDDL returns all DDL statements for Layer 1 in execution order.
func multitenancyDDL() []string {
	// Data tables that receive a user_id column.
	// Ordered so parents come before children (not strictly required since we
	// use IF NOT EXISTS, but cleaner for readability).
	dataTables := []string{
		// Core archive data
		"emails",
		"attachments",
		"media_blobs",
		"media_items",
		"messages",
		"message_attachments",
		"facebook_albums",
		"album_media",
		"facebook_posts",
		"post_media",
		"artefacts",
		"artefact_media",
		"reference_documents",
		"gemini_files",
		// People & places
		"contacts",
		"relationships",
		"places",
		"locations",
		"interests",
		// Per-user configuration
		"subject_configuration",
		"custom_voices",
		// Chat
		"chat_conversations",
		"chat_turns",
		// Profiles & saved content
		"complete_profiles",
		"saved_responses",
		// Import state
		"import_control_last_run",
		// Email processing rules
		"email_classifications",
		"email_matches",
		"email_exclusions",
		// Encryption / key material
		"sensitive_keyring",
		"visitor_key_hints",
		"private_store",
		"master_keys",
	}

	var stmts []string

	// ── Step 1: new identity / session / audit tables ─────────────────────────
	stmts = append(stmts, identityTablesDDL()...)

	// ── Step 2: add nullable user_id + index to every data table ─────────────
	for _, t := range dataTables {
		stmts = append(stmts, addUserIDColumnDDL(t))
		stmts = append(stmts,
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_user_id ON %s (user_id)`, t, t),
		)
	}

	// ── Step 3: app_configuration gets nullable user_id (separate — global vs per-user) ──
	stmts = append(stmts, addNullableUserIDColumnDDL("app_configuration"))
	stmts = append(stmts,
		`CREATE INDEX IF NOT EXISTS idx_app_configuration_user_id ON app_configuration (user_id)`,
	)

	// ── Step 4: unique-constraint updates ─────────────────────────────────────
	stmts = append(stmts, uniqueConstraintUpdatesDDL()...)

	// ── Step 5: Row-Level Security on every data table ────────────────────────
	for _, t := range dataTables {
		stmts = append(stmts, rlsStatements(t)...)
	}
	// app_configuration also gets RLS
	stmts = append(stmts, rlsStatements("app_configuration")...)

	// ── Step 6: sessions.is_visitor — marks sessions created via visitor key login ──
	stmts = append(stmts,
		`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS is_visitor BOOLEAN NOT NULL DEFAULT FALSE`,
	)

	return stmts
}

// ── New identity tables ───────────────────────────────────────────────────────

func identityTablesDDL() []string {
	return []string{
		// ── users ────────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS users (
			id            BIGSERIAL    PRIMARY KEY,
			email         VARCHAR(255) NOT NULL UNIQUE,
			password_hash VARCHAR(255) NOT NULL,
			display_name  VARCHAR(255),
			created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
			last_login_at TIMESTAMPTZ,
			is_active     BOOLEAN      NOT NULL DEFAULT TRUE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_users_email ON users (email)`,

		// ── sessions (DB-backed; replaces RAM SessionMasterStore for auth) ───
		`CREATE TABLE IF NOT EXISTS sessions (
			id         VARCHAR(64)  PRIMARY KEY,
			user_id    BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ  NOT NULL,
			created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user_id    ON sessions (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions (expires_at)`,

		// ── archive_shares (replaces visitor seats) ──────────────────────────
		`CREATE TABLE IF NOT EXISTS archive_shares (
			id                 VARCHAR(64)  PRIMARY KEY,
			user_id            BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			label              VARCHAR(255),
			password_hash      VARCHAR(255),
			expires_at         TIMESTAMPTZ,
			tool_access_policy JSONB,
			created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_archive_shares_user_id ON archive_shares (user_id)`,

		// ── audit_log ────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS audit_log (
			id          BIGSERIAL   PRIMARY KEY,
			user_id     BIGINT      REFERENCES users(id) ON DELETE SET NULL,
			event_type  VARCHAR(50) NOT NULL,
			ip_address  VARCHAR(45),
			user_agent  TEXT,
			details     JSONB,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_user_id    ON audit_log (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_event_type ON audit_log (event_type)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log (created_at)`,
	}
}

// ── Column helpers ────────────────────────────────────────────────────────────

// addUserIDColumnDDL returns an idempotent DO block that adds a nullable
// user_id column with a FK to users(id) ON DELETE CASCADE.
// Nullable (not NOT NULL) so existing rows and existing single-tenant imports
// are unaffected during the incremental rollout of Layers 2–8.
func addUserIDColumnDDL(table string) string {
	return fmt.Sprintf(`DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name   = '%s'
      AND column_name  = 'user_id'
  ) THEN
    ALTER TABLE %s
      ADD COLUMN user_id BIGINT REFERENCES users(id) ON DELETE CASCADE;
  END IF;
END$$`, table, table)
}

// addNullableUserIDColumnDDL is the same as addUserIDColumnDDL but uses
// ON DELETE SET NULL — appropriate for tables where global (user_id IS NULL)
// rows should survive user deletion (e.g. app_configuration).
func addNullableUserIDColumnDDL(table string) string {
	return fmt.Sprintf(`DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name   = '%s'
      AND column_name  = 'user_id'
  ) THEN
    ALTER TABLE %s
      ADD COLUMN user_id BIGINT REFERENCES users(id) ON DELETE SET NULL;
  END IF;
END$$`, table, table)
}

// ── Unique constraint updates ─────────────────────────────────────────────────

func uniqueConstraintUpdatesDDL() []string {
	return []string{
		// emails: (uid, folder) → (uid, folder, user_id)
		// The old constraint prevented two different users from having the same
		// message in the same folder.  Replace with a per-user constraint.
		`DO $$
BEGIN
  -- Drop the old single-user unique if present
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    WHERE t.relname = 'emails' AND c.conname = 'uq_email_uid_folder'
  ) THEN
    ALTER TABLE emails DROP CONSTRAINT uq_email_uid_folder;
  END IF;
  -- Add scoped constraint (NULL user_id treated as a distinct value by PG UNIQUE)
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    WHERE t.relname = 'emails' AND c.conname = 'uq_email_uid_folder_user'
  ) THEN
    ALTER TABLE emails
      ADD CONSTRAINT uq_email_uid_folder_user UNIQUE (uid, folder, user_id);
  END IF;
END$$`,

		// subject_configuration: add UNIQUE(user_id) to enforce one config per user
		`DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    WHERE t.relname = 'subject_configuration'
      AND c.conname = 'uq_subject_configuration_user'
  ) THEN
    ALTER TABLE subject_configuration
      ADD CONSTRAINT uq_subject_configuration_user UNIQUE (user_id);
  END IF;
END$$`,

		// import_control_last_run: replace single-column UNIQUE(import_type) with
		// partial unique indexes so global rows (user_id IS NULL) and per-user rows
		// are each unique within their own scope.
		`DO $$
DECLARE
  old_name TEXT;
BEGIN
  -- Find any single-column unique constraint on import_type (auto-named by PG)
  SELECT c.conname INTO old_name
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  WHERE t.relname = 'import_control_last_run'
    AND c.contype = 'u'
    AND c.conname != 'uq_import_control_type_user'
    AND array_length(c.conkey, 1) = 1;

  IF old_name IS NOT NULL THEN
    EXECUTE 'ALTER TABLE import_control_last_run DROP CONSTRAINT ' || quote_ident(old_name);
  END IF;
END$$`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_import_control_global
			ON import_control_last_run (import_type)
			WHERE user_id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_import_control_user
			ON import_control_last_run (import_type, user_id)
			WHERE user_id IS NOT NULL`,

		// app_configuration: replace UNIQUE(key) with partial indexes so the
		// same key can exist once globally and once per user.
		`DO $$
BEGIN
  -- Drop the auto-generated single-column constraint if present
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    WHERE t.relname = 'app_configuration'
      AND c.conname = 'app_configuration_key_key'
  ) THEN
    ALTER TABLE app_configuration DROP CONSTRAINT app_configuration_key_key;
  END IF;
END$$`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_app_config_global
			ON app_configuration (key)
			WHERE user_id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_app_config_user
			ON app_configuration (key, user_id)
			WHERE user_id IS NOT NULL`,
	}
}

// ── Row-Level Security ────────────────────────────────────────────────────────

// rlsStatements returns the three statements needed to set up RLS on one table:
//  1. ENABLE ROW LEVEL SECURITY
//  2. DROP POLICY IF EXISTS (idempotent reset)
//  3. CREATE POLICY
//
// The policy grants access when ANY of:
//   - user_id IS NULL              — legacy/global rows visible to all
//   - app.current_user_id not set  — no auth context (e.g. during migrations/seeds)
//   - user_id matches the session  — normal authenticated request
//
// FORCE ROW LEVEL SECURITY is intentionally omitted here.  The table owner
// (the DB role the app currently uses) bypasses ENABLE-only RLS.  Enforcement
// becomes automatic once the app is configured to connect as a non-owner role
// (planned for Layer 10 — security hardening).
func rlsStatements(table string) []string {
	policy := table + "_user_isolation"
	return []string{
		fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table),
		fmt.Sprintf(`DROP POLICY IF EXISTS %s ON %s`, policy, table),
		fmt.Sprintf(`CREATE POLICY %s ON %s
    USING (
        user_id IS NULL
        OR current_setting('app.current_user_id', TRUE) = ''
        OR user_id = NULLIF(current_setting('app.current_user_id', TRUE), '')::BIGINT
    )
    WITH CHECK (
        user_id IS NULL
        OR current_setting('app.current_user_id', TRUE) = ''
        OR user_id = NULLIF(current_setting('app.current_user_id', TRUE), '')::BIGINT
    )`, policy, table),
	}
}
