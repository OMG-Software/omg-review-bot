package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/store"
)

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
		_, hdr := s.post("/orgs/acme/models/add", url.Values{"provider": {"openai"}, "base_url": {base}, "model": {"gpt-x"},
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
	s.post("/orgs/acme/models/m1/update", url.Values{"provider": {"openai"}, "base_url": {"https://api.example.com/v1"}, "model": {"gpt-y"}})
	if m, _ := s.b.OrgModels(acmeID); m.Configs[0].Model != "gpt-y" || m.Configs[0].APIKey != "sk-very-secret-123" {
		t.Errorf("after update: %+v", m.Configs)
	}

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
