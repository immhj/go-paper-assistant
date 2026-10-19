package router

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"paperassistant/internal/config"
	"paperassistant/internal/controller"
)

type Dependencies struct {
	Config   config.Config
	Health   *controller.Health
	Admin    *controller.Admin
	Platform *controller.Platform
	Chat     *controller.Chat
	Projects *controller.Projects
	Papers   *controller.Papers
}

func New(deps Dependencies) http.Handler {
	mux := http.NewServeMux()
	if deps.Health != nil {
		mux.HandleFunc("GET /healthz", deps.Health.Get)
	}
	// "/{$}" matches only the root, so unknown paths of any method get a uniform 404 instead of a
	// 405 that would hint which paths exist.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusTemporaryRedirect)
	})

	if deps.Platform != nil {
		existing := func(handler http.HandlerFunc) http.Handler { return deps.Platform.ExistingAuth(handler) }
		required := func(handler http.HandlerFunc) http.Handler { return deps.Platform.RequireAccount(handler) }
		optional := func(handler http.HandlerFunc) http.Handler { return deps.Platform.OptionalAuth(handler) }

		mux.Handle("POST /api/register/code", existing(deps.Platform.RegisterCode))
		mux.Handle("POST /api/register", existing(deps.Platform.Register))
		mux.Handle("POST /api/login", existing(deps.Platform.Login))
		mux.Handle("POST /api/password-reset/code", existing(deps.Platform.PasswordResetCode))
		mux.Handle("POST /api/password-reset", existing(deps.Platform.PasswordReset))
		mux.Handle("GET /api/session", optional(deps.Platform.Session))
		mux.Handle("DELETE /api/guest", existing(deps.Platform.ClearGuest))
		mux.Handle("POST /api/guest/import", required(deps.Platform.ImportGuest))
		mux.Handle("GET /api/me", required(deps.Platform.Me))
		mux.Handle("POST /api/me", required(deps.Platform.UpdateMe))
		if deps.Projects != nil {
			mux.Handle("POST /api/projects/ensure-active", optional(deps.Projects.EnsureActive))
			mux.Handle("GET /api/projects", optional(deps.Projects.List))
			mux.Handle("POST /api/projects", optional(deps.Projects.Create))
			mux.Handle("PATCH /api/projects/{projectNo}", optional(deps.Projects.Rename))
			mux.Handle("POST /api/projects/{projectNo}/select", optional(deps.Projects.Select))
			mux.Handle("POST /api/projects/{projectNo}/archive", optional(deps.Projects.Archive))
			mux.Handle("POST /api/projects/{projectNo}/restore", optional(deps.Projects.Restore))
			mux.Handle("GET /api/projects/{projectNo}/conversations", optional(deps.Projects.ListConversations))
			mux.Handle("POST /api/projects/{projectNo}/conversations", optional(deps.Projects.CreateConversation))
			mux.Handle("PATCH /api/projects/{projectNo}/conversations/{conversationNo}", optional(deps.Projects.RenameConversation))
			mux.Handle("POST /api/projects/{projectNo}/conversations/{conversationNo}/select", optional(deps.Projects.SelectConversation))
			mux.Handle("POST /api/projects/{projectNo}/conversations/{conversationNo}/archive", optional(deps.Projects.ArchiveConversation))
			mux.Handle("POST /api/projects/{projectNo}/conversations/{conversationNo}/restore", optional(deps.Projects.RestoreConversation))
			mux.Handle("GET /api/projects/{projectNo}/papers", optional(deps.Projects.ListPapers))
			mux.Handle("PUT /api/projects/{projectNo}/papers/{paperId}", optional(deps.Projects.AddPaper))
			mux.Handle("DELETE /api/projects/{projectNo}/papers/{paperId}", optional(deps.Projects.RemovePaper))
			mux.Handle("GET /api/projects/{projectNo}/papers/{paperId}/read-results", optional(deps.Projects.ReadResults))
		}
		if deps.Papers != nil {
			// Account-only (ADR 0019): RequireAccount never mints a guest session for these routes.
			mux.Handle("GET /api/papers/{paperId}/private-fulltext", required(deps.Papers.PrivateFullTextStatus))
			mux.Handle("PUT /api/papers/{paperId}/private-fulltext", required(deps.Papers.UploadPrivateFullText))
			mux.Handle("DELETE /api/papers/{paperId}/private-fulltext", required(deps.Papers.DeletePrivateFullText))
		}
		if deps.Chat != nil {
			mux.Handle("GET /api/conversations", optional(deps.Chat.ListConversations))
			mux.Handle("POST /api/conversations", optional(deps.Chat.CreateConversation))
			mux.Handle("POST /api/conversations/{conversationID}/activate", optional(deps.Chat.ActivateConversation))
			mux.Handle("GET /api/history", optional(deps.Chat.History))
			mux.Handle("POST /api/chat", optional(deps.Chat.Send))
		}
	}
	mux.Handle("GET /app/", http.StripPrefix("/app/", http.FileServer(http.Dir("web/app"))))
	// Admin routes are deliberately absent here: they are served only by NewAdmin on the
	// loopback-bound admin listener (ADR 0023), so /admin/ and /api/admin/* are 404 publicly.
	return recoverer(logger(securityHeaders(mux)))
}

// NewAdmin builds the handler for the admin listener. It must only be bound to a loopback
// address (reached through an SSH tunnel); Basic Auth remains as a second line of defence.
func NewAdmin(deps Dependencies) http.Handler {
	adminMux := http.NewServeMux()
	adminMux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServer(http.Dir("web/admin"))))
	if deps.Admin != nil {
		adminMux.HandleFunc("GET /api/admin/overview", deps.Admin.Overview)
		adminMux.HandleFunc("GET /api/admin/users", deps.Admin.Users)
		adminMux.HandleFunc("GET /api/admin/config", deps.Admin.RuntimeConfig)
		adminMux.HandleFunc("GET /api/admin/runtime-controls", deps.Admin.RuntimeControls)
		adminMux.HandleFunc("POST /api/admin/runtime-controls", deps.Admin.UpdateRuntimeControls)
		adminMux.HandleFunc("GET /api/admin/users/{userID}/conversations", deps.Admin.Conversations)
		adminMux.HandleFunc("GET /api/admin/conversations/{conversationID}/messages", deps.Admin.Messages)
		adminMux.HandleFunc("POST /api/admin/users/{userID}/conversations/{conversationID}/restore", deps.Admin.RestoreConversation)
	}
	if deps.Platform != nil {
		adminMux.HandleFunc("GET /api/admin/accounts", deps.Platform.ListAccounts)
		adminMux.HandleFunc("POST /api/admin/accounts/{accountID}/status", deps.Platform.SetAccountStatus)
		adminMux.HandleFunc("GET /api/admin/guests", deps.Platform.GuestMetrics)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	})
	mux.Handle("GET /admin/", basicAuth(deps.Config.Admin, adminMux))
	mux.Handle("GET /api/admin/", basicAuth(deps.Config.Admin, adminMux))
	mux.Handle("POST /api/admin/", basicAuth(deps.Config.Admin, adminMux))
	return recoverer(logger(securityHeaders(mux)))
}

func basicAuth(cfg config.AdminConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(cfg.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(password), []byte(cfg.Password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="paper-assistant-admin", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/healthz") {
			slog.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
		}
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				slog.Error("http panic", "value", value, "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
