package bot

import (
	"context"
	"database/sql"
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
	a, u, err := b.StartChatGPT(set, cur)
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
	raw, ok, _ := st.Get(chatgptKey(id, 7))
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
	// Even put among set 8's, it only opens for set 7.
	_ = st.Set(chatgptKey(id, 8), raw)
	if _, err := b.loadChatGPT(id, 8); !errors.Is(err, errUnreadable) {
		t.Errorf("set 7's sign-in under set 8 = %v", err)
	}
	host := srv.LastAuthorize.Get("ext_agent_host_id")
	if _, u, _ := b.StartChatGPT(7, settings.LLM{}); !strings.Contains(u, url.QueryEscape(host)) || !strings.HasPrefix(host, "urn:uuid:") {
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
	a, u, _ := b.StartChatGPT(7, settings.LLM{})
	cb, _ := chatgpt.ParseCallback(srv.Approve(t, u))
	id, who, err := b.FinishChatGPT(context.Background(), 7, a, cb)
	if !errors.Is(err, chatgpt.ErrNoPlanUsage) || id != "" || who.ClientID != "client-abc" || who.RefreshToken != "" {
		t.Fatalf("FinishChatGPT = %q, %+v, %v", id, who, err)
	}
	waitRevoked(t, srv, 1)
	_, u, _ = b.StartChatGPT(7, settings.LLM{ChatGPTClient: who.ClientID, AccountEmail: who.Email})
	q, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if q.Get("client_id") != "client-abc" || q.Get("prompt") != "consent" || q.Get("login_hint") != "dev@example.com" || q.Has("id_token_hint") {
		t.Errorf("sign-in after a decline = %v", q)
	}
}

// TestChatGPTSignInAgainHint: signing a model in again names its account
// with the sign-in's ID token, so OpenAI skips its account picker; only the
// model's own set's sign-in is read for it.
func TestChatGPTSignInAgainHint(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, who := signInChatGPT(t, b, srv, 7, settings.LLM{})
	stored, _ := b.loadChatGPT(id, 7)
	cur := settings.LLM{Account: id, ChatGPTClient: who.ClientID, AccountEmail: who.Email, AccountSubject: who.Subject}
	_, u, _ := b.StartChatGPT(7, cur)
	q, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if stored.IDToken == "" || q.Get("id_token_hint") != stored.IDToken || q.Get("client_id") != who.ClientID || q.Has("prompt") {
		t.Errorf("sign-in again = %v, want the ID token %q as the hint", q, stored.IDToken)
	}
	_, u, _ = b.StartChatGPT(8, cur)
	if q, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1]); q.Has("id_token_hint") {
		t.Errorf("another set's sign-in gave its ID token: %v", q)
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

// TestForgetChatGPT: a sign-in no model names any more is the one to drop,
// and dropping it deletes it and ends it at OpenAI; the others are kept.
func TestForgetChatGPT(t *testing.T) {
	b, st, srv := chatgptBot(t)
	a1, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	a2, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	gone, _ := b.loadChatGPT(a1, 7)
	before := settings.Models{Configs: []settings.LLM{{ID: "m1", Account: a1}, {ID: "m2", Account: a2}, {ID: "m3"}}}
	after := settings.Models{Configs: []settings.LLM{{ID: "m2", Account: a2}, {ID: "m3"}}}
	drop := ForgottenChatGPT(before, after)
	if !slices.Equal(drop, []string{a1}) {
		t.Fatalf("to drop = %v, want [%s]", drop, a1)
	}
	if err := b.DropChatGPT(drop[0], 7); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get(chatgptKey(a1, 7)); ok {
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
		auth := r.Header.Get("Authorization")
		if calls.Add(1) == 1 {
			// The sign-in's token is refused, as if it had been ended early.
			if auth != "Bearer "+first.AccessToken {
				t.Errorf("first request's token = %q, want the sign-in's", auth)
			}
			http.Error(w, `{"error":{"code":"invalid_token"}}`, http.StatusUnauthorized)
			return
		}
		if cur, _ := b.loadChatGPT(id, 7); auth != "Bearer "+cur.AccessToken || cur.AccessToken == first.AccessToken {
			t.Errorf("retry's token = %q, want the renewed one", auth)
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
	if _, err := b.TestModel(context.Background(), cfg, 8); err == nil || !strings.Contains(err.Error(), "sign-in is gone") {
		t.Errorf("another set's model using the sign-in = %v", err)
	}
	cfg.Account = ""
	if _, err := b.TestModel(context.Background(), cfg, 7); err == nil || !strings.Contains(err.Error(), "isn't signed in") {
		t.Errorf("a model with no sign-in = %v", err)
	}
}

// TestChatGPTPlanScopeDropped: a renewal that comes back without the plan's
// use is saved (so signing out ends the live refresh token), and its
// access token isn't used then or after.
func TestChatGPTPlanScopeDropped(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	srv.Scopes = "openid profile email offline_access"
	expire(t, b, id, 7)
	ts := chatgptTokens{b: b, id: id, set: 7}
	for i := range 2 {
		if tok, err := ts.Token(context.Background(), ""); !errors.Is(err, chatgpt.ErrNoPlanUsage) || tok != "" {
			t.Errorf("call %d: %q, %v", i, tok, err)
		}
	}
	if cred, _ := b.loadChatGPT(id, 7); srv.Refreshes != 1 || !srv.Live(cred.RefreshToken) {
		t.Errorf("%d renewals; saved refresh token live: %v", srv.Refreshes, srv.Live(cred.RefreshToken))
	}
}

// TestChatGPTNoReplacement: a renewal without the replacement refresh token
// gives its access token, but the used-up refresh token isn't kept or sent
// again: once that token expires, the sign-in has ended.
func TestChatGPTNoReplacement(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	srv.NoReplacement = true
	expire(t, b, id, 7)
	ts := chatgptTokens{b: b, id: id, set: 7}
	if tok, err := ts.Token(context.Background(), ""); err != nil || tok == "" {
		t.Fatalf("renewal = %q, %v", tok, err)
	}
	if cred, _ := b.loadChatGPT(id, 7); cred.RefreshToken != "" {
		t.Errorf("kept refresh token %q", cred.RefreshToken)
	}
	expire(t, b, id, 7)
	if _, err := ts.Token(context.Background(), ""); !errors.Is(err, chatgpt.ErrSignInEnded) || srv.Refreshes != 1 {
		t.Errorf("after expiry = %v, %d renewals", err, srv.Refreshes)
	}
}

// TestChatGPTUnsavedRenewal: a renewal that couldn't be saved is used (and
// saved) by the next call, and is the one signing out ends at OpenAI.
func TestChatGPTUnsavedRenewal(t *testing.T) {
	b, _, srv := chatgptBot(t)
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	renew := func() chatgpt.Credentials {
		t.Helper()
		cred, _ := b.loadChatGPT(id, 7)
		if err := srv.Client().Refresh(context.Background(), &cred); err != nil {
			t.Fatal(err)
		}
		b.chatgptUnsaved.Store(id, unsavedSignIn{set: 7, cred: cred}) // as when the save fails
		return cred
	}
	cred := renew()
	if _, err := (chatgptTokens{b: b, id: id, set: 8}).Token(context.Background(), ""); !errors.Is(err, errUnreadable) {
		t.Errorf("another set's call = %v", err)
	}
	if tok, err := (chatgptTokens{b: b, id: id, set: 7}).Token(context.Background(), ""); err != nil || tok != cred.AccessToken {
		t.Errorf("token = %q, %v, want the unsaved renewal's", tok, err)
	}
	if saved, _ := b.loadChatGPT(id, 7); saved.RefreshToken != cred.RefreshToken {
		t.Errorf("saved refresh token %q, want %q", saved.RefreshToken, cred.RefreshToken)
	}
	if _, ok := b.chatgptUnsaved.Load(id); ok {
		t.Error("still kept unsaved after saving")
	}

	cred = renew()
	b.DropChatGPT(id, 7)
	if r := waitRevoked(t, srv, 1); !slices.Contains(r, cred.RefreshToken) || srv.Live(cred.RefreshToken) {
		t.Errorf("revoked %v, want the unsaved %q", r, cred.RefreshToken)
	}
}

// TestChatGPTOtherAccount: signing a model's client in again with another
// account is refused and that sign-in ended.
func TestChatGPTOtherAccount(t *testing.T) {
	b, _, srv := chatgptBot(t)
	_, who := signInChatGPT(t, b, srv, 7, settings.LLM{})
	srv.Subject = "user-999"
	cur := settings.LLM{Account: "x", ChatGPTClient: who.ClientID, AccountEmail: who.Email, AccountSubject: who.Subject}
	a, u, _ := b.StartChatGPT(7, cur)
	cb, _ := chatgpt.ParseCallback(srv.Approve(t, u))
	if id, got, err := b.FinishChatGPT(context.Background(), 7, a, cb); !errors.Is(err, chatgpt.ErrOtherAccount) || id != "" || got.Subject != "user-999" {
		t.Errorf("FinishChatGPT = %q, %+v, %v", id, got, err)
	}
	waitRevoked(t, srv, 1)
}

// TestChatGPTRevokeRetried: a revocation OpenAI fails is tried again; one
// never confirmed is said in the set's activity.
func TestChatGPTRevokeRetried(t *testing.T) {
	b, st, srv := chatgptBot(t)
	b.chatgptRetry = []time.Duration{time.Millisecond, time.Millisecond}
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	srv.FailRevokes = 2
	b.DropChatGPT(id, 7)
	waitRevoked(t, srv, 1)

	id, _ = signInChatGPT(t, b, srv, 7, settings.LLM{})
	srv.FailRevokes = 3
	b.DropChatGPT(id, 7)
	for range 200 {
		acts, _ := st.RecentActivity(7, 10, "", 0)
		if len(acts) > 0 {
			if !strings.Contains(acts[0].Message, "didn't confirm") || !strings.Contains(acts[0].Message, "dev@example.com") {
				t.Errorf("activity = %q", acts[0].Message)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("an unconfirmed revocation isn't in the set's activity")
}

// TestChatGPTSignIns: every sign-in of a set is listed, and no other set's.
func TestChatGPTSignIns(t *testing.T) {
	b, st, srv := chatgptBot(t)
	s1, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	s2, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	kept, _ := signInChatGPT(t, b, srv, 8, settings.LLM{})
	ids, want := b.ChatGPTSignIns(7), []string{s1, s2}
	slices.Sort(want) // listed in key order
	if !slices.Equal(ids, want) {
		t.Fatalf("set 7's sign-ins = %v, want %v", ids, want)
	}
	for _, id := range ids {
		b.DropChatGPT(id, 7)
	}
	if keys, _ := st.Keys(chatgptKeys(7)); len(keys) != 0 {
		t.Errorf("set 7's sign-ins left: %v", keys)
	}
	if _, err := b.loadChatGPT(kept, 8); err != nil {
		t.Errorf("set 8's sign-in: %v", err)
	}
	waitRevoked(t, srv, 2)
}

// TestChatGPTUndeletable: a sign-in that can't be deleted is still ended at
// OpenAI, and the error says it's left stored.
func TestChatGPTUndeletable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := chatgpttest.New(t)
	b.chatgptAuth = srv.Client()
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	cred, _ := b.loadChatGPT(id, 7)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON kv WHEN OLD.key LIKE 'chatgpt:sign-in:%' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if err := b.DropChatGPT(id, 7); err == nil || !strings.Contains(err.Error(), "couldn't delete") || !strings.Contains(err.Error(), "still asked to end it") {
		t.Errorf("DropChatGPT = %v, want that it's left stored but ended", err)
	}
	if r := waitRevoked(t, srv, 1); !slices.Contains(r, cred.RefreshToken) {
		t.Errorf("revoked %v, want %q", r, cred.RefreshToken)
	}
}

// TestChatGPTUnsavedThenRenewed: when a renewal kept unsaved still can't be
// saved, is renewed again, and that one saves, the older one is let go of:
// saved later, it would put back a used-up refresh token.
func TestChatGPTUnsavedThenRenewed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b := New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := chatgpttest.New(t)
	b.chatgptAuth = srv.Client()
	id, _ := signInChatGPT(t, b, srv, 7, settings.LLM{})
	expire(t, b, id, 7)

	// Saves of sign-ins fail, as with a full disk, until a renewal lets them.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER no_save BEFORE UPDATE ON kv WHEN NEW.key LIKE 'chatgpt:sign-in:%' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	ts := chatgptTokens{b: b, id: id, set: 7}
	if _, err := ts.Token(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	x, ok := b.chatgptUnsaved.Load(id)
	if !ok {
		t.Fatal("the unsaved renewal wasn't kept")
	}
	older := x.(unsavedSignIn)
	older.cred.Expiry = time.Now().Add(-time.Minute) // due again
	b.chatgptUnsaved.Store(id, older)

	srv.OnRefresh = func() { _, _ = db.Exec(`DROP TRIGGER no_save`) }
	tok, err := ts.Token(context.Background(), "")
	if err != nil || tok == older.cred.AccessToken || srv.Refreshes != 2 {
		t.Fatalf("second renewal = %q, %v after %d renewals", tok, err, srv.Refreshes)
	}
	if _, ok := b.chatgptUnsaved.Load(id); ok {
		t.Error("the older renewal is still kept unsaved")
	}
	if again, err := ts.Token(context.Background(), ""); err != nil || again != tok {
		t.Errorf("next token = %q, %v, want %q", again, err, tok)
	}
	saved, _ := b.loadChatGPT(id, 7)
	if saved.RefreshToken == older.cred.RefreshToken || !srv.Live(saved.RefreshToken) {
		t.Errorf("saved refresh token %q (live %v), the older was %q", saved.RefreshToken, srv.Live(saved.RefreshToken), older.cred.RefreshToken)
	}
	b.DropChatGPT(id, 7)
	if r := waitRevoked(t, srv, 1); !slices.Contains(r, saved.RefreshToken) {
		t.Errorf("revoked %v, want the live %q", r, saved.RefreshToken)
	}
}
