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
	add := func(provider, base string) string {
		t.Helper()
		_, hdr := s.post("/settings/models/add", url.Values{"provider": {provider}, "base_url": {base}, "model": {"qwen3"},
			"context": {""}, "max_output": {""}})
		return hdr.Get("Location")
	}
	last := func() settings.LLM {
		t.Helper()
		st, err := settings.Load(s.st)
		if err != nil {
			t.Fatal(err)
		}
		return st.Models.Configs[len(st.Models.Configs)-1]
	}

	loc := add("ollama", ollamaSrv.URL)
	if c := last(); c.ContextTokens != 40960 || c.MaxOutputTokens != 20480 || !strings.Contains(loc, "maximums") {
		t.Errorf("Ollama model: limits %d/%d, redirect %s; want 40960/20480 with a note", c.ContextTokens, c.MaxOutputTokens, loc)
	}
	loc = add("openai", ollamaSrv.URL+"/v1") // reports nothing on the OpenAI API
	if c := last(); c.ContextTokens != settings.FallbackContextTokens || c.MaxOutputTokens != settings.FallbackOutputTokens ||
		!strings.Contains(loc, "couldn") {
		t.Errorf("unknown limits: %d/%d, redirect %s; want the fallbacks with a warning", c.ContextTokens, c.MaxOutputTokens, loc)
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
