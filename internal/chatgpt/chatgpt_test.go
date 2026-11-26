package chatgpt_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt"
	"github.com/OMG-Software/omg-review-bot/internal/chatgpt/chatgpttest"
)

func attempt(t *testing.T, clientID, hint string) chatgpt.Attempt {
	t.Helper()
	a, err := chatgpt.NewAttempt("urn:uuid:host", clientID, hint)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthorizeURL(t *testing.T) {
	c := chatgpt.New()
	a := attempt(t, "", "")
	raw := c.AuthorizeURL(a)
	if !strings.HasPrefix(raw, chatgpt.Production.Authorize+"?") {
		t.Fatalf("authorize URL = %s", raw)
	}
	if strings.Contains(raw, "+") {
		t.Errorf("spaces encoded as '+': %s", raw)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	sum := sha256.Sum256([]byte(a.Verifier))
	want := map[string]string{
		"client_id": "dynamic_agent_client", "agent_name_hint": chatgpt.AppName, "ext_agent_host_id": "urn:uuid:host",
		"response_type": "code", "redirect_uri": chatgpt.RedirectURI, "resource": chatgpt.Resource,
		"state": a.State, "nonce": a.Nonce, "code_challenge_method": "S256",
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]),
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	scopes := strings.Fields(q.Get("scope"))
	for _, s := range []string{"openid", "offline_access", chatgpt.PlanScope} {
		if !slices.Contains(scopes, s) {
			t.Errorf("scope %q not asked for: %v", s, scopes)
		}
	}
	if q.Has("login_hint") || q.Has("prompt") || strings.Contains(raw, a.Verifier) {
		t.Errorf("first sign-in has a login hint, a prompt or the verifier: %s", raw)
	}

	// Signing a known account in again: its client and email, and consent
	// asked for again only when the plan's use was declined.
	again := attempt(t, "client-abc", "dev@example.com")
	q2, _ := url.ParseQuery(strings.SplitN(c.AuthorizeURL(again), "?", 2)[1])
	if q2.Get("client_id") != "client-abc" || q2.Get("login_hint") != "dev@example.com" || q2.Has("agent_name_hint") || q2.Has("prompt") {
		t.Errorf("returning sign-in = %v", q2)
	}
	again.Consent = true
	q3, _ := url.ParseQuery(strings.SplitN(c.AuthorizeURL(again), "?", 2)[1])
	if q3.Get("prompt") != "consent" {
		t.Errorf("consent not asked for again: %v", q3)
	}
	if a.State == again.State || a.Verifier == again.Verifier || len(a.Verifier) < 43 {
		t.Error("attempts don't get fresh, long enough secrets")
	}
}

func TestNewHostID(t *testing.T) {
	id, err := chatgpt.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("host ID = %q", id)
	}
}

func TestParseCallback(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:1455/auth/callback?code=c1&state=s1&client_id=x",
		"  http://127.0.0.1:1455/auth/callback?code=c1&state=s1&client_id=x#frag \n",
		"code=c1&state=s1&client_id=x",
	} {
		cb, err := chatgpt.ParseCallback(raw)
		if err != nil || cb.Code != "c1" || cb.State != "s1" || cb.ClientID != "x" {
			t.Errorf("ParseCallback(%q) = %+v, %v", raw, cb, err)
		}
	}
	if cb, err := chatgpt.ParseCallback("http://127.0.0.1:1455/auth/callback?error=access_denied&state=s1"); err != nil || cb.Error != "access_denied" {
		t.Errorf("declined callback = %+v, %v", cb, err)
	}
	for _, bad := range []string{"", "hello", "http://127.0.0.1:1455/auth/callback", "http://127.0.0.1:1455/auth/callback?code=c1"} {
		if _, err := chatgpt.ParseCallback(bad); err == nil {
			t.Errorf("ParseCallback(%q) accepted", bad)
		}
	}
}

// TestSignIn: a first sign-in registers a client, its code is exchanged with
// the PKCE verifier, its ID token checked; each renewal replaces the refresh
// token, and the old one is no use after; revoking ends the sign-in.
func TestSignIn(t *testing.T) {
	ctx := context.Background()
	srv := chatgpttest.New(t)
	c := srv.Client()
	a := attempt(t, "", "")
	cb, err := chatgpt.ParseCallback(srv.Approve(t, c.AuthorizeURL(a)))
	if err != nil {
		t.Fatal(err)
	}
	cred, err := c.Finish(ctx, a, cb)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Email != "dev@example.com" || cred.Subject != "user-123" || cred.ClientID != "client-abc" || cred.HostID != "urn:uuid:host" ||
		cred.AccessToken == "" || cred.RefreshToken == "" || cred.IDToken == "" || !cred.PlanUsage() {
		t.Fatalf("credentials = %+v", cred)
	}
	if d := time.Until(cred.Expiry); d < 59*time.Minute || d > time.Hour+time.Minute {
		t.Errorf("expires in %s", d)
	}
	// The code is used once.
	if _, err := c.Finish(ctx, a, cb); err == nil {
		t.Error("a code was exchanged twice")
	}

	old := cred
	if err := c.Refresh(ctx, &cred); err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken == old.RefreshToken || cred.AccessToken == old.AccessToken || cred.IDToken != old.IDToken || !cred.PlanUsage() {
		t.Errorf("after renewal: %+v (was %+v)", cred, old)
	}
	if err := c.Refresh(ctx, &old); !errors.Is(err, chatgpt.ErrSignInEnded) {
		t.Errorf("renewing with a replaced refresh token = %v, want ErrSignInEnded", err)
	}
	if err := c.Revoke(ctx, cred); err != nil {
		t.Fatal(err)
	}
	if got := srv.Revoked(); len(got) != 1 || got[0] != cred.RefreshToken || srv.Live(cred.RefreshToken) {
		t.Errorf("revoked = %v", got)
	}
}

// TestFinishRefuses: a sign-in is refused when the callback is another
// attempt's, was declined, lacks the plan's use, or its ID token doesn't
// check out.
func TestFinishRefuses(t *testing.T) {
	ctx := context.Background()
	srv := chatgpttest.New(t)
	c := srv.Client()
	finish := func(a chatgpt.Attempt, raw string) (chatgpt.Credentials, error) {
		cb, err := chatgpt.ParseCallback(raw)
		if err != nil {
			t.Fatal(err)
		}
		return c.Finish(ctx, a, cb)
	}

	a, b := attempt(t, "", ""), attempt(t, "", "")
	if _, err := finish(b, srv.Approve(t, c.AuthorizeURL(a))); err == nil || !strings.Contains(err.Error(), "another sign-in") {
		t.Errorf("another attempt's callback = %v", err)
	}
	if _, err := finish(a, chatgpttest.Decline(c.AuthorizeURL(a))); !errors.Is(err, chatgpt.ErrDenied) {
		t.Errorf("declined = %v", err)
	}
	// The verifier is the proof the code is this bot's.
	stolen := srv.Approve(t, c.AuthorizeURL(a))
	forged := a
	forged.Verifier = strings.Repeat("x", 43)
	if _, err := finish(forged, stolen); err == nil {
		t.Error("a code was exchanged without its verifier")
	}

	srv.Nonce = "another nonce"
	if _, err := finish(a, srv.Approve(t, c.AuthorizeURL(a))); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("ID token with another nonce = %v", err)
	}
	srv.Nonce = ""

	late := srv.Client()
	late.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	cb, _ := chatgpt.ParseCallback(srv.Approve(t, late.AuthorizeURL(a)))
	if _, err := late.Finish(ctx, a, cb); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired ID token = %v", err)
	}

	// Signed by a key that isn't OpenAI's.
	other := chatgpttest.New(t)
	wrongKeys := srv.Client()
	wrongKeys.E.JWKS = other.Endpoints().JWKS
	cb, _ = chatgpt.ParseCallback(srv.Approve(t, wrongKeys.AuthorizeURL(a)))
	if _, err := wrongKeys.Finish(ctx, a, cb); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("ID token signed with another key = %v", err)
	}

	srv.Scopes = "openid profile email offline_access"
	cred, err := finish(a, srv.Approve(t, c.AuthorizeURL(a)))
	if !errors.Is(err, chatgpt.ErrNoPlanUsage) || cred.ClientID != "client-abc" || cred.PlanUsage() {
		t.Errorf("sign-in without the plan's use = %+v, %v", cred, err)
	}
}
