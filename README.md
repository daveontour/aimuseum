# Digital Museum

A personal digital archive and AI-powered memory explorer. Digital Museum aggregates your emails, messages, photos, and social media into a single searchable archive, then lets you explore it through a conversational AI that has direct access to your data.

## Features

- **AI chat with tool access** — ask the AI to search your emails, messages, photos, Facebook posts, and more. Supports Google Gemini and Anthropic Claude.
- **Multi-source archiving** — import from Gmail/IMAP, WhatsApp, iMessage, Instagram, Facebook, and local filesystems.
- **Voice personalities** — choose how the AI responds: as an expert, a friend, or in the subject's own voice with a selectable mood.
- **Relationship tracking** — contact profiles, interaction graphs, and relationship analysis.
- **Sensitive data vault** — encrypted key-value store protected by a master key.
- **Custom voices** — create AI personas with custom instructions and creativity levels.
- **Today's Thing of Interest** — daily AI-generated prompts based on the subject's interests.
- **Reference documents** — attach documents for the AI to consult when answering questions.
- **Statistics and visualisations** — email counts, contact maps, media timelines, and location maps.
- **Artefacts** - a place to store documents and images and the stories behind them
- **Interview Mode** - have the AI interview you based on your background. Choose the style and purpose of the interview
- **Random Question** - have the AI generate a random question based on your profile that may be confronting and then have the AI answer it.
- **Have a Chat** - start a conversation between 2 AIs about you and see where it goes 
- **Voice Input and Output** - create your input via voice and listen to the response
- **Visitor Access** - allow visitors access to your archive, with fine-grained access control

## Tech Stack

- **Backend:** Go 1.25, [Chi](https://github.com/go-chi/chi) router
- **Database:** PostgreSQL (pgx v5), with `pg_trgm` for full-text search and `pgcrypto` for encryption
- **AI providers:** Google Gemini (`gemini-2.5-flash`), Anthropic Claude (`claude-sonnet-4-6`)
- **Email:** IMAP via `go-imap`, Gmail via OAuth2
- **Frontend:** Vanilla JS, Leaflet (maps), Cytoscape (relationship graphs), Marked (Markdown), Highlight.js

## Prerequisites

- Go 1.25+
- PostgreSQL 14+ with the `pgcrypto` and `pg_trgm` extensions available
- At least one AI provider API key (Gemini or Anthropic)

## Configuration

Copy `.env.example` to `.env` and fill in the values. The server searches for `.env` starting from the executable directory and walking upward.

### Required

| Variable | Description |
|---|---|
| `DB_HOST` | PostgreSQL host |
| `DB_PORT` | PostgreSQL port (default `5432`) |
| `DB_NAME` | Database name (default `musego`) |
| `DB_USER` | Database user |
| `DB_PASSWORD` | Database password |
| `KEYRING_PEPPER` | Secret string used to derive encryption keys — keep this safe and consistent |

### AI Providers (at least one required for chat)

| Variable | Description |
|---|---|
| `GEMINI_API_KEY` | Google Gemini API key |
| `GEMINI_MODEL_NAME` | Model override (default `gemini-2.5-flash`) |
| `ANTHROPIC_API_KEY` | Anthropic Claude API key |
| `CLAUDE_MODEL_NAME` | Model override (default `claude-sonnet-4-6`) |
| `TAVILY_API_KEY` | Tavily web search key (optional — enables AI web search) |

### Server

| Variable | Description |
|---|---|
| `HOST_PORT` | HTTP port (default `8001`) |
| `PAGE_TITLE` | Browser tab title |
| `SESSION_COOKIE_SECURE` | Set `true` when running behind HTTPS |
| `ENABLE_PPROF` | Set `true` to enable pprof on `:6060` |

### Gmail / OAuth2

| Variable | Description |
|---|---|
| `GMAIL_CLIENT_ID` | Google OAuth2 client ID |
| `GMAIL_CLIENT_SECRET` | Google OAuth2 client secret |
| `GMAIL_REDIRECT_URL` | OAuth2 callback URL (default `http://localhost:8001/gmail/auth/callback`) |

### Import Defaults (all optional)

| Variable | Description |
|---|---|
| `DEFAULT_IMAP_HOST` | Pre-fill IMAP host in the import UI |
| `DEFAULT_IMAP_PORT` | Pre-fill IMAP port (default `993`) |
| `DEFAULT_IMAP_USERNAME` | Pre-fill IMAP username |
| `DEFAULT_IMESSAGE_DIRECTORY_PATH` | Default path to iMessage database directory |
| `DEFAULT_NEW_ONLY_OPTION` | Import only new items by default (`true`/`false`) |


## Running

```bash
# Install dependencies
go mod download

# Run the server
go run ./cmd/server

# The server starts at http://localhost:8001
```

On first run the server will create the database (if it doesn't exist), apply all migrations, and seed reference data from the `static/data/` directory.

To build a binary:

```bash
go build -o bin/server ./cmd/server
```

## Data Import

Imports are managed from **Data Import**. Supported sources:

| Source | Notes |
|---|---|
| **Facebook** | Requires a JSON archive downloaded from https://accountscenter.facebook.com/info_and_permissions/dyi |
| **Instagram** | Requires a JSON archive downloaded from https://accountscenter.facebook.com/info_and_permissions/dyi |
| **WhatsApp** | Upload an ZIP archive of an iMazing backup |
| **iMessage** |Upload an ZIP archive of an iMazing backup |
| **Gmail** | OAuth2 flow — link your Google account in settings |
| **IMAP** | Any IMAP mailbox with folder selection |
| **Filesystem** | Import images by directory or individual files |

All imports run as background jobs with real-time progress streamed to the UI.

## Seed Data Files

Three JSON files in `static/data/` are read at server startup and used to pre-populate database tables. Each file is processed with an upsert-style logic — rows that already exist are left unchanged, so editing a file and restarting the server is safe and additive.

### `static/data/email_classifications.json`

Maps relationship-category labels to lists of contact display names. Seeded into the `email_classifications` table and used during contact import to tag each contact with a relationship type.

**Format:** a JSON object whose keys are classification labels and whose values are arrays of contact name strings.

```json
{
  "friend":       ["Alice Smith", "Bob Jones"],
  "family":       ["Carol Burton"],
  "colleague":    ["Dan Nguyen"],
  "acquaintance": ["Eve Taylor"],
  "business":     ["Acme Corp"],
  "social":       ["Book Club Group"],
  "promotional":  ["SomeBrand Newsletter"],
  "spam":         [],
  "important":    [],
  "unknown":      []
}
```

Built-in category labels: `friend`, `family`, `colleague`, `acquaintance`, `business`, `social`, `promotional`, `spam`, `important`, `unknown`. Additional categories can be added as extra keys.

---

### `static/data/email_matches.json`

Maps a canonical display name to all email addresses that person has used across different accounts and time periods. Seeded into the `email_matches` table and used during import to consolidate messages from the same person under a single name regardless of which address they sent from.

**Format:** a JSON array of objects, each with a `primary_name` string and an `emails` array of strings.

```json
[
  {
    "primary_name": "Alice Smith",
    "emails": [
      "alice@gmail.com",
      "alice.smith@work.com",
      "asmith@oldcompany.com"
    ]
  }
]
```

---

### `static/data/exclusions.json`

Defines patterns for senders that should be excluded from contact processing — automated systems, mailing lists, and no-reply addresses. Seeded into the `email_exclusions` table.

**Format:** a JSON object with three keys:

| Key | Type | Behaviour |
|---|---|---|
| `email` | `string[]` | Exclude if the sender's **email address** contains any of these substrings (e.g. `"noreply"`, `"marketing"`). |
| `name` | `string[]` | Exclude if the sender's **display name** contains any of these substrings. |
| `name_email` | `[{name, email}]` | Exclude a specific **name + email pair** (used when a real person's name appears alongside an automated address that would otherwise match a real contact). |

```json
{
  "email": ["noreply", "no-reply", "marketing", "info@", "support@"],
  "name":  ["marketing", "no-reply"],
  "name_email": [
    { "name": "Alice Smith", "email": "notifications@someservice.com" }
  ]
}
```

---

## Architecture

```
cmd/server/          Entry point, server startup
internal/
  api/router/        Route definitions
  handler/           HTTP request handlers
  service/           Business logic
  repository/        Database access (pgx)
  model/             Shared data types
  ai/                Claude and Gemini provider implementations
  import/            Per-source import plugins
  crypto/            Encryption and key derivation
  config/            Environment-based configuration
  database/          Migrations and connection pool
static/
  js/museum/         Frontend JavaScript
  css/               Stylesheets
  data/              Seed data (voice instructions, email classifications)
templates/           HTML templates
```

## Security Notes

- Never commit `.env` or any file containing `KEYRING_PEPPER` or API keys — both are listed in `.gitignore`.
- `KEYRING_PEPPER` is used to derive encryption keys for the sensitive data vault. Changing it will make existing encrypted data unreadable.
- Set `SESSION_COOKIE_SECURE=true` when deploying behind HTTPS.
- The master key system controls access to subject configuration, imported data management, and sensitive data. Visitor access is read-only by default.
