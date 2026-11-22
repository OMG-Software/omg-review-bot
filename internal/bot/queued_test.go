package bot

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// waitQueued waits for the queued comments being posted, as the worker would.
func waitQueued(b *Bot) {
	b.mu.Lock()
	var pending []*queuedNotice
	for _, qn := range b.notices {
		pending = append(pending, qn)
	}
	b.mu.Unlock()
	for _, qn := range pending {
		<-qn.done
	}
}

// A queued review whose commit has nothing reviewable says so, rather than
// staying "queued" forever.
func TestQueuedNoticeNothingToReview(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"x","comments":[]}`, func(*settings.Settings) {})
	e.gh.mainStatus = "removed" // go.sum is ignored, so nothing is left
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.posted) != 1 || !strings.Contains(e.gh.posted[0], "**Queued for review**") {
		t.Fatalf("posted = %q, want one queued notice", e.gh.posted)
	}
	if got := e.gh.edited[9001]; !strings.Contains(got, "**Nothing to review**") {
		t.Errorf("notice = %q, want it to say there was nothing to review", got)
	}
	if len(e.gh.reviews) != 0 {
		t.Errorf("%d reviews posted, want none", len(e.gh.reviews))
	}
}

// A PR closed while its review is queued gets its notice closed too.
func TestQueuedNoticeClosedBeforeStart(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"x","comments":[]}`, func(*settings.Settings) {})
	e.b.pollOnce(e.ctx) // queues the review
	waitQueued(e.b)
	pr, err := e.st.GetPR("acme/web", 7)
	if err != nil || pr == nil {
		t.Fatalf("PR not tracked: %v", err)
	}
	pr.Open = false
	if err := e.st.SavePR(pr); err != nil {
		t.Fatal(err)
	}
	drain(e.ctx, e.b)
	if len(e.gh.posted) != 1 {
		t.Fatalf("posted = %q, want one queued notice", e.gh.posted)
	}
	got := e.gh.edited[9001]
	if !strings.Contains(got, "**Review skipped**") || !strings.Contains(got, "closed before the review started") ||
		!strings.Contains(got, noticeMarker("aaa111")) {
		t.Errorf("notice = %q, want it to say the PR closed first", got)
	}
	if len(e.gh.reviews) != 0 {
		t.Errorf("%d reviews posted, want none", len(e.gh.reviews))
	}
}

// A queued comment is closed even when the worker can't read the PR's row.
func TestQueuedNoticeClosedWithoutPRRow(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"x","comments":[]}`, func(*settings.Settings) {})
	e.b.pollOnce(e.ctx) // posts #7's queued comment
	waitQueued(e.b)
	// Hand that comment to a job for a PR the store has no row for.
	j7, j99 := job{kind: jobReview, repo: "acme/web", number: 7}, job{kind: jobReview, repo: "acme/web", number: 99}
	e.b.mu.Lock()
	e.b.notices[j99.key()] = e.b.notices[j7.key()]
	delete(e.b.notices, j7.key())
	e.b.mu.Unlock()

	e.b.handleReview(e.ctx, j99)
	if got := e.gh.edited[9001]; !strings.Contains(got, "**Review skipped**") || !strings.Contains(got, "couldn't read") {
		t.Errorf("notice = %q, want it closed", got)
	}
}

// A policy read error keeps a queued comment open only for a review that will
// run again: one for a PR that has closed meanwhile is closed.
func TestQueuedNoticeClosedDespitePolicyError(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"x","comments":[]}`, func(*settings.Settings) {})
	e.b.pollOnce(e.ctx)
	waitQueued(e.b)
	pr, err := e.st.GetPR("acme/web", 7)
	if err != nil || pr == nil {
		t.Fatalf("PR not tracked: %v", err)
	}
	pr.Open = false
	if err := e.st.SavePR(pr); err != nil {
		t.Fatal(err)
	}
	// The policy can't be read for now: its table is out of the way.
	db, err := sql.Open("sqlite", filepath.Join(e.b.dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE orgs RENAME TO orgs_away`); err != nil {
		t.Fatal(err)
	}

	drain(e.ctx, e.b)
	if got := e.gh.edited[9001]; !strings.Contains(got, "**Review skipped**") || !strings.Contains(got, "closed before the review started") {
		t.Errorf("notice = %q, want it closed", got)
	}
}

// A policy read error postpones a review that will run again: its queued
// comment stays open, and the rerun edits that same comment once the policy
// reads.
func TestQueuedNoticeKeptOpenForPostponedReview(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"x","comments":[]}`, func(*settings.Settings) {})
	e.b.pollOnce(e.ctx)
	waitQueued(e.b)
	if pr, _ := e.st.GetPR("acme/web", 7); pr == nil || pr.AccountID == 0 {
		t.Fatalf("PR = %+v, want one with an account, whose policy is read", pr)
	}
	db, err := sql.Open("sqlite", filepath.Join(e.b.dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE orgs RENAME TO orgs_away`); err != nil {
		t.Fatal(err)
	}

	drain(e.ctx, e.b)
	if got, ok := e.gh.edited[9001]; ok {
		t.Fatalf("notice edited to %q, want it left queued for the rerun", got)
	}
	if pr, _ := e.st.GetPR("acme/web", 7); pr == nil || pr.Status != "pending" {
		t.Fatalf("PR = %+v, want it still pending", pr)
	}

	if _, err := db.Exec(`ALTER TABLE orgs_away RENAME TO orgs`); err != nil {
		t.Fatal(err)
	}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.posted) != 1 || len(e.gh.reviews) != 1 || !strings.Contains(e.gh.edited[9001], "**Review posted**") {
		t.Errorf("posted = %q, reviews = %d, notice = %q; want the rerun to post on the same notice",
			e.gh.posted, len(e.gh.reviews), e.gh.edited[9001])
	}
}

// After a restart loses the queue, the review is queued again and reuses
// the commit's queued comment instead of posting a second one.
func TestQueuedNoticeReusedAfterRestart(t *testing.T) {
	e := newReviewEnv(t, `{"summary":"x","comments":[]}`, func(*settings.Settings) {})
	e.b.pollOnce(e.ctx)
	waitQueued(e.b)
	if len(e.gh.posted) != 1 {
		t.Fatalf("posted = %q, want one queued notice", e.gh.posted)
	}

	// A new bot on the same data: the old one's queue is gone.
	b2 := New(e.st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	b2.pollOnce(e.ctx)
	drain(e.ctx, b2)
	if len(e.gh.posted) != 1 {
		t.Errorf("posted %d notices, want the first reused: %q", len(e.gh.posted), e.gh.posted)
	}
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.edited[9001], "**Review posted**") {
		t.Errorf("reviews = %d, notice = %q; want the review posted on the reused notice", len(e.gh.reviews), e.gh.edited[9001])
	}
}
