package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt"
	"github.com/OMG-Software/omg-review-bot/internal/chatgpt/chatgpttest"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// chatgptBot is a bot whose ChatGPT sign-ins go to a stand-in.
func chatgptBot(t *testing.T) (*Bot, *store.Store, *chatgpttest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := chatgpttest.New(t)
	b.chatgptAuth = srv.Client()
	return b, st, srv
}

// signInChatGPT signs a ChatGPT account in for a model of set, as the
// models page does.
func signInChatGPT(t *testing.T, b *Bot, srv *chatgpttest.Server, set int64, cur settings.LLM) (string, chatgpt.Credentials) {
	t.Helper()
	a, u, err := b.StartChatGPT(cur)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := chatgpt.ParseCallback(srv.Approve(t, u))
	if err != nil {
		t.Fatal(err)
	}
	id, who, err := b.FinishChatGPT(context.Background(), set, a, cb)
	if err != nil {
		t.Fatal(err)
	}
	return id, who
}

// expire makes a sign-in's access token due for renewal.
func expire(t *testing.T, b *Bot, id string, set int64) {
	t.Helper()
	cred, err := b.loadChatGPT(id, set)
	if err != nil {
		t.Fatal(err)
	}
	cred.Expiry, cred.SavedAt = time.Now().Add(-time.Minute), time.Now().Add(-time.Hour)
	if err := b.storeChatGPT(id, set, cred); err != nil {
		t.Fatal(err)
	}
}

// TestChatGPTSignInSealed: a sign-in is stored sealed, opens only for the
// set of models it was made for, and its tokens never leave the bot. The
// installation keeps one host ID.
func TestChatGPTSignInSealed(t *testing.T) {
	b, st, srv := chatgptBot(t)
	id, who := signInChatGPT(t, b, srv, 7, settings.LLM{Provider: settings.ProviderChatGPT})
	if who.Email != "dev@example.com" || who.ClientID != "client-abc" || who.AccessToken != "" || who.RefreshToken != "" || who.IDToken != "" {
		t.Errorf("FinishChatGPT returned %+v, want who signed in without tokens", who)
	}
	raw, ok, _ := st.Get(chatgptKey(id))
	if !ok || !strings.HasPrefix(raw, "v1:") || strings.Contains(raw, "rt-") || strings.Contains(raw, "dev@example.com") {
		t.Errorf("sign-in stored as %q, want sealed", raw)
	}
	cred, err := b.loadChatGPT(id, 7)
	if err != nil || cred.RefreshToken == "" || !cred.PlanUsage() {
		t.Fatalf("sign-in = %+v, %v", cred, err)
	}
	for _, other := range []int64{0, 8} {
		if _, err := b.loadChatGPT(id, other); err == nil {
			t.Errorf("set %d opened set 7's sign-in", other)
		}
	}
	host := srv.LastAuthorize.Get("ext_agent_host_id")
	if _, u, _ := b.StartChatGPT(settings.LLM{}); !strings.Contains(u, url.QueryEscape(host)) || !strings.HasPrefix(host, "urn:uuid:") {
		t.Errorf("host ID %q not kept for the next sign-in (%s)", host, u)
	}
}

// TestChatGPTTokens: the stored access token is used until it's due; then
// it's renewed once, however many calls want it at the same moment, and
// the new refresh token replaces the old. A renewal isn't cut short with
// the job that asked for it: the new refresh token would be lost.
func TestChatGPTTokens(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	ts := chatgptTokens{b: b, id: id, set: 7}
	ctx := context.Background()
	first, _ := b.loadChatGPT(id, 7)
	if tok, err := ts.Token(ctx, ""); err != nil || tok != first.AccessToken || srv.Refreshes != 0 {
		t.Fatalf("token = %q, %v after %d renewals", tok, err, srv.Refreshes)
	}

	expire(t, b, id, 7)
	var wg sync.WaitGroup
	got := make([]string, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], errs[i] = ts.Token(ctx, "") }()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] || got[i] == first.AccessToken {
			t.Errorf("call %d: %q, %v", i, got[i], errs[i])
		}
	}
	renewed, _ := b.loadChatGPT(id, 7)
	if srv.Refreshes != 1 || renewed.RefreshToken == first.RefreshToken || srv.Live(first.RefreshToken) || !srv.Live(renewed.RefreshToken) {
		t.Errorf("%d renewals, refresh token %q (was %q)", srv.Refreshes, renewed.RefreshToken, first.RefreshToken)
	}
	// A refused token is renewed, unless another call already has.
	if tok, err := ts.Token(ctx, first.AccessToken); err != nil || tok != renewed.AccessToken || srv.Refreshes != 1 {
		t.Errorf("renewing a token already renewed: %q, %v, %d renewals", tok, err, srv.Refreshes)
	}
	if tok, err := ts.Token(ctx, renewed.AccessToken); err != nil || tok == renewed.AccessToken || srv.Refreshes != 2 {
		t.Errorf("renewing a refused token: %q, %v, %d renewals", tok, err, srv.Refreshes)
	}

	expire(t, b, id, 7)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ts.Token(cancelled, ""); err != nil || srv.Refreshes != 3 {
		t.Errorf("renewal for a cancelled job: %v, %d renewals", err, srv.Refreshes)
	}
}

// TestChatGPTSignInEnded: a sign-in ended at OpenAI says to sign in again.
func TestChatGPTSignInEnded(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, _ := signInChatGPT(t, b, srv, 0, settings.LLM{})
	cred, _ := b.loadChatGPT(id, 0)
	if err := srv.Client().Revoke(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	expire(t, b, id, 0)
	_, err := chatgptTokens{b: b, id: id, set: 0}.Token(context.Background(), "")
	if !errors.Is(err, chatgpt.ErrSignInEnded) || !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("ended sign-in = %v", err)
	}
}

// TestChatGPTNoPlanUsage: a sign-in without the plan's use isn't kept, but
// who it was is returned, so the next sign-in reuses the client and asks
// for consent again.
func TestChatGPTNoPlanUsage(t *testing.T) {
	b, _, srv := chatgptBot(t)
	srv.Scopes = "openid profile email offline_access"
	a, u, _ := b.StartChatGPT(settings.LLM{})
	cb, _ := chatgpt.ParseCallback(srv.Approve(t, u))
	id, who, err := b.FinishChatGPT(context.Background(), 7, a, cb)
	if !errors.Is(err, chatgpt.ErrNoPlanUsage) || id != "" || who.ClientID != "client-abc" || who.RefreshToken != "" {
		t.Fatalf("FinishChatGPT = %q, %+v, %v", id, who, err)
	}
	waitRevoked(t, srv, 1)
	_, u, _ = b.StartChatGPT(settings.LLM{ChatGPTClient: who.ClientID, AccountEmail: who.Email})
	q, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if q.Get("client_id") != "client-abc" || q.Get("prompt") != "consent" || q.Get("login_hint") != "dev@example.com" {
		t.Errorf("sign-in after a decline = %v", q)
	}
}

func waitRevoked(t *testing.T, srv *chatgpttest.Server, n int) []string {
	t.Helper()
	for range 200 {
		if r := srv.Revoked(); len(r) >= n {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("revoked %v, want %d", srv.Revoked(), n)
	return nil
}

// TestForgetChatGPT: a sign-in no model names any more is deleted and
// ended at OpenAI; the others are kept.
func TestForgetChatGPT(t *testing.T) {
	b, st, srv := chatgptBot(t)
	a1, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	a2, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	gone, _ := b.loadChatGPT(a1, 7)
	before := settings.Models{Configs: []settings.LLM{{ID: "m1", Account: a1}, {ID: "m2", Account: a2}}}
	after := settings.Models{Configs: []settings.LLM{{ID: "m2", Account: a2}}}
	b.ForgetChatGPT(7, before, after)
	if _, ok, _ := st.Get(chatgptKey(a1)); ok {
		t.Error("the removed model's sign-in is still stored")
	}
	if _, err := b.loadChatGPT(a2, 7); err != nil {
		t.Errorf("the kept model's sign-in: %v", err)
	}
	if r := waitRevoked(t, srv, 1); !slices.Contains(r, gone.RefreshToken) {
		t.Errorf("revoked %v, want %q", r, gone.RefreshToken)
	}
}

// TestChatGPTModel: a ChatGPT plan model calls the Responses API with its
// sign-in's token, renewing it when it's refused, and only for its own set.
func TestChatGPTModel(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	first, _ := b.loadChatGPT(id, 7)
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first token is refused, as if it had been ended early.
		if calls.Add(1) == 1 || r.Header.Get("Authorization") == "Bearer "+first.AccessToken {
			http.Error(w, `{"error":{"code":"invalid_token"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	}))
	t.Cleanup(api.Close)
	// Not normalized: that pins a ChatGPT plan model to OpenAI's address.
	cfg := settings.NewLLM()
	cfg.Provider, cfg.BaseURL, cfg.Model, cfg.Account = settings.ProviderChatGPT, api.URL+"/v1", "gpt-b", id
	ownPublicOnly = false
	t.Cleanup(func() { ownPublicOnly = true })
	msg, err := b.TestModel(context.Background(), cfg, 7)
	if err != nil || !strings.Contains(msg, `replied "OK"`) || srv.Refreshes != 1 {
		t.Errorf("test = %q, %v after %d renewals", msg, err, srv.Refreshes)
	}
	if _, err := b.TestModel(context.Background(), cfg, 8); err == nil || !strings.Contains(err.Error(), "can't be read") {
		t.Errorf("another set's model using the sign-in = %v", err)
	}
	cfg.Account = ""
	if _, err := b.TestModel(context.Background(), cfg, 7); err == nil || !strings.Contains(err.Error(), "isn't signed in") {
		t.Errorf("a model with no sign-in = %v", err)
	}
}
