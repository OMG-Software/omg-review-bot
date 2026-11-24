package bot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// Tests for what keeps a pull request from going round and round: replies
// left to the fix check, re-reviews kept to what changed, passes that stop at
// the approval level, and one review notice per pull request.

// runRepliesFirst runs the queued jobs with the reply checks first, as when
// a "Fixed in …" reply is seen before the push's review starts, then drains
// whatever they queue.
func (e *reviewEnv) runRepliesFirst(t *testing.T) {
	t.Helper()
	var jobs []job
	for len(e.b.jobs) > 0 {
		jobs = append(jobs, e.b.dequeued(<-e.b.jobs))
	}
	replied := false
	for _, j := range jobs {
		if j.kind == jobReplies {
			replied = true
			e.b.runJob(e.ctx, j)
		}
	}
	if !replied {
		t.Fatal("no reply check was queued for the reply")
	}
	e.lm.mu.Lock()
	answered := len(e.lm.prompts["thread_reply"])
	e.lm.mu.Unlock()
	if answered != 0 || len(e.gh.replies) != 0 {
		t.Fatalf("reply answered before the push's review: %d model replies, posted %q", answered, e.gh.replies)
	}
	for _, j := range jobs {
		if j.kind != jobReplies {
			e.b.runJob(e.ctx, j)
		}
	}
	drain(e.ctx, e.b)
}

// A reply that comes with a push waits for that push's review. If the fix
// check confirms the fix, the thread is resolved with no answer: the reply
// already says why. If it doesn't, the reply is answered after the review.
func TestFixedReplyLeftToFixCheck(t *testing.T) {
	for _, fixed := range []bool{true, false} {
		t.Run(fmt.Sprintf("fixed=%v", fixed), func(t *testing.T) {
			e := newReviewEnv(t, reviewOf(divFinding), noApproval)
			e.b.pollOnce(e.ctx)
			drain(e.ctx, e.b)
			if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 {
				t.Fatalf("first review = %+v", e.gh.reviews)
			}
			const findingID = int64(1000)

			e.push("ccc333", e.gh.patch, e.gh.content, reviewOf(), "2026-01-01T00:10:00Z")
			e.gh.mu.Lock()
			e.gh.comments = append(e.gh.comments, map[string]any{"id": int64(901), "in_reply_to_id": findingID,
				"user": map[string]any{"login": "alice", "type": "User"}, "body": "Fixed in ccc333: the divisor is checked first.",
				"path": "main.go", "line": 11, "created_at": time.Now().Format(time.RFC3339)})
			e.gh.mu.Unlock()
			e.lm.mu.Lock()
			e.lm.verify = fmt.Sprintf(`{"results":[{"id":%d,"fixed":%v,"reason":"The divisor is checked."}]}`, findingID, fixed)
			e.lm.mu.Unlock()
			e.b.pollOnce(e.ctx)
			e.runRepliesFirst(t)

			if len(e.gh.reviews) != 2 || len(e.lm.prompts["fix_check"]) != 1 {
				t.Fatalf("reviews = %d, fix checks = %d; want the push reviewed and its fix checked", len(e.gh.reviews), len(e.lm.prompts["fix_check"]))
			}
			if fixed {
				if len(e.gh.replies) != 0 || len(e.lm.prompts["thread_reply"]) != 0 {
					t.Errorf("replies = %q, model replies = %d; want the thread resolved without a comment", e.gh.replies, len(e.lm.prompts["thread_reply"]))
				}
				if len(e.gh.resolves) != 1 || e.gh.resolves[0] != fmt.Sprintf("T%d", findingID) {
					t.Errorf("resolved threads = %v", e.gh.resolves)
				}
				if !e.st.IsHandled(901) {
					t.Error("the reply wasn't marked handled, so it would be answered later")
				}
				if !strings.Contains(e.gh.reviews[1].Body, "1 earlier finding(s) confirmed fixed") {
					t.Errorf("second review doesn't count the fix:\n%s", e.gh.reviews[1].Body)
				}
				return
			}
			if len(e.gh.replies) != 1 || len(e.lm.prompts["thread_reply"]) != 1 || len(e.gh.resolves) != 0 {
				t.Errorf("replies = %q, model replies = %d, resolved = %v; want the reply answered after the review, the thread open",
					e.gh.replies, len(e.lm.prompts["thread_reply"]), e.gh.resolves)
			}
			if !strings.Contains(e.lm.prompts["thread_reply"][0], "latest commit ccc333") {
				t.Errorf("reply not written against the pushed code:\n%s", e.lm.prompts["thread_reply"][0])
			}
		})
	}
}

// Only people's replies since the bot last spoke count: a thread the bot has
// answered since gets the usual confirmation when it's resolved, as nothing
// after the bot's answer says why.
func TestNoteRepliesSinceBotSpoke(t *testing.T) {
	c := func(id, parent int64, login, kind string) github.ReviewComment {
		return github.ReviewComment{ID: id, InReplyToID: parent, User: github.User{Login: login, Type: kind}, CreatedAt: time.Now()}
	}
	cs := []github.ReviewComment{
		c(1, 0, botLogin, "Bot"),
		c(5, 1, "alice", "User"),
		c(6, 1, botLogin, "Bot"),   // the bot answered: alice's reply is dealt with
		c(7, 1, "ci[bot]", "Bot"),  // other bots don't count
		c(9, 1, "alice", "User"),   // a new reply since
		c(8, 2, "mallory", "User"), // another thread
	}
	threads := []botThread{{root: &cs[0]}}
	noteReplies(threads, cs, botLogin)
	if len(threads[0].replies) != 1 || threads[0].replies[0] != 9 {
		t.Errorf("replies = %v, want [9]", threads[0].replies)
	}
	// In posting order, by ID, whatever the times say.
	cs[2].CreatedAt = time.Now().Add(-time.Hour)
	cs[4].CreatedAt = time.Now().Add(-2 * time.Hour)
	threads = []botThread{{root: &cs[0]}}
	noteReplies(threads, cs, botLogin)
	if len(threads[0].replies) != 1 || threads[0].replies[0] != 9 {
		t.Errorf("replies with skewed times = %v, want [9]", threads[0].replies)
	}
}

// A re-review raises new findings only on the code changed since the last
// review, says so in the prompt and the review, and edits the PR's one
// review notice instead of posting another.
func TestReReviewOnlyOnChangedLines(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), noApproval)
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.gh.posted) != 1 {
		t.Fatalf("first review: %d reviews, notices %q", len(e.gh.reviews), e.gh.posted)
	}

	const added = "@@ -18,3 +20,4 @@ func other() {\n \tx := 0\n+\ty := 2\n \tz := 3"
	oldCode := `{"path":"main.go","line":12,"severity":"P2","title":"Printing the result","body":"Was there before."}`
	newCode := `{"path":"main.go","line":21,"severity":"P2","title":"y is never used","body":"Remove it."}`
	e.push("ccc333", e.gh.patch+"\n"+added, strings.Repeat("// line\n", 30), reviewOf(oldCode, newCode), "2026-01-01T00:10:00Z")
	e.gh.mu.Lock()
	e.gh.compareFiles = []any{map[string]any{"filename": "main.go", "status": "modified", "patch": added}}
	e.gh.mu.Unlock()
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	if len(e.gh.reviews) != 2 {
		t.Fatalf("reviews = %d, want the push re-reviewed", len(e.gh.reviews))
	}
	rv := e.gh.reviews[1]
	if len(rv.Comments) != 1 || rv.Comments[0].Line != 21 || strings.Contains(rv.Body, "Printing the result") {
		t.Errorf("re-review comments = %+v; want only the finding on the changed line 21\n%s", rv.Comments, rv.Body)
	}
	if !strings.Contains(rv.Body, "New findings are only on code changed since the last review (`aaa111`)") {
		t.Errorf("re-review doesn't say what it covered:\n%s", rv.Body)
	}
	prompts := e.lm.prompts["code_review"]
	last := prompts[len(prompts)-1]
	for _, want := range []string{"This is a re-review after new commits", "at commit aaa111", "- main.go: lines 20-22"} {
		if !strings.Contains(last, want) {
			t.Errorf("re-review prompt missing %q:\n%s", want, last)
		}
	}
	if len(e.gh.posted) != 1 || !strings.Contains(e.gh.edited[9001], noticeMarker("ccc333")) {
		t.Errorf("notices posted = %q, first now %q; want the one notice edited for ccc333", e.gh.posted, e.gh.edited[9001])
	}
}

// When nothing in the reviewed code changed since the last review, the
// re-review doesn't ask the model at all, and says why.
func TestReReviewSkipsUnchangedParts(t *testing.T) {
	e := newReviewEnv(t, reviewOf(divFinding), noApproval)
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	asked := len(e.lm.prompts["code_review"])

	e.push("ccc333", e.gh.patch, e.gh.content, reviewOf(printFinding), "2026-01-01T00:10:00Z")
	e.gh.mu.Lock()
	e.gh.compareFiles = []any{map[string]any{"filename": "go.sum", "status": "modified", "patch": "@@ -1 +1 @@\n-y\n+z"}}
	e.gh.mu.Unlock()
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)

	if n := len(e.lm.prompts["code_review"]); n != asked {
		t.Errorf("model asked %d more times; want none, nothing reviewable changed", n-asked)
	}
	if len(e.gh.reviews) != 2 || len(e.gh.reviews[1].Comments) != 0 ||
		!strings.Contains(e.gh.reviews[1].Body, "None of the code reviewed here changed since the last review (`aaa111`)") {
		t.Fatalf("second review = %+v", e.gh.reviews[len(e.gh.reviews)-1])
	}
}

// A review asked for by hand, or a review after one that was never posted,
// looks at the whole pull request.
func TestReReviewScopeOnlyAfterAPostedReview(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		noApproval(cfg)
		cfg.Review.ReviewExisting = false // the first sync records a baseline instead of reviewing
	})
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 0 {
		t.Fatalf("baseline PR reviewed")
	}
	e.push("ccc333", e.gh.patch, e.gh.content, reviewOf(), "2026-01-01T00:10:00Z")
	e.gh.mu.Lock()
	e.gh.compareFiles = []any{map[string]any{"filename": "go.sum", "status": "modified", "patch": "@@ -1 +1 @@\n-y\n+z"}}
	e.gh.mu.Unlock()
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if len(e.gh.reviews) != 1 || len(e.lm.prompts["code_review"]) == 0 || strings.Contains(e.lm.prompts["code_review"][0], "re-review") {
		t.Errorf("first review after a baseline: %d reviews, prompts %q; want a whole review", len(e.gh.reviews), e.lm.prompts["code_review"])
	}
}

// Passes go on while they find something that would hold back approval;
// a pass that finds only lesser issues is the last.
func TestPassesStopAtApprovalLevel(t *testing.T) {
	e := newReviewEnv(t, reviewOf(), func(cfg *settings.Settings) {
		cfg.Review.CommentLevel, cfg.Review.ApprovalLevel, cfg.Review.RequestChangesLevel = "P5", "P3", "P3"
	})
	nit := `{"path":"main.go","line":12,"severity":"P4","title":"Name could be clearer","body":"Rename b."}`
	e.lm.reviews = []string{reviewOf(nit), reviewOf(divFinding)}
	e.b.pollOnce(e.ctx)
	drain(e.ctx, e.b)
	if n := len(e.lm.prompts["code_review"]); n != 1 {
		t.Errorf("model asked %d times; want 1, a P4 alone doesn't call for another pass", n)
	}
	if len(e.gh.reviews) != 1 || len(e.gh.reviews[0].Comments) != 1 || e.gh.reviews[0].Comments[0].Line != 12 {
		t.Errorf("review = %+v; want the P4 posted", e.gh.reviews)
	}
}

func TestPassRank(t *testing.T) {
	rank := settings.SeverityRank
	for _, c := range []struct{ comment, approval, request, want string }{
		{"P5", "P3", "P3", "P3"},
		{"P5", settings.LevelOff, "P2", "P2"},              // no approvals: the change-request level
		{"P4", settings.LevelOff, settings.LevelOff, "P4"}, // neither: the comment level
		{"P3", "P5", "P5", "P3"},                           // never past the comment level
	} {
		var st settings.Settings
		st.Review.CommentLevel, st.Review.ApprovalLevel, st.Review.RequestChangesLevel = c.comment, c.approval, c.request
		if got := passRank(st); got != rank(c.want) {
			t.Errorf("passRank(comment %s, approval %s, request %s) = %d, want %d (%s)", c.comment, c.approval, c.request, got, rank(c.want), c.want)
		}
	}
}
