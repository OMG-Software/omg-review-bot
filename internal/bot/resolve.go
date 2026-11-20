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

// blocks reports whether a finding at sev holds back approval. A level the
// bot can't read counts, so an unreadable finding never lets a PR through.
func blocks(st settings.Settings, sev string) bool {
	return settings.SeverityRank(sev) <= settings.SeverityRank(st.Review.ApprovalLevel)
}

func countBlocking(st settings.Settings, threads []botThread) int {
	n := 0
	for _, t := range threads {
		if !t.resolved && blocks(st, t.severity()) {
			n++
		}
	}
	return n
}

func countBlockingFindings(st settings.Settings, fs []finding) int {
	n := 0
	for _, f := range fs {
		if blocks(st, f.Severity) {
			n++
		}
	}
	return n
}

// replyAndResolve replies in a finding's thread, then marks it resolved.
// Without a reply the thread is left open, so a resolution is never silent.
func (b *Bot) replyAndResolve(ctx context.Context, cl *github.Client, pr *store.PR, t *botThread, text string) error {
	if _, err := cl.ReplyToReviewComment(ctx, pr.Repo, pr.Number, t.root.ID, text); err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	if err := cl.ResolveReviewThread(ctx, t.nodeID); err != nil {
		return fmt.Errorf("resolve thread: %w", err)
	}
	t.resolved = true
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
		b.setCurrent("Checking fixes in %s on %s#%d", path, pr.Repo, pr.Number)
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
			reason := strings.TrimSpace(textutil.Truncate(r.Reason, 500))
			text := fmt.Sprintf("✅ Confirmed fixed in `%s`.", short(head))
			if reason != "" {
				text += " " + reason
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

// withdrawApproval dismisses the bot's standing approvals: a later COMMENT
// review doesn't replace an approval on GitHub, so it has to be dismissed.
func (b *Bot) withdrawApproval(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, login string, blocking int) {
	reviews, err := cl.Reviews(ctx, pr.Repo, pr.Number)
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not list reviews to withdraw approval: %v", err)
		return
	}
	msg := fmt.Sprintf("%d unresolved finding(s) at %s or above.", blocking, settings.SeverityInfo(st.Review.ApprovalLevel).Label)
	for _, r := range reviews {
		if r.User.Login != login || r.State != "APPROVED" {
			continue
		}
		if err := cl.DismissReview(ctx, pr.Repo, pr.Number, r.ID, msg); err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not withdraw approval %d: %v", r.ID, err)
			continue
		}
		b.logf("info", pr.Repo, pr.Number, "withdrew approval: %s", msg)
	}
}

// syncApproval keeps the approval in line with the findings between pushes,
// e.g. after a thread is resolved in conversation: it approves the reviewed
// head once nothing at or above the approval level is open, and withdraws an
// approval when something is.
func (b *Bot) syncApproval(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, login string) error {
	if !st.Review.ApprovalOn() || pr.HeadSHA == "" || pr.ReviewedSHA != pr.HeadSHA || !pr.Open {
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
	reviewedHead, approvedHead, approved := false, false, false
	for _, r := range reviews { // oldest first
		if r.User.Login != login {
			continue
		}
		if ns, ok := noteSeverities(r.Body); ok && r.CommitID == pr.HeadSHA {
			notes, reviewedHead = ns, true
		}
		if r.State == "APPROVED" {
			approved = true
			approvedHead = approvedHead || r.CommitID == pr.HeadSHA
		}
	}
	if !reviewedHead {
		return nil // no review was posted for this commit (e.g. nothing reviewable)
	}
	blocking := countBlocking(st, threads)
	for _, s := range notes {
		if blocks(st, s) {
			blocking++
		}
	}
	level := settings.SeverityInfo(st.Review.ApprovalLevel).Label
	switch {
	case blocking == 0 && !approvedHead:
		body := fmt.Sprintf("✅ **Approved:** every finding at %s or above is now resolved.\n<!-- prbot:approval -->", level)
		if _, err := cl.CreateReview(ctx, pr.Repo, pr.Number, github.ReviewRequest{CommitID: pr.HeadSHA, Body: body, Event: "APPROVE"}); err != nil {
			return fmt.Errorf("approve: %w", err)
		}
		b.logf("info", pr.Repo, pr.Number, "approved %s: no unresolved findings at %s or above", short(pr.HeadSHA), level)
	case blocking > 0 && approved:
		b.withdrawApproval(ctx, st, cl, pr, login, blocking)
	}
	return nil
}
