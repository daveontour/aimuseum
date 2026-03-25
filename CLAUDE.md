# Digital Museum — CLAUDE.md

## Project Overview

Digital Museum is a multi-tenant AI-powered digital archive platform. Each registered user
owns one archive containing their entire digital life (emails, messages, photos, Facebook,
iMessage, WhatsApp, documents), imported into a PostgreSQL database and queryable through
an AI chat interface. Both Claude and Gemini can access the data via a tool-calling layer
and answer questions, adopt personas, and explore the archive conversationally.

Multiple users can be hosted on a single deployment. Every piece of archive data is scoped
to its owning user via a `user_id` foreign key, enforced at the repository layer and backed
by PostgreSQL Row-Level Security.

## Tech Stack

- **Backend:** Go 1.25, Chi v5 router, pgx v5 PostgreSQL driver
- **Frontend:** Vanilla JavaScript (no framework), marked.js, highlight.js, Font Awesome
- **Database:** PostgreSQL 14+ with `pg_trgm` and `pgcrypto`
- **AI Providers:** Anthropic Claude (claude-sonnet-4-6) and Google Gemini (gemini-2.5-flash)
- **Module:** `github.com/daveontour/aimuseum`

## Project Layout

```
cmd/
  server/         ← HTTP server entry point (main.go)
  runner/         ← Batch data importer (runner.json config)
  launcher/       ← Windows GUI launcher
internal/
  ai/             ← Claude & Gemini providers, tool definitions & executor
  api/router/     ← Route wiring (router.go)
  appctx/         ← Shared context key (ContextKeyUserID / UserIDFromCtx)
  config/         ← Env-var config loading
  crypto/         ← Encryption / key derivation (keyring scoped by user_id)
  database/       ← Connection pool, migrations (including multitenancy DDL)
  handler/        ← HTTP request handlers (~35 files)
  keystore/       ← RAM master key (unlocks encrypted data per session)
  middleware/      ← Logger, Recoverer, AuthMiddleware
  model/          ← Shared data types / DTOs
  repository/     ← Database access via pgx (~17 repos, all user-scoped)
  service/        ← Business logic (~20 services)
static/
  css/            ← museum_of.css (all styles, ~8000 lines)
  data/           ← voice_instructions.json, seed JSON files
  images/         ← Voice persona images
  js/museum/      ← Frontend modules (foundation.js, app.js, chat.js, auth.js, …)
templates/        ← index.template.html (SPA), login.html, share.html
sqlc/             ← schema.sql (full DB schema), sqlc.yaml
runner.json       ← Data import configuration
```

## Build & Run

```bash
# Run dev server (reads .env automatically)
make run                          # go run ./cmd/server

# Build binaries
make build-exe                    # bin/digitalmuseum.exe
make build-runner                 # runner.exe (data importer)
make build-launcher               # launcher.exe (Windows GUI)

# Run data importer
make run-runner                   # go run ./cmd/runner -config runner.json

# Tests / lint
make test
make lint                         # requires golangci-lint
make tidy                         # go mod tidy

# Regenerate DB queries (after schema changes)
make generate                     # requires sqlc
```

## Configuration (`.env`)

The server searches for `.env` upward from the working directory.

| Variable | Required | Description |
|---|---|---|
| `DB_HOST` / `DB_PORT` / `DB_NAME` / `DB_USER` / `DB_PASSWORD` | Yes | PostgreSQL connection |
| `ANTHROPIC_API_KEY` | At least one AI key needed | Claude API |
| `CLAUDE_MODEL_NAME` | No | Default: `claude-sonnet-4-6` |
| `GEMINI_API_KEY` | At least one AI key needed | Gemini API |
| `GEMINI_MODEL_NAME` | No | Default: `gemini-2.5-flash` |
| `TAVILY_API_KEY` | No | Enables `search_tavily` web-search tool |
| `KEYRING_PEPPER` | Yes | Secret for encryption key derivation |
| `PORT` | No | HTTP port (default 8080) |
| `SESSION_COOKIE_SECURE` | No | Set `true` for HTTPS deployments |
| `AUTH_REQUIRED` | No | Set `true` to enforce authentication on all routes |
| `ENABLE_PPROF` | No | Set `true` to expose `/debug/pprof` on `:6060` |

## Authentication & Authorisation

### Overview

The system uses two independent security concepts that work in tandem:

1. **Authentication** — who is the user? Handled by `AuthService` + `AuthMiddleware` using a DB-backed session cookie (`dm_session`).
2. **Data authorisation** — which rows can they see? Handled by the repository layer adding `AND user_id = $N` to every query, backed by PostgreSQL Row-Level Security.

### Authentication Flow

1. User registers via `POST /auth/register` or logs in via `POST /auth/login`.
2. Passwords are hashed with **argon2id** (minimum 12 characters); `internal/crypto/` provides `HashPassword` / `VerifyPassword`.
3. On successful login, a 32-byte random session ID is created and stored in the `sessions` table with a 24-hour TTL.
4. The session ID is set as an `HttpOnly; SameSite=Strict` cookie named `dm_session`.
5. On every subsequent request, `AuthMiddleware` reads the cookie, looks up the session in the DB, slides the TTL, and injects `user_id` into the request context via `appctx.ContextKeyUserID`.

### Auth Middleware (`internal/middleware/auth.go`)

Applied globally in the router. Two modes controlled by `AUTH_REQUIRED` env var:

- **`AUTH_REQUIRED=false`** (default): middleware enriches the context when a valid session exists but never blocks unauthenticated requests. All existing single-tenant behaviour is preserved.
- **`AUTH_REQUIRED=true`**: unauthenticated requests to non-exempt paths receive a `302` redirect to `/login` (for browser navigation) or a `401 JSON` error (for API/XHR calls, detected via `Accept` header).

**Exempt routes** (never require authentication):
```
GET  /health
GET  /static/*
GET  /login
POST /auth/login
POST /auth/register
GET  /share/*        (share token info)
POST /share/*        (share token join)
GET  /s/*            (share visitor HTML page)
```

### Auth Endpoints (`internal/handler/auth_handler.go`)

```
POST /auth/register        { email, password, display_name }  → 201
POST /auth/login           { email, password }                 → 200 + Set-Cookie
POST /auth/logout                                              → 204
GET  /auth/me                                                  → 200 { id, email, display_name }
POST /auth/change-password { current_password, new_password }  → 204
```

Rate limiting: 10 requests/minute per IP on `/auth/login` and `/auth/register` (in-process token bucket, no external dependency).

### Context Key (`internal/appctx/appctx.go`)

All layers that need the current user read from context via the shared package:

```go
uid := appctx.UserIDFromCtx(ctx)  // returns 0 if unauthenticated
```

This package imports nothing internal, preventing import cycles between middleware, services, repositories, and crypto.

### Data Scoping — Repository Layer

Every repository method calls `uidFromCtx(ctx)` and appends `AND user_id = $N` to SELECT/UPDATE/DELETE queries, and includes `user_id = uidVal(uid)` in INSERT statements.

`uidVal(uid)` returns `nil` (SQL NULL) when `uid == 0`, preserving backward compatibility for unauthenticated/single-tenant use.

The helper `userscope.go` exists in both `internal/repository/` and `internal/importstorage/`.

### Data Scoping — PostgreSQL RLS

Row-Level Security is enabled on all data tables as a second line of defence. When the application sets `app.current_user_id` in the session, PostgreSQL enforces the policy independently of application code. RLS is `ENABLE` (not `FORCE`), so the DB owner role bypasses it — full enforcement requires a non-owner application DB role (Layer 10).

### Share Token System (`internal/service/archive_share_service.go`)

An archive owner can create share tokens that grant visitors read access to their archive:

1. Owner creates a token via `POST /api/shares` (optional password, optional expiry, optional tool access policy). Token stored in `archive_shares` table.
2. Visitor visits `/s/{token}` — the share visitor HTML page.
3. Page fetches `GET /share/{token}` for metadata (label, has_password, expiry, owner name).
4. Visitor submits password (if required) via `POST /share/{token}`.
5. Server validates token + password, then calls `authSvc.CreateShareSession(ctx, ownerUserID)` to create a normal `dm_session` scoped to the **owner's** `user_id`.
6. Visitor's browser is set the `dm_session` cookie and redirected to `/`.
7. The visitor now sees the owner's archive through the normal repository filter — no special code paths needed.

### Keyring (Encryption Layer)

Two separate security concepts:
- **`dm_session` cookie** — identifies who the user is (DB-backed sessions table).
- **`dm_keyring_sid` cookie** — carries the keyring unlock password in a RAM store (`SessionMasterStore`). Unlocking is separate from authentication; users must unlock their keyring to access encrypted reference documents and private store.

`internal/crypto/keys.go` scopes all keyring operations by user_id from context, using `AND user_id IS NULL` for uid==0 (legacy single-tenant rows).

## Architecture Patterns

### Adding a New API Endpoint

1. Add the handler method in `internal/handler/<domain>_handler.go`
2. Register the route in `internal/api/router/router.go`
3. Add the service method in `internal/service/<domain>_service.go`
4. Add the repository method in `internal/repository/<domain>_repo.go` (raw pgx, not sqlc)
5. **For data tables:** repository methods must use `addUIDFilter(q, args, uidFromCtx(ctx))` on SELECT/UPDATE/DELETE, and `uidVal(uidFromCtx(ctx))` on INSERT.

### Adding a New AI Tool

1. Add the tool definition (JSON schema) in `internal/ai/provider.go` — `GetToolDefinitions()`
2. Add the execution case in `internal/ai/tools.go` — `NewToolExecutor()` switch statement
3. All SQL in `tools.go` must include `AND user_id = $N` via `toolsUIDFilter(ctx, q, args)`
4. Optionally add access-tier controls in `internal/ai/tool_access.go`

### Database Migrations

- Migration logic lives in `internal/database/` Go files (not `.sql` files)
- Applied automatically at server startup via `database.Migrate()`
- `migrate_multitenancy.go` contains the multi-tenancy DDL (users, sessions, archive_shares, user_id columns, RLS)
- **Never modify existing migration logic** — always add a new migration function

### Import Pipeline

Four tiers for getting data into the system:

| Tier | Mechanism | Endpoint |
|---|---|---|
| A | IMAP credentials (no upload) | `POST /imap/process` |
| B | ZIP file upload (Facebook, Instagram, WhatsApp, iMessage exports) | `POST /import/upload` |
| C1 | Browser folder picker (photos) | `POST /import/photo-batch` |
| D | Server-triggered (contacts, thumbnails, reference import) | existing endpoints |

All import handlers capture `uid := appctx.UserIDFromCtx(r.Context())` before launching background goroutines, then pass it as `context.WithValue(context.Background(), appctx.ContextKeyUserID, uid)` so that `importstorage` INSERT statements pick up the correct `user_id`.

### Frontend Module Pattern

All frontend JS uses a revealing-module pattern (IIFE returning a public API):

```javascript
const MyModule = (() => {
    // private state
    function init() { /* wire DOM event listeners */ }
    function open() { }
    return { init, open };
})();
```

- Constants and DOM element cache: `foundation.js`
- Event listener wiring: `app.js` (calls `ModuleName.init()` from `Modals.initAll()`)
- **Script load order matters with `defer`:** modules loaded after `app.js` must
  self-initialize at the bottom of their file (`MyModule.init();`) because
  `Modals.initAll()` will have already run by the time the later script executes.
- All dates displayed should be in local format
- `auth.js` — `AuthModule` — fetches `GET /auth/me` on load, shows account dropdown, handles logout, redirects to `/login` on 401

### Adding a New Frontend Feature

1. Create `static/js/museum/<feature>.js` with the module pattern
2. Add `<script src="/static/js/museum/<feature>.js" defer></script>` in
   `templates/index.template.html` **after** `app.js`
3. Add `<feature>.init();` at the bottom of the new JS file (self-init)
4. Add the trigger button / HTML to `templates/index.template.html`
5. Add CSS to `static/css/museum_of.css`

### Chat System

- **Backend:** `POST /chat/generate` → `ChatHandler` → `ChatService.GenerateResponse()`
- System prompt = subject config + voice instructions (`static/data/voice_instructions.json`)
  with `{SUBJECT_NAME}`, `{he}`, `{him}`, `{his}` substituted at runtime
- History: last 30 turns from `chat_turns` table are sent with every request
- Tool loop: max 5 iterations per request in both providers
- **Provider selection:** passed as `provider` field in request (`"claude"` or `"gemini"`)
- All AI tool SQL queries are scoped by user_id via `toolsUIDFilter(ctx, q, args)` in `internal/ai/tools.go`

### Voice System

- Built-in voices defined in `static/data/voice_instructions.json`
- Custom voices stored in `custom_voices` DB table, served via `GET /api/voices`
- Each voice message gets a CSS class `voice-<name>` for styling
- Voice images: `static/images/<voice>.png` and `<voice>_sm.png`

## Key Files Quick Reference

| What | Where |
|---|---|
| Route wiring | `internal/api/router/router.go` |
| Auth middleware | `internal/middleware/auth.go` |
| Auth service | `internal/service/auth_service.go` |
| Auth handler | `internal/handler/auth_handler.go` |
| Share token service | `internal/service/archive_share_service.go` |
| Share token handler | `internal/handler/share_handler.go` |
| Context key (user_id) | `internal/appctx/appctx.go` |
| Repository user scoping | `internal/repository/userscope.go` |
| Import storage user scoping | `internal/importstorage/userscope.go` |
| Multi-tenancy DB migration | `internal/database/migrate_multitenancy.go` |
| Upload import handler | `internal/handler/upload_import_handler.go` |
| AI provider interface | `internal/ai/provider.go` |
| Tool definitions | `internal/ai/provider.go` → `GetToolDefinitions()` |
| Tool execution | `internal/ai/tools.go` → `NewToolExecutor()` |
| Chat orchestration | `internal/service/chat_service.go` |
| Chat HTTP handler | `internal/handler/chat_handler.go` |
| DB schema | `sqlc/schema.sql` |
| Frontend main | `static/js/museum/app.js` |
| Frontend auth | `static/js/museum/auth.js` |
| Frontend chat renderer | `static/js/museum/chat.js` |
| Constants / DOM cache | `static/js/museum/foundation.js` |
| All styles | `static/css/museum_of.css` |
| Main SPA template | `templates/index.template.html` |
| Login / register page | `templates/login.html` |
| Share visitor page | `templates/share.html` |

## Data Import

**Web-based (multi-tenant):**
- `POST /import/upload` — upload a platform export ZIP (facebook/instagram/whatsapp/imessage); extracted to `tmp/imports/{user_id}/{job_id}/` and run with user_id in context
- `POST /import/photo-batch` — batch upload images from a browser folder picker
- `GET /import/jobs` — aggregated status of all import jobs

**Admin / local (single-tenant or server-side):**
The `cmd/runner` binary reads `runner.json` and imports data in pipeline stages.
Jobs in the same stage run in parallel; stages are sequential.

Supported types: `filesystem`, `whatsapp`, `imessage`, `instagram`, `facebook_all`,
`imap`, `reference_files`, `contacts_extract`, `reference_import`, `image_export`, `thumbnails`

Set `"execute": true` on a job to enable it; `false` to skip.

## Security Notes

- `.env` contains API keys and the DB password — **never commit it**
- `KEYRING_PEPPER` is used to derive encryption keys for sensitive data and encrypted
  reference documents — rotating it requires re-encrypting all affected records
- The RAM master key unlocks `private_store` and encrypted documents per session;
  it is never persisted to disk
- Tool access is tiered: Visitor / Master — controlled via `PUT /api/settings/llm-tools-access`
- All archive data tables have `user_id` (nullable FK to `users`) — NULL means legacy/unauthenticated single-tenant data
- RLS policies are defined but use `ENABLE` not `FORCE`; the DB owner role bypasses them
- Share visitor sessions are full `dm_session` cookies scoped to the owner's `user_id` — visitors see exactly the owner's data through normal repository filters

## What NOT to Do

- Don't use `sqlc generate` output directly; repository methods are hand-written with pgx
- Don't add Node.js / npm tooling — the frontend is intentional vanilla JS
- Don't modify existing migration logic — always add a new migration function
- Don't commit `.env`, `runner.json` (contains personal paths), or binary files
- Don't add `user_id` filtering to the `users`, `sessions`, or `archive_shares` tables — these are identity/auth tables, not per-user data tables
- Don't use `context.Background()` in import background goroutines — always thread the user_id via `context.WithValue(context.Background(), appctx.ContextKeyUserID, uid)` where `uid` is captured from the HTTP request context before the goroutine starts
- Don't return nil slices from list handlers — always substitute an empty slice so JSON encodes as `[]` not `null`
