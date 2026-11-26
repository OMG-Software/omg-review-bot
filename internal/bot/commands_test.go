package bot

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

func TestReviewCommandRegexp(t *testing.T) {
	re := reviewCommandRE("testbot")
	for s, want := range map[string]bool{
		"@testbot review":                                true,
		"@TestBot[bot] Review!":                          true,
		"  @testbot review.  ":                           true,
		"Fixed the division.\r\n\r\n@testbot review\r\n": true,
		"@testbot review\nThanks!":                       true,
		"@testbot review this function please":           false,
		"@testbot reviewing this, why flag main.go?":     false,
		"> @testbot review":                              false,
		"please @testbot review":                         false,
		"@testbot-other review":                          false,
		"@testbotx review":                               false,
		"@testbot\nreview":                               false,
		"`@testbot review` asks for a review, I'm told.": false,
	} {
		if re.MatchString(s) != want {
			t.Errorf("%q: got %v want %v", s, !want, want)
		}
	}
}

// say adds a PR conversation comment by login, made after the PR was tracked.
func (e *reviewEnv) say(login, body string) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.people = append(e.gh.people, map[string]any{
		"id": int64(8001 + len(e.gh.people)), "user": map[string]any{"login": login, "type": "User"},
		"body": body, "created_at": time.Now().Add(time.Minute).Format(time.RFC3339),
	})
}

func TestReviewCommand(t *testing.T) {
	const clean = `{"summary":"Looks fine.","comments":[]}`
	e := newReviewEnv(t, clean, func(*settings.Settings) {})
	e.gh.perms = map[string]string{"alice": "write", "carol": "admin"}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 {
		t.Fatalf("expected the first review, got %d", len(e.gh.reviews))
	}
	pr, err := e.st.GetPR("acme/web", 7)
	if err != nil || pr == nil {
		t.Fatalf("PR not tracked: %v", err)
	}
	check := func() {
		t.Helper()
		if err := e.b.CheckReplies(pr.Repo, pr.Number, pr.AccountID); err != nil {
			t.Fatal(err)
		}
		drain(e.ctx, e.b)
	}
	lastPost := func() string {
		if len(e.gh.posted) == 0 {
			return ""
		}
		return e.gh.posted[len(e.gh.posted)-1]
	}

	// Someone with write access asks: the same commit is reviewed again, and
	// the model isn't asked to chat about the request.
	e.say("alice", "Fixed the division.\r\n\r\n@testbot review\r\n")
	check()
	if len(e.gh.reviews) != 2 || e.gh.reviews[1].CommitID != "aaa111" {
		t.Fatalf("after alice's request: %d reviews, want a second one of aaa111", len(e.gh.reviews))
	}
	if n := len(e.lm.prompts["text"]); n != 0 {
		t.Errorf("the model was asked to answer a review request (%d chat prompts)", n)
	}

	// A request is acted on once.
	perms := e.gh.permCalls
	check()
	if len(e.gh.reviews) != 2 || e.gh.permCalls != perms {
		t.Errorf("alice's request was handled again: %d reviews, %d permission lookups (was %d)", len(e.gh.reviews), e.gh.permCalls, perms)
	}

	// Someone who can only read the repository is told why, and nothing is queued.
	posts := len(e.gh.posted)
	e.say("mallory", "@testbot review")
	check()
	if len(e.gh.reviews) != 2 {
		t.Errorf("mallory's request was reviewed: %d reviews", len(e.gh.reviews))
	}
	if len(e.gh.posted) != posts+1 || !strings.HasPrefix(lastPost(), "@mallory ") || !strings.Contains(lastPost(), "write access") {
		t.Errorf("want one reply to mallory about write access, got %q", e.gh.posted[posts:])
	}
	check()
	if len(e.gh.posted) != posts+1 {
		t.Errorf("mallory was answered again: %q", e.gh.posted[posts:])
	}

	// A request the bot can't take now is answered with the reason.
	e.b.StopReviews()
	e.say("carol", "@testbot review")
	check()
	e.b.ResumeReviews()
	if len(e.gh.reviews) != 2 || !strings.HasPrefix(lastPost(), "@carol I couldn't queue a review: reviews are stopped") {
		t.Errorf("want carol told reviews are stopped, got %d reviews and %q", len(e.gh.reviews), lastPost())
	}

	// A mention that isn't a request is answered as before.
	e.say("alice", "@testbot reviewing this, why flag main.go?")
	check()
	if n := len(e.lm.prompts["text"]); n != 1 || len(e.gh.reviews) != 2 {
		t.Errorf("want the mention answered (1 chat prompt, 2 reviews), got %d prompts and %d reviews", n, len(e.gh.reviews))
	}
}

// A request whose review was queued but that couldn't be recorded as handled
// doesn't queue another review on the next check; it's recorded once it can be.
func TestReviewCommandQueuedOnceWhenNotRecorded(t *testing.T) {
	const clean = `{"summary":"Looks fine.","comments":[]}`
	e := newReviewEnv(t, clean, func(*settings.Settings) {})
	e.gh.perms = map[string]string{"alice": "write"}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	pr, err := e.st.GetPR("acme/web", 7)
	if err != nil || pr == nil || len(e.gh.reviews) != 1 {
		t.Fatalf("expected the first review of a tracked PR: %d reviews, %v", len(e.gh.reviews), err)
	}
	check := func() {
		t.Helper()
		if err := e.b.CheckReplies(pr.Repo, pr.Number, pr.AccountID); err != nil {
			t.Fatal(err)
		}
		drain(e.ctx, e.b)
	}

	db, err := sql.Open("sqlite", filepath.Join(e.b.dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_handled BEFORE INSERT ON handled_comments BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	e.say("alice", "@testbot review")
	check()
	check()
	if len(e.gh.reviews) != 2 {
		t.Fatalf("an unrecorded request was reviewed %d times, want once", len(e.gh.reviews)-1)
	}
	if e.st.IsHandled(8001) {
		t.Fatal("the request was recorded while writes failed")
	}

	if _, err := db.Exec(`DROP TRIGGER fail_handled`); err != nil {
		t.Fatal(err)
	}
	check()
	if !e.st.IsHandled(8001) || len(e.gh.reviews) != 2 {
		t.Errorf("once writes work: recorded %v with %d reviews, want recorded with 2", e.st.IsHandled(8001), len(e.gh.reviews))
	}
	e.b.mu.Lock()
	left := len(e.b.commandsTaken)
	e.b.mu.Unlock()
	if left != 0 {
		t.Errorf("%d requests still held in memory after being recorded", left)
	}
}
