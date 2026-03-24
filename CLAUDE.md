# Digital Museum — CLAUDE.md

## Project Overview

Digital Museum is a personal AI-powered digital archive. A single subject's entire digital
life (emails, messages, photos, Facebook, iMessage, WhatsApp, documents) is imported into
a PostgreSQL database and made queryable through an AI chat interface. Both Claude and Gemini
can access the data via a tool-calling layer and answer questions, adopt personas, and explore
the archive conversationally.

There is one deployment per subject. The app is not multi-tenant.

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
  config/         ← Env-var config loading
  crypto/         ← Encryption / key derivation
  database/       ← Connection pool, migrations, seed data
  handler/        ← HTTP request handlers (~30 files)
  keystore/       ← RAM master key (unlocks encrypted data)
  middleware/     ← Logger, Recoverer
  model/          ← Shared data types / DTOs
  repository/     ← Database access via pgx (~17 repos)
  service/        ← Business logic (~20 services)
static/
  css/            ← museum_of.css (all styles, ~8000 lines)
  data/           ← voice_instructions.json, seed JSON files
  images/         ← Voice persona images
  js/museum/      ← Frontend modules (foundation.js, app.js, chat.js, …)
templates/        ← index.template.html (single-page app, ~265 KB)
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
| `ENABLE_PPROF` | No | Set `true` to expose `/debug/pprof` on `:6060` |

## Architecture Patterns

### Adding a New API Endpoint

1. Add the handler method in `internal/handler/<domain>_handler.go`
2. Register the route in `internal/api/router/router.go`
3. Add the service method in `internal/service/<domain>_service.go`
4. Add the repository method in `internal/repository/<domain>_repo.go` (raw pgx, not sqlc)

### Adding a New AI Tool

1. Add the tool definition (JSON schema) in `internal/ai/provider.go` — `GetToolDefinitions()`
2. Add the execution case in `internal/ai/tools.go` — `NewToolExecutor()` switch statement
3. Optionally add access-tier controls in `internal/ai/tool_access.go`

### Database Migrations

- Migration files live in `internal/database/migrations/`
- Applied automatically at server startup via `database.Migrate()`
- Naming: `NNN_description.sql` (sequential integers)
- **Never modify an existing migration** — always add a new one

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

### Voice System

- Built-in voices defined in `static/data/voice_instructions.json`
- Custom voices stored in `custom_voices` DB table, served via `GET /api/voices`
- Each voice message gets a CSS class `voice-<name>` for styling
- Voice images: `static/images/<voice>.png` and `<voice>_sm.png`

## Key Files Quick Reference

| What | Where |
|---|---|
| Route wiring | `internal/api/router/router.go` |
| AI provider interface | `internal/ai/provider.go` |
| Tool definitions | `internal/ai/provider.go` → `GetToolDefinitions()` |
| Tool execution | `internal/ai/tools.go` → `NewToolExecutor()` |
| Chat orchestration | `internal/service/chat_service.go` |
| Chat HTTP handler | `internal/handler/chat_handler.go` |
| DB schema | `sqlc/schema.sql` |
| Frontend main | `static/js/museum/app.js` |
| Frontend chat renderer | `static/js/museum/chat.js` |
| Constants / DOM cache | `static/js/museum/foundation.js` |
| All styles | `static/css/museum_of.css` |
| HTML template | `templates/index.template.html` |

## Data Import

The `cmd/runner` binary reads `runner.json` and imports data in pipeline stages.
Jobs in the same stage run in parallel; stages are sequential.

Supported import types: `filesystem`, `whatsapp`, `imessage`, `instagram`,
`facebook_all`, `imap`, `reference_files`, `contacts_extract`, `reference_import`,
`image_export`, `thumbnails`

Set `"execute": true` on a job to enable it; `false` to skip.

## Security Notes

- `.env` contains API keys and the DB password — **never commit it**
- `KEYRING_PEPPER` is used to derive encryption keys for sensitive data and encrypted
  reference documents — rotating it requires re-encrypting all affected records
- The RAM master key unlocks `private_store` and encrypted documents per session;
  it is never persisted to disk
- Tool access is tiered: Visitor / Master — controlled via `PUT /api/settings/llm-tools-access`

## What NOT to Do

- Don't use `sqlc generate` output directly; repository methods are hand-written with pgx
- Don't add Node.js / npm tooling — the frontend is intentional vanilla JS
- Don't modify existing migration files — always add a new one
- Don't commit `.env`, `runner.json` (contains personal paths), or binary files
