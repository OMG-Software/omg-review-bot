package web

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
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

	for _, p := range []string{"/", "/prs", "/prs?all=1", "/repos", "/settings", "/skill", "/activity", "/account",
		"/orgs", "/reviews", "/reviews?status=posted&page=2", "/admin/users", "/admin/webhooks"} {
		code, body := get(p)
		if code != http.StatusOK || !strings.Contains(body, "OMG Review Bot") {
			t.Errorf("GET %s = %d", p, code)
		}
	}
	if code, _ := get("/prs/acme/web/1"); code != http.StatusNotFound {
		t.Errorf("unknown PR = %d", code)
	}

	form := url.Values{
		"gh_app_id": {"42"}, "gh_api_base": {"https://api.github.com"}, "llm_provider": {"ollama"},
		"llm_base_url": {"http://localhost:11434"}, "llm_model": {"qwen2.5-coder:14b"}, "llm_context": {"16384"},
		"llm_max_output": {"2048"}, "rv_enabled": {"on"}, "rv_ignore": {"*.lock\nvendor/"},
		"rv_comment_level": {"P4"}, "rv_approval_level": {"P5"}, // approval broader than comments: clamped to P4
		"rv_guidelines": {"CLAUDE.md\n/CONTRIBUTING.md"},
	}
	post("/settings", form, ts.URL)
	got, _ := settings.Load(st)
	if got.GitHub.AppID != 42 || got.LLM.Provider != "ollama" || got.LLM.ContextTokens != 16384 ||
		len(got.Review.IgnorePatterns) != 2 || got.Replies.Enabled || got.Review.CommentLevel != "P4" || got.Review.ApprovalLevel != "P4" ||
		len(got.Review.GuidelineFiles) != 2 || got.Review.GuidelineFiles[1] != "/CONTRIBUTING.md" {
		t.Errorf("settings not saved as submitted: %+v", got)
	}
	_, body := get("/settings")
	for _, want := range []string{`<option value="P4" selected>[P4] MINOR (Quality) and above</option>`,
		`<option value="off" >Never approve</option>`, `<b>[P1] CRITICAL</b> security hole`} {
		if strings.Count(body, want) < 1 {
			t.Errorf("settings page missing %q", want)
		}
	}
	if strings.Count(body, " selected>[P4] MINOR (Quality) and above") != 2 {
		t.Error("comment and approval selects should both show P4")
	}
	if _, body := get("/skill"); !strings.Contains(body, "No skill installed yet") || !strings.Contains(body, "github.com/OMG-Software/code-change-review-skill") {
		t.Error("skill page doesn't point to the recommended skill when none is installed")
	}

	post("/skill/save", url.Values{"content": {"---\nname: team-review\ndescription: ours\n---\nBe strict."}}, ts.URL)
	if _, body := get("/skill"); !strings.Contains(body, "team-review") {
		t.Error("saved skill not shown")
	}

	// Signed-out clients are bounced to /login, which sends them on to the
	// administrator's page while GitHub sign-in isn't set up.
	c.Jar, _ = cookiejar.New(nil)
	if _, body := get("/settings"); !strings.Contains(body, `action="/admin-login"`) || !strings.Contains(body, `name="next" value="/settings"`) {
		t.Error("expected the administrator sign-in page, keeping the page asked for")
	}
	if r := post("/admin-login", url.Values{"password": {"supersecret1"}}, ts.URL); r.StatusCode != http.StatusOK {
		t.Fatalf("admin sign-in = %d", r.StatusCode)
	}
	if code, _ := get("/settings"); code != http.StatusOK {
		t.Errorf("signed back in, /settings = %d", code)
	}
}

// TestSkillUpload uploads skill archives the way the Skill page's form (and
// its drag-and-drop) does.
func TestSkillUpload(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bot.New(st, dir, log)
	srv, err := New(st, b, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	form := func(path string, v url.Values) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	form("/setup", url.Values{"password": {"supersecret1"}, "confirm": {"supersecret1"}})

	// upload posts a file and returns the flash message (msg) or error (err).
	upload := func(name string, data []byte) (msg, errMsg string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write(data)
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/skill/upload", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		q := resp.Request.URL.Query() // after the redirect back to /skill
		return q.Get("msg"), q.Get("err")
	}
	zipOf := func(files map[string]string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, content := range files {
			w, _ := zw.Create(name)
			_, _ = io.WriteString(w, content)
		}
		_ = zw.Close()
		return buf.Bytes()
	}
	githubZip := zipOf(map[string]string{
		"code-change-review-skill-main/README.md":                                        "repo readme",
		"code-change-review-skill-main/skills/code-change-review/SKILL.md":               "---\nname: code-change-review\n---\nReview.",
		"code-change-review-skill-main/skills/code-change-review/references/a.md":        "A",
		"code-change-review-skill-main/skills/code-change-review/references/langs/go.md": "Go",
	})
	defaultDir := filepath.Join(dir, "skill")

	// 1) A GitHub "Download ZIP" installs into the bot's skill folder.
	msg, e := upload("code-change-review-skill-main.zip", githubZip)
	if e != "" || !strings.Contains(msg, `Installed skill "code-change-review"`) || !strings.Contains(msg, "2 reference files") {
		t.Fatalf("upload: msg=%q err=%q", msg, e)
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "references", "langs", "go.md")); err != nil {
		t.Errorf("reference file not deployed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "README.md")); err == nil {
		t.Error("files outside the skill folder were deployed")
	}

	// 2) Bad archives are refused and leave the installed skill alone.
	if _, e := upload("two.zip", zipOf(map[string]string{"a/SKILL.md": "a", "b/SKILL.md": "b"})); !strings.Contains(e, "2 skills") {
		t.Errorf("two-skill zip: err=%q", e)
	}
	if _, e := upload("notes.txt", []byte("hi")); !strings.Contains(e, "upload a .zip") {
		t.Errorf("wrong file type: err=%q", e)
	}
	if raw, _ := os.ReadFile(filepath.Join(defaultDir, "SKILL.md")); !strings.Contains(string(raw), "code-change-review") {
		t.Error("a refused upload changed the installed skill")
	}

	// 3) When Settings -> Skill folder points into a git clone, a zip is
	// installed into the bot's own folder instead and the clone is untouched.
	clone := filepath.Join(dir, "clone")
	cloneSkill := filepath.Join(clone, "skills", "mine")
	_ = os.MkdirAll(filepath.Join(clone, ".git"), 0o755)
	_ = os.MkdirAll(cloneSkill, 0o755)
	_ = os.WriteFile(filepath.Join(cloneSkill, "SKILL.md"), []byte("---\nname: mine\n---\nMine."), 0o644)
	cfg := settings.Defaults()
	cfg.Review.SkillDir = cloneSkill
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if _, e := upload("SKILL.md", []byte("---\nname: other\n---\nx")); !strings.Contains(e, "git clone") {
		t.Errorf("SKILL.md upload into a clone: err=%q", e)
	}
	msg, e = upload("skill.zip", zipOf(map[string]string{"new/SKILL.md": "---\nname: uploaded\n---\nNew."}))
	if e != "" || !strings.Contains(msg, `"uploaded"`) || !strings.Contains(msg, "left untouched") {
		t.Fatalf("zip upload with a clone: msg=%q err=%q", msg, e)
	}
	if raw, _ := os.ReadFile(filepath.Join(cloneSkill, "SKILL.md")); !strings.Contains(string(raw), "name: mine") {
		t.Error("the git clone was modified")
	}
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		t.Error("the clone's .git was removed")
	}
	if got, _ := settings.Load(st); got.Review.SkillDir != "" || b.SkillDir(got) != defaultDir {
		t.Errorf("skill folder setting = %q, want the bot's own folder", got.Review.SkillDir)
	}
}
