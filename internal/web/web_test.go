package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "correct horse battery") || checkPassword(h, "wrong") || checkPassword("garbage", "x") {
		t.Fatal("password check misbehaves")
	}
}

// TestPagesRender walks the UI as a browser would: setup, sign-in, every
// page, and a settings save, checking templates execute and CSRF holds.
func TestPagesRender(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(st, bot.New(st, dir, log), log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(path string, form url.Values, origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	get := func(path string) (int, string) {
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Before setup everything redirects to /setup.
	if _, body := get("/settings"); !strings.Contains(body, "Choose an admin password") {
		t.Fatal("expected setup page before a password exists")
	}
	if r := post("/setup", url.Values{"password": {"supersecret1"}, "confirm": {"supersecret1"}}, "http://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST allowed: %d", r.StatusCode)
	}
	post("/setup", url.Values{"password": {"supersecret1"}, "confirm": {"supersecret1"}}, ts.URL)

	for _, p := range []string{"/", "/prs", "/prs?all=1", "/repos", "/settings", "/skill", "/activity", "/account"} {
		code, body := get(p)
		if code != http.StatusOK || !strings.Contains(body, "PR Review Bot") {
			t.Errorf("GET %s = %d", p, code)
		}
	}
	if code, _ := get("/prs/acme/web/1"); code != http.StatusNotFound {
		t.Errorf("unknown PR = %d", code)
	}

	form := url.Values{
		"gh_app_id": {"42"}, "gh_api_base": {"https://api.github.com"}, "llm_provider": {"ollama"},
		"llm_base_url": {"http://localhost:11434"}, "llm_model": {"qwen2.5-coder:14b"}, "llm_context": {"16384"},
		"llm_max_output": {"2048"}, "rv_enabled": {"on"}, "rv_ignore": {"*.lock\nvendor/"}, "rv_min_severity": {"minor"},
	}
	post("/settings", form, ts.URL)
	got, _ := settings.Load(st)
	if got.GitHub.AppID != 42 || got.LLM.Provider != "ollama" || got.LLM.ContextTokens != 16384 ||
		len(got.Review.IgnorePatterns) != 2 || got.Replies.Enabled || got.Review.MinSeverity != "minor" {
		t.Errorf("settings not saved as submitted: %+v", got)
	}

	post("/skill/save", url.Values{"content": {"---\nname: team-review\ndescription: ours\n---\nBe strict."}}, ts.URL)
	if _, body := get("/skill"); !strings.Contains(body, "team-review") {
		t.Error("saved skill not shown")
	}

	// Signed-out clients are bounced to /login.
	c.Jar, _ = cookiejar.New(nil)
	if _, body := get("/settings"); !strings.Contains(body, `action="/login"`) {
		t.Error("expected login page when signed out")
	}
}
