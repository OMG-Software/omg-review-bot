package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openMetricsStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, o := range []struct {
		id    int64
		login string
	}{{1, "acme"}, {2, "globex"}} {
		if err := st.UpsertOrg(o.id, o.login, "Organization", 10+o.id); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// addReview stores a finished review.
func addReview(t *testing.T, st *Store, r Review) int64 {
	t.Helper()
	id, err := st.CreateReview(&r)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	if err := st.UpdateReview(&r); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestModelUsage(t *testing.T) {
	st := openMetricsStore(t)
	now := time.Now().Unix()
	call := func(c ModelCall) {
		t.Helper()
		if c.TS == 0 {
			c.TS = now
		}
		if err := st.RecordModelCall(c); err != nil {
			t.Fatal(err)
		}
	}
	// Review 1 (acme) failed over from m1 to m2; review 2 (acme) went to m2
	// in two parts; m1 also answered a reply. Review 3 (globex) ran on its
	// own m1: a different model from the instance's m1.
	call(ModelCall{AccountID: 1, ReviewID: 1, Kind: CallReview, ConfigID: "m1", Label: "Fast", Model: "small", OK: false})
	call(ModelCall{AccountID: 1, ReviewID: 1, Kind: CallReview, ConfigID: "m2", Label: "Big", Model: "large", OK: true, PromptTokens: 100, CompletionTokens: 10, DurationMS: 400})
	call(ModelCall{AccountID: 1, ReviewID: 2, Kind: CallReview, ConfigID: "m2", Label: "Big", Model: "large", OK: true, PromptTokens: 50, CompletionTokens: 5, DurationMS: 200})
	call(ModelCall{AccountID: 1, ReviewID: 2, Kind: CallReview, ConfigID: "m2", Label: "Big (renamed)", Model: "large", OK: true, PromptTokens: 50, CompletionTokens: 5, DurationMS: 600})
	call(ModelCall{AccountID: 1, Kind: CallReply, ConfigID: "m1", Label: "Fast", Model: "small", OK: true})
	call(ModelCall{AccountID: 2, ReviewID: 3, Kind: CallReview, ModelSet: 2, ConfigID: "m1", Label: "Own", Model: "theirs", OK: true})
	call(ModelCall{AccountID: 1, ReviewID: 9, Kind: CallReview, ConfigID: "m1", Label: "Fast", Model: "small", OK: true, TS: now - 86400*40}) // before the period
	// A review whose record couldn't be created has no ID: its call counts
	// as a call, not as a review.
	call(ModelCall{AccountID: 1, Kind: CallReview, ConfigID: "m2", Label: "Big (renamed)", Model: "large", OK: true, PromptTokens: 0, DurationMS: 400})

	since := now - 86400*30
	acme, err := st.ModelUsageSince(1, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(acme) != 2 {
		t.Fatalf("acme's models = %+v, want m2 and m1", acme)
	}
	big, fast := acme[0], acme[1]
	if big.ConfigID != "m2" || big.Label != "Big (renamed)" || big.Reviews != 2 || big.Answered != 2 || big.Calls != 4 ||
		big.Failed != 0 || big.PromptTokens != 200 || big.CompletionTokens != 20 || big.AvgMS != 400 {
		t.Errorf("m2 = %+v", big)
	}
	if fast.ConfigID != "m1" || fast.Reviews != 1 || fast.Answered != 0 || fast.Replies != 1 || fast.Calls != 2 || fast.Failed != 1 || fast.FailRate() != 50 {
		t.Errorf("m1 = %+v", fast)
	}
	all, err := st.ModelUsageSince(AllAccounts, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("every model = %+v, want 3 (globex's own m1 apart from the instance's)", all)
	}
	for _, m := range all {
		if m.ModelSet == 2 && (m.SetOrg != "globex" || m.Model != "theirs" || m.Reviews != 1) {
			t.Errorf("globex's own model = %+v", m)
		}
	}

	// m2 was edited from an earlier model: its card still counts the review
	// that model was sent.
	call(ModelCall{AccountID: 1, ReviewID: 10, Kind: CallReview, ConfigID: "m2", Label: "Big", Model: "medium", OK: true})
	use, err := st.ConfigUsageSince(0, since)
	if err != nil {
		t.Fatal(err)
	}
	// The same counts as the metrics page's for the same models.
	if u := use["m2"]; u.Reviews != big.Reviews+1 || u.Answered != big.Answered+1 || u.Failed != 0 {
		t.Errorf("instance m2 use = %+v, want its earlier model's review too", u)
	}
	if u := use["m1"]; u.Reviews != fast.Reviews || u.Answered != 0 || u.Replies != 1 || u.Failed != 1 {
		t.Errorf("instance m1 use = %+v (a review it was sent and failed is sent, not answered)", u)
	}
	if len(use) != 2 {
		t.Errorf("instance use = %+v, want m1 and m2 only (globex's own m1 is another set)", use)
	}
}

func TestRepoAndOrgMetrics(t *testing.T) {
	st := openMetricsStore(t)
	// Midday yesterday (UTC), so every review falls on one day, whatever
	// the time the test runs.
	ref := time.Now().UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour)
	now := ref.Unix()
	addReview(t, st, Review{Repo: "acme/web", Number: 1, AccountID: 1, Status: "posted", Event: "APPROVE", StartedAt: now - 100, FinishedAt: now - 50,
		Comments: 2, Notes: 1, Sev: [5]int{1, 0, 2, 0, 0}, Fixed: 1, PromptTokens: 10, CompletionTokens: 1})
	addReview(t, st, Review{Repo: "acme/web", Number: 2, AccountID: 1, Status: "posted", Event: "REQUEST_CHANGES", StartedAt: now - 90, FinishedAt: now - 40,
		Comments: 1, Sev: [5]int{0, 1, 0, 0, 0}})
	addReview(t, st, Review{Repo: "acme/api", Number: 1, AccountID: 1, Status: "posted", Event: "COMMENT", StartedAt: now - 80, FinishedAt: now - 30,
		Comments: 4, Sev: [5]int{0, 0, 0, 1, 3}})
	addReview(t, st, Review{Repo: "acme/api", Number: 2, AccountID: 1, Status: "error", StartedAt: now - 70})
	// A review that chose its verdict but failed to post isn't a verdict.
	addReview(t, st, Review{Repo: "acme/api", Number: 3, AccountID: 1, Status: "error", Event: "APPROVE", StartedAt: now - 65})
	addReview(t, st, Review{Repo: "globex/app", Number: 1, AccountID: 2, Status: "posted", StartedAt: now - 60, FinishedAt: now - 20,
		Comments: 1, Sev: [5]int{1, 0, 0, 0, 0}})

	repos, err := st.RepoStatsSince(1, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Repo != "acme/web" {
		t.Fatalf("acme's repos = %+v, want acme/web (2 reviews) first", repos)
	}
	web, api := repos[0], repos[1]
	if web.Reviews != 2 || web.PRs != 2 || web.Findings != 4 || web.Sev != [5]int{1, 1, 2, 0, 0} || web.Serious != 2 ||
		web.Fixed != 1 || web.FixRate() != 25 || web.Approvals != 1 || web.ChangesRequested != 1 || web.Org != "acme" {
		t.Errorf("acme/web = %+v", web)
	}
	if api.Reviews != 1 || api.Errors != 2 || api.Approvals != 0 || api.Sev != [5]int{0, 0, 0, 1, 3} {
		t.Errorf("acme/api = %+v", api)
	}
	all, err := st.RepoStatsSince(AllAccounts, 0, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("every repo = %+v (%v), want 3", all, err)
	}

	orgs, err := st.OrgMetricsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 2 || orgs[0].Login != "acme" || orgs[0].Posted != 3 || orgs[0].Repos != 2 || orgs[0].Findings != 8 ||
		orgs[0].Sev != [5]int{1, 1, 2, 1, 3} || orgs[0].Errors != 2 || orgs[1].Login != "globex" || orgs[1].Posted != 1 {
		t.Errorf("organizations = %+v", orgs)
	}

	stats, err := st.ReviewStatsSince(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Approvals != 1 || stats.ChangesRequested != 1 || stats.Posted != 3 {
		t.Errorf("acme's stats = %+v", stats)
	}
	days, err := st.ReviewsPerDay(1, 1, ref)
	if err != nil || len(days) != 1 || days[0].Sev != [5]int{1, 1, 2, 1, 3} || days[0].Fixed != 1 {
		t.Errorf("the reviews' day = %+v (%v)", days, err)
	}
}

// TestPostedPerDayByOrg: each organization's posted reviews per day, over
// the same days as ReviewsPerDay, add up to its totals.
func TestPostedPerDayByOrg(t *testing.T) {
	st := openMetricsStore(t)
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	day := func(d, h int) int64 { return time.Date(2026, 3, d, h, 0, 0, 0, time.UTC).Unix() }
	for _, r := range []Review{
		{Repo: "acme/web", Number: 1, AccountID: 1, Status: "posted", StartedAt: day(10, 9)},
		{Repo: "acme/web", Number: 2, AccountID: 1, Status: "posted", StartedAt: day(10, 11)},
		{Repo: "acme/web", Number: 3, AccountID: 1, Status: "error", StartedAt: day(10, 12)}, // not posted
		{Repo: "acme/web", Number: 4, AccountID: 1, Status: "posted", StartedAt: day(9, 23)},
		{Repo: "globex/app", Number: 1, AccountID: 2, Status: "posted", StartedAt: day(10, 1)},
		{Repo: "gone/app", Number: 1, AccountID: 9, Status: "posted", StartedAt: day(8, 0)},  // no stored organization
		{Repo: "acme/web", Number: 5, AccountID: 1, Status: "posted", StartedAt: day(7, 23)}, // before the 3 days
	} {
		addReview(t, st, r)
	}
	got, err := st.PostedPerDayByOrg(3, now)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, c := range got {
		rows = append(rows, fmt.Sprintf("%s %d %q %d", c.Day.Format("2006-01-02"), c.AccountID, c.Login, c.Posted))
	}
	want := `[2026-03-08 9 "" 1 2026-03-09 1 "acme" 1 2026-03-10 1 "acme" 2 2026-03-10 2 "globex" 1]`
	if fmt.Sprint(rows) != want {
		t.Errorf("per day by organization = %v, want %s", rows, want)
	}
	days, err := st.ReviewsPerDay(AllAccounts, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range days {
		n := 0
		for _, c := range got {
			if c.Day.Equal(d.Day) {
				n += c.Posted
			}
		}
		if n != d.Posted {
			t.Errorf("%s: organizations' reviews add up to %d, the day's to %d", d.Day.Format("2006-01-02"), n, d.Posted)
		}
	}
}

func TestRepliesAndFirstReviewLatency(t *testing.T) {
	st := openMetricsStore(t)
	now := time.Now().Unix()
	for _, p := range []PR{
		{Repo: "acme/web", Number: 1, AccountID: 1, Open: true, TrackedAt: now - 1000},
		{Repo: "acme/web", Number: 2, AccountID: 1, Open: true, TrackedAt: now - 1000},
		{Repo: "acme/web", Number: 3, AccountID: 1, Open: true, TrackedAt: now - 1000},
		{Repo: "globex/app", Number: 1, AccountID: 2, Open: true, TrackedAt: now - 1000},
	} {
		if err := st.SavePR(&p); err != nil {
			t.Fatal(err)
		}
	}
	// First reviews 100s, 300s and 900s after tracking; a later round of
	// PR 1 doesn't count, nor does a failed run.
	addReview(t, st, Review{Repo: "acme/web", Number: 1, AccountID: 1, Status: "posted", StartedAt: now - 950, FinishedAt: now - 900})
	addReview(t, st, Review{Repo: "acme/web", Number: 1, AccountID: 1, Status: "posted", StartedAt: now - 20, FinishedAt: now - 10})
	addReview(t, st, Review{Repo: "acme/web", Number: 2, AccountID: 1, Status: "posted", StartedAt: now - 750, FinishedAt: now - 700})
	addReview(t, st, Review{Repo: "acme/web", Number: 3, AccountID: 1, Status: "error", StartedAt: now - 500, FinishedAt: now - 400})
	addReview(t, st, Review{Repo: "acme/web", Number: 3, AccountID: 1, Status: "posted", StartedAt: now - 150, FinishedAt: now - 100})
	addReview(t, st, Review{Repo: "globex/app", Number: 1, AccountID: 2, Status: "posted", StartedAt: now - 10, FinishedAt: now - 5})

	l, err := st.FirstReviewLatency(1, now-3600)
	if err != nil {
		t.Fatal(err)
	}
	if l.PRs != 3 || l.Median != 300 || l.P90 != 900 {
		t.Errorf("acme's latency = %+v, want 3 PRs, median 300s, p90 900s", l)
	}
	if l, _ := st.FirstReviewLatency(1, now-800); l.PRs != 2 {
		t.Errorf("first reviews since 800s ago = %d, want 2 (PR 1's first came earlier)", l.PRs)
	}

	for _, h := range []struct {
		id     int64
		kind   string
		repo   string
		number int
		reply  int64
	}{
		{1, "thread", "acme/web", 1, 11},
		{2, "thread", "acme/web", 2, 12},
		{6, "thread", "acme/web", 2, 12}, // two messages, one reply: counted once
		{3, "mention", "acme/web", 1, 13},
		{4, "thread-skipped", "acme/web", 1, 0},
		{5, "mention", "globex/app", 1, 15},
	} {
		if err := st.MarkHandled(h.id, h.kind, h.repo, h.number, h.reply); err != nil {
			t.Fatal(err)
		}
	}
	if r, err := st.RepliesSince(1, now-60); err != nil || r.Threads != 2 || r.Mentions != 1 || r.Total() != 3 {
		t.Errorf("acme's replies = %+v (%v)", r, err)
	}
	if r, _ := st.RepliesSince(AllAccounts, now-60); r.Total() != 4 {
		t.Errorf("every reply = %+v, want 4", r)
	}
}

func TestPercentile(t *testing.T) {
	for _, c := range []struct {
		in   []int
		p    int
		want int
	}{
		{[]int{5}, 50, 5}, {[]int{5}, 90, 5},
		{[]int{1, 2, 3, 4}, 50, 2}, {[]int{1, 2, 3, 4}, 90, 4},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 90, 9},
	} {
		if got := percentile(c.in, c.p); got != c.want {
			t.Errorf("percentile(%v, %d) = %d, want %d", c.in, c.p, got, c.want)
		}
	}
}
