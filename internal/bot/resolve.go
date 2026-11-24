package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// botThread is an inline review thread the bot started (one finding).
type botThread struct {
	root     *github.ReviewComment
	nodeID   string // GraphQL thread ID; "" when unknown
	resolved bool
	replies  []int64 // people's replies since the bot last spoke in the thread (see noteReplies)
}

// noteReplies records, for each thread, the replies people posted since the
// bot last spoke in it. Other bots' replies aren't counted.
func noteReplies(threads []botThread, comments []github.ReviewComment, login string) {
	idx := make(map[int64]int, len(threads))
	for i, t := range threads {
		idx[t.root.ID] = i
	}
	// In the order they were posted: GitHub's IDs only grow, and unlike the
	// times (to the second) they never tie.
	sorted := append([]github.ReviewComment(nil), comments...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, c := range sorted {
		i, ok := idx[c.InReplyToID] // GitHub points replies at the thread's first comment
		if c.InReplyToID == 0 || !ok {
			continue
		}
		switch {
		case c.User.Login == login:
			threads[i].replies = nil
		case isBotUser(c.User):
		default:
			threads[i].replies = append(threads[i].replies, c.ID)
		}
	}
}

func (t botThread) severity() string { return findingSeverity(t.root.Body) }

// botRoots returns the bot's top-level review comments: one per finding.
func botRoots(comments []github.ReviewComment, login string) []github.ReviewComment {
	var out []github.ReviewComment
	for _, c := range comments {
		if c.User.Login == login && c.InReplyToID == 0 {
			out = append(out, c)
		}
	}
	return out
}

// loadBotThreads pairs the bot's findings with their thread state. known is
// false when GitHub's GraphQL API couldn't be read; every thread then counts
// as unresolved, so the bot errs towards not approving.
func (b *Bot) loadBotThreads(ctx context.Context, cl *github.Client, pr *store.PR, mine []github.ReviewComment) (threads []botThread, known bool) {
	threads = make([]botThread, 0, len(mine))
	for i := range mine {
		threads = append(threads, botThread{root: &mine[i]})
	}
	if len(threads) == 0 {
		return threads, true
	}
	ts, err := cl.ReviewThreads(ctx, pr.Repo, pr.Number)
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not read review thread states: %v", err)
		return threads, false
	}
	byRoot := make(map[int64]github.ReviewThread, len(ts))
	for _, t := range ts {
		byRoot[t.RootCommentID] = t
	}
	for i := range threads {
		if t, ok := byRoot[threads[i].root.ID]; ok {
			threads[i].nodeID, threads[i].resolved = t.ID, t.IsResolved
		}
	}
	return threads, true
}

// atOrAbove reports whether a finding at sev reaches level (the approval or
// request-changes level). A level the bot can't read on the finding counts,
// so an unreadable finding never lets a PR through. A level that is off
// (rank 0) is reached by nothing.
func atOrAbove(sev, level string) bool {
	lr := settings.SeverityRank(level)
	return lr > 0 && settings.SeverityRank(sev) <= lr
}

// countOpen counts the unresolved threads at level or above.
func countOpen(level string, threads []botThread) int {
	n := 0
	for _, t := range threads {
		if !t.resolved && atOrAbove(t.severity(), level) {
			n++
		}
	}
	return n
}

// countFindings counts the findings at level or above.
func countFindings(level string, fs []finding) int {
	n := 0
	for _, f := range fs {
		if atOrAbove(f.Severity, level) {
			n++
		}
	}
	return n
}

// countLevels counts the severities (e.g. a review's notes) at level or above.
func countLevels(level string, sevs []string) int {
	n := 0
	for _, s := range sevs {
		if atOrAbove(s, level) {
			n++
		}
	}
	return n
}

// replyAndResolve replies in a finding's thread, then marks it resolved.
// Without a reply the thread is left open, so a resolution is never silent.
// The exception is a thread someone has answered since the bot last spoke
// ("Fixed in …"): that answer already says why it's resolved, so the thread
// is resolved without another comment, and the answer isn't replied to
// later either.
func (b *Bot) replyAndResolve(ctx context.Context, cl *github.Client, pr *store.PR, t *botThread, text string) error {
	if len(t.replies) == 0 {
		if _, err := cl.ReplyToReviewComment(ctx, pr.Repo, pr.Number, t.root.ID, text); err != nil {
			return fmt.Errorf("reply: %w", err)
		}
	}
	if err := cl.ResolveReviewThread(ctx, t.nodeID); err != nil {
		return fmt.Errorf("resolve thread: %w", err)
	}
	t.resolved = true
	for _, id := range t.replies {
		_ = b.store.MarkHandled(id, "thread-fixed", pr.Repo, pr.Number, 0)
	}
	return nil
}

// ---------- fix verification ----------

var verifySchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"results"},
	"properties": map[string]any{
		"results": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"id", "fixed", "reason"},
				"properties": map[string]any{
					"id":     map[string]any{"type": "integer"},
					"fixed":  map[string]any{"type": "boolean"},
					"reason": map[string]any{"type": "string"},
				},
			},
		},
	},
}

type verifyOutput struct {
	Results []struct {
		ID     int64  `json:"id"`
		Fixed  bool   `json:"fixed"`
		Reason string `json:"reason"`
	} `json:"results"`
}

type verifyResult struct {
	fixed int
	usage llm.Response
}

// verifyFixes asks the model whether unresolved findings on files that changed
// since the last review are fixed at head. Each confirmed fix gets a reply in
// its thread, then the thread is resolved. Failures are logged, not returned:
// they must not stop the review itself.
func (b *Bot) verifyFixes(ctx context.Context, st settings.Settings, lc llm.Client, cl *github.Client, pr *store.PR,
	head string, threads []botThread, files []github.PRFile, raw *strings.Builder) (res verifyResult) {
	var open []int
	for i, t := range threads {
		if !t.resolved && t.nodeID != "" {
			open = append(open, i)
		}
	}
	if len(open) == 0 {
		return res
	}
	var changed map[string]bool // nil: check every file
	if pr.ReviewedSHA != "" {
		if pr.ReviewedSHA == head {
			return res // nothing changed since the last review
		}
		if names, err := cl.ChangedFiles(ctx, pr.Repo, pr.ReviewedSHA, head); err == nil {
			changed = make(map[string]bool, len(names))
			for _, n := range names {
				changed[n] = true
			}
		} else {
			b.logf("info", pr.Repo, pr.Number, "comparing %s...%s failed (%v); checking every open finding", short(pr.ReviewedSHA), short(head), err)
		}
	}
	status := map[string]string{}  // path in the PR -> file status
	renamed := map[string]string{} // old path -> new path
	for _, f := range files {
		status[f.Filename] = f.Status
		if f.PreviousFilename != "" {
			renamed[f.PreviousFilename] = f.Filename
		}
	}

	byPath := map[string][]int{}
	var paths []string
	for _, i := range open {
		p := threads[i].root.Path
		if np, ok := renamed[p]; ok {
			p = np
		}
		if changed != nil && !changed[p] && !changed[threads[i].root.Path] {
			continue
		}
		if byPath[p] == nil {
			paths = append(paths, p)
		}
		byPath[p] = append(byPath[p], i)
	}

	resolve := func(i int, text string) {
		t := &threads[i]
		if err := b.replyAndResolve(ctx, cl, pr, t, text); err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not close the finding on %s:%d: %v", t.root.Path, t.root.AnyLine(), err)
			return
		}
		res.fixed++
		b.logf("info", pr.Repo, pr.Number, "resolved fixed finding on %s:%d", t.root.Path, t.root.AnyLine())
	}

	for _, path := range paths {
		idx := byPath[path]
		st0, inPR := status[path]
		if !inPR || st0 == "removed" {
			why := "this file is no longer changed by the pull request"
			if st0 == "removed" {
				why = "the pull request now deletes this file"
			}
			for _, i := range idx {
				resolve(i, fmt.Sprintf("✅ Resolved as of `%s`: %s, so this finding no longer applies.", short(head), why))
			}
			continue
		}
		b.setCurrent(ctx, pr, "Checking fixes in %s on %s#%d", path, pr.Repo, pr.Number)
		content, err := cl.FileContent(ctx, pr.Repo, path, head)
		if err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not read %s to verify fixes: %v", path, err)
			continue
		}
		var issues strings.Builder
		var lines []int
		for _, i := range idx {
			c := threads[i].root
			fmt.Fprintf(&issues, "\n--- Issue id %d (raised at line %d):\n%s\n", c.ID, c.AnyLine(), textutil.Truncate(stripFooter(c.Body), 1500))
			if h := lastLines(c.DiffHunk, 12); h != "" {
				fmt.Fprintf(&issues, "Code it was raised on (diff at the time):\n```diff\n%s\n```\n", h)
			}
			lines = append(lines, c.AnyLine())
		}
		total := (st.LLM.ContextTokens - st.LLM.MaxOutputTokens - 256) * charsPerToken
		budget := total - len(verifySystemPrompt) - issues.Len() - 1000
		if budget < 2000 {
			b.logf("warn", pr.Repo, pr.Number, "model context too small to verify fixes in %s", path)
			continue
		}
		user := fmt.Sprintf("Pull request %s#%d: %s\nFile: %s (latest commit %s)\n\nCurrent file (line numbers on the left):\n```\n%s```\n\nIssues raised earlier on this file:\n%s\nRespond with the JSON object only.",
			pr.Repo, pr.Number, pr.Title, path, short(head), fileView(content, lines, budget), issues.String())
		var out verifyOutput
		usage, rawOut, err := b.chatJSON(ctx, lc, st, verifySchema, "fix_check", verifySystemPrompt, user, &out)
		fmt.Fprintf(raw, "===== fix check: %s =====\n%s\n\n", path, rawOut)
		res.usage.PromptTokens += usage.PromptTokens
		res.usage.CompletionTokens += usage.CompletionTokens
		if err != nil {
			b.logf("warn", pr.Repo, pr.Number, "fix check for %s failed: %v", path, err)
			continue
		}
		want := map[int64]int{}
		for _, i := range idx {
			want[threads[i].root.ID] = i
		}
		for _, r := range out.Results {
			i, ok := want[r.ID]
			if !ok || !r.Fixed {
				continue
			}
			delete(want, r.ID) // one reply per thread even if the model repeats an id
			reason := sanitizeModelText(textutil.Truncate(r.Reason, 500))
			text := fmt.Sprintf("✅ Confirmed fixed in `%s`.", short(head))
			if reason != "" {
				text = afterPrefix(text, reason)
			}
			resolve(i, text)
		}
	}
	return res
}

// stripFooter drops the bot's sub-line and hidden marker from a finding comment.
func stripFooter(body string) string {
	if i := strings.Index(body, "\n\n<sub>"); i >= 0 {
		return body[:i]
	}
	return body
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func sortedUnique(xs []int) []int {
	out := append([]int(nil), xs...)
	sort.Ints(out)
	j := 0
	for i, x := range out {
		if i == 0 || x != out[j-1] {
			out[j] = x
			j++
		}
	}
	return out[:j]
}

// ---------- approval ----------

// standing says which of the bot's earlier reviews to dismiss: a later
// COMMENT review replaces neither an approval nor a change request on
// GitHub, so each has to be dismissed once it no longer holds.
type standing struct {
	withdrawApproval bool // something at the approval level is open
	blocking         int  // how many, for the dismissal message
	liftRequest      bool // nothing at the request-changes level is open
}

// dismissStanding dismisses the bot's approvals and/or change requests, as s
// says. reviews is the PR's review list (oldest first) when the caller
// already has it; nil fetches it.
//
// GitHub keeps each review's own state, so a change request the bot later
// replaced with an approval still reads CHANGES_REQUESTED. Change requests
// are only dismissed when one is in force: the bot's latest approval or
// change request is one, or the approvals that replaced it are being
// withdrawn now.
func (b *Bot) dismissStanding(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, login string, s standing, reviews []github.Review) {
	if !s.withdrawApproval && !s.liftRequest {
		return
	}
	if reviews == nil {
		var err error
		if reviews, err = cl.Reviews(ctx, pr.Repo, pr.Number); err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not list reviews to update the bot's approval or change request: %v", err)
			return
		}
	}
	latest, approvals, requests := "", 0, 0
	for _, r := range reviews {
		if r.User.Login != login {
			continue
		}
		switch r.State {
		case "APPROVED":
			latest = r.State
			approvals++
		case "CHANGES_REQUESTED":
			latest = r.State
			requests++
		}
	}
	withdrawing := s.withdrawApproval && approvals > 0
	lifting := s.liftRequest && requests > 0 && (latest == "CHANGES_REQUESTED" || withdrawing)
	if !withdrawing && !lifting {
		return
	}
	withdraw := fmt.Sprintf("%d unresolved finding(s) at %s or above.", s.blocking, settings.SeverityInfo(st.Review.ApprovalLevel).Label)
	lift := "Changes are no longer requested by this bot."
	if st.Review.RequestChangesOn() {
		lift = fmt.Sprintf("No unresolved findings at %s or above.", settings.SeverityInfo(st.Review.RequestChangesLevel).Label)
	}
	for _, r := range reviews {
		if r.User.Login != login {
			continue
		}
		var msg, what string
		switch {
		case r.State == "APPROVED" && withdrawing:
			msg, what = withdraw, "approval"
		case r.State == "CHANGES_REQUESTED" && lifting:
			msg, what = lift, "change request"
		default:
			continue
		}
		if err := cl.DismissReview(ctx, pr.Repo, pr.Number, r.ID, msg); err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not withdraw %s %d: %v", what, r.ID, err)
			continue
		}
		b.logf("info", pr.Repo, pr.Number, "withdrew %s: %s", what, msg)
	}
}

// syncApproval keeps the approval and change request in line with the
// findings between pushes, e.g. after a thread is resolved in conversation:
// it approves the reviewed head once nothing at or above the approval level
// is open, withdraws an approval when something is, and lifts a change
// request once nothing at or above the request-changes level is open. It
// doesn't request changes between pushes: a thread reopened by hand only
// withdraws the approval, and the next review requests changes again.
func (b *Bot) syncApproval(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, login string) error {
	if (!st.Review.ApprovalOn() && !st.Review.RequestChangesOn()) || pr.HeadSHA == "" || pr.ReviewedSHA != pr.HeadSHA || !pr.Open {
		return nil
	}
	comments, err := cl.ReviewComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	threads, known := b.loadBotThreads(ctx, cl, pr, botRoots(comments, login))
	if !known {
		return nil
	}
	reviews, err := cl.Reviews(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	var notes []string
	reviewedHead, approvedHead := false, false
	for _, r := range reviews { // oldest first
		if r.User.Login != login {
			continue
		}
		if ns, ok := noteSeverities(r.Body); ok && r.CommitID == pr.HeadSHA {
			notes, reviewedHead = ns, true
		}
		if r.State == "APPROVED" && r.CommitID == pr.HeadSHA {
			approvedHead = true
		}
	}
	if !reviewedHead {
		return nil // no review was posted for this commit (e.g. nothing reviewable)
	}
	apOn := st.Review.ApprovalOn()
	blocking := countOpen(st.Review.ApprovalLevel, threads) + countLevels(st.Review.ApprovalLevel, notes)
	requesting := countOpen(st.Review.RequestChangesLevel, threads) + countLevels(st.Review.RequestChangesLevel, notes)
	if apOn && blocking == 0 && !approvedHead {
		// An approval replaces the bot's change request too, so nothing to lift.
		level := settings.SeverityInfo(st.Review.ApprovalLevel).Label
		body := fmt.Sprintf("✅ **Approved:** every finding at %s or above is now resolved.\n<!-- prbot:approval -->", level)
		if _, err := cl.CreateReview(ctx, pr.Repo, pr.Number, github.ReviewRequest{CommitID: pr.HeadSHA, Body: body, Event: "APPROVE"}); err != nil {
			return fmt.Errorf("approve: %w", err)
		}
		b.logf("info", pr.Repo, pr.Number, "approved %s: no unresolved findings at %s or above", short(pr.HeadSHA), level)
		return nil
	}
	b.dismissStanding(ctx, st, cl, pr, login, standing{
		withdrawApproval: apOn && blocking > 0, blocking: blocking,
		liftRequest: requesting == 0,
	}, reviews)
	return nil
}
