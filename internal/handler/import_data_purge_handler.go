package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/daveontour/aimuseum/internal/appctx"
	facebookallimport "github.com/daveontour/aimuseum/internal/import/facebookall"
	"github.com/daveontour/aimuseum/internal/keystore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ImportDataPurgeHandler removes archive data by logical import kind (owner master unlock).
type ImportDataPurgeHandler struct {
	pool         *pgxpool.Pool
	sessionStore *keystore.SessionMasterStore
}

// NewImportDataPurgeHandler constructs an ImportDataPurgeHandler.
func NewImportDataPurgeHandler(pool *pgxpool.Pool, sessionStore *keystore.SessionMasterStore) *ImportDataPurgeHandler {
	return &ImportDataPurgeHandler{pool: pool, sessionStore: sessionStore}
}

// RegisterRoutes mounts POST /api/import-data/purge.
func (h *ImportDataPurgeHandler) RegisterRoutes(r chi.Router) {
	r.Post("/api/import-data/purge", h.Purge)
}

type purgeRequest struct {
	Kind string `json:"kind"`
}

// Purge handles POST /api/import-data/purge { "kind": "..." }.
func (h *ImportDataPurgeHandler) Purge(w http.ResponseWriter, r *http.Request) {
	if !RequireOwnerMasterUnlock(w, r, h.sessionStore) {
		return
	}
	ctx := r.Context()
	uid := appctx.UserIDFromCtx(ctx)
	if uid == 0 {
		writeError(w, http.StatusForbidden, "authenticated user required")
		return
	}

	var req purgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	kind := req.Kind
	if kind == "" {
		writeError(w, http.StatusBadRequest, "kind is required")
		return
	}

	var deleted int64
	var err error
	switch kind {
	case "emails_gmail":
		tag, e := h.pool.Exec(ctx, `DELETE FROM emails WHERE user_id = $1 AND source = 'gmail'`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "emails_imap":
		tag, e := h.pool.Exec(ctx, `DELETE FROM emails WHERE user_id = $1 AND (source IS NULL OR source <> 'gmail')`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "whatsapp":
		tag, e := h.pool.Exec(ctx, `DELETE FROM messages WHERE user_id = $1 AND service = 'WhatsApp'`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "instagram":
		tag, e := h.pool.Exec(ctx, `DELETE FROM messages WHERE user_id = $1 AND service = 'Instagram'`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "imessage":
		tag, e := h.pool.Exec(ctx, `
			DELETE FROM messages
			WHERE user_id = $1 AND service IN ('iMessage', 'SMS', 'MMS')`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "facebook_messenger":
		var n int64
		n, err = h.purgeFacebookMessenger(ctx, uid)
		deleted = n
	case "facebook_all":
		err = facebookallimport.ClearFacebookAllDataForUser(ctx, h.pool, uid)
		if err == nil {
			deleted = 1
		}
	case "facebook_albums":
		var n int64
		n, err = h.purgeFacebookAlbums(ctx, uid)
		deleted = n
	case "facebook_posts":
		var n int64
		n, err = h.purgeFacebookPosts(ctx, uid)
		deleted = n
	case "facebook_places":
		tag, e := h.pool.Exec(ctx, `DELETE FROM locations WHERE user_id = $1 AND source = 'facebook'`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "filesystem_media":
		var n int64
		n, err = h.purgeFilesystemMedia(ctx, uid)
		deleted = n
	case "thumbnails":
		tag, e := h.pool.Exec(ctx, `
			UPDATE media_blobs SET thumbnail_data = NULL
			WHERE id IN (SELECT media_blob_id FROM media_items WHERE user_id = $1)
			  AND thumbnail_data IS NOT NULL`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "reference_documents":
		tag, e := h.pool.Exec(ctx, `DELETE FROM reference_documents WHERE user_id = $1`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	case "contacts":
		tag, e := h.pool.Exec(ctx, `DELETE FROM contacts WHERE user_id = $1 AND id <> 0`, uid)
		err = e
		if err == nil {
			deleted = tag.RowsAffected()
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown purge kind: %s", kind))
		return
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, map[string]any{"ok": true, "kind": kind, "deleted": deleted})
}

func (h *ImportDataPurgeHandler) purgeFacebookMessenger(ctx context.Context, uid int64) (int64, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		WITH deleted_items AS (
			DELETE FROM media_items WHERE source = 'Facebook' AND user_id = $1 RETURNING media_blob_id
		)
		DELETE FROM media_blobs WHERE id IN (SELECT media_blob_id FROM deleted_items WHERE media_blob_id IS NOT NULL)
	`, uid)
	if err != nil {
		return 0, fmt.Errorf("facebook messenger media: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM messages WHERE service = 'Facebook Messenger' AND user_id = $1`, uid)
	if err != nil {
		return 0, fmt.Errorf("facebook messenger messages: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (h *ImportDataPurgeHandler) purgeFacebookAlbums(ctx context.Context, uid int64) (int64, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		WITH deleted_items AS (
			DELETE FROM media_items WHERE source = 'facebook_album' AND user_id = $1 RETURNING media_blob_id
		)
		DELETE FROM media_blobs WHERE id IN (SELECT media_blob_id FROM deleted_items WHERE media_blob_id IS NOT NULL)
	`, uid)
	if err != nil {
		return 0, fmt.Errorf("facebook albums media: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM facebook_albums WHERE user_id = $1`, uid)
	if err != nil {
		return 0, fmt.Errorf("facebook albums: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (h *ImportDataPurgeHandler) purgeFacebookPosts(ctx context.Context, uid int64) (int64, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		WITH deleted_items AS (
			DELETE FROM media_items WHERE source = 'facebook_post' AND user_id = $1 RETURNING media_blob_id
		)
		DELETE FROM media_blobs WHERE id IN (SELECT media_blob_id FROM deleted_items WHERE media_blob_id IS NOT NULL)
	`, uid)
	if err != nil {
		return 0, fmt.Errorf("facebook posts media: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM facebook_posts WHERE user_id = $1`, uid)
	if err != nil {
		return 0, fmt.Errorf("facebook posts: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (h *ImportDataPurgeHandler) purgeFilesystemMedia(ctx context.Context, uid int64) (int64, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		WITH deleted_items AS (
			DELETE FROM media_items WHERE source = 'filesystem' AND user_id = $1 RETURNING media_blob_id
		)
		DELETE FROM media_blobs WHERE id IN (SELECT media_blob_id FROM deleted_items WHERE media_blob_id IS NOT NULL)
	`, uid)
	if err != nil {
		return 0, fmt.Errorf("filesystem media: %w", err)
	}

	n := tag.RowsAffected()
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}
