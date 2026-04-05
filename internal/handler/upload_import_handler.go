package handler

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/daveontour/aimuseum/internal/appctx"
	"github.com/daveontour/aimuseum/internal/config"
	facebookimport "github.com/daveontour/aimuseum/internal/import/facebook"
	facebookalbumsimport "github.com/daveontour/aimuseum/internal/import/facebookalbums"
	facebookplacesimport "github.com/daveontour/aimuseum/internal/import/facebookplaces"
	facebookpostsimport "github.com/daveontour/aimuseum/internal/import/facebookposts"
	imessageimport "github.com/daveontour/aimuseum/internal/import/imessage"
	instagramimport "github.com/daveontour/aimuseum/internal/import/instagram"
	whatsappimport "github.com/daveontour/aimuseum/internal/import/whatsapp"
	appimporter "github.com/daveontour/aimuseum/internal/importer"
	"github.com/daveontour/aimuseum/internal/importstorage"
	"github.com/daveontour/aimuseum/internal/keystore"
	"github.com/daveontour/aimuseum/internal/repository"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── Upload job singleton ───────────────────────────────────────────────────────

var uploadJob = appimporter.NewImportJob("ZIP upload import", map[string]any{
	"status":        "idle",
	"status_line":   nil,
	"error_message": nil,
	"import_type":   nil,
})

// ── UploadImportHandler ───────────────────────────────────────────────────────

// UploadImportHandler handles web-based upload import endpoints.
type UploadImportHandler struct {
	pool              *pgxpool.Pool
	subjectConfigRepo *repository.SubjectConfigRepo
	sessionStore      *keystore.SessionMasterStore
	uploadCfg         config.UploadConfig
}

// NewUploadImportHandler creates an UploadImportHandler.
func NewUploadImportHandler(pool *pgxpool.Pool, subjectRepo *repository.SubjectConfigRepo, sessionStore *keystore.SessionMasterStore, uploadCfg config.UploadConfig) *UploadImportHandler {
	return &UploadImportHandler{pool: pool, subjectConfigRepo: subjectRepo, sessionStore: sessionStore, uploadCfg: uploadCfg}
}

// RegisterRoutes mounts upload import routes.
func (h *UploadImportHandler) RegisterRoutes(r chi.Router) error {
	r.Post("/import/upload", h.Upload)
	r.Get("/import/upload/stream", h.UploadStream)
	r.Post("/import/upload/cancel", h.UploadCancel)
	r.Get("/import/upload/status", h.UploadStatus)
	r.Post("/import/photo-batch", h.PhotoBatch)
	r.Get("/import/jobs", h.Jobs)
	r.Get("/api/upload-config", h.UploadConfigJSON)
	return h.mountTUS(r)
}

// ── POST /import/upload ───────────────────────────────────────────────────────

// Upload accepts a ZIP file and starts a background import.
func (h *UploadImportHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if !RequireOwnerMasterUnlock(w, r, h.sessionStore) {
		return
	}
	if err := uploadJob.AssertNotRunning(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.uploadCfg.MaxUploadBytes)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
		return
	}

	importType := r.FormValue("type")
	validTypes := map[string]bool{
		"facebook":  true,
		"instagram": true,
		"whatsapp":  true,
		"imessage":  true,
	}
	if !validTypes[importType] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid import type %q; must be one of: facebook, instagram, whatsapp, imessage", importType))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or unreadable file field: "+err.Error())
		return
	}
	defer file.Close()

	uid := appctx.UserIDFromCtx(r.Context())
	jobID := randomJobID()

	var uidStr string
	if uid == 0 {
		uidStr = "0"
	} else {
		uidStr = fmt.Sprintf("%d", uid)
	}

	tmpDir := filepath.Join("tmp", "imports", uidStr, jobID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp directory: "+err.Error())
		return
	}

	// Ensure tmpDir is always removed — either by this handler on error, or by
	// the background goroutine on completion.  The flag is flipped to false just
	// before the goroutine is launched so the defer becomes a no-op on the
	// happy path.
	handlerOwnsCleanup := true
	defer func() {
		if handlerOwnsCleanup {
			os.RemoveAll(tmpDir)
		}
	}()

	// Write uploaded ZIP to temp file
	zipPath := filepath.Join(tmpDir, "upload.zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create temp ZIP: "+err.Error())
		return
	}
	if _, err := io.Copy(zipFile, file); err != nil {
		zipFile.Close()
		writeError(w, http.StatusInternalServerError, "failed to write uploaded file: "+err.Error())
		return
	}
	zipFile.Close()

	// Extract ZIP, then remove the archive immediately to free disk space
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create extract directory: "+err.Error())
		return
	}
	if err := extractZip(zipPath, extractDir); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to extract ZIP: "+err.Error())
		return
	}
	_ = os.Remove(zipPath) // ZIP is no longer needed once extracted

	// Unwrap single-folder ZIPs: most platform exports wrap everything in one
	// top-level directory (e.g. "facebook-export-2024/"). Pass the actual data
	// root to the importer rather than the extraction wrapper directory.
	importRoot := resolveImportRoot(extractDir)

	handlerOwnsCleanup = false // goroutine takes over from here
	h.startZipImportBackground(uid, importType, tmpDir, importRoot, false)

	writeJSON(w, map[string]any{
		"message": fmt.Sprintf("%s import started from uploaded ZIP (original: %s)", importType, header.Filename),
		"job_id":  jobID,
	})
}

// ── GET /import/upload/stream ─────────────────────────────────────────────────

// UploadStream serves SSE progress for the upload import job.
func (h *UploadImportHandler) UploadStream(w http.ResponseWriter, r *http.Request) {
	uploadJob.ServeSSE(w, r)
}

// ── GET /import/upload/status ─────────────────────────────────────────────────

// UploadStatus returns the current JSON status of the upload import job.
func (h *UploadImportHandler) UploadStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, uploadJob.Status())
}

// UploadCancel handles POST /import/upload/cancel (ZIP/TUS upload import job).
func (h *UploadImportHandler) UploadCancel(w http.ResponseWriter, r *http.Request) {
	if !RequireOwnerMasterUnlock(w, r, h.sessionStore) {
		return
	}
	writeJSON(w, uploadJob.Cancel())
}

// normalizeUploadSourceRef trims and normalizes slashes for multipart paths.
func normalizeUploadSourceRef(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, `\`, `/`))
}

// ── POST /import/photo-batch ──────────────────────────────────────────────────

// PhotoBatch accepts multipart image files and inserts them with the user_id from context.
func (h *UploadImportHandler) PhotoBatch(w http.ResponseWriter, r *http.Request) {
	if !RequireOwnerMasterUnlock(w, r, h.sessionStore) {
		return
	}
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
		return
	}

	ctx := r.Context()
	storage := importstorage.NewImageStorage(h.pool)

	overwriteExisting := false
	if r.MultipartForm != nil {
		if vals := r.MultipartForm.Value["overwrite_existing"]; len(vals) > 0 {
			v := strings.TrimSpace(strings.ToLower(vals[0]))
			overwriteExisting = v == "1" || v == "true" || v == "on" || v == "yes"
		}
	}

	var imported, updated, skipped, errors int

	// Use explicit "files" field order (do not range MultipartForm.File — map order is random).
	// Client sends matching "rel_paths" entries because some browsers strip path segments from Filename.
	fileHeaders := r.MultipartForm.File["files"]
	relPaths := r.MultipartForm.Value["rel_paths"]
	for i, fh := range fileHeaders {
		sourceRef := normalizeUploadSourceRef(fh.Filename)
		if i < len(relPaths) {
			if p := normalizeUploadSourceRef(relPaths[i]); p != "" {
				sourceRef = p
			}
		}
		if sourceRef == "" {
			errors++
			continue
		}

		if !overwriteExisting {
			exists, err := storage.FilesystemMediaItemExists(ctx, sourceRef)
			if err != nil {
				errors++
				continue
			}
			if exists {
				skipped++
				continue
			}
		}

		f, err := fh.Open()
		if err != nil {
			errors++
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			errors++
			continue
		}

		mediaType := importstorage.NormalizeFilesystemUploadMediaType(sourceRef, fh.Header.Get("Content-Type"), data)
		title := path.Base(sourceRef)
		if title == "." || title == "/" {
			title = sourceRef
		}
		tags := importstorage.TagsFromSourceRef(sourceRef)
		_, isUpdate, err := storage.SaveImage(ctx, sourceRef, data, mediaType, title, tags, false)
		if err != nil {
			errors++
			continue
		}
		if isUpdate {
			updated++
		} else {
			imported++
		}
	}

	writeJSON(w, map[string]any{
		"imported": imported,
		"updated":  updated,
		"skipped":  skipped,
		"errors":   errors,
	})
}

// signalUploadZipTUSPreExtract starts the upload job and broadcasts SSE status after the
// tus upload has finished but before the blob is copied from tus storage and extracted.
func (h *UploadImportHandler) signalUploadZipTUSPreExtract(importType string) error {
	if err := uploadJob.AssertNotRunning(); err != nil {
		return err
	}
	uploadJob.Start()
	msg := "Upload complete — copying file and extracting archive…"
	uploadJob.UpdateState(map[string]any{
		"status":        "in_progress",
		"status_line":   msg,
		"error_message": nil,
		"import_type":   importType,
	})
	uploadJob.Broadcast("status", map[string]any{"status_line": msg})
	return nil
}

// startZipImportBackground starts the shared ZIP import goroutine. tmpDir is the job temp
// root (containing extracted/); it is removed when the import finishes.
// If jobAlreadyStarted is true (TUS path after signalUploadZipTUSPreExtract), Start() is skipped.
func (h *UploadImportHandler) startZipImportBackground(uid int64, importType, tmpDir, importRoot string, jobAlreadyStarted bool) {
	if !jobAlreadyStarted {
		uploadJob.Start()
	}
	uploadJob.UpdateState(map[string]any{
		"status":        "in_progress",
		"status_line":   fmt.Sprintf("Starting %s import...", importType),
		"error_message": nil,
		"import_type":   importType,
	})
	uploadJob.Broadcast("status", map[string]any{"status_line": fmt.Sprintf("Starting %s import...", importType)})

	ctx := context.WithValue(context.Background(), appctx.ContextKeyUserID, uid)
	go func() {
		defer os.RemoveAll(tmpDir)
		switch importType {
		case "whatsapp":
			runUploadWhatsApp(ctx, h.pool, h.subjectConfigRepo, uploadJob, importRoot)
		case "imessage":
			runUploadIMessage(ctx, h.pool, h.subjectConfigRepo, uploadJob, importRoot)
		case "instagram":
			runUploadInstagram(ctx, h.pool, h.subjectConfigRepo, uploadJob, importRoot)
		case "facebook":
			runUploadFacebook(ctx, h.pool, h.subjectConfigRepo, uploadJob, importRoot)
		}
	}()
}

// UploadConfigJSON exposes tus chunk size and max upload GiB for the SPA.
func (h *UploadImportHandler) UploadConfigJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"chunkSizeMB": h.uploadCfg.TUSChunkSizeMB,
		"maxUploadGB": h.uploadCfg.MaxUploadGB,
	})
}

// ── GET /import/jobs ──────────────────────────────────────────────────────────

// Jobs returns a JSON map of all import job statuses.
func (h *UploadImportHandler) Jobs(w http.ResponseWriter, r *http.Request) {
	jobs := map[string]any{
		"upload":           uploadJob.Status(),
		"filesystem":       filesystemJob.Status(),
		"whatsapp":         whatsappJob.Status(),
		"imessage":         imessageJob.Status(),
		"instagram":        instagramJob.Status(),
		"facebook_all":     facebookAllJob.Status(),
		"thumbnails":       thumbnailsJob.Status(),
		"contacts_extract": contactsExtractJob.Status(),
		"reference_import": referenceImportJob.Status(),
	}
	writeJSON(w, jobs)
}

// ── Upload run functions ───────────────────────────────────────────────────────

func runUploadWhatsApp(ctx context.Context, pool *pgxpool.Pool, subjectRepo *repository.SubjectConfigRepo, job *appimporter.ImportJob, directoryPath string) {
	defer job.Finish()

	storage := importstorage.NewMessageStorage(ctx, pool, subjectRepo)

	progressCallback := func(stats whatsappimport.ImportStats, justCompleted string) {
		// statusLine := fmt.Sprintf("Processing conversation %d of %d: %s | Messages: %d (%d created, %d updated) | Attachments: %d found, %d missing | Errors: %d",
		// 	stats.ConversationsProcessed, stats.TotalConversations, justCompleted,
		// 	stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
		// 	stats.AttachmentsFound, stats.AttachmentsMissing, stats.Errors)
		statusLine := fmt.Sprintf("Processing conversation %d of %d: %s | Messages: %d (%d created, %d updated) | Attachments: %d found",
			stats.ConversationsProcessed, stats.TotalConversations, justCompleted,
			stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
			stats.AttachmentsFound)
		job.UpdateState(map[string]any{
			"status_line": statusLine,
		})
		job.Broadcast("progress", job.GetState())
	}

	cancelledCheck := func() bool { return job.IsCancelled() }

	stats, err := whatsappimport.ImportWhatsAppFromDirectory(ctx, storage, directoryPath, progressCallback, cancelledCheck)

	if job.IsCancelled() {
		job.UpdateState(map[string]any{"status": "cancelled", "status_line": "Import cancelled."})
		job.Broadcast("cancelled", job.GetState())
		return
	}
	if err != nil {
		msg := fmt.Sprintf("import error: %s", err)
		job.UpdateState(map[string]any{"status": "error", "status_line": msg, "error_message": msg})
		job.Broadcast("error", job.GetState())
		return
	}

	statusLine := fmt.Sprintf("Completed: %d conversations, %d messages (%d created, %d updated), %d attachments found, %d missing",
		stats.ConversationsProcessed, stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
		stats.AttachmentsFound, stats.AttachmentsMissing)
	job.UpdateState(map[string]any{
		"status":      "completed",
		"status_line": statusLine,
	})
	runThumbnailsAfterImportIfIdle(pool)
	job.Broadcast("completed", job.GetState())
}

func runUploadIMessage(ctx context.Context, pool *pgxpool.Pool, subjectRepo *repository.SubjectConfigRepo, job *appimporter.ImportJob, directoryPath string) {
	defer job.Finish()

	storage := importstorage.NewMessageStorage(ctx, pool, subjectRepo)

	progressCallback := func(stats imessageimport.ImportStats) {
		statusLine := ""
		if stats.TotalConversations > 0 {
			// statusLine = fmt.Sprintf("Processing conversation %d of %d: %s | Messages: %d (%d created, %d updated) | Attachments: %d found, %d missing | Errors: %d",
			// 	stats.ConversationsProcessed, stats.TotalConversations, stats.CurrentConversation,
			// 	stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
			// 	stats.AttachmentsFound, stats.AttachmentsMissing, stats.Errors)
			statusLine = fmt.Sprintf("Processing conversation %d of %d: %s | Messages: %d (%d created, %d updated) | Attachments: %d found",
				stats.ConversationsProcessed, stats.TotalConversations, stats.CurrentConversation,
				stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
				stats.AttachmentsFound)
		}
		job.UpdateState(map[string]any{
			"status_line": statusLine,
		})
		job.Broadcast("progress", job.GetState())
	}

	cancelledCheck := func() bool { return job.IsCancelled() }

	stats, err := imessageimport.ImportIMessagesFromDirectory(ctx, storage, directoryPath, progressCallback, cancelledCheck)

	if job.IsCancelled() {
		job.UpdateState(map[string]any{"status": "cancelled", "status_line": "Import cancelled."})
		job.Broadcast("cancelled", job.GetState())
		return
	}
	if err != nil {
		msg := fmt.Sprintf("import error: %s", err)
		job.UpdateState(map[string]any{"status": "error", "status_line": msg, "error_message": msg})
		job.Broadcast("error", job.GetState())
		return
	}

	statusLine := fmt.Sprintf("Completed: %d conversations, %d messages (%d created, %d updated), %d attachments found, %d missing",
		stats.ConversationsProcessed, stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
		stats.AttachmentsFound, stats.AttachmentsMissing)
	job.UpdateState(map[string]any{
		"status":      "completed",
		"status_line": statusLine,
	})
	runThumbnailsAfterImportIfIdle(pool)
	job.Broadcast("completed", job.GetState())
}

func runUploadInstagram(ctx context.Context, pool *pgxpool.Pool, subjectRepo *repository.SubjectConfigRepo, job *appimporter.ImportJob, directoryPath string) {
	defer job.Finish()

	storage := importstorage.NewMessageStorage(ctx, pool, subjectRepo)

	progressCallback := func(stats instagramimport.ImportStats) {
		statusLine := ""
		if stats.TotalConversations > 0 {
			// statusLine = fmt.Sprintf("Processing conversation %d of %d: %s | Messages: %d (%d created, %d updated) | Attachments: %d found, %d missing | Errors: %d",
			// 	stats.ConversationsProcessed, stats.TotalConversations, stats.CurrentConversation,
			// 	stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
			// 	stats.AttachmentsFound, stats.AttachmentsMissing, stats.Errors)
			statusLine = fmt.Sprintf("Processing conversation %d of %d: %s | Messages: %d (%d created, %d updated) | Attachments: %d found",
				stats.ConversationsProcessed, stats.TotalConversations, stats.CurrentConversation,
				stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated,
				stats.AttachmentsFound)
		}
		job.UpdateState(map[string]any{
			"status_line": statusLine,
		})
		job.Broadcast("progress", job.GetState())
	}

	cancelledCheck := func() bool { return job.IsCancelled() }

	stats, err := instagramimport.ImportInstagramFromDirectory(ctx, storage, directoryPath, progressCallback, cancelledCheck, "", "")

	if job.IsCancelled() {
		job.UpdateState(map[string]any{"status": "cancelled", "status_line": "Import cancelled."})
		job.Broadcast("cancelled", job.GetState())
		return
	}
	if err != nil {
		msg := fmt.Sprintf("import error: %s", err)
		job.UpdateState(map[string]any{"status": "error", "status_line": msg, "error_message": msg})
		job.Broadcast("error", job.GetState())
		return
	}

	statusLine := fmt.Sprintf("Completed: %d conversations, %d messages (%d created, %d updated)",
		stats.ConversationsProcessed, stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated)
	job.UpdateState(map[string]any{
		"status":      "completed",
		"status_line": statusLine,
	})
	runThumbnailsAfterImportIfIdle(pool)
	job.Broadcast("completed", job.GetState())
}

func runUploadFacebook(ctx context.Context, pool *pgxpool.Pool, subjectRepo *repository.SubjectConfigRepo, job *appimporter.ImportJob, directoryPath string) {
	defer job.Finish()

	storage := importstorage.NewMessageStorage(ctx, pool, subjectRepo)
	exportRoot := directoryPath
	cancelledCheck := func() bool { return job.IsCancelled() }

	// Messenger
	job.UpdateState(map[string]any{"status_line": "Running Facebook Messenger import..."})
	job.Broadcast("progress", job.GetState())

	messengerProgress := func(stats facebookimport.ImportStats) {
		job.UpdateState(map[string]any{
			"status_line": fmt.Sprintf("Messenger: %d conversations, %d messages (%d created, %d updated)",
				stats.ConversationsProcessed, stats.MessagesImported, stats.MessagesCreated, stats.MessagesUpdated),
		})
		job.Broadcast("progress", job.GetState())
	}
	messengerStats, messengerErr := facebookimport.ImportFacebookFromDirectory(
		ctx, storage, directoryPath, messengerProgress, cancelledCheck, exportRoot, "",
	)

	if job.IsCancelled() {
		job.UpdateState(map[string]any{"status": "cancelled", "status_line": "Import cancelled."})
		job.Broadcast("cancelled", job.GetState())
		return
	}

	// Albums
	job.UpdateState(map[string]any{"status_line": "Running Facebook Albums import..."})
	job.Broadcast("progress", job.GetState())

	albumsProgress := func(stats facebookalbumsimport.ImportStats) {
		job.UpdateState(map[string]any{
			"status_line": fmt.Sprintf("Albums: %d processed, %d imported, %d images imported",
				stats.AlbumsProcessed, stats.AlbumsImported, stats.ImagesImported),
		})
		job.Broadcast("progress", job.GetState())
	}
	albumsStats, albumsErr := facebookalbumsimport.ImportFacebookAlbumsFromDirectory(
		ctx, pool, directoryPath, albumsProgress, cancelledCheck, exportRoot,
	)

	if job.IsCancelled() {
		job.UpdateState(map[string]any{"status": "cancelled", "status_line": "Import cancelled."})
		job.Broadcast("cancelled", job.GetState())
		return
	}

	// Places
	job.UpdateState(map[string]any{"status_line": "Running Facebook Places import..."})
	job.Broadcast("progress", job.GetState())

	placesProgress := func(stats facebookplacesimport.ImportStats) {
		job.UpdateState(map[string]any{
			"status_line": fmt.Sprintf("Places: %d imported (%d created, %d updated)",
				stats.PlacesImported, stats.PlacesCreated, stats.PlacesUpdated),
		})
		job.Broadcast("progress", job.GetState())
	}
	placesStats, placesErr := facebookplacesimport.ImportFacebookPlacesFromDirectory(
		ctx, pool, directoryPath, placesProgress, cancelledCheck,
	)

	if job.IsCancelled() {
		job.UpdateState(map[string]any{"status": "cancelled", "status_line": "Import cancelled."})
		job.Broadcast("cancelled", job.GetState())
		return
	}

	// Posts
	job.UpdateState(map[string]any{"status_line": "Running Facebook Posts import..."})
	job.Broadcast("progress", job.GetState())

	postsProgress := func(stats facebookpostsimport.ImportStats) {
		job.UpdateState(map[string]any{
			"status_line": fmt.Sprintf("Posts: %d processed, %d imported, %d updated",
				stats.PostsProcessed, stats.PostsImported, stats.PostsUpdated),
		})
		job.Broadcast("progress", job.GetState())
	}
	postsStats, postsErr := facebookpostsimport.ImportFacebookPostsFromPath(
		ctx, pool, directoryPath, exportRoot, postsProgress, cancelledCheck,
	)

	// Build summary
	var parts []string
	if messengerStats != nil {
		parts = append(parts, fmt.Sprintf("Messenger: %d conversations, %d messages",
			messengerStats.ConversationsProcessed, messengerStats.MessagesImported))
	}
	if albumsStats != nil {
		parts = append(parts, fmt.Sprintf("Albums: %d imported, %d images",
			albumsStats.AlbumsImported, albumsStats.ImagesImported))
	}
	if placesStats != nil {
		parts = append(parts, fmt.Sprintf("Places: %d imported",
			placesStats.PlacesImported))
	}
	if postsStats != nil {
		parts = append(parts, fmt.Sprintf("Posts: %d imported",
			postsStats.PostsImported))
	}

	anyErr := messengerErr != nil || albumsErr != nil || placesErr != nil || postsErr != nil
	if anyErr {
		var errMsgs []string
		if messengerErr != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("Messenger: %v", messengerErr))
		}
		if albumsErr != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("Albums: %v", albumsErr))
		}
		if placesErr != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("Places: %v", placesErr))
		}
		if postsErr != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("Posts: %v", postsErr))
		}
		errSummary := strings.Join(errMsgs, "; ")
		job.UpdateState(map[string]any{
			"status":        "completed",
			"status_line":   "Import completed with errors: " + errSummary,
			"error_message": errSummary,
		})
	} else {
		statusLine := "Completed: " + strings.Join(parts, " | ")
		job.UpdateState(map[string]any{
			"status":      "completed",
			"status_line": statusLine,
		})
	}

	runThumbnailsAfterImportIfIdle(pool)
	job.Broadcast("completed", job.GetState())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// randomJobID generates a random hex job ID.
func randomJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolveImportRoot returns the actual data root inside an extracted ZIP directory.
// Most platform export ZIPs contain a single top-level folder
// (e.g. "facebook-export-2024-01-01/") wrapping all data. When extractDir holds
// exactly one sub-directory and no loose files, that sub-directory is returned as
// the import root. Otherwise extractDir itself is returned unchanged.
func resolveImportRoot(extractDir string) string {
	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return extractDir
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			// A loose file at the root means this IS the root
			return extractDir
		}
	}
	if len(dirs) == 1 {
		return filepath.Join(extractDir, dirs[0])
	}
	return extractDir
}

// extractZip extracts a ZIP archive to destDir, rejecting path-traversal entries.
func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") {
			continue
		}
		destPath := filepath.Join(destDir, name)
		// Use filepath.Rel for OS-independent containment check (avoids the
		// mixed-separator false-negative that breaks the slash-based check on Windows).
		rel, err := filepath.Rel(destDir, destPath)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(destPath, 0755)
			continue
		}
		if err := extractZipEntry(f, destPath); err != nil {
			return fmt.Errorf("extract %s: %w", f.Name, err)
		}
	}
	return nil
}

func extractZipEntry(f *zip.File, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
