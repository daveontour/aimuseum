package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/daveontour/aimuseum/internal/appctx"
	"github.com/daveontour/aimuseum/internal/tusfilestore"
	"github.com/go-chi/chi/v5"
	tusdhandler "github.com/tus/tusd/v2/pkg/handler"
)

func validZipImportType(s string) bool {
	switch strings.TrimSpace(s) {
	case "facebook", "instagram", "whatsapp", "imessage":
		return true
	default:
		return false
	}
}

func tusRequestFromHook(ev tusdhandler.HookEvent) *http.Request {
	uri := ev.HTTPRequest.URI
	if uri == "" {
		uri = "/"
	}
	req := httptest.NewRequest(ev.HTTPRequest.Method, uri, nil)
	req = req.WithContext(ev.Context)
	req.Header = ev.HTTPRequest.Header.Clone()
	if host := ev.HTTPRequest.Header.Get("Host"); host != "" {
		req.Host = host
	}
	return req
}

func (h *UploadImportHandler) tusSessionOK(ev tusdhandler.HookEvent) bool {
	if h.sessionStore == nil {
		return false
	}
	req := tusRequestFromHook(ev)
	unlocked, master := h.sessionStore.SessionStatus(req)
	return unlocked && master
}

func (h *UploadImportHandler) tusPreCreate(ev tusdhandler.HookEvent) (tusdhandler.HTTPResponse, tusdhandler.FileInfoChanges, error) {
	if !h.tusSessionOK(ev) {
		return tusdhandler.HTTPResponse{}, tusdhandler.FileInfoChanges{},
			tusdhandler.NewError("ERR_UPLOAD_REJECTED", "owner master key unlock required", http.StatusForbidden)
	}
	if err := uploadJob.AssertNotRunning(); err != nil {
		return tusdhandler.HTTPResponse{}, tusdhandler.FileInfoChanges{},
			tusdhandler.NewError("ERR_UPLOAD_REJECTED", err.Error(), http.StatusConflict)
	}
	importType := strings.TrimSpace(ev.Upload.MetaData["import_type"])
	if !validZipImportType(importType) {
		return tusdhandler.HTTPResponse{}, tusdhandler.FileInfoChanges{},
			tusdhandler.NewError("ERR_UPLOAD_REJECTED", "import_type must be facebook, instagram, whatsapp, or imessage", http.StatusBadRequest)
	}
	return tusdhandler.HTTPResponse{}, tusdhandler.FileInfoChanges{}, nil
}

func (h *UploadImportHandler) tusPreFinish(ev tusdhandler.HookEvent) (tusdhandler.HTTPResponse, error) {
	var binPath string
	if ev.Upload.Storage != nil {
		binPath = ev.Upload.Storage[tusfilestore.StorageKeyPath]
	}
	if binPath == "" {
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_SERVER_ERROR", "missing tus storage path", http.StatusInternalServerError)
	}

	importType := strings.TrimSpace(ev.Upload.MetaData["import_type"])
	if !validZipImportType(importType) {
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_UPLOAD_REJECTED", "invalid import_type metadata", http.StatusBadRequest)
	}

	filename := strings.TrimSpace(ev.Upload.MetaData["filename"])
	if filename == "" {
		filename = "upload.zip"
	}

	uid := appctx.UserIDFromCtx(ev.Context)
	jobID := randomJobID()
	var uidStr string
	if uid == 0 {
		uidStr = "0"
	} else {
		uidStr = fmt.Sprintf("%d", uid)
	}

	tmpDir := filepath.Join("tmp", "imports", uidStr, jobID)
	extractDir := filepath.Join(tmpDir, "extracted")
	zipPath := filepath.Join(tmpDir, "upload.zip")

	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_SERVER_ERROR", "failed to create temp directory: "+err.Error(), http.StatusInternalServerError)
	}

	if err := h.signalUploadZipTUSPreExtract(importType); err != nil {
		_ = os.RemoveAll(tmpDir)
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_UPLOAD_REJECTED", err.Error(), http.StatusConflict)
	}

	src, err := os.Open(binPath)
	if err != nil {
		uploadJob.Finish()
		_ = os.RemoveAll(tmpDir)
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_SERVER_ERROR", "failed to open uploaded file: "+err.Error(), http.StatusInternalServerError)
	}
	dst, err := os.Create(zipPath)
	if err != nil {
		src.Close()
		uploadJob.Finish()
		_ = os.RemoveAll(tmpDir)
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_SERVER_ERROR", "failed to create job zip: "+err.Error(), http.StatusInternalServerError)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		src.Close()
		uploadJob.Finish()
		_ = os.RemoveAll(tmpDir)
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_SERVER_ERROR", "failed to copy uploaded file: "+err.Error(), http.StatusInternalServerError)
	}
	if err := dst.Close(); err != nil {
		src.Close()
		uploadJob.Finish()
		_ = os.RemoveAll(tmpDir)
		return tusdhandler.HTTPResponse{}, tusdhandler.NewError("ERR_SERVER_ERROR", "failed to finish job zip: "+err.Error(), http.StatusInternalServerError)
	}
	src.Close()

	// Clean up TUS storage files now — we've already copied the data to zipPath.
	if ev.Upload.Storage != nil {
		_ = os.Remove(ev.Upload.Storage[tusfilestore.StorageKeyPath])
		if p := ev.Upload.Storage[tusfilestore.StorageKeyInfoPath]; p != "" {
			_ = os.Remove(p)
		}
	}

	// Run extraction and import in the background so the PATCH response is
	// returned immediately.  If we block here for a large ZIP the server
	// WriteTimeout fires, the request context is cancelled, and the follow-up
	// SSE stream auth query fails with "session lookup failed".
	go func() {
		if err := extractZip(zipPath, extractDir); err != nil {
			msg := "failed to extract ZIP: " + err.Error()
			uploadJob.UpdateState(map[string]any{"status": "error", "status_line": msg, "error_message": msg})
			uploadJob.Broadcast("error", uploadJob.GetState())
			uploadJob.Finish()
			_ = os.RemoveAll(tmpDir)
			return
		}
		_ = os.Remove(zipPath)
		importRoot := resolveImportRoot(extractDir)
		h.startZipImportBackground(uid, importType, tmpDir, importRoot, true)
	}()

	return tusdhandler.HTTPResponse{}, nil
}

// mountTUS registers POST/HEAD/PATCH/DELETE under /import/tus for tus-js-client.
func (h *UploadImportHandler) mountTUS(r chi.Router) error {
	if err := os.MkdirAll(h.uploadCfg.TUSUploadDir, 0755); err != nil {
		return fmt.Errorf("tus upload dir %q: %w", h.uploadCfg.TUSUploadDir, err)
	}

	composer := tusdhandler.NewStoreComposer()
	tusfilestore.New(h.uploadCfg.TUSUploadDir).UseIn(composer)

	uh, err := tusdhandler.NewUnroutedHandler(tusdhandler.Config{
		BasePath:                 "/import/tus/",
		StoreComposer:            composer,
		MaxSize:                  h.uploadCfg.MaxUploadBytes,
		RespectForwardedHeaders:  true,
		DisableDownload:          true,
		DisableTermination:       false,
		PreUploadCreateCallback:  h.tusPreCreate,
		PreFinishResponseCallback: h.tusPreFinish,
	})
	if err != nil {
		return err
	}

	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.Trim(req.URL.Path, "/")
		if path == "" {
			if req.Method == http.MethodPost {
				uh.PostFile(w, req)
				return
			}
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch req.Method {
		case http.MethodHead:
			uh.HeadFile(w, req)
		case http.MethodPatch:
			uh.PatchFile(w, req)
		case http.MethodDelete:
			uh.DelFile(w, req)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	chain := http.StripPrefix("/import/tus", uh.Middleware(mux))
	r.Mount("/import/tus", chain)
	return nil
}
