package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt/chatgpttest"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// TestChatGPTSignInFlow: an organization's owner adds a ChatGPT plan model
// (no key, OpenAI's address whatever the form says), signs it in by pasting
// back the address the sign-in ended on, and signs it out. A sign-in is
// finished once; one no model holds any more is ended at OpenAI.
func TestChatGPTSignInFlow(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme
	oa := chatgpttest.New(t)
	s.b.UseChatGPTAuth(oa.Client())

	_, hdr := s.post("/orgs/acme/models/add", url.Values{"context": {"200000"}, "max_output": {"32000"}, "provider": {"chatgpt"},
		"base_url": {"https://evil.example.com/v1"}, "api_key": {"sk-not-for-this"}, "model": {"gpt-b"}})
	if loc := hdr.Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("adding a ChatGPT plan model: %s", loc)
	}
	model := func() settings.LLM {
		t.Helper()
		m, err := s.b.OrgModels(acmeID)
		if err != nil || len(m.Configs) != 1 {
			t.Fatalf("own models = %+v, %v", m, err)
		}
		return m.Configs[0]
	}
	if c := model(); c.BaseURL != settings.ChatGPTBaseURL || c.APIKey != "" || c.SignedIn() {
		t.Fatalf("ChatGPT plan model = %+v", c)
	}
	if _, body, _ := s.get("/orgs/acme/models"); !strings.Contains(body, "Not signed in") || !strings.Contains(body, "Sign in with ChatGPT") {
		t.Error("models page doesn't offer the sign-in")
	}

	start := func() string {
		t.Helper()
		_, hdr := s.post("/orgs/acme/models/m1/chatgpt-start", url.Values{})
		if loc := hdr.Get("Location"); strings.Contains(loc, "err=") || !strings.HasSuffix(loc, "#model-m1") {
			t.Fatalf("start: %s", loc)
		}
		_, body, _ := s.get("/orgs/acme/models")
		link := regexp.MustCompile(`href="(` + regexp.QuoteMeta(oa.URL) + `/authorize\?[^"]+)"`).FindStringSubmatch(body)
		if link == nil || !strings.Contains(body, "Finish signing in") {
			t.Fatal("models page doesn't show the sign-in link and paste box")
		}
		return html.UnescapeString(link[1])
	}
	finish := func(pasted string) string {
		t.Helper()
		_, hdr := s.post("/orgs/acme/models/m1/chatgpt-finish", url.Values{"callback": {pasted}})
		loc, _ := url.QueryUnescape(hdr.Get("Location"))
		return loc
	}

	callback := oa.Approve(t, start())
	if loc := finish("not the address"); !strings.Contains(loc, "err=") {
		t.Errorf("a wrong paste was accepted: %s", loc)
	}
	// The attempt survives a wrong paste.
	if loc := finish(callback); strings.Contains(loc, "err=") || !strings.Contains(loc, "dev@example.com") {
		t.Fatalf("finish: %s", loc)
	}
	c := model()
	if !c.SignedIn() || c.AccountEmail != "dev@example.com" || c.ChatGPTClient != "client-abc" || c.AccountSubject != "user-123" {
		t.Fatalf("signed-in model = %+v", c)
	}
	if loc := finish(callback); !strings.Contains(loc, "already finished") {
		t.Errorf("a sign-in finished twice: %s", loc)
	}
	_, body, _ := s.get("/orgs/acme/models")
	if !strings.Contains(body, "Signed in as <strong>dev@example.com</strong>") || !strings.Contains(body, "Sign out") {
		t.Error("models page doesn't show the sign-in")
	}
	if strings.Contains(body, "rt-") || strings.Contains(body, "at-") {
		t.Error("models page shows a token")
	}
	var logged []string
	if acts, err := s.st.RecentActivity(acmeID, 20, "", 0); err == nil {
		for _, a := range acts {
			logged = append(logged, a.Message)
		}
	}
	if !strings.Contains(strings.Join(logged, " | "), "in to ChatGPT as dev@example.com") {
		t.Error("the sign-in isn't in the organization's activity")
	}

	// Signing in again with another account is refused: the model keeps its
	// sign-in, and the other one is ended.
	oa.Subject, oa.Email = "user-999", "other@example.com"
	if loc := finish(oa.Approve(t, start())); !strings.Contains(loc, "isn't the ChatGPT account this model uses (dev@example.com)") {
		t.Errorf("signing in another account: %s", loc)
	}
	if again := model(); again.Account != c.Account || again.AccountSubject != "user-123" {
		t.Errorf("after signing in another account: %+v", again)
	}
	waitRevokes(t, oa, 1)
	oa.Subject, oa.Email = "user-123", "dev@example.com"

	// Signing in again replaces the sign-in, and ends the old one.
	first := c.Account
	if loc := finish(oa.Approve(t, start())); strings.Contains(loc, "err=") {
		t.Fatalf("signing in again: %s", loc)
	}
	if again := model(); again.Account == first || !again.SignedIn() {
		t.Errorf("after signing in again: %+v", again)
	}
	waitRevokes(t, oa, 2)

	// Changing the provider drops the sign-in.
	s.post("/orgs/acme/models/m1/update", url.Values{"context": {"200000"}, "max_output": {"32000"}, "provider": {"openai-api"},
		"base_url": {"https://api.openai.com/v1"}, "api_key": {"sk-openai"}, "model": {"gpt-b"}})
	if c := model(); c.SignedIn() || c.Provider != settings.ProviderOpenAI || c.ChatGPTClient != "" {
		t.Errorf("after changing provider: %+v", c)
	}
	waitRevokes(t, oa, 3)

	// And back, signed in, then out.
	s.post("/orgs/acme/models/m1/update", url.Values{"context": {"200000"}, "max_output": {"32000"}, "provider": {"chatgpt"}, "model": {"gpt-b"}})
	finish(oa.Approve(t, start()))
	if !model().SignedIn() {
		t.Fatal("not signed in")
	}
	if _, hdr := s.post("/orgs/acme/models/m1/chatgpt-signout", url.Values{}); strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("sign out: %s", hdr.Get("Location"))
	}
	if c := model(); c.SignedIn() || c.AccountEmail != "" {
		t.Errorf("after signing out: %+v", c)
	}
	waitRevokes(t, oa, 4)

	// A model of another provider can't be signed in.
	s.post("/orgs/acme/models/m1/update", url.Values{"context": {"200000"}, "max_output": {"32000"}, "provider": {"openai-api"},
		"base_url": {"https://api.openai.com/v1"}, "api_key": {"sk-openai"}, "model": {"gpt-b"}})
	if _, hdr := s.post("/orgs/acme/models/m1/chatgpt-start", url.Values{}); !strings.Contains(hdr.Get("Location"), "err=") {
		t.Error("a sign-in was started for a model of another provider")
	}
	if code, _ := s.post("/orgs/other/models/m1/chatgpt-start", url.Values{}); code == http.StatusSeeOther {
		t.Error("a sign-in was started in an organization alice doesn't own")
	}
}

// TestChatGPTUnreadableSetRemoved: removing an organization's models that
// can't be read still ends the ChatGPT sign-ins they named.
func TestChatGPTUnreadableSetRemoved(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	oa := chatgpttest.New(t)
	s.b.UseChatGPTAuth(oa.Client())
	s.post("/orgs/acme/models/add", url.Values{"context": {"200000"}, "max_output": {"32000"}, "provider": {"chatgpt"}, "model": {"gpt-b"}})
	s.post("/orgs/acme/models/m1/chatgpt-start", url.Values{})
	_, body, _ := s.get("/orgs/acme/models")
	link := regexp.MustCompile(`href="(` + regexp.QuoteMeta(oa.URL) + `/authorize\?[^"]+)"`).FindStringSubmatch(body)
	if link == nil {
		t.Fatal("no sign-in link")
	}
	s.post("/orgs/acme/models/m1/chatgpt-finish", url.Values{"callback": {oa.Approve(t, html.UnescapeString(link[1]))}})
	if m, err := s.b.OrgModels(acmeID); err != nil || !m.Configs[0].SignedIn() {
		t.Fatalf("not signed in: %+v, %v", m, err)
	}

	if err := s.st.SetOrgModels(acmeID, "v1:garbage"); err != nil {
		t.Fatal(err)
	}
	if _, hdr := s.post("/orgs/acme/models/x/delete", url.Values{"all": {"1"}}); strings.Contains(hdr.Get("Location"), "err=") {
		t.Fatalf("removing the unreadable models: %s", hdr.Get("Location"))
	}
	waitRevokes(t, oa, 1)
	if keys, _ := s.st.Keys("chatgpt:sign-in:"); len(keys) != 0 {
		t.Errorf("sign-ins left: %v", keys)
	}
}

// TestChatGPTStaleAttempt: an attempt started before someone else signed
// the model in (or out) can't finish over what they did: its new sign-in
// is ended, and the model keeps theirs.
func TestChatGPTStaleAttempt(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	oa := chatgpttest.New(t)
	s.b.UseChatGPTAuth(oa.Client())
	s.post("/orgs/acme/models/add", url.Values{"context": {"200000"}, "max_output": {"32000"}, "provider": {"chatgpt"}, "model": {"gpt-b"}})
	s.post("/orgs/acme/models/m1/chatgpt-start", url.Values{})
	_, body, _ := s.get("/orgs/acme/models")
	link := regexp.MustCompile(`href="(` + regexp.QuoteMeta(oa.URL) + `/authorize\?[^"]+)"`).FindStringSubmatch(body)
	if link == nil {
		t.Fatal("no sign-in link")
	}
	callback := oa.Approve(t, html.UnescapeString(link[1]))

	// Meanwhile, as if another owner had signed the model in.
	m, err := s.b.OrgModels(acmeID)
	if err != nil {
		t.Fatal(err)
	}
	m.Configs[0].Account, m.Configs[0].ChatGPTClient = "their-sign-in", "their-client"
	if err := s.b.SetOrgModels(acmeID, m); err != nil {
		t.Fatal(err)
	}
	_, hdr := s.post("/orgs/acme/models/m1/chatgpt-finish", url.Values{"callback": {callback}})
	if loc, _ := url.QueryUnescape(hdr.Get("Location")); !strings.Contains(loc, "changed while you were signing in") {
		t.Errorf("finishing a stale attempt: %s", loc)
	}
	if m, _ := s.b.OrgModels(acmeID); m.Configs[0].Account != "their-sign-in" || m.Configs[0].ChatGPTClient != "their-client" {
		t.Errorf("model after a stale attempt: %+v", m.Configs[0])
	}
	waitRevokes(t, oa, 1)
	if keys, _ := s.st.Keys("chatgpt:sign-in:"); len(keys) != 0 {
		t.Errorf("the stale attempt's sign-in was kept: %v", keys)
	}
}

func waitRevokes(t *testing.T, oa *chatgpttest.Server, n int) {
	t.Helper()
	for range 200 {
		if len(oa.Revoked()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d sign-ins ended at OpenAI, want %d", len(oa.Revoked()), n)
}
