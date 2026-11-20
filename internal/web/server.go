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
		if base == "layout.html" || base == "partials.html" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", n)
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

	// Sign-in.
	m.HandleFunc("GET /setup", s.setupPage)
	m.HandleFunc("POST /setup", s.setupSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("GET /admin-login", s.adminLoginPage)
	m.HandleFunc("POST /admin-login", s.adminLoginSubmit)
	m.HandleFunc("GET /auth/github", s.githubStart)
	m.HandleFunc("GET /auth/github/callback", s.githubCallback)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("GET /account", s.accountPage)
	m.HandleFunc("POST /account", s.accountSubmit)
	m.HandleFunc("POST /account/signout-all", s.accountSignOutAll)

	// Everyone signed in (access is checked per organization).
	m.HandleFunc("GET /{$}", s.home)
	m.HandleFunc("GET /orgs", s.orgsPage)
	m.HandleFunc("GET /orgs/{org}", s.orgOverview)
	m.HandleFunc("GET /orgs/{org}/prs", s.orgPRs)
	m.HandleFunc("GET /orgs/{org}/reviews", s.orgReviews)
	m.HandleFunc("GET /orgs/{org}/repos", s.orgRepos)
	m.HandleFunc("POST /orgs/{org}/repos/all", s.orgReposAll)
	m.HandleFunc("POST /orgs/{org}/repos/{id}/toggle", s.orgRepoToggle)
	m.HandleFunc("GET /orgs/{org}/members", s.orgMembers)
	m.HandleFunc("POST /orgs/{org}/members", s.orgMemberAdd)
	m.HandleFunc("POST /orgs/{org}/members/{uid}/role", s.orgMemberRole)
	m.HandleFunc("POST /orgs/{org}/members/{uid}/remove", s.orgMemberRemove)
	m.HandleFunc("POST /orgs/{org}/autojoin", s.orgAutoJoin)
	m.HandleFunc("GET /orgs/{org}/settings", s.orgSettings)
	m.HandleFunc("POST /orgs/{org}/settings", s.orgSettingsSubmit)
	m.HandleFunc("GET /orgs/{org}/activity", s.orgActivity)
	m.HandleFunc("GET /prs/{owner}/{repo}/{number}", s.prPage)
	m.HandleFunc("POST /prs/{owner}/{repo}/{number}/review", s.prReview)
	m.HandleFunc("POST /prs/{owner}/{repo}/{number}/replies", s.prReplies)
	m.HandleFunc("POST /prs/{owner}/{repo}/{number}/skip", s.prSkip)
	m.HandleFunc("GET /reviews/{id}", s.reviewPage)
	m.HandleFunc("GET /api/status", s.apiStatus)

	// Administrators only.
	a := func(pattern string, h http.HandlerFunc) { m.HandleFunc(pattern, s.adminOnly(h)) }
	a("GET /prs", s.prsPage)
	a("GET /reviews", s.reviewsPage)
	a("GET /repos", s.reposPage)
	a("POST /repos/sync", s.reposSync)
	a("POST /repos/all", s.reposAll)
	a("POST /repos/{id}/toggle", s.repoToggle)
	a("GET /settings", s.settingsPage)
	a("POST /settings", s.settingsSubmit)
	a("GET /skill", s.skillPage)
	a("POST /skill/upload", s.skillUpload)
	a("POST /skill/save", s.skillSave)
	a("GET /activity", s.activityPage)
	a("GET /admin/users", s.usersPage)
	a("POST /admin/users/{id}/admin", s.userSetAdmin)
	a("POST /admin/users/{id}/disable", s.userSetDisabled)
	a("GET /admin/webhooks", s.webhooksPage)
	a("POST /api/test/github", s.apiTestGitHub)
	a("POST /api/test/llm", s.apiTestLLM)
	a("POST /api/models", s.apiModels)
	s.mux = m
}

// publicPaths are reachable without signing in (once an admin password exists).
var publicPaths = map[string]bool{"/login": true, "/admin-login": true, "/auth/github": true, "/auth/github/callback": true}

// ServeHTTP applies security headers, CSRF protection and authentication.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")

	path := r.URL.Path
	if path == "/webhooks/github" {
		// Authenticated by its signature, not a session; GitHub sends no Origin.
		s.webhook(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
		return
	}
	switch {
	case strings.HasPrefix(path, "/static/"):
	case !s.hasPassword():
		if path != "/setup" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
	case path == "/setup":
		http.Redirect(w, r, "/admin-login", http.StatusSeeOther)
		return
	case publicPaths[path]:
	default:
		v := s.sessionViewer(r)
		if v == nil {
			if strings.HasPrefix(path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "not signed in"})
				return
			}
			to := "/login"
			if r.Method == http.MethodGet && path != "/" {
				to += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		r = withViewer(r, v)
	}
	s.mux.ServeHTTP(w, r)
}

// adminOnly restricts a handler to administrators.
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v := me(r); v == nil || !v.Admin {
			s.forbidden(w, r, "This page is for administrators.")
			return
		}
		h(w, r)
	}
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusForbidden)
	s.render(w, r, "message.html", page{Title: "Not allowed", D: map[string]any{"Heading": "Not allowed", "Text": msg}})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, "message.html", page{Title: "Not found", D: map[string]any{"Heading": "Not found",
		"Text": "That page doesn't exist, or you don't have access to it."}})
}

type page struct {
	Title  string
	Nav    string
	Msg    string
	Err    string
	Auth   bool
	V      *viewer
	Org    *store.Org // set on organization pages: shows the organization's tabs
	Role   string     // the viewer's role in Org
	Tab    string     // the active organization tab
	Status bot.Status
	D      any
}

// CanManage reports whether the viewer may change Org.
func (p page) CanManage() bool { return p.Role == "admin" || p.Role == store.RoleOwner }

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	q := r.URL.Query()
	if p.Msg == "" {
		p.Msg = q.Get("msg")
	}
	if p.Err == "" {
		p.Err = q.Get("err")
	}
	p.V = me(r)
	p.Auth = p.V != nil
	p.Status = s.bot.Status()
	if p.Org != nil && p.Role == "" {
		p.Role = p.V.Role(p.Org.ID)
	}
	if p.V != nil && !p.V.Admin && p.Status.CurrentRepo != "" && !s.viewerSeesRepo(p.V, p.Status.CurrentRepo) {
		// Don't reveal another organization's repositories.
		p.Status.Current, p.Status.CurrentRepo = "a review for another organization", ""
	}
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

// viewerSeesRepo reports whether the repository belongs to one of the viewer's organizations.
func (s *Server) viewerSeesRepo(v *viewer, fullName string) bool {
	rp, err := s.store.GetRepoByName(fullName)
	return err == nil && rp != nil && v.Role(rp.AccountID) != ""
}

func redirectMsg(w http.ResponseWriter, r *http.Request, to, msg string) {
	http.Redirect(w, r, withQuery(to, "msg", msg), http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, to string, err error) {
	http.Redirect(w, r, withQuery(to, "err", err.Error()), http.StatusSeeOther)
}

func withQuery(to, k, v string) string {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	return to + sep + k + "=" + url.QueryEscape(v)
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
	"lines":     func(ss []string) string { return strings.Join(ss, "\n") },
	"join":      strings.Join,
	"hasPrefix": strings.HasPrefix,
	"dur": func(a, b int64) string {
		if a == 0 || b == 0 {
			return ""
		}
		return (time.Duration(b-a) * time.Second).String()
	},
	"secs": func(n int) string {
		if n <= 0 {
			return "—"
		}
		return (time.Duration(n) * time.Second).String()
	},
	"div":      func(a, b int) int { return a / max(1, b) },
	"add":      func(a, b int) int { return a + b },
	"sub":      func(a, b int) int { return a - b },
	"num":      humanNum,
	"num64":    func(n int64) string { return humanNum(int(n)) },
	"orgPath":  func(login string) string { return "/orgs/" + url.PathEscape(login) },
	"dayChart": dayChart,
	"sevChart": sevChart,
	"query": func(base string, kv ...any) string {
		q := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			if v := fmt.Sprint(kv[i+1]); v != "" && v != "0" {
				q.Set(fmt.Sprint(kv[i]), v)
			}
		}
		if len(q) == 0 {
			return base
		}
		return base + "?" + q.Encode()
	},
}

// humanNum shortens large counts: 1234 -> 1.2k, 2500000 -> 2.5M.
func humanNum(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
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
