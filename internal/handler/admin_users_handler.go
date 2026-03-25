package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daveontour/aimuseum/internal/appctx"
	appcrypto "github.com/daveontour/aimuseum/internal/crypto"
	"github.com/daveontour/aimuseum/internal/repository"
	"github.com/daveontour/aimuseum/internal/service"
	"github.com/go-chi/chi/v5"
)

const adminSessionCookieName = "dm_admin_sid"
const adminSessionTTL = 2 * time.Hour

// AdminUsersHandler provides the /admin UI and user-management API.
// Authentication is via a plaintext password stored in ADMIN_PASSWORD (config).
// Admin sessions are kept in an in-process map — separate from user sessions.
type AdminUsersHandler struct {
	userRepo         *repository.UserRepo
	sensitiveSvc     *service.SensitiveService
	subjectConfigSvc *service.SubjectConfigService
	dashboardSvc     *service.DashboardService
	adminPassword    string // from ADMIN_PASSWORD env var
	secure           bool
	sessions         adminSessions
}

// NewAdminUsersHandler creates an AdminUsersHandler.
func NewAdminUsersHandler(
	userRepo *repository.UserRepo,
	sensitiveSvc *service.SensitiveService,
	subjectConfigSvc *service.SubjectConfigService,
	dashboardSvc *service.DashboardService,
	adminPassword string,
	secure bool,
) *AdminUsersHandler {
	h := &AdminUsersHandler{
		userRepo:         userRepo,
		sensitiveSvc:     sensitiveSvc,
		subjectConfigSvc: subjectConfigSvc,
		dashboardSvc:     dashboardSvc,
		adminPassword:    adminPassword,
		secure:           secure,
	}
	h.sessions.m = make(map[string]time.Time)
	go h.sessions.cleanupLoop()
	return h
}

// RegisterRoutes mounts the admin routes.
func (h *AdminUsersHandler) RegisterRoutes(r chi.Router) {
	r.Get("/admin", h.GetPage)
	r.Post("/admin/login", h.Login)
	r.Post("/admin/logout", h.Logout)
	r.Get("/admin/users", h.ListUsers)
	r.Post("/admin/users", h.CreateUser)
	r.Delete("/admin/users/{id}", h.DeleteUser)
	r.Get("/admin/users/{id}/dashboard", h.GetUserDashboard)
}

// GET /admin — serves the admin HTML page.
func (h *AdminUsersHandler) GetPage(w http.ResponseWriter, r *http.Request) {
	if h.adminPassword == "" {
		writeError(w, http.StatusServiceUnavailable, "admin panel is disabled (ADMIN_PASSWORD not set)")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminPageHTML))
}

// POST /admin/login — { "password": "..." }
func (h *AdminUsersHandler) Login(w http.ResponseWriter, r *http.Request) {
	if h.adminPassword == "" {
		writeError(w, http.StatusServiceUnavailable, "admin panel is disabled")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Constant-time comparison to guard against timing attacks.
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(h.adminPassword)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid admin password")
		return
	}

	sid := h.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    sid,
		Path:     "/admin",
		MaxAge:   int(adminSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   h.secure,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /admin/logout
func (h *AdminUsersHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminSessionCookieName); err == nil {
		h.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   h.secure,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /admin/users
func (h *AdminUsersHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	users, err := h.userRepo.ListAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	type userRow struct {
		ID          int64  `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		IsActive    bool   `json:"is_active"`
		CreatedAt   string `json:"created_at"`
	}
	out := make([]userRow, 0, len(users))
	for _, u := range users {
		out = append(out, userRow{
			ID:          u.ID,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			IsActive:    u.IsActive,
			CreatedAt:   u.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, out)
}

// DELETE /admin/users/{id}
func (h *AdminUsersHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if err := h.userRepo.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /admin/users — { "email": "...", "password": "...", "display_name": "...", "family_name": "...", "gender": "..." }
func (h *AdminUsersHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var req struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		FamilyName  string `json:"family_name"`
		Gender      string `json:"gender"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.Email == "" || req.Password == "" || req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "email, password, and first name are required")
		return
	}
	if len(req.Password) < 12 {
		writeError(w, http.StatusUnprocessableEntity, "password must be at least 12 characters")
		return
	}
	if req.Gender == "" {
		req.Gender = "Male"
	}

	hash, err := appcrypto.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	user, err := h.userRepo.Create(r.Context(), req.Email, hash, req.DisplayName)
	if err != nil {
		writeError(w, http.StatusConflict, "could not create user — email may already be registered")
		return
	}

	userCtx := context.WithValue(r.Context(), appctx.ContextKeyUserID, user.ID)

	// Initialise keyring so the user is ready to unlock immediately on first login.
	if err := h.sensitiveSvc.InitKeyring(userCtx, req.Password); err != nil {
		slog.Warn("admin: keyring init failed", "user_id", user.ID, "err", err)
	}

	// Create the subject configuration.
	familyName := strings.TrimSpace(req.FamilyName)
	gender := req.Gender
	if _, err := h.subjectConfigSvc.CreateOrUpdate(userCtx, service.SubjectConfigUpdateParams{
		SubjectName: req.DisplayName,
		FamilyName:  &familyName,
		Gender:      &gender,
	}); err != nil {
		slog.Warn("admin: subject config init failed", "user_id", user.ID, "err", err)
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"id":           user.ID,
		"email":        user.Email,
		"display_name": user.DisplayName,
	})
}

// GET /admin/users/{id}/dashboard
func (h *AdminUsersHandler) GetUserDashboard(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	userCtx := context.WithValue(r.Context(), appctx.ContextKeyUserID, id)
	dash, err := h.dashboardSvc.GetDashboard(userCtx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load dashboard")
		return
	}
	writeJSON(w, dash)
}

// requireAdmin checks the admin session cookie and returns false (writing 401) if invalid.
func (h *AdminUsersHandler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(adminSessionCookieName)
	if err != nil || !h.sessions.valid(c.Value) {
		writeError(w, http.StatusUnauthorized, "admin authentication required")
		return false
	}
	return true
}

// ── In-process admin session store ───────────────────────────────────────────

type adminSessions struct {
	mu sync.Mutex
	m  map[string]time.Time // sessionID → expiry
}

func (s *adminSessions) create() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	sid := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[sid] = time.Now().Add(adminSessionTTL)
	s.mu.Unlock()
	return sid
}

func (s *adminSessions) valid(sid string) bool {
	s.mu.Lock()
	exp, ok := s.m[sid]
	s.mu.Unlock()
	return ok && time.Now().Before(exp)
}

func (s *adminSessions) delete(sid string) {
	s.mu.Lock()
	delete(s.m, sid)
	s.mu.Unlock()
}

func (s *adminSessions) cleanupLoop() {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for sid, exp := range s.m {
			if now.After(exp) {
				delete(s.m, sid)
			}
		}
		s.mu.Unlock()
	}
}

// ── Embedded admin page HTML ──────────────────────────────────────────────────

const adminPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Digital Museum — Admin</title>
<link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.5.1/css/all.min.css">
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f1922;color:#cdd6e8;min-height:100vh}
.topbar{background:#19264d;padding:14px 24px;display:flex;align-items:center;justify-content:space-between;border-bottom:1px solid #2a3a6b}
.topbar h1{font-size:1.1rem;color:#fff;display:flex;align-items:center;gap:10px}
.topbar h1 i{color:#3b82f6}
#logout-btn{background:none;border:1px solid #2e4068;color:#8fa4c8;padding:6px 14px;border-radius:6px;cursor:pointer;font-size:0.85rem}
#logout-btn:hover{background:#2a3a6b;color:#fff}
.container{max-width:960px;margin:40px auto;padding:0 20px}
.card{background:#19264d;border-radius:10px;padding:32px;box-shadow:0 4px 24px rgba(0,0,0,0.3)}
.card-header{display:flex;align-items:center;justify-content:space-between;margin-bottom:20px}
.card-header h2{color:#fff;font-size:1.2rem;margin:0}
.form-group{margin-bottom:18px}
.form-group label{display:block;color:#8fa4c8;margin-bottom:6px;font-size:0.88rem}
.form-group input,.form-group select{width:100%;padding:10px 14px;border-radius:6px;border:1px solid #2e4068;background:#0f1922;color:#fff;font-size:1rem;outline:none}
.form-group input:focus,.form-group select:focus{border-color:#3b82f6}
.form-row{display:grid;grid-template-columns:1fr 1fr;gap:14px}
.btn{padding:10px 20px;border-radius:6px;border:none;font-size:0.95rem;font-weight:600;cursor:pointer;transition:background 0.15s}
.btn-primary{background:#3b82f6;color:#fff}.btn-primary:hover{background:#2563eb}
.btn-primary:disabled{background:#1e3a6b;cursor:not-allowed}
.btn-success{background:#16a34a;color:#fff;padding:8px 16px;font-size:0.88rem}.btn-success:hover{background:#15803d}
.btn-danger{background:#dc2626;color:#fff;padding:6px 12px;font-size:0.82rem}.btn-danger:hover{background:#b91c1c}
.btn-warning{background:#d97706;color:#fff;padding:6px 12px;font-size:0.82rem}.btn-warning:hover{background:#b45309}
.btn-info{background:#0e7490;color:#fff;padding:6px 12px;font-size:0.82rem}.btn-info:hover{background:#0c5f75}
.error{color:#f87171;background:rgba(248,113,113,0.1);border:1px solid rgba(248,113,113,0.25);padding:10px 14px;border-radius:6px;margin-bottom:16px;font-size:0.9rem;display:none}
#login-section{display:flex;align-items:center;justify-content:center;min-height:100vh}
#admin-section{display:none}
table{width:100%;border-collapse:collapse}
th{text-align:left;color:#8fa4c8;font-size:0.82rem;font-weight:600;padding:8px 12px;border-bottom:1px solid #2a3a6b}
td{padding:10px 12px;border-bottom:1px solid #1e2d4d;font-size:0.9rem;vertical-align:middle}
tr:last-child td{border-bottom:none}
tr:hover td{background:rgba(255,255,255,0.02)}
.badge{display:inline-block;padding:2px 8px;border-radius:4px;font-size:0.75rem;font-weight:600}
.badge-active{background:rgba(34,197,94,0.15);color:#4ade80}
.badge-inactive{background:rgba(248,113,113,0.15);color:#f87171}
.actions{display:flex;gap:6px;flex-wrap:wrap}
.modal-overlay{display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center;padding:20px}
.modal-overlay.open{display:flex}
.modal{background:#19264d;border-radius:10px;padding:28px;width:100%;max-width:460px;box-shadow:0 8px 32px rgba(0,0,0,0.5);max-height:90vh;overflow-y:auto}
.modal-wide{max-width:720px}
.modal h3{color:#fff;margin-bottom:18px}
.modal-actions{display:flex;gap:10px;justify-content:flex-end;margin-top:20px}
.btn-ghost{background:none;border:1px solid #2e4068;color:#8fa4c8;padding:8px 16px;font-size:0.9rem;border-radius:6px;cursor:pointer}.btn-ghost:hover{background:#2a3a6b}
.stat-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:12px;margin-bottom:20px}
.stat-card{background:#0f1922;border-radius:8px;padding:14px 16px;border:1px solid #2a3a6b}
.stat-card .label{color:#8fa4c8;font-size:0.78rem;margin-bottom:4px}
.stat-card .value{color:#fff;font-size:1.4rem;font-weight:700}
.stat-section{color:#3b82f6;font-size:0.8rem;font-weight:700;text-transform:uppercase;letter-spacing:0.05em;margin:16px 0 8px}
</style>
</head>
<body>

<!-- Login screen -->
<div id="login-section">
  <div class="card" style="max-width:380px;width:100%">
    <h2 style="color:#fff;margin-bottom:20px"><i class="fas fa-lock" style="color:#3b82f6;margin-right:8px"></i>Admin Login</h2>
    <div id="login-error" class="error"></div>
    <div class="form-group">
      <label for="admin-password">Admin Password</label>
      <input id="admin-password" type="password" placeholder="Enter admin password" autocomplete="current-password">
    </div>
    <button id="login-btn" class="btn btn-primary" style="width:100%">
      <i class="fas fa-sign-in-alt" style="margin-right:6px"></i>Sign In
    </button>
  </div>
</div>

<!-- Admin panel -->
<div id="admin-section">
  <div class="topbar">
    <h1><i class="fas fa-shield-halved"></i>Digital Museum — Admin</h1>
    <button id="logout-btn"><i class="fas fa-sign-out-alt" style="margin-right:6px"></i>Logout</button>
  </div>
  <div class="container">
    <div class="card">
      <div class="card-header">
        <h2>Users</h2>
        <button class="btn btn-success" id="add-user-btn">
          <i class="fas fa-user-plus" style="margin-right:6px"></i>Add User
        </button>
      </div>
      <div id="users-error" class="error"></div>
      <table id="users-table">
        <thead>
          <tr>
            <th>ID</th><th>Email</th><th>Name</th><th>Status</th><th>Registered</th><th>Actions</th>
          </tr>
        </thead>
        <tbody id="users-body"></tbody>
      </table>
    </div>
  </div>
</div>

<!-- Add user modal -->
<div class="modal-overlay" id="add-modal">
  <div class="modal">
    <h3><i class="fas fa-user-plus" style="color:#3b82f6;margin-right:8px"></i>Add User</h3>
    <div id="add-error" class="error"></div>
    <div class="form-group">
      <label>Email *</label>
      <input id="add-email" type="email" placeholder="user@example.com" autocomplete="off">
    </div>
    <div class="form-row">
      <div class="form-group">
        <label>First Name *</label>
        <input id="add-display-name" type="text" placeholder="e.g. Jane">
      </div>
      <div class="form-group">
        <label>Family Name</label>
        <input id="add-family-name" type="text" placeholder="e.g. Smith">
      </div>
    </div>
    <div class="form-group">
      <label>Gender</label>
      <select id="add-gender">
        <option value="Male">Male</option>
        <option value="Female">Female</option>
      </select>
    </div>
    <div class="form-row">
      <div class="form-group">
        <label>Password * (min 12 chars)</label>
        <input id="add-password" type="password" placeholder="Password" autocomplete="new-password">
      </div>
      <div class="form-group">
        <label>Confirm Password *</label>
        <input id="add-password-confirm" type="password" placeholder="Confirm" autocomplete="new-password">
      </div>
    </div>
    <div class="modal-actions">
      <button class="btn btn-ghost" id="add-cancel">Cancel</button>
      <button class="btn btn-primary" id="add-save">
        <i class="fas fa-user-plus" style="margin-right:6px"></i>Create User
      </button>
    </div>
  </div>
</div>

<!-- Delete confirm modal -->
<div class="modal-overlay" id="del-modal">
  <div class="modal">
    <h3>Delete User</h3>
    <p style="color:#f87171;margin-bottom:8px" id="del-modal-label"></p>
    <p style="color:#8fa4c8;font-size:0.88rem">This permanently deletes the user and all their archive data. This cannot be undone.</p>
    <div class="modal-actions">
      <button class="btn btn-ghost" id="del-cancel">Cancel</button>
      <button class="btn btn-danger" id="del-confirm">Delete</button>
    </div>
  </div>
</div>

<!-- Dashboard stats modal -->
<div class="modal-overlay" id="stats-modal">
  <div class="modal modal-wide">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
      <h3 id="stats-modal-title" style="margin:0"><i class="fas fa-chart-bar" style="color:#3b82f6;margin-right:8px"></i>Archive Statistics</h3>
      <button class="btn btn-ghost" id="stats-close-top" style="padding:4px 10px;font-size:1.1rem;line-height:1" title="Close">&times;</button>
    </div>
    <div id="stats-body"><p style="color:#8fa4c8;text-align:center;padding:24px">Loading\u2026</p></div>
    <div class="modal-actions">
      <button class="btn btn-ghost" id="stats-close">Close</button>
    </div>
  </div>
</div>

<script>
(function() {
  let pendingDelUserId = null;

  async function apiFetch(path, opts) {
    return fetch(path, { credentials: 'same-origin', ...opts });
  }

  // ── Login ──────────────────────────────────────────────────────────────────
  document.getElementById('admin-password').addEventListener('keydown', function(e) {
    if (e.key === 'Enter') document.getElementById('login-btn').click();
  });

  document.getElementById('login-btn').addEventListener('click', async function() {
    const btn = this;
    const errEl = document.getElementById('login-error');
    const pw = document.getElementById('admin-password').value;
    errEl.style.display = 'none';
    btn.disabled = true;
    btn.innerHTML = '<i class="fas fa-spinner fa-spin" style="margin-right:6px"></i>Signing in\u2026';
    try {
      const res = await apiFetch('/admin/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: pw })
      });
      if (res.ok) { showAdmin(); }
      else {
        const d = await res.json().catch(() => ({}));
        errEl.textContent = d.error || 'Invalid password.';
        errEl.style.display = 'block';
      }
    } catch (e) {
      errEl.textContent = 'Network error.';
      errEl.style.display = 'block';
    } finally {
      btn.disabled = false;
      btn.innerHTML = '<i class="fas fa-sign-in-alt" style="margin-right:6px"></i>Sign In';
    }
  });

  // ── Logout ─────────────────────────────────────────────────────────────────
  document.getElementById('logout-btn').addEventListener('click', async function() {
    await apiFetch('/admin/logout', { method: 'POST' });
    document.getElementById('admin-section').style.display = 'none';
    document.getElementById('login-section').style.display = 'flex';
    document.getElementById('admin-password').value = '';
  });

  function showAdmin() {
    document.getElementById('login-section').style.display = 'none';
    document.getElementById('admin-section').style.display = 'block';
    loadUsers();
  }

  // ── Load / render users ────────────────────────────────────────────────────
  async function loadUsers() {
    const errEl = document.getElementById('users-error');
    errEl.style.display = 'none';
    try {
      const res = await apiFetch('/admin/users');
      if (res.status === 401) {
        document.getElementById('admin-section').style.display = 'none';
        document.getElementById('login-section').style.display = 'flex';
        return;
      }
      if (!res.ok) throw new Error('Failed to load users');
      renderUsers((await res.json()) || []);
    } catch (e) {
      errEl.textContent = e.message;
      errEl.style.display = 'block';
    }
  }

  function renderUsers(users) {
    const tbody = document.getElementById('users-body');
    if (users.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" style="color:#8fa4c8;text-align:center;padding:24px">No users registered.</td></tr>';
      return;
    }
    tbody.innerHTML = users.map(u => {
      const date = new Date(u.created_at).toLocaleDateString();
      const badge = u.is_active
        ? '<span class="badge badge-active">Active</span>'
        : '<span class="badge badge-inactive">Inactive</span>';
      return '<tr>' +
        '<td style="color:#8fa4c8">' + u.id + '</td>' +
        '<td>' + escHtml(u.email) + '</td>' +
        '<td>' + escHtml(u.display_name || '\u2014') + '</td>' +
        '<td>' + badge + '</td>' +
        '<td style="color:#8fa4c8">' + date + '</td>' +
        '<td><div class="actions">' +
          '<button class="btn btn-info" onclick="openStatsModal(' + u.id + ',\'' + escAttr(u.email) + '\')">' +
            '<i class="fas fa-chart-bar" style="margin-right:4px"></i>Stats</button>' +
          '<button class="btn btn-danger" onclick="openDelModal(' + u.id + ',\'' + escAttr(u.email) + '\')">' +
            '<i class="fas fa-trash" style="margin-right:4px"></i>Delete</button>' +
        '</div></td>' +
      '</tr>';
    }).join('');
  }

  // ── Add user modal ─────────────────────────────────────────────────────────
  document.getElementById('add-user-btn').addEventListener('click', function() {
    document.getElementById('add-email').value = '';
    document.getElementById('add-display-name').value = '';
    document.getElementById('add-family-name').value = '';
    document.getElementById('add-gender').value = 'Male';
    document.getElementById('add-password').value = '';
    document.getElementById('add-password-confirm').value = '';
    document.getElementById('add-error').style.display = 'none';
    document.getElementById('add-modal').classList.add('open');
    document.getElementById('add-email').focus();
  });

  document.getElementById('add-cancel').addEventListener('click', function() {
    document.getElementById('add-modal').classList.remove('open');
  });

  document.getElementById('add-save').addEventListener('click', async function() {
    const btn = this;
    const errEl = document.getElementById('add-error');
    errEl.style.display = 'none';
    const email = document.getElementById('add-email').value.trim();
    const displayName = document.getElementById('add-display-name').value.trim();
    const familyName = document.getElementById('add-family-name').value.trim();
    const gender = document.getElementById('add-gender').value;
    const password = document.getElementById('add-password').value;
    const confirm = document.getElementById('add-password-confirm').value;
    if (!email || !displayName || !password) {
      errEl.textContent = 'Email, first name, and password are required.';
      errEl.style.display = 'block';
      return;
    }
    if (password !== confirm) {
      errEl.textContent = 'Passwords do not match.';
      errEl.style.display = 'block';
      return;
    }
    if (password.length < 12) {
      errEl.textContent = 'Password must be at least 12 characters.';
      errEl.style.display = 'block';
      return;
    }
    btn.disabled = true;
    btn.innerHTML = '<i class="fas fa-spinner fa-spin" style="margin-right:6px"></i>Creating\u2026';
    try {
      const res = await apiFetch('/admin/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, display_name: displayName, family_name: familyName, gender, password })
      });
      if (res.ok) {
        document.getElementById('add-modal').classList.remove('open');
        loadUsers();
      } else {
        const d = await res.json().catch(() => ({}));
        errEl.textContent = d.error || 'Failed to create user.';
        errEl.style.display = 'block';
      }
    } catch (e) {
      errEl.textContent = 'Network error.';
      errEl.style.display = 'block';
    } finally {
      btn.disabled = false;
      btn.innerHTML = '<i class="fas fa-user-plus" style="margin-right:6px"></i>Create User';
    }
  });

  // ── Stats modal ────────────────────────────────────────────────────────────
  window.openStatsModal = async function(id, email) {
    document.getElementById('stats-modal-title').innerHTML =
      '<i class="fas fa-chart-bar" style="color:#3b82f6;margin-right:8px"></i>Archive Statistics &mdash; ' + escHtml(email);
    document.getElementById('stats-body').innerHTML =
      '<p style="color:#8fa4c8;text-align:center;padding:24px"><i class="fas fa-spinner fa-spin"></i> Loading\u2026</p>';
    document.getElementById('stats-modal').classList.add('open');
    try {
      const res = await apiFetch('/admin/users/' + id + '/dashboard');
      if (!res.ok) throw new Error('Failed to load stats');
      const d = await res.json();
      document.getElementById('stats-body').innerHTML = renderStats(d);
    } catch (e) {
      document.getElementById('stats-body').innerHTML =
        '<p style="color:#f87171;text-align:center;padding:24px">' + escHtml(e.message) + '</p>';
    }
  };

  document.getElementById('stats-close').addEventListener('click', function() {
    document.getElementById('stats-modal').classList.remove('open');
  });
  document.getElementById('stats-close-top').addEventListener('click', function() {
    document.getElementById('stats-modal').classList.remove('open');
  });

  function statCard(label, value) {
    return '<div class="stat-card"><div class="label">' + escHtml(label) +
      '</div><div class="value">' + escHtml(String(value)) + '</div></div>';
  }

  function renderStats(d) {
    const fmt = n => (n == null ? '0' : Number(n).toLocaleString());
    let html = '';

    html += '<div class="stat-section">Overview</div><div class="stat-grid">';
    html += statCard('Subject', d.subject_full_name || '\u2014');
    html += statCard('Emails', fmt(d.emails_count));
    html += statCard('Messages', fmt(d.total_messages));
    html += statCard('Contacts', fmt(d.contacts_count));
    html += statCard('Images', fmt(d.total_images));
    html += statCard('Artefacts', fmt(d.artefacts_count));
    html += '</div>';

    html += '<div class="stat-section">Media</div><div class="stat-grid">';
    html += statCard('Imported Images', fmt(d.imported_images));
    html += statCard('Reference Images', fmt(d.reference_images));
    html += statCard('Thumbnails', fmt(d.thumbnail_count) + ' (' + (d.thumbnail_percentage || 0).toFixed(1) + '%)');
    html += statCard('Facebook Albums', fmt(d.facebook_albums_count));
    html += statCard('Facebook Posts', fmt(d.facebook_posts_count));
    html += '</div>';

    html += '<div class="stat-section">Places &amp; Documents</div><div class="stat-grid">';
    html += statCard('Locations', fmt(d.locations_count));
    html += statCard('Places', fmt(d.places_count));
    html += statCard('Reference Docs', fmt(d.reference_docs_count));
    html += statCard('Docs Enabled', fmt(d.reference_docs_enabled));
    html += statCard('Complete Profiles', fmt(d.complete_profiles_count));
    html += '</div>';

    if (d.message_counts && Object.keys(d.message_counts).length) {
      html += '<div class="stat-section">Messages by Type</div><div class="stat-grid">';
      for (const [k, v] of Object.entries(d.message_counts)) {
        html += statCard(k, fmt(v));
      }
      html += '</div>';
    }

    if (d.messages_by_contact && d.messages_by_contact.length) {
      html += '<div class="stat-section">Top Contacts</div>';
      html += '<table style="margin-bottom:8px"><thead><tr><th>Name</th><th>Messages</th></tr></thead><tbody>';
      d.messages_by_contact.slice(0, 10).forEach(c => {
        html += '<tr><td>' + escHtml(c.name) + '</td><td>' + fmt(c.count) + '</td></tr>';
      });
      html += '</tbody></table>';
    }

    return html;
  }

  // ── Delete modal ───────────────────────────────────────────────────────────
  window.openDelModal = function(id, email) {
    pendingDelUserId = id;
    document.getElementById('del-modal-label').textContent = 'Delete user: ' + email;
    document.getElementById('del-modal').classList.add('open');
  };

  document.getElementById('del-cancel').addEventListener('click', function() {
    document.getElementById('del-modal').classList.remove('open');
  });

  document.getElementById('del-confirm').addEventListener('click', async function() {
    const btn = this;
    btn.disabled = true; btn.textContent = 'Deleting\u2026';
    try {
      const res = await apiFetch('/admin/users/' + pendingDelUserId, { method: 'DELETE' });
      document.getElementById('del-modal').classList.remove('open');
      if (res.ok) { loadUsers(); }
      else {
        const d = await res.json().catch(() => ({}));
        document.getElementById('users-error').textContent = d.error || 'Delete failed.';
        document.getElementById('users-error').style.display = 'block';
      }
    } catch (e) { document.getElementById('del-modal').classList.remove('open'); }
    finally { btn.disabled = false; btn.textContent = 'Delete'; }
  });

  // ── Helpers ────────────────────────────────────────────────────────────────
  function escHtml(s) {
    return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
  }
  function escAttr(s) {
    return String(s).replace(/'/g,"\\'");
  }

  // Auto-check if already logged in on page load.
  (async function() {
    const res = await apiFetch('/admin/users');
    if (res.ok) showAdmin();
  })();
})();
</script>
</body>
</html>`
