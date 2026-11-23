package bot

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// modelAt is a model configuration served at url (an httptest server).
func modelAt(name, url string) settings.LLM {
	c := settings.NewLLM()
	c.Name, c.Provider, c.BaseURL, c.Model = name, "openai", url+"/v1", name
	return c
}

// lastReview returns the newest review record of account 500.
func lastReview(t *testing.T, st *store.Store) store.Review {
	t.Helper()
	rs, _, err := st.QueryReviews(store.ReviewFilter{AccountID: 500, Limit: 1})
	if err != nil || len(rs) == 0 {
		t.Fatalf("no review recorded: %v", err)
	}
	return rs[0]
}

func (l *fakeLLM) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestOwnModelsLiftDailyLimit: an organization at its daily limit adds its
// own model. Its held PR is reviewed at once, on its own model only, and
// those reviews don't count towards the limit. Once the model is removed,
// it's back on the instance's model and the limit.
func TestOwnModelsLiftDailyLimit(t *testing.T) {
	ownPublicOnly = false // the own model is served by httptest, on loopback
	t.Cleanup(func() { ownPublicOnly = true })
	const clean = `{"summary":"Looks fine.","comments":[]}`
	e := newReviewEnv(t, clean, func(cfg *settings.Settings) {
		cfg.Review.ApprovalLevel, cfg.Review.RequestChangesLevel = settings.LevelOff, settings.LevelOff
	})
	gh, st, b, ctx := e.gh, e.st, e.b, e.ctx
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOrgDailyReviews(500, 1); err != nil {
		t.Fatal(err)
	}
	own := &fakeLLM{review: clean}
	ownSrv := httptest.NewServer(own.handler(t))
	t.Cleanup(ownSrv.Close)
	pushes := 0
	push := func() {
		pushes++
		e.push(fmt.Sprintf("head%03d", pushes), gh.patch, gh.content, clean, fmt.Sprintf("2026-01-01T01:%02d:00Z", pushes))
		b.pollOnce(ctx)
		drain(ctx, b)
	}
	status := func() string {
		pr, _ := st.GetPR("acme/web", 7)
		return pr.Status
	}

	b.pollOnce(ctx)
	drain(ctx, b)
	push()
	if len(gh.reviews) != 1 || status() != "daily" || own.count() != 0 {
		t.Fatalf("at the limit: %d reviews, status %q", len(gh.reviews), status())
	}

	// The organization brings its own model (stored as the web UI would,
	// bypassing its public-address check).
	box, err := b.secretBox()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(settings.Models{Configs: []settings.LLM{modelAt("own-model", ownSrv.URL)}})
	sealed, _ := box.Seal(raw, modelsContext(500))
	if err := st.SetOrgModels(500, sealed); err != nil {
		t.Fatal(err)
	}
	instance := e.lm.count()
	b.pollOnce(ctx) // lets the held PR through
	drain(ctx, b)
	push()
	if len(gh.reviews) != 3 || status() != "reviewed" {
		t.Fatalf("with its own model: %d reviews, status %q", len(gh.reviews), status())
	}
	if own.count() == 0 || e.lm.count() != instance {
		t.Errorf("own model calls %d, instance model calls %d → %d: the instance's model was used", own.count(), instance, e.lm.count())
	}
	if r := lastReview(t, st); !r.OwnModel || r.Model != "own-model" {
		t.Errorf("review record = model %q, own %v", r.Model, r.OwnModel)
	}
	if !strings.Contains(gh.reviews[2].Body, "own-model") {
		t.Errorf("review body doesn't name the model:\n%s", gh.reviews[2].Body)
	}
	if q, _ := b.DailyQuota(500, time.Now()); q.Posted != 1 || !q.Own {
		t.Errorf("quota with own model = %+v; own reviews must not count", q)
	}

	// Removing it: the instance's model and the limit again.
	if err := st.SetOrgModels(500, ""); err != nil {
		t.Fatal(err)
	}
	push()
	if len(gh.reviews) != 3 || status() != "daily" {
		t.Fatalf("after removing its own model: %d reviews, status %q", len(gh.reviews), status())
	}
}

// TestOwnModelsUnreadableNotReplaced: if an organization's own models can't
// be read, its review fails rather than running on the instance's model.
func TestOwnModelsUnreadableNotReplaced(t *testing.T) {
	const clean = `{"summary":"Looks fine.","comments":[]}`
	e := newReviewEnv(t, clean, func(*settings.Settings) {})
	if err := e.st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetOrgModels(500, "v1:garbage"); err != nil {
		t.Fatal(err)
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	pr, _ := e.st.GetPR("acme/web", 7)
	if len(e.gh.reviews) != 0 || e.lm.count() != 0 || pr == nil || !strings.Contains(pr.LastError, "own models") {
		t.Fatalf("reviews %d, instance calls %d, PR %+v", len(e.gh.reviews), e.lm.count(), pr)
	}
}

// TestFailover: the first model fails, so the review fails over to the next.
func TestFailover(t *testing.T) {
	var broken atomic.Int32
	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broken.Add(1)
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	t.Cleanup(brokenSrv.Close)
	e := newReviewEnv(t, `{"summary":"Looks fine.","comments":[]}`, func(cfg *settings.Settings) {
		good := cfg.LLM
		cfg.Models = settings.Models{Strategy: settings.StrategyFailover,
			Configs: []settings.LLM{modelAt("broken", brokenSrv.URL), good}}
	})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || broken.Load() == 0 || e.lm.count() == 0 {
		t.Fatalf("reviews %d, broken calls %d, good calls %d", len(e.gh.reviews), broken.Load(), e.lm.count())
	}
	if r := lastReview(t, e.st); r.Model != "test-model" {
		t.Errorf("review recorded model %q, want the one that answered", r.Model)
	}
}

// TestCollate: two models review the same diff and their findings are
// merged: one raised by both is posted once, at the more severe level.
func TestCollate(t *testing.T) {
	second := &fakeLLM{review: `{"summary":"Second view.","comments":[` +
		`{"path":"main.go","line":12,"severity":"P1","title":"Division by zero here","body":"Panics."},` +
		`{"path":"main.go","line":12,"severity":"P4","title":"Print through a logger","body":"Use log."}]}`}
	secondSrv := httptest.NewServer(second.handler(t))
	t.Cleanup(secondSrv.Close)
	e := newReviewEnv(t, `{"summary":"First view.","comments":[`+
		`{"path":"main.go","line":11,"severity":"P2","title":"Division by zero","body":"This always panics."}]}`,
		func(cfg *settings.Settings) {
			cfg.Review.ApprovalLevel, cfg.Review.RequestChangesLevel = settings.LevelOff, settings.LevelOff
			first := cfg.LLM
			cfg.Models = settings.Models{Strategy: settings.StrategyCollate,
				Configs: []settings.LLM{first, modelAt("second", secondSrv.URL)}}
		})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("%d reviews posted", len(e.gh.reviews))
	}
	rv := e.gh.reviews[0]
	if len(rv.Comments) != 2 {
		t.Fatalf("collated comments = %d, want 2: %+v", len(rv.Comments), rv.Comments)
	}
	if !strings.Contains(rv.Comments[0].Body, "P1") || !strings.Contains(rv.Comments[0].Body, "Division by zero") {
		t.Errorf("shared finding not kept at the more severe level: %s", rv.Comments[0].Body)
	}
	if r := lastReview(t, e.st); r.Model != "test-model + second" {
		t.Errorf("review recorded model %q", r.Model)
	}
}

func TestCollateFindings(t *testing.T) {
	p := plan{files: map[string]*diff.File{"a.go": {}, "b.go": {}}}
	got := collateFindings(p, [][]finding{
		{{Path: "a.go", Line: 10, Severity: "P3", Title: "Nil map write"}, {Path: "a.go", Line: 40, Severity: "P2", Title: "Leak"}},
		{{Path: "./a.go", Line: 12, Severity: "P1", Title: "nil map write panics"}, {Path: "b.go", Line: 10, Severity: "P3", Title: "Nil map write"},
			{Path: "a.go", Line: 41, Severity: "P2", Title: "Unrelated thing"}},
	})
	if len(got) != 4 || got[0].Severity != "P1" || got[0].Path != "a.go" {
		t.Errorf("collated = %+v", got)
	}
	if one := [][]finding{{{Path: "x", Title: "t"}, {Path: "x", Title: "t"}}}; len(collateFindings(p, one)) != 2 {
		t.Error("a single model's findings were changed")
	}
}

// TestOrgSlotShare: one organization's jobs hold at most its share of the
// workers; the rest wait, and take a worker its own jobs free.
func TestOrgSlotShare(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	j := func(n int, org int64) job { return job{kind: jobReview, repo: "acme/web", number: n, org: org} }
	a1, a2, a3, o1 := j(1, 500), j(2, 500), j(3, 500), j(4, 600)
	for _, c := range []struct {
		j    job
		want bool
	}{{a1, true}, {a2, true}, {a3, false}, {o1, true}} {
		if got := b.claimPR(c.j, 2); got != c.want {
			t.Errorf("claim #%d = %v, want %v", c.j.number, got, c.want)
		}
	}
	if next, ok := b.nextForPR(a1); !ok || next.number != 3 {
		t.Fatalf("a freed worker of acme's takes %v %v, want its held #3", next, ok)
	}
	if _, ok := b.nextForPR(a2); ok {
		t.Fatal("nothing was held, yet a job was handed on")
	}
	if b.orgBusy[500] != 1 || b.orgBusy[600] != 1 {
		t.Errorf("busy = %v", b.orgBusy)
	}
	// An untracked job (org 0) isn't limited.
	if !b.claimPR(j(5, 0), 0) {
		t.Error("an untracked PR's job was held")
	}
}

// TestFailoverPastBadJSONAndBrokenModels: a model that answers, but never
// with the JSON asked for, fails over to the next once its tries are used
// up; one that can't be built at all (a malformed address) is left out and
// doesn't stop the others.
func TestFailoverPastBadJSONAndBrokenModels(t *testing.T) {
	chatty := &fakeLLM{review: "Sure! Here is my review, in prose."}
	chattySrv := httptest.NewServer(chatty.handler(t))
	t.Cleanup(chattySrv.Close)
	e := newReviewEnv(t, `{"summary":"Looks fine.","comments":[]}`, func(cfg *settings.Settings) {
		good := cfg.LLM
		malformed := modelAt("malformed", "https://[broken")
		cfg.Models = settings.Models{Strategy: settings.StrategyFailover,
			Configs: []settings.LLM{malformed, modelAt("chatty", chattySrv.URL), good}}
	})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || chatty.count() != 2 || e.lm.count() == 0 {
		t.Fatalf("reviews %d, chatty calls %d (want its 2 tries), good calls %d", len(e.gh.reviews), chatty.count(), e.lm.count())
	}
	if r := lastReview(t, e.st); r.Model != "chatty → test-model" {
		t.Errorf("review recorded model %q", r.Model)
	}
}

// TestOwnModelsWithoutInstanceModels: the instance's models aren't set up,
// yet an organization with its own is watched and reviewed; one without is
// left until the instance's are.
func TestOwnModelsWithoutInstanceModels(t *testing.T) {
	ownPublicOnly = false
	t.Cleanup(func() { ownPublicOnly = true })
	const clean = `{"summary":"Looks fine.","comments":[]}`
	e := newReviewEnv(t, clean, func(cfg *settings.Settings) {
		cfg.LLM.Model = ""
	})
	if err := e.st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if e.b.pollOnce(e.ctx); !e.b.Status().Configured || len(e.b.Status().ConfigIssues) == 0 {
		t.Fatalf("status = %+v: GitHub is set up, and the instance's model issue should still be listed", e.b.Status())
	}
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 0 || e.lm.count() != 0 {
		t.Fatalf("reviewed on unset instance models: reviews %d, calls %d", len(e.gh.reviews), e.lm.count())
	}
	// A push's webhook only wakes the run loop, whose enqueueDue skips it too.
	e.push("bbb222", e.gh.patch, e.gh.content, clean, "2026-01-01T00:10:00Z")
	ev := &github.WebhookEvent{Action: "synchronize", Repository: &github.Repo{ID: 100, FullName: "acme/web"},
		PullRequest: &github.PullRequest{Number: 7, State: "open", UpdatedAt: "2026-01-01T00:10:00Z", Head: github.Ref{SHA: "bbb222"}}}
	e.b.webhookPull(e.b.Settings(), ev)
	e.b.enqueueDue(e.b.Settings())
	drain(e.ctx, e.b)
	if pr, _ := e.st.GetPR("acme/web", 7); len(e.gh.reviews) != 0 || e.lm.count() != 0 || pr == nil || pr.LastError != "" {
		t.Fatalf("a webhook started a review on unset instance models: reviews %d, calls %d, PR %+v", len(e.gh.reviews), e.lm.count(), pr)
	}
	own := &fakeLLM{review: clean}
	ownSrv := httptest.NewServer(own.handler(t))
	t.Cleanup(ownSrv.Close)
	box, err := e.b.secretBox() // stored as the web UI would, without its public-address check
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(settings.Models{Configs: []settings.LLM{modelAt("own", ownSrv.URL)}})
	sealed, _ := box.Seal(raw, modelsContext(500))
	if err := e.st.SetOrgModels(500, sealed); err != nil {
		t.Fatal(err)
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || own.count() == 0 || e.lm.count() != 0 {
		t.Fatalf("reviews %d, own calls %d, instance calls %d", len(e.gh.reviews), own.count(), e.lm.count())
	}
}

// TestOrgSlotHandoff: a job that waited for its PR keeps the worker's place
// in the share it was claimed in, even if the PR has moved to another
// account; and a held PR's jobs run in the order they were taken.
func TestOrgSlotHandoff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	pr := func(n int, kind jobKind, org int64) job {
		return job{kind: kind, repo: "acme/web", number: n, org: org}
	}

	// #1 runs for acme (500); a job for it arrives as the repo moves to 600.
	first := pr(1, jobReview, 500)
	if !b.claimPR(first, 1) || b.claimPR(pr(1, jobReplies, 600), 1) {
		t.Fatal("claims")
	}
	next, ok := b.nextForPR(first)
	if !ok || next.org != 500 {
		t.Fatalf("waiting job = %+v %v, want it counted to acme's place", next, ok)
	}
	if _, ok := b.nextForPR(next); ok || len(b.orgBusy) != 0 {
		t.Fatalf("places left taken: %v", b.orgBusy)
	}

	// acme is at its one place with #2; #3's reply then review are held, in
	// that order. When #2 finishes, #3's reply runs, and a newer reply for #3
	// must wait behind the held review.
	two := pr(2, jobReview, 500)
	reply, review, later := pr(3, jobReplies, 500), pr(3, jobReview, 500), pr(3, jobReplies, 500)
	later.force = true // tells it apart from the first reply
	if !b.claimPR(two, 1) || b.claimPR(reply, 1) || b.claimPR(review, 1) {
		t.Fatal("claims")
	}
	got, ok := b.nextForPR(two)
	if !ok || got.kind != jobReplies || got.number != 3 {
		t.Fatalf("handed %+v", got)
	}
	b.claimPR(later, 1)
	if got, ok = b.nextForPR(got); !ok || got.kind != jobReview {
		t.Fatalf("after the held reply came %+v, want the held review", got)
	}
	if got, ok = b.nextForPR(got); !ok || got.kind != jobReplies || !got.force {
		t.Fatalf("then %+v, want the later reply", got)
	}
	if _, ok = b.nextForPR(got); ok || len(b.orgBusy) != 0 || len(b.orgHeld) != 0 {
		t.Fatalf("busy %v held %v", b.orgBusy, b.orgHeld)
	}
}

// TestJobClientKeptForTheJob: a reply job's calls share one client, so a
// failover lasts the job and the job takes one round-robin turn, not one
// per thread or mention.
func TestJobClientKeptForTheJob(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := settings.Defaults()
	cfg.Models = settings.Models{Strategy: settings.StrategyFailover,
		Configs: []settings.LLM{modelAt("a", "http://a.example"), modelAt("b", "http://b.example")}}
	cfg.Normalize()
	mc := &jobClient{b: b, st: cfg}
	first, err := mc.get()
	if err != nil {
		t.Fatal(err)
	}
	f, ok := first.(*failover)
	if !ok {
		t.Fatalf("client is %T, want a failover", first)
	}
	f.skip(fmt.Errorf("down")) // a fails during the first thread
	again, _ := mc.get()
	if again != first || again.Label() != "b" {
		t.Errorf("second call got %v (%s), want the same failover, now on b", again, again.Label())
	}

	cfg.Models.Strategy = settings.StrategyRoundRobin
	rr := &jobClient{b: b, st: cfg}
	x, _ := rr.get()
	y, _ := rr.get()
	if x.Label() != y.Label() || b.roundRobin[0] != 1 {
		t.Errorf("round-robin turns taken: %d (%s, %s), want 1", b.roundRobin[0], x.Label(), y.Label())
	}
}

// TestBadJSONRetriesPerModel: A answers in prose, then fails outright on
// the retry, so B takes over within that call. B's first answer is prose
// too; it still gets its own retry, and its second answer is used.
func TestBadJSONRetriesPerModel(t *testing.T) {
	const valid = `{"summary":"Looks fine.","comments":[]}`
	serve := func(answers ...string) (*httptest.Server, *atomic.Int32) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := int(calls.Add(1))
			if n > len(answers) || answers[n-1] == "" {
				http.Error(w, "down", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": answers[n-1]}, "finish_reason": "stop"}},
			})
		}))
		t.Cleanup(srv.Close)
		return srv, &calls
	}
	aSrv, aCalls := serve("Here is my review, in prose.")
	bSrv, bCalls := serve("Also prose.", valid)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := settings.Defaults()
	cfg.Models = settings.Models{Strategy: settings.StrategyFailover,
		Configs: []settings.LLM{modelAt("a", aSrv.URL), modelAt("b", bSrv.URL)}}
	cfg.Normalize()
	lc, err := b.chatClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out modelOutput
	if _, _, err := b.chatJSON(t.Context(), lc, cfg, reviewSchema, "code_review", "sys", "user", &out); err != nil {
		t.Fatalf("chatJSON: %v (a calls %d, b calls %d)", err, aCalls.Load(), bCalls.Load())
	}
	if out.Summary != "Looks fine." || bCalls.Load() != 2 || lc.Label() != "a → b" {
		t.Errorf("summary %q, b calls %d, label %q", out.Summary, bCalls.Load(), lc.Label())
	}
}

// TestRoundRobin: each job takes the next model in turn, cycling through
// the set; the instance's set and each organization's own keep separate
// turns, and a shorter set still gives a model in range.
func TestRoundRobin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	set := func(own int64, names ...string) settings.Settings {
		cfg := settings.Defaults()
		cfg.Models = settings.Models{Strategy: settings.StrategyRoundRobin}
		for _, n := range names {
			cfg.Models.Configs = append(cfg.Models.Configs, modelAt(n, "https://"+n+".example"))
		}
		cfg.Normalize()
		if own != 0 {
			cfg = cfg.WithOwnModels(own, cfg.Models)
		}
		return cfg
	}
	turn := func(cfg settings.Settings) string {
		lc, err := b.chatClient(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return lc.Label()
	}
	inst, acme := set(0, "a", "b", "c"), set(500, "x", "y")
	var got []string
	for range 4 {
		got = append(got, turn(inst), turn(acme))
	}
	if want := "a x b y c x a y"; strings.Join(got, " ") != want {
		t.Errorf("turns = %v, want %s", got, want)
	}
	// The instance's set shrinks to two after seven turns: still in range.
	if l := turn(set(0, "a", "b")); l != "a" && l != "b" {
		t.Errorf("shorter set gave %q", l)
	}
}

// TestReleaseRequeuesHeld: a worker stopping gives back its place; if it was
// its organization's last, the jobs its share held go back on the queue,
// since no worker of that organization is left to hand them on.
func TestReleaseRequeuesHeld(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	j := func(n int) job { return job{kind: jobReview, repo: "acme/web", number: n, org: 500} }
	if !b.claimPR(j(1), 1) || b.claimPR(j(2), 1) {
		t.Fatal("claims")
	}
	b.releasePR(j(1))
	var back []int
	for len(b.jobs) > 0 {
		back = append(back, (<-b.jobs).number)
	}
	if fmt.Sprint(back) != "[1 2]" || len(b.orgHeld) != 0 || len(b.orgBusy) != 0 {
		t.Errorf("queued again %v, held %v, busy %v", back, b.orgHeld, b.orgBusy)
	}
}

// TestQueuedJobsCarryTheirOrg: a job is queued with its PR's account, so a
// worker taking it needn't look it up while every take waits.
func TestQueuedJobsCarryTheirOrg(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"Looks fine.","comments":[]}`, func(*settings.Settings) {})
	e.b.pollOnce(e.ctx) // queues the PR's review; no worker runs it here
	if len(e.b.jobs) == 0 {
		t.Fatal("nothing queued")
	}
	if j := <-e.b.jobs; j.org != 500 {
		t.Errorf("queued job org = %d, want 500", j.org)
	}
}
