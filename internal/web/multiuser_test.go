package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

const (
	acmeID  = 500 // GitHub account ID of the "acme" organization
	otherID = 600 // another organization the test user must not see
	aliceID = 41
	bobID   = 42
)

// fakeGitHubWeb answers the OAuth and user endpoints sign-in uses, plus the
// user lookup behind "add member".
type fakeGitHubWeb struct {
	mu       sync.Mutex
	userID   int64 // who the next token belongs to
	login    string
	acmeRole string // alice's role in the acme GitHub org: admin | member | "" | forbidden (403)
}

func (f *fakeGitHubWeb) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		send := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch p := r.URL.Path; {
		case p == "/login/oauth/access_token":
			if r.FormValue("client_secret") != "shh" || r.FormValue("code") != "good-code" {
				send(map[string]any{"error": "bad_verification_code", "error_description": "bad code"})
				return
			}
			send(map[string]any{"access_token": "user-token"})
		case p == "/user":
			if r.Header.Get("Authorization") != "Bearer user-token" {
				t.Errorf("/user without the user token")
			}
			send(map[string]any{"id": f.userID, "login": f.login, "name": "Alice A", "avatar_url": "https://avatars.example/a.png"})
		case p == "/user/installations":
			send(map[string]any{"installations": []any{
				map[string]any{"id": 10, "account": map[string]any{"id": acmeID, "login": "acme", "type": "Organization"}},
			}})
		case p == "/user/memberships/orgs/acme":
			if f.acmeRole == "forbidden" { // the App lacks the Members permission
				w.WriteHeader(http.StatusForbidden)
				send(map[string]any{"message": "Resource not accessible by integration"})
				return
			}
			if f.acmeRole == "" {
				w.WriteHeader(http.StatusNotFound)
				send(map[string]any{"message": "Not Found"})
				return
			}
			send(map[string]any{"state": "active", "role": f.acmeRole})
		case p == "/app":
			send(map[string]any{"id": 1, "slug": "testbot", "name": "Test Bot", "html_url": "https://github.example/apps/testbot"})
		case p == "/app/installations/10/access_tokens":
			send(map[string]any{"token": "inst-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case p == "/users/bob":
			send(map[string]any{"id": bobID, "login": "bob", "type": "User"})
		case p == "/users/nobody":
			w.WriteHeader(http.StatusNotFound)
			send(map[string]any{"message": "Not Found"})
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, p)
			http.NotFound(w, r)
		}
	})
}

type testSite struct {
	t   *testing.T
	ts  *httptest.Server
	st  *store.Store
	b   *bot.Bot
	gh  *fakeGitHubWeb
	cl  *http.Client
	cfg settings.Settings
}

func newTestSite(t *testing.T) *testSite {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gh := &fakeGitHubWeb{userID: aliceID, login: "alice", acmeRole: "admin"}
	ghSrv := httptest.NewServer(gh.handler(t))
	t.Cleanup(ghSrv.Close)

	cfg := settings.Defaults()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKey = testPEM(t)
	cfg.GitHub.APIBaseURL = ghSrv.URL
	cfg.GitHub.ClientID = "Iv-test"
	cfg.GitHub.ClientSecret = "shh"
	cfg.GitHub.WebhookSecret = "hook-secret"
	cfg.LLM.Model = "m"
	if err := settings.Save(st, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(pwKey, "pbkdf2-sha256$1$AAAA$AAAA"); err != nil { // setup is done
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bot.New(st, dir, log)
	srv, err := New(st, b, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &testSite{t: t, ts: ts, st: st, b: b, gh: gh, cl: cl, cfg: cfg}
}

func testPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func (s *testSite) get(path string) (int, string, http.Header) {
	s.t.Helper()
	resp, err := s.cl.Get(s.ts.URL + path)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (s *testSite) post(path string, form url.Values) (int, http.Header) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.ts.URL)
	resp, err := s.cl.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// signIn runs the GitHub sign-in flow as the fake's current user.
func (s *testSite) signIn() {
	s.t.Helper()
	code, _, hdr := s.get("/auth/github?next=/orgs")
	if code != http.StatusSeeOther {
		s.t.Fatalf("/auth/github = %d", code)
	}
	loc, _ := url.Parse(hdr.Get("Location"))
	if loc.Path != "/login/oauth/authorize" || loc.Query().Get("client_id") != "Iv-test" ||
		loc.Query().Get("redirect_uri") != s.ts.URL+"/auth/github/callback" {
		s.t.Fatalf("authorize redirect = %s", loc)
	}
	state := loc.Query().Get("state")
	code, body, hdr := s.get("/auth/github/callback?code=good-code&state=" + url.QueryEscape(state))
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/orgs") {
		s.t.Fatalf("callback = %d %s %s", code, hdr.Get("Location"), body)
	}
}

// seedOrgs creates acme and another organization, each with a repository,
// a PR and a posted review.
func (s *testSite) seedOrgs() (acmeReview, otherReview int64) {
	s.t.Helper()
	st := s.st
	must := func(err error) {
		if err != nil {
			s.t.Fatal(err)
		}
	}
	must(st.UpsertOrg(acmeID, "acme", "Organization", 10))
	must(st.UpsertOrg(otherID, "other", "Organization", 11))
	must(st.UpsertRepo(100, 10, acmeID, "acme/web", true))
	must(st.UpsertRepo(200, 11, otherID, "other/secret", true))
	for _, p := range []store.PR{
		{Repo: "acme/web", Number: 7, AccountID: acmeID, Title: "Acme change", Open: true, Status: "reviewed"},
		{Repo: "other/secret", Number: 3, AccountID: otherID, Title: "Secret change", Open: true, Status: "reviewed"},
	} {
		must(st.SavePR(&p))
	}
	now := time.Now().Unix()
	add := func(repo string, n int, acct int64) int64 {
		id, err := st.CreateReview(&store.Review{Repo: repo, Number: n, AccountID: acct, Status: "running", StartedAt: now - 60})
		must(err)
		must(st.UpdateReview(&store.Review{ID: id, Status: "posted", Event: "COMMENT", Comments: 3, Notes: 1, Fixed: 1,
			Sev: [5]int{1, 1, 2, 0, 0}, Summary: "summary of " + repo, FinishedAt: now, Raw: "raw " + repo}))
		return id
	}
	return add("acme/web", 7, acmeID), add("other/secret", 3, otherID)
}

func TestGitHubSignInAndOrgAccess(t *testing.T) {
	s := newTestSite(t)
	acmeRev, otherRev := s.seedOrgs()

	// A forged state is refused.
	s.get("/auth/github")
	if code, body, _ := s.get("/auth/github/callback?code=good-code&state=forged"); code == http.StatusSeeOther || !strings.Contains(body, "didn&#39;t match") {
		t.Fatalf("forged state accepted: %d", code)
	}

	s.signIn()
	m, _ := s.st.GetMembership(acmeID, aliceID)
	if m == nil || m.Role != store.RoleOwner || m.Source != store.SourceGitHub {
		t.Fatalf("GitHub org admin should become owner, got %+v", m)
	}

	// Her organization: visible, with stats.
	code, body, _ := s.get("/orgs/acme")
	if code != 200 || !strings.Contains(body, "Reviews posted per day") || !strings.Contains(body, "acme/web") {
		t.Fatalf("org overview = %d", code)
	}
	for _, p := range []string{"/orgs", "/orgs/acme/prs", "/orgs/acme/reviews", "/orgs/acme/repos", "/orgs/acme/members",
		"/orgs/acme/settings", "/orgs/acme/activity", "/prs/acme/web/7", fmt.Sprintf("/reviews/%d", acmeRev), "/account"} {
		if code, _, _ := s.get(p); code != 200 {
			t.Errorf("GET %s = %d, want 200", p, code)
		}
	}
	if _, body, _ := s.get(fmt.Sprintf("/reviews/%d", acmeRev)); !strings.Contains(body, "raw acme/web") {
		t.Error("members should see a review's raw model output")
	}
	if _, body, _ := s.get("/"); strings.Contains(body, "other/secret") {
		t.Error("home page leaks another organization")
	}

	// Another organization: invisible, not even its existence.
	for _, p := range []string{"/orgs/other", "/orgs/other/reviews", "/prs/other/secret/3", fmt.Sprintf("/reviews/%d", otherRev)} {
		if code, body, _ := s.get(p); code != http.StatusNotFound || strings.Contains(body, "Secret change") {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
	if code, _ := s.post("/orgs/other/repos/200/toggle", url.Values{"enabled": {"0"}}); code != http.StatusNotFound {
		t.Errorf("toggling another org's repo = %d", code)
	}
	if code, _ := s.post("/orgs/acme/repos/200/toggle", url.Values{"enabled": {"0"}}); code != http.StatusNotFound {
		t.Errorf("toggling another org's repo through her own org = %d", code)
	}
	if rp, _ := s.st.GetRepo(200); !rp.Enabled {
		t.Fatal("another organization's repository was changed")
	}

	// Administrator pages are closed to her.
	for _, p := range []string{"/settings", "/skill", "/prs", "/reviews", "/repos", "/activity", "/admin/users", "/admin/webhooks"} {
		if code, _, _ := s.get(p); code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", p, code)
		}
	}
	if code, _ := s.post("/settings", url.Values{"llm_model": {"evil"}}); code != http.StatusForbidden {
		t.Errorf("POST /settings = %d", code)
	}

	// As owner she sets the org's own policy and adds a member.
	if code, _ := s.post("/orgs/acme/settings", url.Values{"rv_enabled": {"on"}, "rv_comment_level": {"P2"}, "rv_approval_level": {"P1"}}); code != http.StatusSeeOther {
		t.Fatalf("org settings = %d", code)
	}
	if got := s.b.SettingsFor(acmeID); got.Review.CommentLevel != "P2" || got.Review.ApprovalLevel != "P1" || got.LLM.Model != "m" {
		t.Errorf("org policy not applied: %+v", got.Review)
	}
	if s.b.SettingsFor(otherID).Review.CommentLevel != "P5" {
		t.Error("one org's policy leaked into another")
	}
	s.post("/orgs/acme/members", url.Values{"login": {"@Bob"}, "role": {"member"}})
	if m, _ := s.st.GetMembership(acmeID, bobID); m == nil || m.Role != store.RoleMember || m.Source != store.SourceManual {
		t.Fatalf("bob not added: %+v", m)
	}
	if _, hdr := s.post("/orgs/acme/members", url.Values{"login": {"nobody"}}); !strings.Contains(hdr.Get("Location"), "err=") {
		t.Error("adding an unknown login should fail")
	}
	// The last owner can't be removed or demoted.
	if _, hdr := s.post(fmt.Sprintf("/orgs/acme/members/%d/role", aliceID), url.Values{"role": {"member"}}); !strings.Contains(hdr.Get("Location"), "err=") {
		t.Error("demoting the last owner should fail")
	}

	// Bob signs in: a member can look but not manage.
	s.gh.mu.Lock()
	s.gh.userID, s.gh.login, s.gh.acmeRole = bobID, "bob", "member"
	s.gh.mu.Unlock()
	s.cl.Jar, _ = cookiejar.New(nil)
	s.signIn()
	if code, _, _ := s.get("/orgs/acme"); code != 200 {
		t.Fatalf("member can't see org: %d", code)
	}
	if m, _ := s.st.GetMembership(acmeID, bobID); m == nil || m.Source != store.SourceManual {
		t.Fatal("sign-in must not drop access added by hand")
	}
	for _, p := range []string{"/orgs/acme/settings", "/orgs/acme/repos/100/toggle", "/orgs/acme/members", "/prs/acme/web/7/skip"} {
		if code, _ := s.post(p, url.Values{"enabled": {"0"}, "login": {"x"}, "skip": {"1"}}); code != http.StatusForbidden {
			t.Errorf("member POST %s = %d, want 403", p, code)
		}
	}

	// Alice stops being a GitHub admin: her GitHub-derived access goes at next sign-in.
	s.gh.mu.Lock()
	s.gh.userID, s.gh.login, s.gh.acmeRole = aliceID, "alice", ""
	s.gh.mu.Unlock()
	s.cl.Jar, _ = cookiejar.New(nil)
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m != nil {
		t.Errorf("access derived from GitHub should be removed, got %+v", m)
	}

	// Blocking ends her session immediately.
	if err := s.st.SetUserDisabled(aliceID, true); err != nil {
		t.Fatal(err)
	}
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("blocked user still signed in: %d", code)
	}
}

func TestWebhookDelivery(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if err := s.st.SetRepoPolled(100, ""); err != nil {
		t.Fatal(err)
	}
	send := func(event, id string, payload any, secret string) (int, string) {
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/webhooks/github", strings.NewReader(string(body)))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", id)
		req.Header.Set("X-Hub-Signature-256", github.Sign(secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	pr := map[string]any{
		"action":     "opened",
		"repository": map[string]any{"id": 100, "full_name": "acme/web", "owner": map[string]any{"id": acmeID, "login": "acme"}},
		"pull_request": map[string]any{"number": 9, "title": "New", "state": "open", "updated_at": "2026-11-20T10:00:00Z",
			"user": map[string]any{"login": "carol"}, "head": map[string]any{"ref": "f", "sha": "abc123"}, "base": map[string]any{"ref": "main", "sha": "def"}},
	}
	if code, _ := send("pull_request", "d1", pr, "wrong-secret"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d", code)
	}
	if p, _ := s.st.GetPR("acme/web", 9); p != nil {
		t.Fatal("unsigned delivery changed state")
	}
	code, body := send("pull_request", "d2", pr, "hook-secret")
	if code != 200 || !strings.Contains(body, "pending") {
		t.Fatalf("pull_request delivery = %d %s", code, body)
	}
	p, _ := s.st.GetPR("acme/web", 9)
	if p == nil || p.Status != "pending" || p.HeadSHA != "abc123" || p.AccountID != acmeID {
		t.Fatalf("PR from webhook = %+v", p)
	}
	if _, body := send("pull_request", "d2", pr, "hook-secret"); !strings.Contains(body, "duplicate") {
		t.Errorf("redelivery not deduplicated: %s", body)
	}
	pr["action"], pr["pull_request"].(map[string]any)["state"] = "closed", "closed"
	send("pull_request", "d3", pr, "hook-secret")
	if p, _ := s.st.GetPR("acme/web", 9); p.Open || p.Status != "closed" {
		t.Errorf("closed PR = %+v", p)
	}
	if ds, _ := s.st.RecentDeliveries(10); len(ds) != 2 || ds[0].AccountID != acmeID || ds[0].Result == "" {
		t.Errorf("deliveries = %+v", ds)
	}
	if !s.b.Status().WebhooksActive {
		t.Error("status should show webhooks as active")
	}

	// Without a secret, webhooks are refused outright.
	cfg := s.cfg
	cfg.GitHub.WebhookSecret = ""
	_ = settings.Save(s.st, cfg)
	if code, _ := send("ping", "d4", map[string]any{}, "hook-secret"); code != http.StatusNotFound {
		t.Errorf("webhook with no secret configured = %d", code)
	}
}

// TestAdminLoginIsSeparate checks the GitHub sign-in page carries no trace of
// the administrator password, which has its own page at /admin-login.
func TestAdminLoginIsSeparate(t *testing.T) {
	s := newTestSite(t)
	h, err := hashPassword("admin-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.Set(pwKey, h); err != nil {
		t.Fatal(err)
	}

	code, body, _ := s.get("/login")
	if code != http.StatusOK || !strings.Contains(body, "Sign in with GitHub") {
		t.Fatalf("/login = %d", code)
	}
	for _, leak := range []string{"admin-login", `type="password"`, "Administrator password"} {
		if strings.Contains(body, leak) {
			t.Errorf("/login mentions %q", leak)
		}
	}
	if code, _ := s.post("/login", url.Values{"password": {"admin-password-1"}}); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /login = %d, want 405", code)
	}

	if code, body, _ := s.get("/admin-login?next=/settings"); code != http.StatusOK || !strings.Contains(body, `action="/admin-login"`) {
		t.Fatalf("/admin-login = %d", code)
	}
	if code, _ := s.post("/admin-login", url.Values{"password": {"wrong"}}); code != http.StatusOK {
		t.Errorf("wrong password = %d, want the form again", code)
	}
	if code, _, _ := s.get("/settings"); code != http.StatusSeeOther {
		t.Error("wrong password signed in")
	}
	code, hdr := s.post("/admin-login", url.Values{"password": {"admin-password-1"}, "next": {"/settings"}})
	if code != http.StatusSeeOther || hdr.Get("Location") != "/settings" {
		t.Fatalf("admin sign-in = %d %s", code, hdr.Get("Location"))
	}
	if code, _, _ := s.get("/settings"); code != http.StatusOK {
		t.Errorf("admin /settings = %d", code)
	}
	// Signing out returns the administrator to their own page.
	if _, hdr := s.post("/logout", nil); hdr.Get("Location") != "/admin-login" {
		t.Errorf("admin sign-out went to %q", hdr.Get("Location"))
	}

	// A GitHub user signing out goes back to the GitHub page.
	s.signIn()
	if _, hdr := s.post("/logout", nil); hdr.Get("Location") != "/login" {
		t.Errorf("GitHub sign-out went to %q", hdr.Get("Location"))
	}
}

// TestSecureCookies checks cookies are marked Secure when the request came
// over HTTPS through the configured Public URL, and only then.
func TestSecureCookies(t *testing.T) {
	s := newTestSite(t)
	cookieFor := func(host string) *http.Cookie {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, s.ts.URL+"/auth/github", nil)
		req.Host = host
		resp, err := s.cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == oauthCookie {
				return c
			}
		}
		t.Fatalf("no %s cookie for host %s", oauthCookie, host)
		return nil
	}
	direct := strings.TrimPrefix(s.ts.URL, "http://")

	if c := cookieFor(direct); c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("no Public URL: cookie = %+v, want HttpOnly, Lax, not Secure", c)
	}
	s.cfg.GitHub.PublicURL = "https://bot.example"
	if err := settings.Save(s.st, s.cfg); err != nil {
		t.Fatal(err)
	}
	if c := cookieFor("bot.example"); !c.Secure {
		t.Error("request through the https Public URL: cookie not Secure")
	}
	if c := cookieFor(direct); c.Secure {
		t.Error("direct plain-HTTP request: cookie marked Secure, which the browser would drop")
	}
}

// TestResetPasswordEndsSessions: after `prbot -reset-password`, setting the
// new password on the setup page signs everyone out, as a password change does.
func TestResetPasswordEndsSessions(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Fatalf("signed in: /orgs = %d", code)
	}
	if err := s.st.Delete(pwKey); err != nil { // what -reset-password does
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/setup",
		strings.NewReader(url.Values{"password": {"new-password-1"}, "confirm": {"new-password-1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.ts.URL)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup = %d", resp.StatusCode)
	}
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("session from before the reset still works: %d %s", code, hdr.Get("Location"))
	}
}

// TestSafeNext: post-sign-in redirects stay on this site.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/orgs/acme":             "/orgs/acme",
		"/reviews?status=posted": "/reviews?status=posted",
		"":                       "/",
		"https://evil.example":   "/",
		"//evil.example":         "/",
		`/\evil.example`:         "/",
		`/./\evil.example`:       "/",
		"/\t/evil.example":       "/",
		"/\n/evil.example":       "/",
		"/\r/evil.example":       "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnverifiedGitHubAccessIsPaused: when GitHub won't say what someone's
// role is, access that came from GitHub is removed rather than kept.
func TestUnverifiedGitHubAccessIsPaused(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice, a GitHub admin of acme: owner
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Role != store.RoleOwner {
		t.Fatalf("alice should own acme, got %+v", m)
	}
	s.gh.mu.Lock()
	s.gh.acmeRole = "forbidden"
	s.gh.mu.Unlock()
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m != nil {
		t.Errorf("unverified GitHub-derived access kept: %+v", m)
	}
}

// TestAddFormDoesntChangeRoles: adding someone who already has access is
// refused, so the form can't demote the last owner.
func TestAddFormDoesntChangeRoles(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme
	if _, hdr := s.post("/orgs/acme/members", url.Values{"login": {"bob"}, "role": {"member"}}); strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("adding bob failed: %s", hdr.Get("Location"))
	}
	_, hdr := s.post("/orgs/acme/members", url.Values{"login": {"bob"}, "role": {"owner"}})
	if !strings.Contains(hdr.Get("Location"), "err=") {
		t.Errorf("re-adding bob as owner should be refused, went to %s", hdr.Get("Location"))
	}
	if m, _ := s.st.GetMembership(acmeID, bobID); m == nil || m.Role != store.RoleMember {
		t.Errorf("bob's role changed through the add form: %+v", m)
	}
}
