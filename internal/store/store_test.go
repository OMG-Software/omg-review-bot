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
		`INSERT INTO pulls(repo, number) VALUES('acme/web', 7)`,
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

	if err := st.UpsertRepo(1, 10, 500, "acme/site", true); err != nil { // renamed
		t.Fatal(err)
	}
	if pr("acme/web", 7) != nil {
		t.Error("PR left under the old name")
	}
	if p := pr("acme/site", 7); p == nil || !p.Open || p.ReviewedSHA != "aaa" {
		t.Errorf("PR under the new name = %+v; want it open with its review state", p)
	}
	if rs, _ := st.ListReviews(500, "acme/site", 7, 10); len(rs) != 1 {
		t.Errorf("reviews under the new name = %d, want 1", len(rs))
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
