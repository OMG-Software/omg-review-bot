// Package web serves the management UI.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	store  *store.Store
	bot    *bot.Bot
	log    *slog.Logger
	pages  map[string]*template.Template
	secret []byte
	mux    *http.ServeMux
}

func New(st *store.Store, b *bot.Bot, log *slog.Logger) (*Server, error) {
	s := &Server{store: st, bot: b, log: log, pages: map[string]*template.Template{}}
	if err := s.loadSecret(); err != nil {
		return nil, err
	}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		base := strings.TrimPrefix(n, "templates/")
		if base == "layout.html" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(assets, "templates/layout.html", n)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", n, err)
		}
		s.pages[base] = t
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	m.HandleFunc("GET /setup", s.setupPage)
	m.HandleFunc("POST /setup", s.setupSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("GET /account", s.accountPage)
	m.HandleFunc("POST /account", s.accountSubmit)

	m.HandleFunc("GET /{$}", s.dashboard)
	m.HandleFunc("GET /prs", s.prsPage)
	m.HandleFunc("GET /prs/{owner}/{repo}/{number}", s.prPage)
	m.HandleFunc("POST /prs/{owner}/{repo}/{number}/review", s.prReview)
	m.HandleFunc("POST /prs/{owner}/{repo}/{number}/replies", s.prReplies)
	m.HandleFunc("POST /prs/{owner}/{repo}/{number}/skip", s.prSkip)
	m.HandleFunc("GET /reviews/{id}", s.reviewPage)
	m.HandleFunc("GET /repos", s.reposPage)
	m.HandleFunc("POST /repos/sync", s.reposSync)
	m.HandleFunc("POST /repos/all", s.reposAll)
	m.HandleFunc("POST /repos/{id}/toggle", s.repoToggle)
	m.HandleFunc("GET /settings", s.settingsPage)
	m.HandleFunc("POST /settings", s.settingsSubmit)
	m.HandleFunc("GET /skill", s.skillPage)
	m.HandleFunc("POST /skill/upload", s.skillUpload)
	m.HandleFunc("POST /skill/save", s.skillSave)
	m.HandleFunc("GET /activity", s.activityPage)

	m.HandleFunc("GET /api/status", s.apiStatus)
	m.HandleFunc("POST /api/test/github", s.apiTestGitHub)
	m.HandleFunc("POST /api/test/llm", s.apiTestLLM)
	m.HandleFunc("POST /api/models", s.apiModels)
	s.mux = m
}

// ServeHTTP applies security headers, CSRF protection and authentication.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")

	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
		return
	}
	path := r.URL.Path
	isStatic := strings.HasPrefix(path, "/static/")
	switch {
	case isStatic:
	case !s.hasPassword():
		if path != "/setup" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
	case path == "/setup":
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case path == "/login":
	case !s.validSession(r):
		if strings.HasPrefix(path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "not signed in"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.mux.ServeHTTP(w, r)
}

type page struct {
	Title  string
	Nav    string
	Msg    string
	Err    string
	Auth   bool
	Status bot.Status
	D      any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	q := r.URL.Query()
	if p.Msg == "" {
		p.Msg = q.Get("msg")
	}
	if p.Err == "" {
		p.Err = q.Get("err")
	}
	p.Auth = s.validSession(r)
	p.Status = s.bot.Status()
	t := s.pages[name]
	if t == nil {
		http.Error(w, "unknown page "+name, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.log.Error("render", "page", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func redirectMsg(w http.ResponseWriter, r *http.Request, to, msg string) {
	http.Redirect(w, r, to+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, to string, err error) {
	http.Redirect(w, r, to+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var funcs = template.FuncMap{
	"ago": func(unix int64) string {
		if unix == 0 {
			return "—"
		}
		return agoString(time.Since(time.Unix(unix, 0)))
	},
	"agoT": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return agoString(time.Since(t))
	},
	"when": func(unix int64) string {
		if unix == 0 {
			return ""
		}
		return time.Unix(unix, 0).Format("2006-01-02 15:04:05")
	},
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
	"lines": func(ss []string) string { return strings.Join(ss, "\n") },
	"join":  strings.Join,
	"dur": func(a, b int64) string {
		if a == 0 || b == 0 {
			return ""
		}
		return (time.Duration(b-a) * time.Second).String()
	},
	"div": func(a, b int) int { return a / max(1, b) },
	"pct": func(a, b int) int {
		if b == 0 {
			return 0
		}
		return a * 100 / b
	},
}

func agoString(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
