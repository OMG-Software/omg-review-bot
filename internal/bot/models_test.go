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
