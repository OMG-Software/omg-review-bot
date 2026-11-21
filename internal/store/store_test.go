package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMigrateOldDatabase opens a database made by the single-tenant release
// and checks the new columns are added and old rows linked to their organization.
func TestMigrateOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE repos (id INTEGER PRIMARY KEY, installation_id INTEGER NOT NULL, full_name TEXT NOT NULL,
		  enabled INTEGER NOT NULL DEFAULT 1, synced INTEGER NOT NULL DEFAULT 0, etag TEXT NOT NULL DEFAULT '', last_polled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE pulls (repo TEXT NOT NULL, number INTEGER NOT NULL, installation_id INTEGER NOT NULL DEFAULT 0,
		  title TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '',
		  head_ref TEXT NOT NULL DEFAULT '', draft INTEGER NOT NULL DEFAULT 0, open INTEGER NOT NULL DEFAULT 1, labels TEXT NOT NULL DEFAULT '',
		  head_sha TEXT NOT NULL DEFAULT '', reviewed_sha TEXT NOT NULL DEFAULT '', attempt_sha TEXT NOT NULL DEFAULT '',
		  attempts INTEGER NOT NULL DEFAULT 0, gh_updated_at TEXT NOT NULL DEFAULT '', comments_seen TEXT NOT NULL DEFAULT '',
		  status TEXT NOT NULL DEFAULT 'new', last_error TEXT NOT NULL DEFAULT '', skip INTEGER NOT NULL DEFAULT 0,
		  not_before INTEGER NOT NULL DEFAULT 0, tracked_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (repo, number))`,
		`CREATE TABLE reviews (id INTEGER PRIMARY KEY AUTOINCREMENT, repo TEXT NOT NULL, number INTEGER NOT NULL,
		  head_sha TEXT NOT NULL DEFAULT '', gh_review_id INTEGER NOT NULL DEFAULT 0, url TEXT NOT NULL DEFAULT '',
		  summary TEXT NOT NULL DEFAULT '', comments INTEGER NOT NULL DEFAULT 0, notes INTEGER NOT NULL DEFAULT 0,
		  model TEXT NOT NULL DEFAULT '', skill TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		  raw TEXT NOT NULL DEFAULT '', batches INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
		  completion_tokens INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE activity (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, level TEXT NOT NULL,
		  repo TEXT NOT NULL DEFAULT '', number INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL)`,
		`INSERT INTO repos(id, installation_id, full_name) VALUES(100, 10, 'acme/web')`,
		`INSERT INTO pulls(repo, number, installation_id) VALUES('acme/web', 7, 10)`,
		// A repository since removed, whose name another account has now: not
		// the same installation, so not that account's.
		`INSERT INTO pulls(repo, number, installation_id) VALUES('beta/app', 3, 99)`,
		`INSERT INTO activity(ts, level, repo, message) VALUES(1, 'info', 'acme/web', 'old entry')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// A fixed time, so the review can't land in a different day's bucket
	// than the one the test expects when it runs near midnight UTC.
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO reviews(repo, number, status, comments, started_at) VALUES('acme/web', 7, 'posted', 2, ?)`, at.Unix()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer st.Close()
	// The repository sync records the organization; old rows follow.
	if err := st.UpsertRepo(100, 10, 500, "acme/web", true); err != nil {
		t.Fatal(err)
	}
	if err := st.BackfillAccounts(); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPR("acme/web", 7); p == nil || p.AccountID != 500 {
		t.Errorf("PR not linked: %+v", p)
	}
	if err := st.UpsertRepo(200, 60, 600, "beta/app", true); err != nil {
		t.Fatal(err)
	}
	if err := st.BackfillAccounts(); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPR("beta/app", 3); p == nil || p.AccountID != 0 {
		t.Errorf("a PR of another installation was given to the account that has the name now: %+v", p)
	}
	stats, err := st.ReviewStatsSince(500, at.Add(-time.Hour).Unix())
	if err != nil || stats.Posted != 1 || stats.Comments != 2 {
		t.Errorf("stats = %+v, %v", stats, err)
	}
	if acts, _ := st.RecentActivity(500, 10, "", 0); len(acts) != 1 {
		t.Errorf("activity not linked: %+v", acts)
	}
	if other, err := st.ReviewStatsSince(600, 0); err != nil || other.Reviews != 0 {
		t.Errorf("another organization sees the review: %+v, %v", other, err)
	}
	days, _ := st.ReviewsPerDay(500, 7, at)
	if len(days) != 7 || days[6].Posted != 1 {
		t.Errorf("per-day = %+v", days)
	}
}

// TestFilters covers the optional filters written into the SQL as
// (?=… OR column=?) conditions, so an empty or AllAccounts value matches everything.
func TestFilters(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, r := range []struct {
		id, acct int64
		name     string
	}{{1, 500, "acme/web"}, {2, 500, "acme/api"}, {3, 600, "globex/core"}} {
		if err := st.UpsertRepo(r.id, 10, r.acct, r.name, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []PR{
		{Repo: "acme/web", Number: 1, AccountID: 500, Open: true, Status: "pending"},
		{Repo: "acme/web", Number: 2, AccountID: 500, Open: false, Status: "closed"},
		{Repo: "globex/core", Number: 1, AccountID: 600, Open: true, Status: "reviewed"},
	} {
		if err := st.SavePR(&p); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	for _, r := range []Review{
		{Repo: "acme/web", Number: 1, AccountID: 500, Status: "posted", StartedAt: now},
		{Repo: "acme/web", Number: 1, AccountID: 500, Status: "error", StartedAt: now},
		{Repo: "acme/api", Number: 3, AccountID: 500, Status: "posted", StartedAt: now},
		{Repo: "globex/core", Number: 1, AccountID: 600, Status: "posted", StartedAt: now},
	} {
		if _, err := st.CreateReview(&r); err != nil {
			t.Fatal(err)
		}
	}

	reviews := func(f ReviewFilter) int {
		t.Helper()
		_, n, err := st.QueryReviews(f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct {
		f    ReviewFilter
		want int
	}{
		{ReviewFilter{AccountID: AllAccounts}, 4},
		{ReviewFilter{AccountID: 500}, 3},
		{ReviewFilter{AccountID: 500, Repo: "acme/web"}, 2},
		{ReviewFilter{AccountID: 500, Status: "posted"}, 2},
		{ReviewFilter{AccountID: 500, Repo: "acme/web", Status: "error"}, 1},
		{ReviewFilter{AccountID: 600, Repo: "acme/web"}, 0},
	} {
		if got := reviews(c.f); got != c.want {
			t.Errorf("QueryReviews(%+v) = %d, want %d", c.f, got, c.want)
		}
	}

	if prs, _ := st.ListPRs(500, false, 10); len(prs) != 1 {
		t.Errorf("open acme PRs = %d, want 1", len(prs))
	}
	if prs, _ := st.ListPRs(500, true, 10); len(prs) != 2 {
		t.Errorf("all acme PRs = %d, want 2", len(prs))
	}
	if prs, _ := st.ListPRs(AllAccounts, true, 10); len(prs) != 3 {
		t.Errorf("all PRs = %d, want 3", len(prs))
	}
	if repos, _ := st.ListRepos(500); len(repos) != 2 || repos[0].FullName != "acme/api" {
		t.Errorf("acme repos = %+v", repos)
	}

	st.Log("info", "acme/web", 1, "on the PR")
	st.Log("info", "acme/web", 0, "on the repo")
	st.Log("info", "globex/core", 1, "elsewhere")
	if acts, _ := st.RecentActivity(500, 10, "", 0); len(acts) != 2 {
		t.Errorf("acme activity = %d, want 2", len(acts))
	}
	if acts, _ := st.RecentActivity(500, 10, "acme/web", 1); len(acts) != 1 || acts[0].Message != "on the PR" {
		t.Errorf("PR activity = %+v", acts)
	}

	for _, u := range []struct {
		id    int64
		login string
	}{{41, "zed"}, {42, "amy"}, {43, "bob"}} {
		if err := st.EnsureUser(u.id, u.login); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOrg(600, "globex", "Organization", 11); err != nil {
		t.Fatal(err)
	}
	_ = st.SetMembership(500, 42, RoleMember, SourceManual)
	_ = st.SetMembership(500, 43, RoleMember, SourceManual)
	_ = st.SetMembership(500, 41, RoleOwner, SourceGitHub)
	_ = st.SetMembership(600, 42, RoleOwner, SourceManual)
	ms, _ := st.ListMembers(500)
	var order []string
	for _, m := range ms {
		order = append(order, m.Login)
	}
	if strings.Join(order, ",") != "zed,amy,bob" {
		t.Errorf("members = %v, want owners first then by login", order)
	}
	if ms, _ := st.UserMemberships(42); len(ms) != 2 || ms[0].OrgLogin != "acme" || ms[1].Role != RoleOwner {
		t.Errorf("amy's memberships = %+v", ms)
	}
	if m, _ := st.GetMembership(600, 43); m != nil {
		t.Errorf("bob isn't in globex, got %+v", m)
	}
}

// TestNextDueIgnoresPastReviews: a review that's already due (queued, or in an
// org with reviews off) mustn't make the run loop wake every second.
func TestNextDueIgnoresPastReviews(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Unix()
	for i, nb := range []int64{now - 600, now + 120, now + 30} {
		if err := st.SavePR(&PR{Repo: "acme/web", Number: i + 1, Open: true, Status: "pending", NotBefore: nb}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.NextDue(now); got != now+30 {
		t.Errorf("NextDue = %d, want the next future time %d", got, now+30)
	}
	if got := st.NextDue(now + 200); got != 0 {
		t.Errorf("NextDue with nothing ahead = %d, want 0", got)
	}
}

// TestLastOwnerGuard: the last owner can't be demoted or removed, even when
// two owners demote each other at the same moment.
func TestLastOwnerGuard(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{41, 42} {
		if err := st.EnsureUser(id, fmt.Sprint("u", id)); err != nil {
			t.Fatal(err)
		}
	}
	owners := func() (n int) {
		ms, _ := st.ListMembers(500)
		for _, m := range ms {
			if m.Role == RoleOwner {
				n++
			}
		}
		return n
	}

	for round := 0; round < 20; round++ {
		_ = st.SetMembership(500, 41, RoleOwner, SourceManual)
		_ = st.SetMembership(500, 42, RoleOwner, SourceManual)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, id := range []int64{41, 42} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i == 0 {
					errs[i] = st.SetMemberRole(500, id, RoleMember)
				} else {
					errs[i] = st.RemoveMember(500, id)
				}
			}()
		}
		wg.Wait()
		if n := owners(); n != 1 {
			t.Fatalf("round %d: %d owners left (errors %v), want 1", round, n, errs)
		}
	}

	// Adding is insert-only: re-adding the last remaining owner as a member,
	// while the other owner is removed, can't leave the organization ownerless.
	for round := 0; round < 20; round++ {
		_ = st.SetMembership(500, 41, RoleOwner, SourceManual)
		_ = st.SetMembership(500, 42, RoleOwner, SourceManual)
		var wg sync.WaitGroup
		var added bool
		var addErr, rmErr error
		wg.Add(2)
		go func() { defer wg.Done(); added, addErr = st.AddMember(500, 42, RoleMember) }()
		go func() { defer wg.Done(); rmErr = st.RemoveMember(500, 41) }()
		wg.Wait()
		if added || addErr != nil {
			t.Fatalf("round %d: AddMember on an existing member = %v, %v; want false, nil", round, added, addErr)
		}
		if n := owners(); n != 1 {
			t.Fatalf("round %d: %d owners left (remove error %v), want 1", round, n, rmErr)
		}
	}
	if added, err := st.AddMember(500, 41, RoleMember); !added || err != nil {
		t.Fatalf("AddMember for someone new = %v, %v; want true, nil", added, err)
	}

	// MembershipsByUser groups everyone's organizations in one query.
	if err := st.UpsertOrg(600, "beta", "Organization", 11); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMember(600, 41, RoleOwner); err != nil {
		t.Fatal(err)
	}
	byUser, err := st.MembershipsByUser()
	if err != nil {
		t.Fatal(err)
	}
	if ms := byUser[41]; len(ms) != 2 || ms[0].OrgLogin != "acme" || ms[1].OrgLogin != "beta" || ms[1].Role != RoleOwner {
		t.Errorf("memberships of 41 = %+v; want acme then beta (owner)", ms)
	}
	if ms := byUser[42]; len(ms) != 1 || ms[0].OrgLogin != "acme" {
		t.Errorf("memberships of 42 = %+v; want acme", ms)
	}

	ms, _ := st.ListMembers(500)
	last := ms[0].ID
	if err := st.SetMemberRole(500, last, RoleMember); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demoting the last owner: %v, want ErrLastOwner", err)
	}
	if err := st.RemoveMember(500, last); !errors.Is(err, ErrLastOwner) {
		t.Errorf("removing the last owner: %v, want ErrLastOwner", err)
	}
	if err := st.RemoveMember(500, 999); err != nil {
		t.Errorf("removing someone already gone: %v", err)
	}
}

// TestDayWindowStart: totals and the per-day chart cover the same whole UTC days.
func TestDayWindowStart(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	if got, want := DayWindowStart(30, now), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Unix(); got != want {
		t.Errorf("DayWindowStart(30) = %s, want 1 May midnight", time.Unix(got, 0).UTC())
	}
	if got := DayWindowStart(1, now); got != time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("DayWindowStart(1) = %s, want today's midnight", time.Unix(got, 0).UTC())
	}
}

// TestRepoRenameAndRemoval: a renamed repository keeps its PRs (and their
// review state) and reviews under the new name; one moved to another account,
// or removed from the App, has its PRs closed so none stays due.
func TestRepoRenameAndRemoval(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pr := func(repo string, number int) *PR {
		p, err := st.GetPR(repo, number)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := st.UpsertRepo(1, 10, 500, "acme/web", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePR(&PR{Repo: "acme/web", Number: 7, Open: true, Status: "pending", ReviewedSHA: "aaa", AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(&Review{Repo: "acme/web", Number: 7, Status: "posted", AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	st.Log("info", "acme/web", 7, "reviewed")
	// Left over from an earlier repository that had the new name.
	if err := st.SavePR(&PR{Repo: "acme/site", Number: 7, Open: true, Status: "pending", ReviewedSHA: "old", AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	earlier, err := st.CreateReview(&Review{Repo: "acme/site", Number: 7, Status: "posted", Summary: "earlier", AccountID: 500})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.UpsertRepo(1, 10, 500, "acme/site", true); err != nil { // renamed
		t.Fatal(err)
	}
	if pr("acme/web", 7) != nil {
		t.Error("PR left under the old name")
	}
	if p := pr("acme/site", 7); p == nil || !p.Open || p.ReviewedSHA != "aaa" {
		t.Errorf("PR under the new name = %+v; want it open with its review state", p)
	}
	if rs, _ := st.ListReviews(500, "acme/site", 7, 10); len(rs) != 1 || rs[0].ID == earlier {
		t.Errorf("reviews under the new name = %+v, want only the renamed repository's", rs)
	}
	if rv, _ := st.LatestPostedReview(500, "acme/site", 7); rv == nil || rv.ID == earlier {
		t.Errorf("latest review of the renamed PR = %+v, want the renamed repository's", rv)
	}
	if rv, _ := st.GetReview(earlier); rv == nil || rv.Repo != "acme/site"+EarlierRepoSuffix {
		t.Errorf("the earlier repository's review = %+v, want it kept as history", rv)
	}
	if acts, _ := st.RecentActivity(500, 10, "acme/site", 7); len(acts) != 1 {
		t.Errorf("activity under the new name = %d entries, want 1", len(acts))
	}

	if err := st.UpsertRepo(1, 20, 600, "beta/site", true); err != nil { // moved to another account
		t.Fatal(err)
	}
	if p := pr("acme/site", 7); p == nil || p.Open {
		t.Errorf("PR of a repository moved to another account = %+v; want it kept, closed", p)
	}

	if err := st.SavePR(&PR{Repo: "beta/site", Number: 8, Open: true, Status: "pending", AccountID: 600}); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneRepos(nil); err != nil { // removed from the App
		t.Fatal(err)
	}
	if p := pr("beta/site", 8); p == nil || p.Open {
		t.Errorf("PR of a removed repository = %+v; want it closed", p)
	}
}

// TestOrgSummaries: the per-organization counts, gathered with one query per
// table, land on the right organization.
func TestOrgSummaries(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, o := range []struct {
		id    int64
		login string
	}{{500, "acme"}, {600, "beta"}} {
		if err := st.UpsertOrg(o.id, o.login, "Organization", o.id/10); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{41, 42} {
		if err := st.EnsureUser(id, fmt.Sprint("u", id)); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = st.AddMember(500, 41, RoleOwner)
	_, _ = st.AddMember(500, 42, RoleMember)
	_, _ = st.AddMember(600, 42, RoleOwner)
	_ = st.UpsertRepo(1, 50, 500, "acme/web", true)
	_ = st.UpsertRepo(2, 50, 500, "acme/api", false)
	_ = st.UpsertRepo(3, 60, 600, "beta/app", true)
	_ = st.SavePR(&PR{Repo: "acme/web", Number: 1, Open: true, AccountID: 500})
	_ = st.SavePR(&PR{Repo: "acme/web", Number: 2, Open: false, AccountID: 500})
	now := time.Now().Unix()
	for _, r := range []Review{
		{Repo: "acme/web", Number: 1, Status: "posted", StartedAt: now, AccountID: 500},
		{Repo: "acme/web", Number: 1, Status: "failed", StartedAt: now - 10, AccountID: 500},
		{Repo: "beta/app", Number: 3, Status: "posted", StartedAt: now - 90*24*3600, AccountID: 600}, // outside 30 days
	} {
		if _, err := st.CreateReview(&r); err != nil {
			t.Fatal(err)
		}
	}

	sums, err := st.OrgSummaries(nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[int64]OrgSummary{}
	for _, sm := range sums {
		by[sm.ID] = sm
	}
	a, b := by[500], by[600]
	if a.Members != 2 || a.Owners != 1 || a.Repos != 2 || a.EnabledRepos != 1 || a.OpenPRs != 1 || a.Reviews30d != 1 || a.LastReview != now {
		t.Errorf("acme = %+v", a)
	}
	if b.Members != 1 || b.Owners != 1 || b.Repos != 1 || b.OpenPRs != 0 || b.Reviews30d != 0 || b.LastReview != now-90*24*3600 {
		t.Errorf("beta = %+v", b)
	}
	if only, _ := st.OrgSummaries([]int64{600}); len(only) != 1 || only[0].ID != 600 {
		t.Errorf("OrgSummaries([600]) = %+v", only)
	}
	if _, err := st.countsByAccount(`SELECT 1, 2, 3, 4, 5`); err == nil {
		t.Error("countsByAccount accepted a query with too many columns")
	}
}

// TestAbandonedDeliveryIsRetried: a delivery claimed but never finished (the
// bot stopped mid-way) can be handled again by a redelivery once it's been
// abandoned for a while; a finished one stays a duplicate. Only the handler
// holding the current claim can record the result.
func TestAbandonedDeliveryIsRetried(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Unix()
	first, err := st.RecordDelivery(Delivery{ID: "d1", Event: "ping", ReceivedAt: now})
	if first == 0 || err != nil {
		t.Fatalf("first delivery = %v, %v", first, err)
	}
	// Still being handled, for all anyone knows: a duplicate.
	if c, _ := st.RecordDelivery(Delivery{ID: "d1", Event: "ping", ReceivedAt: now + 60}); c != 0 {
		t.Error("a delivery claimed a minute ago was handled again")
	}
	// Never finished, long ago: handled again, once.
	later := now + int64(abandonedDelivery.Seconds()) + 60
	second, _ := st.RecordDelivery(Delivery{ID: "d1", Event: "ping", ReceivedAt: later})
	if second == 0 {
		t.Fatal("an abandoned delivery wasn't handled again")
	}
	if c, _ := st.RecordDelivery(Delivery{ID: "d1", Event: "ping", ReceivedAt: later + 1}); c != 0 {
		t.Error("the reclaimed delivery was claimed twice")
	}
	// The second handler finishes; the first, still running, fails late.
	if err := st.SetDeliveryResult("d1", second, 0, "pong"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeliveryResult("d1", first, 0, "error: timed out"); err != nil {
		t.Fatal(err)
	}
	// Finished: a duplicate however long after.
	if c, _ := st.RecordDelivery(Delivery{ID: "d1", Event: "ping", ReceivedAt: later + 3600}); c != 0 {
		t.Error("a finished delivery was handled again (a late result from a handler that lost its claim overwrote it)")
	}

	// A failed delivery redelivered within the same second gets a claim of
	// its own, so the earlier handler's claim no longer writes.
	c1, _ := st.RecordDelivery(Delivery{ID: "d2", Event: "ping", ReceivedAt: now})
	if err := st.SetDeliveryResult("d2", c1, 0, "error: boom"); err != nil {
		t.Fatal(err)
	}
	c2, _ := st.RecordDelivery(Delivery{ID: "d2", Event: "ping", ReceivedAt: now})
	if c2 == 0 || c2 == c1 {
		t.Fatalf("claims of a delivery retried in the same second: %d then %d; want two different ones", c1, c2)
	}
	if err := st.SetDeliveryResult("d2", c2, 0, "pong"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeliveryResult("d2", c1, 0, "error: late"); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.RecordDelivery(Delivery{ID: "d2", Event: "ping", ReceivedAt: now + 1}); c != 0 {
		t.Error("the earlier claim's late result overwrote the retry's")
	}
}

// TestGitHubMembershipWritesSpareManualAccess: the writes sign-in uses never
// change or remove access added by hand, even if it was added after the
// membership was read.
func TestGitHubMembershipWritesSpareManualAccess(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureUser(41, "u41"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetGitHubMembership(500, 41, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.GetMembership(500, 41); m == nil || m.Role != RoleOwner || m.Source != SourceGitHub {
		t.Fatalf("GitHub membership = %+v", m)
	}
	// Another owner sets the role by hand.
	if err := st.EnsureUser(42, "u42"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMembership(500, 42, RoleOwner, SourceManual); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMemberRole(500, 41, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := st.SetGitHubMembership(500, 41, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if removed, err := st.RemoveGitHubMembership(500, 41); removed || err != nil {
		t.Errorf("RemoveGitHubMembership on access added by hand = %v, %v; want false, nil", removed, err)
	}
	if m, _ := st.GetMembership(500, 41); m == nil || m.Role != RoleMember || m.Source != SourceManual {
		t.Errorf("access added by hand = %+v; want it left as a manual member", m)
	}
}

// TestActivityAccount: an entry is filed under the organization it's given,
// else the PR's, else the repository's; when a name has belonged to two
// accounts, the newer repository's.
func TestActivityAccount(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// acme/web was acme's (repo 100); the name now belongs to repo 900, globex's.
	for _, r := range []struct{ id, acct int64 }{{100, 500}, {900, 600}} {
		if _, err := st.db.Exec(`INSERT INTO repos(id, installation_id, full_name, account_id) VALUES(?,?,?,?)`,
			r.id, r.acct/10, "acme/web", r.acct); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SavePR(&PR{Repo: "acme/web", Number: 1, Open: true, AccountID: 500}); err != nil {
		t.Fatal(err)
	}
	st.LogFor(500, "info", "", 0, "org event")
	st.Log("info", "acme/web", 1, "PR event")
	st.Log("info", "acme/web", 0, "repo event")
	st.Log("info", "", 0, "instance event")
	want := map[string]int64{"org event": 500, "PR event": 500, "repo event": 600, "instance event": 0}
	acts, err := st.RecentActivity(AllAccounts, 10, "", 0)
	if err != nil || len(acts) != len(want) {
		t.Fatalf("activity = %+v, %v", acts, err)
	}
	for _, a := range acts {
		if a.AccountID != want[a.Message] {
			t.Errorf("%q filed under %d, want %d", a.Message, a.AccountID, want[a.Message])
		}
	}
}

// TestReusedOrgLogin: when another GitHub account takes an earlier
// organization's login, the earlier one keeps its history under a name of
// its own, so each is found by its own login.
func TestReusedOrgLogin(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.DeactivateOrgsExcept(nil); err != nil { // uninstalled
		t.Fatal(err)
	}
	if err := st.UpsertOrg(900, "Acme", "Organization", 20); err != nil { // the login, now another account's
		t.Fatal(err)
	}
	if o, _ := st.GetOrgByLogin("acme"); o == nil || o.ID != 900 {
		t.Errorf("acme = %+v, want the account that has the login now", o)
	}
	old, _ := st.GetOrg(500)
	if old == nil || old.Login != "acme (earlier account 500)" {
		t.Fatalf("earlier organization = %+v", old)
	}
	if o, _ := st.GetOrgByLogin(old.Login); o == nil || o.ID != 500 {
		t.Errorf("earlier organization by its new name = %+v", o)
	}
	// It comes back under its current login if it installs the App again.
	if err := st.UpsertOrg(500, "acme-old", "Organization", 30); err != nil {
		t.Fatal(err)
	}
	if o, _ := st.GetOrg(500); o == nil || o.Login != "acme-old" || !o.Active {
		t.Errorf("earlier organization reinstalled = %+v", o)
	}
}

// TestOrgPolicyError: a policy that can't be read is an error, not "none".
func TestOrgPolicyError(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := st.OrgPolicy(1); p != "" || err != nil {
		t.Errorf("policy of an unknown organization = %q, %v", p, err)
	}
	st.Close()
	if _, err := st.OrgPolicy(1); err == nil {
		t.Error("reading a policy from a closed database reported no error")
	}
}

// TestRenameBeforeBackfill: a repository stored by the single-organization
// release (no account yet) and renamed before the first sync after the
// upgrade keeps its PRs, open and with their review state, under the new name.
func TestRenameBeforeBackfill(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertRepo(1, 10, 0, "acme/web", true); err != nil { // the old release's row
		t.Fatal(err)
	}
	if err := st.SavePR(&PR{Repo: "acme/web", Number: 7, InstallationID: 10, Open: true, Status: "reviewed", ReviewedSHA: "aaa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(&Review{Repo: "acme/web", Number: 7, Status: "posted", StartedAt: time.Now().Unix() + 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRepo(1, 10, 500, "acme/site", true); err != nil { // first sync after the upgrade, renamed
		t.Fatal(err)
	}
	if p, _ := st.GetPR("acme/site", 7); p == nil || !p.Open || p.ReviewedSHA != "aaa" {
		t.Errorf("PR under the new name = %+v; want it moved, open, with its review state", p)
	}
	if p, _ := st.GetPR("acme/web", 7); p != nil {
		t.Errorf("PR left under the old name: %+v", p)
	}
	if err := st.BackfillAccounts(); err != nil {
		t.Fatal(err)
	}
	if rs, _ := st.ListReviews(500, "acme/site", 7, 10); len(rs) != 1 {
		t.Errorf("reviews of the renamed repository = %d, want 1", len(rs))
	}
}

// TestReviewsWithoutBreakdown: reviews recorded before severity and fixed
// counts were are marked as such, and counted, so the statistics can say
// they're missing rather than show zeros; new reviews are detailed.
func TestReviewsWithoutBreakdown(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Unix()
	if _, err := st.db.Exec(`INSERT INTO reviews(repo, number, status, comments, notes, started_at, account_id) VALUES('acme/web', 1, 'posted', 4, 1, ?, 500)`, now); err != nil {
		t.Fatal(err) // as the old release stored it
	}
	id, err := st.CreateReview(&Review{Repo: "acme/web", Number: 2, Status: "posted", StartedAt: now, AccountID: 500})
	if err != nil {
		t.Fatal(err)
	}
	if rv, _ := st.GetReview(id); rv == nil || !rv.Detailed {
		t.Errorf("new review = %+v; want it detailed", rv)
	}
	rs, _ := st.ListReviews(500, "acme/web", 1, 10)
	if len(rs) != 1 || rs[0].Detailed {
		t.Errorf("review from before the upgrade = %+v; want it not detailed", rs)
	}
	stats, err := st.ReviewStatsSince(500, now-60)
	if err != nil || stats.Undetailed != 1 {
		t.Errorf("stats = %+v, %v; want 1 review without a breakdown", stats, err)
	}
}

// TestOrgLoginsUnique: the database itself refuses two organizations under
// one login, and opening a database from before that rule renames any it
// holds (the active one, else the newest, keeps the login).
func TestOrgLoginsUnique(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// As an earlier build left it: no unique index, and duplicates.
	for _, q := range []string{
		`DROP INDEX idx_orgs_login_unique`,
		`INSERT INTO orgs(id, login, active, created_at) VALUES(500, 'acme', 0, 100)`,
		`INSERT INTO orgs(id, login, active, created_at) VALUES(900, 'Acme', 1, 50)`,
		`INSERT INTO orgs(id, login, active, created_at) VALUES(600, 'beta', 0, 100)`,
		`INSERT INTO orgs(id, login, active, created_at) VALUES(700, 'beta', 0, 200)`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for id, want := range map[int64]string{
		900: "Acme", 500: "acme (earlier account 500)", // the active one keeps it
		700: "beta", 600: "beta (earlier account 600)", // else the newest
	} {
		if o, _ := st.GetOrg(id); o == nil || o.Login != want {
			t.Errorf("org %d = %+v, want login %q", id, o, want)
		}
	}
	if _, err := st.db.Exec(`INSERT INTO orgs(id, login) VALUES(800, 'ACME')`); err == nil {
		t.Error("a second organization under a taken login was stored")
	}
}

// TestRenameChainAndSwap: repositories renamed in a chain (a to b while b
// goes to c) or swapped between syncs each keep their own PRs and reviews,
// whichever the sync records first.
func TestRenameChainAndSwap(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.UpsertRepo(1, 10, 500, "acme/a", true))
	must(st.UpsertRepo(2, 10, 500, "acme/b", true))
	reviewOf := map[string]int64{}
	for _, p := range []PR{
		{Repo: "acme/a", Number: 1, Title: "one", Open: true, AccountID: 500},
		{Repo: "acme/b", Number: 1, Title: "two", Open: true, AccountID: 500},
	} {
		must(st.SavePR(&p))
		id, err := st.CreateReview(&Review{Repo: p.Repo, Number: 1, AccountID: 500, Status: "posted", StartedAt: time.Now().Unix()})
		must(err)
		reviewOf[p.Title] = id
	}
	check := func(when string, names map[string]string) {
		t.Helper()
		for name, title := range names {
			if p, _ := st.GetPR(name, 1); p == nil || p.Title != title || !p.Open {
				t.Errorf("%s: PR %s#1 = %+v, want %q's", when, name, p, title)
			}
			rs, _, _ := st.QueryReviews(ReviewFilter{AccountID: 500, Repo: name, Limit: 10})
			if len(rs) != 1 || rs[0].ID != reviewOf[title] {
				t.Errorf("%s: reviews of %s = %+v, want %q's", when, name, rs, title)
			}
		}
	}
	sync := func(names map[int64]string) {
		t.Helper()
		cur := map[int64]RepoTarget{}
		for id, n := range names {
			cur[id] = RepoTarget{FullName: n, AccountID: 500}
		}
		must(st.ParkRenamedRepos(cur))
		must(st.ParkRenamedRepos(cur))     // a sync that stopped after parking, run again
		for _, id := range []int64{1, 2} { // the first takes the second's old name
			must(st.UpsertRepo(id, 10, 500, names[id], true))
		}
	}
	sync(map[int64]string{1: "acme/b", 2: "acme/c"})
	check("after a chain of renames", map[string]string{"acme/b": "one", "acme/c": "two"})
	sync(map[int64]string{1: "acme/c", 2: "acme/b"})
	check("after a swap", map[string]string{"acme/c": "one", "acme/b": "two"})
}

// TestUpdateGitHubMembership changes a GitHub-derived role but never adds
// access, nor touches access added by hand.
func TestUpdateGitHubMembership(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertOrg(500, "acme", "Organization", 10); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{41, 42} {
		if _, err := st.RecordSignIn(id, fmt.Sprint("user", id), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := st.UpdateGitHubMembership(500, 41, RoleOwner); ok || err != nil {
		t.Errorf("update of access that isn't there = %v, %v", ok, err)
	}
	if m, _ := st.GetMembership(500, 41); m != nil {
		t.Errorf("update added access: %+v", m)
	}
	if err := st.SetGitHubMembership(500, 41, RoleMember); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateGitHubMembership(500, 41, RoleOwner); !ok || err != nil {
		t.Errorf("update of GitHub-derived access = %v, %v", ok, err)
	}
	if m, _ := st.GetMembership(500, 41); m == nil || m.Role != RoleOwner {
		t.Errorf("after the update = %+v, want owner", m)
	}
	if _, err := st.AddMember(500, 42, RoleMember); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.UpdateGitHubMembership(500, 42, RoleOwner); ok {
		t.Error("update changed access added by hand")
	}
}

// TestBackfillKeepsGivenActivity: an entry recorded as unassigned on purpose
// (LogAs with 0) stays unassigned after a restart's backfill, and an older
// unassigned entry is matched to a PR only when a stored repository of the
// same installation backs it, and then it is.
func TestBackfillKeepsGivenActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertRepo(999, 30, 900, "acme/web", true); err != nil { // another account's, under the name
		t.Fatal(err)
	}
	if err := st.SavePR(&PR{Repo: "acme/web", Number: 1, Open: true, AccountID: 900, InstallationID: 30}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePR(&PR{Repo: "acme/api", Number: 2, Open: true, AccountID: 900, InstallationID: 31}); err != nil { // no repository backs it
		t.Fatal(err)
	}
	st.LogAs(0, "info", "acme/web", 0, "given: account not known")
	if _, err := st.db.Exec(`INSERT INTO activity(ts, level, repo, number, message, account_id) VALUES(?,?,?,?,?,0)`,
		time.Now().Unix()+1, "info", "acme/api", 0, "legacy: no backing repository"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO activity(ts, level, repo, number, message, account_id) VALUES(?,?,?,?,?,0)`,
		time.Now().Unix()+1, "info", "acme/web", 1, "legacy: backed PR"); err != nil {
		t.Fatal(err)
	}
	if err := st.BackfillAccounts(); err != nil {
		t.Fatal(err)
	}
	acts, _ := st.RecentActivity(900, 50, "", 0)
	if len(acts) != 1 || acts[0].Message != "legacy: backed PR" {
		var got []string
		for _, a := range acts {
			got = append(got, a.Message)
		}
		t.Errorf("filed under account 900 by the backfill = %q, want only the entry of the PR its repository backs", got)
	}
	if acts, _ := st.RecentActivity(0, 50, "", 0); len(acts) != 2 {
		t.Errorf("unassigned entries after the backfill = %d, want 2", len(acts))
	}
}
