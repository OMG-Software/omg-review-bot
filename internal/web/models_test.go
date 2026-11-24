package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// Model forms in these tests give their token limits: left empty, saving
// asks the model's server for them (see TestModelMaximums), and these
// servers are made up or real ones the tests mustn't reach.
//
// TestOrgOwnModels: an organization's owners bring their own model. Only a
// public https server is accepted; the key is stored sealed and never shown;
// while the organization has one, the daily limit doesn't apply; members
// can't change them; removing the last puts it back on the instance's.
func TestOrgOwnModels(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme

	if code, body, _ := s.get("/orgs/acme/models"); code != http.StatusOK || !strings.Contains(body, "uses the instance's models") {
		t.Fatalf("GET /orgs/acme/models = %d", code)
	}
	add := func(base string) string {
		_, hdr := s.post("/orgs/acme/models/add", url.Values{"context": {"32768"}, "max_output": {"4096"}, "provider": {"openai"}, "base_url": {base}, "model": {"gpt-x"},
			"api_key": {"sk-very-secret-123"}})
		return hdr.Get("Location")
	}
	for _, bad := range []string{"http://api.example.com/v1", "https://127.0.0.1/v1", "https://localhost:1234/v1",
		"https://10.0.0.5/v1", "https://[::1]/v1", "https://169.254.169.254/latest", "https://printer.local/v1"} {
		if loc := add(bad); !strings.Contains(loc, "err=") {
			t.Errorf("own model at %s accepted (went to %s)", bad, loc)
		}
	}
	if own, _ := s.st.OrgHasOwnModels(acmeID); own {
		t.Fatal("a refused model was stored")
	}

	if loc := add("https://api.example.com/v1"); strings.Contains(loc, "err=") {
		t.Fatalf("public https model refused: %s", loc)
	}
	sealed, _ := s.st.OrgModels(acmeID)
	if !strings.HasPrefix(sealed, "v1:") || strings.Contains(sealed, "sk-very") || strings.Contains(sealed, "api.example.com") {
		t.Errorf("own models not stored sealed: %q", sealed)
	}
	m, err := s.b.OrgModels(acmeID)
	if err != nil || len(m.Configs) != 1 || m.Configs[0].APIKey != "sk-very-secret-123" || m.Configs[0].Model != "gpt-x" {
		t.Fatalf("own models = %+v, %v", m, err)
	}
	if other, _ := s.b.OrgModels(otherID); len(other.Configs) != 0 {
		t.Error("one organization's models leaked into another's")
	}
	if q, err := s.b.DailyQuota(acmeID, time.Now()); err != nil || !q.Own || q.Full() {
		t.Errorf("daily quota with own models = %+v, %v", q, err)
	}
	_, body, _ := s.get("/orgs/acme/models")
	if strings.Contains(body, "sk-very") || !strings.Contains(body, "its own models") {
		t.Error("models page shows the key, or not that acme uses its own models")
	}
	if _, body, _ := s.get("/orgs/acme/settings"); !strings.Contains(body, "so this limit doesn") {
		t.Error("review settings page doesn't say the daily limit is lifted")
	}

	// Changing it without a key keeps the saved key.
	s.post("/orgs/acme/models/m1/update", url.Values{"context": {"32768"}, "max_output": {"4096"}, "provider": {"openai"}, "base_url": {"https://api.example.com/v1"}, "model": {"gpt-y"}})
	if m, _ := s.b.OrgModels(acmeID); m.Configs[0].Model != "gpt-y" || m.Configs[0].APIKey != "sk-very-secret-123" {
		t.Errorf("after update: %+v", m.Configs)
	}
	// Ollama Cloud needs a key: refused without one, and once saved, an
	// update that leaves the key field empty keeps it and passes.
	cloud := func(path, key string) string {
		_, hdr := s.post(path, url.Values{"context": {"32768"}, "max_output": {"4096"}, "provider": {"ollama-cloud"}, "base_url": {"https://ollama.com"}, "model": {"big"}, "api_key": {key}})
		return hdr.Get("Location")
	}
	if loc := cloud("/orgs/acme/models/add", ""); !strings.Contains(loc, "err=") {
		t.Errorf("Ollama Cloud model without a key accepted (went to %s)", loc)
	}
	if loc := cloud("/orgs/acme/models/add", "ok-cloud-key"); strings.Contains(loc, "err=") {
		t.Fatalf("Ollama Cloud model with a key refused: %s", loc)
	}
	if loc := cloud("/orgs/acme/models/m2/update", ""); strings.Contains(loc, "err=") {
		t.Errorf("update keeping the saved Ollama Cloud key refused: %s", loc)
	}
	if m, _ := s.b.OrgModels(acmeID); len(m.Configs) != 2 || m.Configs[1].APIKey != "ok-cloud-key" {
		t.Errorf("after Ollama Cloud update: %+v", m.Configs)
	}
	// Pointed at another server without the key entered again, the saved key
	// isn't carried along: it would go to a server it wasn't entered for.
	s.post("/orgs/acme/models/m2/update", url.Values{"context": {"32768"}, "max_output": {"4096"}, "provider": {"openai"}, "base_url": {"https://other.example.com/v1"}, "model": {"big"}})
	if m, _ := s.b.OrgModels(acmeID); len(m.Configs) != 2 || m.Configs[1].APIKey != "" || m.Configs[1].BaseURL != "https://other.example.com/v1" {
		t.Errorf("saved key kept for another server: %+v", m.Configs)
	}
	s.post("/orgs/acme/models/m2/delete", url.Values{})

	// A member sees them but can't change them.
	if err := s.st.EnsureUser(bobID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.AddMember(acmeID, bobID, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetMemberRole(acmeID, aliceID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	if code, _ := s.post("/orgs/acme/models/m1/delete", url.Values{}); code != http.StatusForbidden {
		t.Errorf("member removing a model = %d, want 403", code)
	}
	if code, body, _ := s.get("/orgs/acme/models"); code != http.StatusOK || !strings.Contains(body, "Only owners can change these") {
		t.Errorf("member's models page = %d", code)
	}
	if err := s.st.SetMemberRole(acmeID, aliceID, store.RoleOwner); err != nil {
		t.Fatal(err)
	}

	// Removing the last one goes back to the instance's models and limit.
	s.post("/orgs/acme/models/m1/delete", url.Values{})
	if own, _ := s.st.OrgHasOwnModels(acmeID); own {
		t.Error("removing the last own model left the organization on its own models")
	}
	if q, _ := s.b.DailyQuota(acmeID, time.Now()); q.Own {
		t.Error("the daily limit is still lifted after the own models were removed")
	}
}

// TestModelEditsDontUndoEachOther: changes to one set of models made at the
// same time all land; none is lost by another saving what it read before.
func TestModelEditsDontUndoEachOther(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice owns acme
	const n = 6
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			form := url.Values{"context": {"32768"}, "max_output": {"4096"}, "provider": {"openai"}, "base_url": {"https://api.example.com/v1"}, "model": {fmt.Sprintf("m%d", i)}}
			req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/orgs/acme/models/add", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", s.ts.URL)
			if resp, err := s.cl.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if m, err := s.b.OrgModels(acmeID); err != nil || len(m.Configs) != n {
		t.Fatalf("after %d adds at once, %d models (%v)", n, len(m.Configs), err)
	}
}

// TestOrgModelsPageInstanceUnset: with the instance's models not set up, an
// organization's page says so instead of naming a model that isn't one.
func TestOrgModelsPageInstanceUnset(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	cfg := s.b.Settings()
	cfg.Models.Configs[0].Model = ""
	if err := settings.Save(s.st, cfg); err != nil {
		t.Fatal(err)
	}
	_, body, _ := s.get("/orgs/acme/models")
	if !strings.Contains(body, "aren't set up yet") || strings.Contains(body, "uses the instance's models") {
		t.Errorf("org models page with unset instance models:\n%s", body)
	}
}

// TestOrgModelsPageUnreadable: when an organization's stored models can't be
// read, its page says so, not that it uses the instance's models.
func TestOrgModelsPageUnreadable(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	if err := s.st.SetOrgModels(acmeID, "v1:garbage"); err != nil {
		t.Fatal(err)
	}
	_, body, _ := s.get("/orgs/acme/models")
	if !strings.Contains(body, "can't be read, so its pull requests aren't being reviewed") ||
		strings.Contains(body, "uses the instance's models") || !strings.Contains(body, "Remove all of its own models") {
		t.Errorf("org models page with unreadable models:\n%s", body)
	}
}

// TestLimitsNote: a reported limit the bot can't use as it is (a window
// below its minimum) is described as the bot's limit, not the model's.
func TestLimitsNote(t *testing.T) {
	c := settings.NewLLM()
	c.SetMaximums(2048, 256, true, true)
	note := limitsNote(llm.Limits{Context: 2048, Output: 256}, c, true, true)
	if c.ContextTokens != settings.MinContextTokens || !strings.Contains(note, "window of only 2048") ||
		!strings.Contains(note, "cap of only 256") || strings.Contains(note, "maximum") {
		t.Errorf("small window: %d/%d, %q", c.ContextTokens, c.MaxOutputTokens, note)
	}
	c = settings.NewLLM()
	c.SetMaximums(40960, 8192, true, true)
	if note := limitsNote(llm.Limits{Context: 40960, Output: 8192}, c, true, true); strings.Count(note, "the model's maximum") != 2 {
		t.Errorf("ordinary limits: %q", note)
	}
	// An 8192 window typed and no cap reported: the default output, 16384,
	// is cut to half the window, and isn't called the default.
	c = settings.NewLLM()
	c.ContextTokens = 8192
	c.SetMaximums(0, 0, false, true)
	if note := limitsNote(llm.Limits{}, c, false, true); c.MaxOutputTokens != 4096 ||
		!strings.Contains(note, "4096 output tokens, half the 8192-token context window") || strings.Contains(note, "the default (") {
		t.Errorf("default output in a small window: %d, %q", c.MaxOutputTokens, note)
	}
	c = settings.NewLLM()
	c.SetMaximums(0, 0, true, true)
	if note := limitsNote(llm.Limits{}, c, true, true); strings.Count(note, "the default (") != 2 {
		t.Errorf("defaults: %q", note)
	}
}

// TestModelMaximums: a model added with its token limits left empty gets
// the model's maximums from its server, or the fallbacks (and says so) when
// the server can't tell. "Use model maximums" answers with them for the form.
func TestModelMaximums(t *testing.T) {
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/show" {
			_, _ = w.Write([]byte(`{"model_info":{"qwen3.context_length":40960}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ollamaSrv.Close)
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn()
	if err := s.st.SetUserAdmin(aliceID, true); err != nil {
		t.Fatal(err)
	}
	addWith := func(provider, base, context, output string) string {
		t.Helper()
		_, hdr := s.post("/settings/models/add", url.Values{"provider": {provider}, "base_url": {base}, "model": {"qwen3"},
			"context": {context}, "max_output": {output}})
		msg, _ := url.QueryUnescape(hdr.Get("Location"))
		return msg
	}
	add := func(provider, base string) string { return addWith(provider, base, "", "") }
	last := func() settings.LLM {
		t.Helper()
		st, err := settings.Load(s.st)
		if err != nil {
			t.Fatal(err)
		}
		return st.Models.Configs[len(st.Models.Configs)-1]
	}

	loc := add("ollama", ollamaSrv.URL)
	if c := last(); c.ContextTokens != 40960 || c.MaxOutputTokens != 20480 || !strings.Contains(loc, "40960 context tokens, the model's maximum") ||
		!strings.Contains(loc, "20480 output tokens, half the context window") {
		t.Errorf("Ollama model: limits %d/%d, redirect %s; want 40960/20480 with a note", c.ContextTokens, c.MaxOutputTokens, loc)
	}
	loc = add("openai", ollamaSrv.URL+"/v1") // reports nothing on the OpenAI API
	if c := last(); c.ContextTokens != settings.FallbackContextTokens || c.MaxOutputTokens != settings.FallbackOutputTokens ||
		!strings.Contains(loc, "couldn") || !strings.Contains(loc, "the default") || strings.Contains(loc, "maximum") {
		t.Errorf("unknown limits: %d/%d, redirect %s; want the fallbacks, called defaults, with a warning", c.ContextTokens, c.MaxOutputTokens, loc)
	}
	// Only the context left blank: the output typed in isn't described.
	loc = addWith("ollama", ollamaSrv.URL, "", "4096")
	if c := last(); c.ContextTokens != 40960 || c.MaxOutputTokens != 4096 || !strings.Contains(loc, "40960 context tokens") || strings.Contains(loc, "output tokens") {
		t.Errorf("context only: limits %d/%d, redirect %s; want 40960/4096, the output not mentioned", c.ContextTokens, c.MaxOutputTokens, loc)
	}

	// Saved with a 32768 window, then the window cleared and 20000 output
	// typed: the typed output is checked against the server's window
	// (40960, half 20480), not the old one (half 16384).
	addWith("ollama", ollamaSrv.URL, "32768", "1024")
	id := last().ID
	_, hdr := s.post("/settings/models/"+id+"/update", url.Values{"provider": {"ollama"}, "base_url": {ollamaSrv.URL}, "model": {"qwen3"},
		"context": {""}, "max_output": {"20000"}})
	if c := last(); c.ContextTokens != 40960 || c.MaxOutputTokens != 20000 {
		t.Errorf("window cleared, output typed: limits %d/%d (redirect %s); want 40960/20000", c.ContextTokens, c.MaxOutputTokens, hdr.Get("Location"))
	}
	if loc, _ := url.QueryUnescape(hdr.Get("Location")); strings.Contains(loc, "output limit typed") {
		t.Errorf("an output that fits is said to have been changed: %s", loc)
	}
	// More than half the window typed: lowered to half, and the message says so.
	_, hdr = s.post("/settings/models/"+id+"/update", url.Values{"provider": {"ollama"}, "base_url": {ollamaSrv.URL}, "model": {"qwen3"},
		"context": {""}, "max_output": {"30000"}})
	loc, _ = url.QueryUnescape(hdr.Get("Location"))
	if c := last(); c.MaxOutputTokens != 20480 || !strings.Contains(loc, "the output limit typed, 30000, was changed to 20480, half the 40960-token context window") {
		t.Errorf("too large an output typed: limit %d, redirect %s; want 20480 and a note", c.MaxOutputTokens, loc)
	}

	// A full set refuses at once, without asking the model's server.
	for tries := 0; len(func() settings.Models { st, _ := settings.Load(s.st); return st.Models }().Configs) < settings.MaxModels; tries++ {
		if tries == settings.MaxModels {
			t.Fatal("the set didn't fill up")
		}
		if loc := addWith("ollama", ollamaSrv.URL, "8192", "1024"); strings.Contains(loc, "err=") {
			t.Fatalf("adding a model to fill the set failed: %s", loc)
		}
	}
	asked := 0
	countSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked++; http.NotFound(w, r) }))
	t.Cleanup(countSrv.Close)
	if loc := add("ollama", countSrv.URL); asked != 0 || !strings.Contains(loc, "at most") {
		t.Errorf("adding to a full set: server asked %d times, redirect %s; want no request and the capacity error", asked, loc)
	}

	form := url.Values{"provider": {"ollama"}, "base_url": {ollamaSrv.URL}, "model": {"qwen3"}}
	req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/settings/models/limits", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.ts.URL)
	resp, err := s.cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool           `json:"ok"`
		Limits map[string]int `json:"limits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK || out.Limits["context"] != 40960 || out.Limits["max_output"] != 20480 {
		t.Errorf("limits probe = %+v (%v)", out, err)
	}
}
