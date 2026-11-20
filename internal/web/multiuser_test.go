package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	mu     sync.Mutex
	userID int64 // who the next token belongs to
	// noInstallations: the App isn't installed anywhere the person can see
	noInstallations bool
	failUser        bool // reading the profile fails, naming an internal address
	login           string
	acmeRole        string // alice's role in the acme GitHub org: admin | member | "" | forbidden (403)
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
		case p == "/user" && f.failUser:
			w.WriteHeader(http.StatusBadGateway)
			send(map[string]any{"message": "dial tcp 10.1.2.3:443: connect: connection refused"})
		case p == "/user":
			if r.Header.Get("Authorization") != "Bearer user-token" {
				t.Errorf("/user without the user token")
			}
			send(map[string]any{"id": f.userID, "login": f.login, "name": "Alice A", "avatar_url": "https://avatars.example/a.png"})
		case p == "/user/installations" && f.noInstallations:
			send(map[string]any{"installations": []any{}})
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
	srv *Server
	// serving is the server the test server hands requests to, so a test
	// can restart the bot without touching the running test server.
	serving atomic.Pointer[Server]
	db      string // the database file
	gh      *fakeGitHubWeb
	cl      *http.Client
	cfg     settings.Settings
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
	s := &testSite{t: t, st: st, b: b, srv: srv, db: filepath.Join(dir, "t.db"), gh: gh, cfg: cfg}
	s.serving.Store(srv)
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serving.Load().ServeHTTP(w, r) }))
	t.Cleanup(s.ts.Close)
	s.cl = s.newClient()
	return s
}

// restart hands requests to srv from now on, as a restarted bot would get
// them; the address and the browser's cookies stay.
func (s *testSite) restart(srv *Server) { s.serving.Store(srv) }

// cleanStop records that the server stopped cleanly, as Close does when no
// sessions are left to end, without waiting for retries to notice they're
// done.
func (s *testSite) cleanStop() {
	s.t.Helper()
	if err := os.Remove(s.srv.runMarker); err != nil {
		s.t.Fatal(err)
	}
}

// newClient is another browser: cookies of its own, redirects not followed.
func (s *testSite) newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// startSignIn and finishSignIn are trySignIn's two steps in browser cl,
// returning errors rather than failing the test, so other goroutines can
// use them.
func (s *testSite) startSignIn(cl *http.Client) (string, error) {
	resp, err := cl.Get(s.ts.URL + "/auth/github?next=/orgs")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return "", err
	}
	return loc.Query().Get("state"), nil
}

func (s *testSite) finishSignIn(cl *http.Client, state string) (int, error) {
	resp, err := cl.Get(s.ts.URL + "/auth/github/callback?code=good-code&state=" + url.QueryEscape(state))
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
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
	code, body, hdr := s.trySignIn()
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/orgs") {
		s.t.Fatalf("callback = %d %s %s", code, hdr.Get("Location"), body)
	}
}

// trySignIn goes through GitHub sign-in and returns the callback's response.
func (s *testSite) trySignIn() (int, string, http.Header) {
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
	return s.get("/auth/github/callback?code=good-code&state=" + url.QueryEscape(state))
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
	// An organization with no reviews yet says so, not "last review —".
	if err := s.st.UpsertOrg(700, "fresh", "Organization", 12); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.AddMember(700, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := s.get("/orgs"); !strings.Contains(body, "no reviews yet") || !strings.Contains(body, "last review") {
		t.Error("/orgs should say a new organization has no reviews yet, and give the others' last review")
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
	sendRaw := func(event, id string, body []byte, secret string) (int, string) {
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
	send := func(event, id string, payload any, secret string) (int, string) {
		body, _ := json.Marshal(payload)
		return sendRaw(event, id, body, secret)
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

	// A delivery that failed is handled again when it's redelivered (GitHub
	// keeps its ID); one that succeeded is still a duplicate.
	if code, _ := sendRaw("ping", "d5", []byte(`{"zen":`), "hook-secret"); code != http.StatusBadRequest {
		t.Fatalf("unreadable payload = %d, want 400", code)
	}
	if code, body := send("ping", "d5", map[string]any{"zen": "hi"}, "hook-secret"); code != 200 || !strings.Contains(body, "pong") {
		t.Errorf("redelivery of a failed delivery = %d %s, want it handled", code, body)
	}
	if _, body := send("ping", "d5", map[string]any{"zen": "hi"}, "hook-secret"); !strings.Contains(body, "duplicate") {
		t.Errorf("redelivery after it succeeded not deduplicated: %s", body)
	}

	// Payloads are read before the signature is checked, so the size is capped.
	big, _ := json.Marshal(map[string]any{"zen": strings.Repeat("x", 6<<20)})
	if code, _ := sendRaw("ping", "d6", big, "hook-secret"); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized payload = %d, want 413", code)
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
	// The page to return to is encoded into the sign-in link, so an "&" in it
	// doesn't split off into a parameter of its own.
	_, body, _ = s.get("/login?next=" + url.QueryEscape("/reviews?status=posted&page=2"))
	if want := `href="/auth/github?next=%2freviews%3fstatus%3dposted%26page%3d2"`; !strings.Contains(body, want) {
		t.Errorf("/login sign-in link isn't encoded; want %s", want)
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
// over HTTPS through the configured Public URL, and only then; and that
// GitHub sign-in starts on the Public URL.
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
	// Started on another address, sign-in moves to the Public URL first: GitHub
	// returns there, and the state cookie must have been set there.
	req, _ := http.NewRequest(http.MethodGet, s.ts.URL+"/auth/github?next=/orgs", nil)
	resp, err := s.cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "https://bot.example/auth/github?next=%2Forgs&hop=1" {
		t.Errorf("sign-in started on %s = %d to %q, want the Public URL", direct, resp.StatusCode, loc)
	}
	for _, c := range resp.Cookies() {
		if c.Name == oauthCookie {
			t.Errorf("state cookie set on %s, which GitHub won't return to", direct)
		}
	}
	// Arrived from that redirect, it goes on to GitHub even if the Host
	// doesn't match (a proxy that passes on its own): no redirect loop.
	req, _ = http.NewRequest(http.MethodGet, s.ts.URL+"/auth/github?next=/orgs&hop=1", nil)
	if resp, err = s.cl.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Path != "/login/oauth/authorize" {
		t.Errorf("sign-in after the redirect to the Public URL = %d to %q, want GitHub", resp.StatusCode, resp.Header.Get("Location"))
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

// TestFailedAccessUpdateEndsSessions: when sign-in can't update someone's
// access, their existing sessions stop working, even if ending them in the
// database fails too; the next sign-in that updates it lets them back in.
func TestFailedAccessUpdateEndsSessions(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Fatalf("signed in: /orgs = %d", code)
	}
	syncAccess, endSessions := s.srv.syncAccess, s.srv.endSessions
	s.srv.syncAccess = func(context.Context, *github.UserClient, *store.User) ([]string, error) {
		return nil, errors.New("database is locked")
	}
	s.srv.endSessions = func(int64) error { return errors.New("database is locked") }
	// The failed sign-in leaves the browser's earlier session cookie in place.
	if code, body, _ := s.trySignIn(); code != http.StatusOK || !strings.Contains(body, "Couldn&#39;t update your access") {
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("earlier session still works after a failed access update: %d %s", code, hdr.Get("Location"))
	}
	s.srv.syncAccess, s.srv.endSessions = syncAccess, endSessions
	s.signIn()
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Errorf("signed in again after a successful update: /orgs = %d", code)
	}
}

// TestAllowListPausesAccess: an organization the "only these accounts" setting
// excludes keeps the GitHub-derived access people already have, and GitHub
// withdrawing it still removes it, but nobody gains new access to it.
func TestAllowListPausesAccess(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if err := s.st.RemoveMembership(acmeID, aliceID); err != nil { // only what GitHub gives
		t.Fatal(err)
	}
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Source != store.SourceGitHub {
		t.Fatalf("alice's access from GitHub = %+v", m)
	}
	s.cfg.GitHub.Accounts = []string{"other"} // excludes acme
	if err := settings.Save(s.st, s.cfg); err != nil {
		t.Fatal(err)
	}
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil {
		t.Error("access to an organization the allow-list excludes was removed at sign-in")
	}
	s.gh.mu.Lock()
	s.gh.acmeRole = "forbidden"
	s.gh.mu.Unlock()
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m != nil {
		t.Errorf("access GitHub no longer confirms was kept because the organization is excluded: %+v", m)
	}
	s.gh.mu.Lock()
	s.gh.acmeRole = "admin"
	s.gh.mu.Unlock()
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m != nil {
		t.Errorf("new access given to an organization the allow-list excludes: %+v", m)
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
	// Nor does signing in change a role an owner set by hand, even for a
	// GitHub admin: an owner role from GitHub must be stored as such, so it
	// goes when GitHub takes it back.
	if err := s.st.SetMemberRole(acmeID, bobID, store.RoleOwner); err != nil { // so acme keeps an owner
		t.Fatal(err)
	}
	if err := s.st.SetMemberRole(acmeID, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	s.signIn() // alice is a GitHub admin of acme
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Role != store.RoleMember || m.Source != store.SourceManual {
		t.Errorf("sign-in changed a role set by hand: %+v", m)
	}
}

// TestMemberIDMustBeValid: a zero or malformed member ID matches nobody,
// rather than the organization's first member.
func TestMemberIDMustBeValid(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme
	s.post("/orgs/acme/members", url.Values{"login": {"bob"}, "role": {"owner"}})
	for _, p := range []string{"/orgs/acme/members/0/remove", "/orgs/acme/members/x/remove", "/orgs/acme/members/-1/role"} {
		if code, _ := s.post(p, url.Values{"role": {"member"}}); code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, code)
		}
	}
	for _, id := range []int64{aliceID, bobID} {
		if m, _ := s.st.GetMembership(acmeID, id); m == nil || m.Role != store.RoleOwner {
			t.Errorf("member %d changed: %+v", id, m)
		}
	}
	if m, _ := s.st.GetMembership(acmeID, 0); m != nil {
		t.Errorf("GetMembership(org, 0) = %+v, want nil", m)
	}
}

// TestOrgEventsInOrgActivity: changes an owner makes to their organization
// show on its own activity page, not only on the administrator's.
func TestOrgEventsInOrgActivity(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme
	s.post("/orgs/acme/members", url.Values{"login": {"bob"}, "role": {"member"}})
	if code, body, _ := s.get("/orgs/acme/activity"); code != http.StatusOK || !strings.Contains(body, "added @bob to acme") {
		t.Errorf("acme's activity page (%d) doesn't show bob being added", code)
	}
}

// TestEndingSessionsRetried: when sign-in can't update someone's access and
// ending their sessions in the database fails too, it's retried until it's
// stored, so their earlier sessions stay ended after a restart (which loses
// the in-memory block). Their access is left as it was.
func TestEndingSessionsRetried(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	before, _ := s.st.GetMembership(acmeID, aliceID)
	syncAccess, endSessions := s.srv.syncAccess, s.srv.endSessions
	var mu sync.Mutex
	calls := 0
	s.srv.retryWait = 5 * time.Millisecond
	s.srv.syncAccess = func(context.Context, *github.UserClient, *store.User) ([]string, error) {
		return nil, errors.New("database is locked")
	}
	s.srv.endSessions = func(id int64) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			return errors.New("database is locked")
		}
		return endSessions(id)
	}
	if code, _, _ := s.trySignIn(); code != http.StatusOK {
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, busy := s.srv.retrying.Load(int64(aliceID)); !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ending sessions was never stored")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 3 {
		t.Errorf("tried to end sessions %d times, want 3 (two failures, then stored)", n)
	}
	// A restart: the in-memory block is gone, the stored one isn't.
	s.srv.unreconciled.Delete(int64(aliceID))
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("earlier session works again after a restart: %d %s", code, hdr.Get("Location"))
	}
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || before == nil || m.Role != before.Role || m.Source != before.Source {
		t.Errorf("access changed by a failed sign-in: %+v, was %+v", m, before)
	}
	s.srv.syncAccess, s.srv.endSessions = syncAccess, endSessions
}

// TestFailedOrgWriteAtSignIn: when sign-in can't record a newly installed
// organization, the person isn't signed in as if they had no access to it;
// they're told it failed, and the next sign-in that can record it gives them
// their access.
func TestFailedOrgWriteAtSignIn(t *testing.T) {
	s := newTestSite(t) // acme isn't recorded yet
	db, err := sql.Open("sqlite", s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_orgs BEFORE INSERT ON orgs BEGIN SELECT RAISE(ABORT, 'database is locked'); END`); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := s.trySignIn(); code != http.StatusOK || !strings.Contains(body, "Couldn&#39;t update your access") {
		t.Errorf("sign-in that couldn't record the organization = %d; want it refused with an explanation", code)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_orgs`); err != nil {
		t.Fatal(err)
	}
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Role != store.RoleOwner {
		t.Errorf("access after a sign-in that could record the organization = %+v; want owner", m)
	}
}

// TestStatusHidesInstanceErrors: the status a member gets, in pages and from
// /api/status alike, has none of the instance's configuration problems,
// which an administrator does see.
func TestStatusHidesInstanceErrors(t *testing.T) {
	s := newTestSite(t)
	s.cfg.LLM.Model = "" // a configuration problem
	if err := settings.Save(s.st, s.cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.b.Run(ctx) // one poll, which records the configuration problems
	if got := s.srv.statusFor(&viewer{ID: aliceID, Admin: true}); len(got.ConfigIssues) == 0 {
		t.Fatal("an administrator's status shows no configuration problem; the test needs one")
	}
	for _, v := range []*viewer{{ID: aliceID}, nil} {
		if got := s.srv.statusFor(v); got.LastPollErr != "" || got.ConfigIssues != nil {
			t.Errorf("status for %+v = %+v; want no poll error or configuration problems", v, got)
		}
	}
}

// TestUninstalledOrgKeepsAccess: an organization the App was uninstalled
// from isn't among the person's installations any more, but their access to
// it, and so to its history, is kept.
func TestUninstalledOrgKeepsAccess(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if err := s.st.RemoveMembership(acmeID, aliceID); err != nil { // only what GitHub gives
		t.Fatal(err)
	}
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Source != store.SourceGitHub {
		t.Fatalf("alice's access from GitHub = %+v", m)
	}
	if err := s.st.DeactivateOrgsExcept(nil); err != nil { // the App was uninstalled everywhere
		t.Fatal(err)
	}
	s.gh.mu.Lock()
	s.gh.noInstallations = true
	s.gh.mu.Unlock()
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Role != store.RoleOwner {
		t.Errorf("access to an uninstalled organization after sign-in = %+v; want it kept", m)
	}
	if code, _, _ := s.get("/orgs/acme"); code != http.StatusOK {
		t.Errorf("an uninstalled organization's page = %d; want its history shown", code)
	}
}

// TestPublicURLChecked: a bare host gets https://, and a Public URL that
// isn't a full address is refused when settings are saved.
func TestPublicURLChecked(t *testing.T) {
	s := newTestSite(t)
	v := s.cfg
	v.GitHub.PublicURL = "prbot.example.com/"
	v.Normalize()
	if v.GitHub.PublicURL != "https://prbot.example.com" {
		t.Errorf("bare host normalized to %q", v.GitHub.PublicURL)
	}
	for _, bad := range []string{"ftp://prbot.example.com", "https://", "https://prbot.example.com/?x=1", "https://example.com/bot"} {
		if settings.CheckPublicURL(bad) == nil {
			t.Errorf("CheckPublicURL(%q) accepted it", bad)
		}
	}
	for _, ok := range []string{"", "https://prbot.example.com", "http://10.0.0.5:8080"} {
		if err := settings.CheckPublicURL(ok); err != nil {
			t.Errorf("CheckPublicURL(%q) = %v", ok, err)
		}
	}
}

// TestPendingSessionEndsSurviveRestart: when sign-in can't update someone's
// access and ending their sessions in the database keeps failing, a restart
// before any retry gets through still doesn't let their earlier session back
// in: the bot noted it on disk, and the restarted bot, whose database works
// again, ends it. Their access is left as it was.
func TestPendingSessionEndsSurviveRestart(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	before, _ := s.st.GetMembership(acmeID, aliceID)
	s.srv.retryWait = time.Hour // no retry gets through before the restart
	s.srv.syncAccess = func(context.Context, *github.UserClient, *store.User) ([]string, error) {
		return nil, errors.New("database is locked")
	}
	s.srv.endSessions = func(int64) error { return errors.New("database is locked") }
	if code, _, _ := s.trySignIn(); code != http.StatusOK {
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	if ids, _ := s.srv.pending.load(); len(ids) != 1 || ids[0] != aliceID {
		t.Fatalf("sessions noted to end = %v, want alice's", ids)
	}

	// Restart: a new server on the same database and data directory, which
	// works again. The old one's in-memory block goes with it.
	s.srv.unreconciled.Delete(int64(aliceID)) // and its retry stops
	s.cleanStop()                             // only the note keeps them out, not an unclean stop
	srv2, err := New(s.st, s.b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.restart(srv2)
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("earlier session works again after a restart: %d %s", code, hdr.Get("Location"))
	}
	if ids, _ := srv2.pending.load(); len(ids) != 0 {
		t.Errorf("sessions still noted to end after the restarted bot ended them: %v", ids)
	}
	if u, _ := s.st.GetUser(aliceID); u == nil || u.Epoch == 0 {
		t.Errorf("alice's sessions weren't ended in the database: %+v", u)
	}
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || before == nil || m.Role != before.Role || m.Source != before.Source {
		t.Errorf("access changed: %+v, was %+v", m, before)
	}
	s.signIn() // and a sign-in that works lets her back in
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Errorf("signed in again after the restart: /orgs = %d", code)
	}
}

// TestPendingNoteRetried: when ending the sessions in the database and
// noting them on disk both fail, the note is retried with the database
// write, so once it's stored a restart still keeps the old session out.
func TestPendingNoteRetried(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	before, _ := s.st.GetMembership(acmeID, aliceID)
	s.srv.retryWait = 5 * time.Millisecond
	s.srv.syncAccess = func(context.Context, *github.UserClient, *store.User) ([]string, error) {
		return nil, errors.New("database is locked")
	}
	s.srv.endSessions = func(int64) error { return errors.New("database is locked") }
	// The note can't be written: a directory is in the way.
	if err := os.MkdirAll(s.srv.pending.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := s.trySignIn(); code != http.StatusOK {
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	if ids, _ := s.srv.pending.load(); len(ids) != 0 {
		t.Fatalf("noted %v with a directory in the way", ids)
	}
	if err := os.Remove(s.srv.pending.path); err != nil { // the disk recovers
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ids, _ := s.srv.pending.load(); len(ids) == 1 && ids[0] == aliceID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the note was never retried")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.srv.unreconciled.Delete(int64(aliceID)) // restart; the old retry stops
	s.cleanStop()                             // only the note keeps them out, not an unclean stop
	srv2, err := New(s.st, s.b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.restart(srv2)
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("earlier session works again after a restart: %d %s", code, hdr.Get("Location"))
	}
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || before == nil || m.Role != before.Role || m.Source != before.Source {
		t.Errorf("access changed: %+v, was %+v", m, before)
	}
}

// TestRetryNotesLaterFailure: a retry started by an earlier failure, whose
// note a sign-in that got through has since cleared, writes the note of a
// later failure that couldn't write its own, once the disk recovers. That
// later failure doesn't start a retry of its own: this one is still running.
func TestRetryNotesLaterFailure(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	s.srv.retryWait = 300 * time.Millisecond // the sign-ins below come first
	s.srv.endSessions = func(int64) error { return errors.New("database is locked") }
	real := s.srv.syncAccess
	var failSync atomic.Bool
	s.srv.syncAccess = func(ctx context.Context, uc *github.UserClient, u *store.User) ([]string, error) {
		if failSync.Load() {
			return nil, errors.New("database is locked")
		}
		return real(ctx, uc, u)
	}
	failSync.Store(true)
	if code, _, _ := s.trySignIn(); code != http.StatusOK { // noted; a retry starts
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	if ids, _ := s.srv.pending.load(); len(ids) != 1 {
		t.Fatalf("first failure noted %v", ids)
	}
	failSync.Store(false)
	s.signIn() // gets through: the note is cleared
	if ids, _ := s.srv.pending.load(); len(ids) != 0 {
		t.Fatalf("note after a sign-in got through = %v", ids)
	}
	// The next failure can't write its note: a directory is in the way.
	if err := os.MkdirAll(s.srv.pending.path, 0o700); err != nil {
		t.Fatal(err)
	}
	failSync.Store(true)
	if code, _, _ := s.trySignIn(); code != http.StatusOK {
		t.Fatalf("second sign-in with a failed access update = %d", code)
	}
	if err := os.Remove(s.srv.pending.path); err != nil { // the disk recovers
		t.Fatal(err)
	}
	if _, busy := s.srv.retrying.Load(int64(aliceID)); !busy {
		t.Fatal("the first failure's retry stopped before the second failure; the test needs it running")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ids, _ := s.srv.pending.load(); len(ids) == 1 && ids[0] == aliceID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the later failure was never noted on disk")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.srv.unreconciled.Delete(int64(aliceID)) // let the retry stop
}

// TestUnreadablePendingNoteEndsEverySession: if the note of sessions to end
// can't be read at startup, nobody knows whose they are, so every session
// ends; the file is set aside, and access is left as it was.
func TestUnreadablePendingNoteEndsEverySession(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	before, _ := s.st.GetMembership(acmeID, aliceID)
	if err := os.WriteFile(s.srv.pending.path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cleanStop()                                                               // only the note keeps them out, not an unclean stop
	srv2, err := New(s.st, s.b, slog.New(slog.NewTextHandler(io.Discard, nil))) // restart
	if err != nil {
		t.Fatalf("restart with an unreadable note: %v", err)
	}
	s.restart(srv2)
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("a session survived a restart with an unreadable note: %d %s", code, hdr.Get("Location"))
	}
	if _, err := os.Stat(s.srv.pending.path + ".unreadable"); err != nil {
		t.Errorf("the unreadable note wasn't set aside: %v", err)
	}
	if ids, err := srv2.pending.load(); err != nil || len(ids) != 0 {
		t.Errorf("note after setting it aside = %v, %v", ids, err)
	}
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || before == nil || m.Role != before.Role || m.Source != before.Source {
		t.Errorf("access changed: %+v, was %+v", m, before)
	}
	s.signIn()
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Errorf("signed in again: /orgs = %d", code)
	}
}

// TestEarlierRepoHistoryNotLinked: reviews kept under an earlier
// repository's name show without a link to a PR page that doesn't exist.
func TestEarlierRepoHistoryNotLinked(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if _, err := s.st.CreateReview(&store.Review{Repo: "acme/web" + store.EarlierRepoSuffix, Number: 7, AccountID: acmeID,
		Status: "posted", StartedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	s.signIn()
	code, body, _ := s.get("/orgs/acme/reviews")
	if code != http.StatusOK {
		t.Fatalf("acme's reviews = %d", code)
	}
	if !strings.Contains(body, "acme/web (earlier repository)#7") {
		t.Error("the earlier repository's review isn't listed")
	}
	if !strings.Contains(body, `<option value="acme/web (earlier repository)"`) {
		t.Error("the repository filter doesn't offer the earlier repository")
	}
	if code, body, _ := s.get("/orgs/acme/reviews?repo=" + url.QueryEscape("acme/web"+store.EarlierRepoSuffix)); code != http.StatusOK ||
		!strings.Contains(body, "acme/web (earlier repository)#7") || strings.Contains(body, ">acme/web#7<") {
		t.Errorf("filtered by the earlier repository = %d; want only its review", code)
	}
	if strings.Contains(body, `href="/prs/acme/web%20%28earlier`) || strings.Contains(body, `href="/prs/acme/web (earlier`) {
		t.Error("the earlier repository's review links to a PR page")
	}
}

// TestEarlierOrgPageReachable: an organization whose login another account
// has since taken keeps its own page, under its new name.
func TestEarlierOrgPageReachable(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if err := s.st.UpsertOrg(900, "other", "Organization", 30); err != nil { // takes "other"'s login
		t.Fatal(err)
	}
	if _, err := s.st.AddMember(otherID, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	s.signIn()
	old, _ := s.st.GetOrg(otherID)
	code, body, _ := s.get("/orgs/" + url.PathEscape(old.Login))
	if code != http.StatusOK || !strings.Contains(body, "other/secret") {
		t.Errorf("earlier organization's page (%s) = %d; want its own history", old.Login, code)
	}
	code, body, _ = s.get("/orgs")
	if code != http.StatusOK || !strings.Contains(body, `/orgs/other%20%28earlier%20account`) {
		t.Errorf("/orgs doesn't link to the earlier organization by its new name")
	}
}

// TestGitHubDecidesLastOwner: for access that came from GitHub, GitHub is the
// source of truth. An org's only owner who stops being a GitHub admin of it
// loses the owner role at their next sign-in, even though that leaves the
// org with no owner (the last-owner guard only stops changes made by hand),
// and gets it back once GitHub makes them an admin again.
func TestGitHubDecidesLastOwner(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if err := s.st.RemoveMembership(acmeID, aliceID); err != nil { // only what GitHub gives
		t.Fatal(err)
	}
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Role != store.RoleOwner || m.Source != store.SourceGitHub {
		t.Fatalf("alice from GitHub = %+v; want acme's owner", m)
	}
	s.gh.mu.Lock()
	s.gh.acmeRole = "member" // no longer a GitHub admin, and acme doesn't let members in
	s.gh.mu.Unlock()
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m != nil {
		t.Errorf("the last owner kept access GitHub no longer gives: %+v", m)
	}
	ms, err := s.st.ListMembers(acmeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Role == store.RoleOwner {
			t.Errorf("acme still has an owner: %+v", m)
		}
	}
	s.gh.mu.Lock()
	s.gh.acmeRole = "admin"
	s.gh.mu.Unlock()
	s.signIn()
	if m, _ := s.st.GetMembership(acmeID, aliceID); m == nil || m.Role != store.RoleOwner {
		t.Errorf("a GitHub admin signing in didn't give acme an owner again: %+v", m)
	}
}

// TestReviewsWithoutBreakdownShown: a review from before severity and fixed
// counts were recorded shows "—" for them, not zeros, and the statistics
// say how many reviews they don't cover.
func TestReviewsWithoutBreakdownShown(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	db, err := sql.Open("sqlite", s.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`INSERT INTO reviews(repo, number, status, comments, notes, started_at, finished_at, account_id)
VALUES('acme/web', 7, 'posted', 4, 1, ?, ?, ?)`, time.Now().Unix()-30, time.Now().Unix(), acmeID) // as the old release stored it
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	s.signIn()
	for path, want := range map[string]string{
		"/orgs/acme":                   "not known for 1 earlier review",
		"/orgs/acme/reviews":           "Not recorded for reviews from before the upgrade",
		"/prs/acme/web/7":              "Not recorded for reviews from before the upgrade",
		fmt.Sprintf("/reviews/%d", id): "not recorded for reviews from before the upgrade",
	} {
		code, body, _ := s.get(path)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s = %d; want it to contain %q", path, code, want)
		}
	}
}

// TestSignInErrorsNotShown: when GitHub can't be reached, the person signing
// in is told so, without the error, which can name internal addresses.
func TestSignInErrorsNotShown(t *testing.T) {
	s := newTestSite(t)
	s.gh.mu.Lock()
	s.gh.failUser = true
	s.gh.mu.Unlock()
	code, body, _ := s.trySignIn()
	if code != http.StatusOK || !strings.Contains(body, "Couldn&#39;t finish signing in with GitHub") {
		t.Fatalf("sign-in when GitHub fails = %d", code)
	}
	if strings.Contains(body, "10.1.2.3") || strings.Contains(body, "dial tcp") {
		t.Error("the sign-in page shows the error from GitHub")
	}
}

// TestHomeRedirectEscaped: someone with one organization is sent to its page,
// even when its login has characters a path must escape.
func TestHomeRedirectEscaped(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.gh.mu.Lock()
	s.gh.noInstallations = true // no access from GitHub
	s.gh.mu.Unlock()
	if err := s.st.UpsertOrg(900, "other", "Organization", 30); err != nil { // renames the earlier "other"
		t.Fatal(err)
	}
	s.signIn()
	if _, err := s.st.AddMember(otherID, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	old, _ := s.st.GetOrg(otherID)
	code, _, hdr := s.get("/")
	if want := "/orgs/" + url.PathEscape(old.Login); code != http.StatusSeeOther || hdr.Get("Location") != want {
		t.Errorf("/ = %d to %q, want %q", code, hdr.Get("Location"), want)
	}
}

// TestRunningJobShownByAccount: the running job is shown to members of the
// account it was started for, whoever has the repository's name now.
func TestRunningJobShownByAccount(t *testing.T) {
	member := &viewer{ID: aliceID, Orgs: []store.Member{{OrgID: acmeID, Role: store.RoleMember}}}
	job := bot.Status{Current: "Reviewing acme/web#7", CurrentRepo: "acme/web", CurrentAccount: otherID}
	if st := visibleStatus(job, member); st.Current == job.Current || st.CurrentRepo != "" {
		t.Errorf("another account's job, under a name acme has now, shown to acme: %+v", st)
	}
	job.CurrentAccount = acmeID
	if st := visibleStatus(job, member); st.Current != job.Current {
		t.Errorf("acme's own job hidden from acme: %+v", st)
	}
	job.CurrentAccount = 0 // not linked to an organization: administrators only
	if st := visibleStatus(job, member); st.Current == job.Current {
		t.Errorf("a job of no organization shown to a member: %+v", st)
	}
}

// TestRemovedFromPausedOrgNotPromised: removing someone's GitHub-derived
// access to a paused organization doesn't say sign-in will give it back,
// as it won't while the organization is paused.
func TestRemovedFromPausedOrgNotPromised(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme
	if _, err := s.st.RecordSignIn(bobID, "bob", "Bob", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetGitHubMembership(acmeID, bobID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	s.cfg.GitHub.Accounts = []string{"someone-else"} // pauses acme
	if err := settings.Save(s.st, s.cfg); err != nil {
		t.Fatal(err)
	}
	code, hdr := s.post(fmt.Sprintf("/orgs/acme/members/%d/remove", bobID), nil)
	loc, _ := url.Parse(hdr.Get("Location"))
	if code != http.StatusSeeOther || loc == nil || !strings.Contains(loc.Query().Get("msg"), "Removed @bob") {
		t.Fatalf("remove = %d %s", code, hdr.Get("Location"))
	}
	if msg := loc.Query().Get("msg"); strings.Contains(msg, "access again") {
		t.Errorf("removal from a paused organization says %q", msg)
	}
}

// TestConcurrentRotationsAgree: when two password changes overlap, the key
// the server signs with is the one stored, so a restart keeps the sessions
// it issued.
func TestConcurrentRotationsAgree(t *testing.T) {
	s := newTestSite(t)
	save := s.srv.saveSecret
	var calls atomic.Int32
	saved, secondDone := make(chan struct{}), make(chan struct{})
	s.srv.saveSecret = func(v string) error {
		err := save(v)
		if calls.Add(1) == 1 { // the first is slow to publish its key
			close(saved)
			select {
			case <-secondDone:
			case <-time.After(300 * time.Millisecond):
			}
		}
		return err
	}
	first := make(chan error, 1)
	go func() { first <- s.srv.rotateSecret() }()
	<-saved
	if err := s.srv.rotateSecret(); err != nil {
		t.Fatal(err)
	}
	close(secondDone)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	stored, _, _ := s.st.Get(secretKey)
	if live := hex.EncodeToString(*s.srv.secret.Load()); live != stored {
		t.Error("after overlapping rotations the server signs with one key and stored another")
	}
}

// TestOlderSignInKeepsNewerFailure: a sign-in that updated access but
// hasn't finished yet doesn't undo a later sign-in's failure to update it:
// the earlier sessions stay blocked.
func TestOlderSignInKeepsNewerFailure(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // the session that must end
	s.srv.retryWait = time.Hour
	s.srv.endSessions = func(int64) error { return errors.New("database is locked") }
	real := s.srv.syncAccess
	firstIn, secondDone := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.srv.syncAccess = func(ctx context.Context, uc *github.UserClient, u *store.User) ([]string, error) {
		if calls.Add(1) == 1 { // the first sign-in: updated, but slow to finish
			notes, err := real(ctx, uc, u)
			close(firstIn)
			select {
			case <-secondDone:
			case <-time.After(300 * time.Millisecond):
			}
			return notes, err
		}
		return nil, errors.New("database is locked") // the second can't update access
	}
	ca, cb := s.newClient(), s.newClient()
	sa, err := s.startSignIn(ca)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := s.startSignIn(cb)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan int, 1)
	go func() {
		code, _ := s.finishSignIn(ca, sa)
		first <- code
	}()
	<-firstIn
	if code, err := s.finishSignIn(cb, sb); err != nil || code != http.StatusOK {
		t.Fatalf("second sign-in = %d, %v; want it refused", code, err)
	}
	close(secondDone)
	<-first
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("earlier session works after a later sign-in failed to update access: %d %s", code, hdr.Get("Location"))
	}
	s.srv.unreconciled.Delete(int64(aliceID)) // let any retry stop
}

// TestRetryKeepsNewerFailure: a retry that has ended someone's sessions
// doesn't clear the note of a failure recorded after it, and that failure
// gets a retry of its own.
func TestRetryKeepsNewerFailure(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	s.srv.retryWait = 5 * time.Millisecond
	real, realEnd := s.srv.syncAccess, s.srv.endSessions
	var failSync atomic.Bool
	failSync.Store(true)
	s.srv.syncAccess = func(ctx context.Context, uc *github.UserClient, u *store.User) ([]string, error) {
		if failSync.Load() {
			return nil, errors.New("database is locked")
		}
		return real(ctx, uc, u)
	}
	var ends atomic.Int32
	done := make(chan struct{})
	cg, cf := s.newClient(), s.newClient()
	s.srv.endSessions = func(id int64) error {
		switch ends.Add(1) {
		case 1: // the failed sign-in's own try
			return errors.New("database is locked")
		case 2: // the retry gets through; meanwhile one sign-in works, then one fails
			go func() {
				defer close(done)
				failSync.Store(false)
				if st, err := s.startSignIn(cg); err == nil {
					_, _ = s.finishSignIn(cg, st)
				}
				failSync.Store(true)
				if st, err := s.startSignIn(cf); err == nil {
					_, _ = s.finishSignIn(cf, st)
				}
			}()
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
			}
			return realEnd(id)
		default:
			return errors.New("database is locked")
		}
	}
	if code, _, _ := s.trySignIn(); code != http.StatusOK {
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry never got through")
	}
	deadline := time.Now().Add(5 * time.Second)
	for ends.Load() < 4 && time.Now().Before(deadline) { // the last failure's own retry
		time.Sleep(5 * time.Millisecond)
	}
	if ids, _ := s.srv.pending.load(); len(ids) != 1 || ids[0] != aliceID {
		t.Errorf("sessions noted to end after the last failure = %v, want alice's", ids)
	}
	if _, busy := s.srv.retrying.Load(int64(aliceID)); !busy {
		t.Error("nothing is retrying the last failure")
	}
	s.srv.unreconciled.Delete(int64(aliceID)) // let the retry stop
}

// TestUnreadablePendingNoteKept: a note that can't be read isn't replaced
// by one listing only the newcomer, which would drop the people it lists.
func TestUnreadablePendingNoteKept(t *testing.T) {
	p := &pendingEnds{path: filepath.Join(t.TempDir(), "pending-session-ends.json")}
	if err := os.WriteFile(p.path, []byte("[41,"), 0o600); err != nil { // cut short
		t.Fatal(err)
	}
	if err := p.add(42); err == nil {
		t.Error("adding to an unreadable note reported success")
	}
	if b, _ := os.ReadFile(p.path); string(b) != "[41," {
		t.Errorf("unreadable note replaced with %q", b)
	}
	if err := os.Remove(p.path); err != nil {
		t.Fatal(err)
	}
	if err := p.add(42); err != nil { // readable again: the retry gets through
		t.Fatal(err)
	}
	if ids, err := p.load(); err != nil || len(ids) != 1 || ids[0] != 42 {
		t.Errorf("note = %v, %v", ids, err)
	}
}

// TestPendingNoteSyncRetried: when the directory sync after the rename
// fails, the add reports it, and its retry syncs again before reporting
// success, rather than taking the listed ID as already on disk.
func TestPendingNoteSyncRetried(t *testing.T) {
	p := &pendingEnds{path: filepath.Join(t.TempDir(), "pending-session-ends.json")}
	orig := syncDir
	t.Cleanup(func() { syncDir = orig })
	syncDir = func(string) error { return errors.New("disk on fire") }
	if err := p.add(42); err == nil {
		t.Fatal("an add whose rename wasn't synced reported success")
	}
	synced := 0
	syncDir = func(string) error { synced++; return nil }
	if err := p.add(42); err != nil {
		t.Fatal(err)
	}
	if synced == 0 {
		t.Error("the retry reported success without syncing the directory")
	}
	if ids, err := p.load(); err != nil || len(ids) != 1 || ids[0] != 42 {
		t.Errorf("note = %v, %v", ids, err)
	}
}

// TestShortRangeChartDated: the 7-day chart dates every bar, not just the
// last; the 30-day one, every week.
func TestShortRangeChartDated(t *testing.T) {
	labels := func(n int) int {
		days := make([]store.DayStats, n)
		for i := range days {
			days[i].Day = time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC)
		}
		return strings.Count(string(dayChart(days)), `text-anchor="middle"`)
	}
	if n := labels(7); n != 7 {
		t.Errorf("7-day chart has %d date labels, want 7", n)
	}
	if n := labels(30); n != 5 {
		t.Errorf("30-day chart has %d date labels, want 5 (one a week)", n)
	}
}

// TestUncleanStopEndsSessions: after a run that didn't stop cleanly (it may
// have had sessions to end that it couldn't record), every session ends;
// access, including access added by hand, stays as it was.
func TestUncleanStopEndsSessions(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if _, err := s.st.AddMember(otherID, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	s.signIn()
	before, _ := s.st.UserMemberships(aliceID)
	srv2, err := New(s.st, s.b, slog.New(slog.NewTextHandler(io.Discard, nil))) // the first never stopped
	if err != nil {
		t.Fatal(err)
	}
	s.restart(srv2)
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("a session survived a run that didn't stop cleanly: %d %s", code, hdr.Get("Location"))
	}
	if after, _ := s.st.UserMemberships(aliceID); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("access changed: %v, was %v", after, before)
	}
	s.signIn()
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Errorf("signing in again after an unclean stop: /orgs = %d", code)
	}
}

// TestCleanStopKeepsSessions: a clean stop and start keeps everyone signed in.
func TestCleanStopKeepsSessions(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	if err := s.srv.Close(); err != nil {
		t.Fatal(err)
	}
	srv2, err := New(s.st, s.b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.restart(srv2)
	if code, _, _ := s.get("/orgs"); code != http.StatusOK {
		t.Errorf("session after a clean restart: /orgs = %d", code)
	}
}

// TestStopWithUnrecordedEndsSignsOut: when neither the database nor the
// disk took the record of sessions to end, and the bot stops before a retry
// gets through, the next start still ends them (every session), and access
// added by hand stays.
func TestStopWithUnrecordedEndsSignsOut(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	if _, err := s.st.AddMember(otherID, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	s.signIn()
	before, _ := s.st.UserMemberships(aliceID)
	s.srv.retryWait = time.Hour
	s.srv.syncAccess = func(context.Context, *github.UserClient, *store.User) ([]string, error) {
		return nil, errors.New("database is locked")
	}
	s.srv.endSessions = func(int64) error { return errors.New("database is locked") }
	if err := os.MkdirAll(s.srv.pending.path, 0o700); err != nil { // and the note can't be written
		t.Fatal(err)
	}
	if code, _, _ := s.trySignIn(); code != http.StatusOK {
		t.Fatalf("sign-in with a failed access update = %d", code)
	}
	if err := s.srv.Close(); err != nil { // stops with the retry still waiting
		t.Fatal(err)
	}
	if err := os.Remove(s.srv.pending.path); err != nil { // the disk works again; there's no note
		t.Fatal(err)
	}
	srv2, err := New(s.st, s.b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.restart(srv2)
	if code, _, hdr := s.get("/orgs"); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Errorf("a session that had to end survived the restart: %d %s", code, hdr.Get("Location"))
	}
	if after, _ := s.st.UserMemberships(aliceID); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("access changed: %v, was %v", after, before)
	}
	s.srv.unreconciled.Delete(int64(aliceID)) // let the old retry stop
}
