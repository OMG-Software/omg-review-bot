package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

var errSkip = errors.New("review skipped")
var errAlreadyReviewed = errors.New("commit already reviewed")

// Conservative estimate: code tokenizes denser than prose.
const charsPerToken = 3

type finding struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

type modelOutput struct {
	Summary  string    `json:"summary"`
	Comments []finding `json:"comments"`
}

var reviewSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"summary", "comments"},
	"properties": map[string]any{
		"summary": map[string]any{"type": "string"},
		"comments": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"path", "line", "severity", "title", "body"},
				"properties": map[string]any{
					"path":     map[string]any{"type": "string"},
					"line":     map[string]any{"type": "integer"},
					"severity": map[string]any{"type": "string", "enum": severityIDs()},
					"title":    map[string]any{"type": "string"},
					"body":     map[string]any{"type": "string"},
				},
			},
		},
	},
}

func (b *Bot) handleReview(ctx context.Context, j job) {
	// Before prMu: the queued comment may still be being posted.
	qn := b.takeQueued(ctx, j)
	b.prMu.Lock()
	pr, err := b.store.GetPR(j.repo, j.number)
	manualRetry := pr != nil && pr.Status == "pending" && pr.ForceRetry
	force := j.force || manualRetry
	waiting := !j.force && manualRetry && pr.NotBefore > time.Now().Unix()
	var st settings.Settings
	policyErr := false
	if err == nil && pr != nil {
		if st, err = b.policySettings(b.Settings(), pr.AccountID); err != nil {
			// Not reviewed with the wrong policy. enqueueDue tries again, but
			// not a manual review if automatic ones are off: that's retried
			// here.
			policyErr = true
			b.logf("warn", pr.Repo, pr.Number, "review postponed: couldn't read the organization's policy: %v", err)
			if j.force && pr.AccountID == j.account {
				b.retryForced(j)
			}
		}
	}
	if err != nil || pr == nil || !pr.Open || pr.Status == "reviewing" || waiting || !allowedRepo(st, pr.Repo) ||
		(j.force && pr.AccountID != j.account) || // the name passed to another account since it was asked for
		(!force && (pr.Status != "pending" || !st.Review.Enabled)) {
		b.prMu.Unlock()
		// A review postponed by a policy error is queued again and edits the
		// same comment, and one already under way owns it. Every other way of
		// ending here is final for this job, so the comment says so.
		switch {
		case pr == nil:
			// Unreadable or no longer tracked. If it's due again once it can be
			// read, the new queued comment reuses this one.
			b.closeQueued(qn, "The review couldn't start: the bot couldn't read this pull request's record.")
		case pr.Status == "reviewing" || waiting:
		case policyErr && pr.Open && allowedRepo(st, pr.Repo) &&
			((j.force && pr.AccountID == j.account) || // retryForced queues it again
				(!j.force && pr.Status == "pending")): // enqueueDue does, once the policy reads
		default:
			b.closeQueued(qn, notReviewedReason(st, pr, j))
		}
		return
	}
	if !force { // a review asked for by hand goes past the limit
		if rounds, over := b.overRoundLimit(pr, st); over {
			pr.Status = "limit"
			_ = b.store.SavePR(pr)
			b.prMu.Unlock()
			b.logf("info", pr.Repo, pr.Number, "not reviewing %s: the bot has reviewed this PR %d times, its limit", short(pr.HeadSHA), rounds)
			b.closeQueued(qn, "This pull request has reached the most reviews the bot posts automatically.")
			b.announceLimit(ctx, st, pr, rounds)
			return
		}
	}
	if q, full := b.dailyCapped(pr); full {
		b.holdForDailyLimit(pr, q)
		b.prMu.Unlock()
		b.closeQueued(qn, "This organization has reached its daily review limit. The review will run automatically once there's room.")
		b.announceDailyLimit(ctx, st, pr, q)
		return
	}
	pr.Status = "reviewing"
	pr.LastError = ""
	_ = b.store.SavePR(pr)
	b.prMu.Unlock()

	b.setCurrent(ctx, pr, "Reviewing %s#%d", pr.Repo, pr.Number)
	fetchedPull, err := b.reviewPR(ctx, st, pr, qn, force)
	if qn != nil && !qn.claimed {
		// The review stopped before it could take the comment over (GitHub
		// unreachable, say, or the PR closed); the status below says whether
		// it will be retried.
		defer func() {
			switch cur, getErr := b.store.GetPR(j.repo, j.number); {
			case errors.Is(err, errSkip):
				b.closeQueued(qn, "The pull request was closed before the review started.")
			case errors.Is(err, errAlreadyReviewed):
				b.closeQueued(qn, "This commit has already been reviewed.")
			case getErr != nil:
				// Whether it will be retried isn't known, so it isn't said.
				b.closeQueued(qn, "The review couldn't start.")
			case cur != nil && cur.Status == "pending":
				b.closeQueued(qn, "The review couldn't start. It will be tried again automatically.")
			default:
				b.closeQueued(qn, "The review couldn't start. It won't be retried until the next push, or until someone asks for a review in the bot.")
			}
		}()
	}

	b.prMu.Lock()
	defer b.prMu.Unlock()
	cur, _ := b.store.GetPR(j.repo, j.number)
	if cur == nil || cur.AccountID != pr.AccountID {
		// Gone, or the name passed to another account while reviewing: this
		// result isn't that PR's.
		return
	}
	cur.Status = "" // leave "reviewing" so recompute can decide
	cur.ForceRetry = false
	switch {
	case errors.Is(err, errSkip):
		b.recompute(cur, st)
	case err != nil && !errors.Is(err, errAlreadyReviewed):
		cur.LastError = err.Error()
		if cur.AttemptSHA != cur.HeadSHA {
			cur.AttemptSHA, cur.Attempts = cur.HeadSHA, 0
		}
		cur.Attempts++
		if cur.Attempts < maxAttempts {
			cur.Status = "pending"
			cur.ForceRetry = force && cur.Open
			cur.NotBefore = time.Now().Add(time.Duration(cur.Attempts*cur.Attempts) * 2 * time.Minute).Unix()
			b.logf("warn", cur.Repo, cur.Number, "review failed (attempt %d/%d, will retry): %v", cur.Attempts, maxAttempts, err)
		} else {
			cur.Status = "error"
			b.logf("error", cur.Repo, cur.Number, "review failed after %d attempts: %v", cur.Attempts, err)
		}
	default:
		head := fetchedPull.Head.SHA
		if cur.HeadSHA != head && cur.HeadSHA == pr.HeadSHA &&
			(cur.GHUpdatedAt == pr.GHUpdatedAt || newerOnGitHub(fetchedPull.UpdatedAt, cur.GHUpdatedAt)) {
			// A poll or webhook may have updated PR metadata for the old head.
			// Only replace that head if the worker has a newer snapshot.
			cur.HeadSHA, cur.GHUpdatedAt = head, fetchedPull.UpdatedAt
		}
		cur.ReviewedSHA = head
		cur.Attempts = 0
		cur.LastError = ""
		cur.Status = "reviewed"
		b.recompute(cur, st) // the head may have moved while we were reviewing
	}
	_ = b.store.SavePR(cur)
	// The run loop may be asleep until a deadline worked out before this
	// one (a retry, say) existed.
	b.kickDue()
}

// notReviewedReason says why a queued review didn't run, for its comment.
func notReviewedReason(st settings.Settings, pr *store.PR, j job) string {
	switch {
	case !pr.Open:
		return "The pull request was closed before the review started."
	case !allowedRepo(st, pr.Repo):
		return "The repository is paused, so it isn't reviewed."
	case j.force && pr.AccountID != j.account:
		return "The pull request now belongs to another organization."
	case !st.Review.Enabled && !j.force:
		return "Automatic reviews were turned off before the review started."
	}
	return "This commit doesn't need a review any more."
}

// retryForced queues a manual review again once its organization's policy
// couldn't be read, waiting twice as long each time, up to half an hour.
func (b *Bot) retryForced(j job) {
	j.tries++
	wait := b.policyRetry
	for i := 1; i < j.tries && wait < 30*time.Minute; i++ {
		wait *= 2
	}
	b.requeueForced(j, min(wait, 30*time.Minute))
}

// requeueForced queues manual review j after wait, and keeps trying while
// the queue is full, so the request isn't dropped.
func (b *Bot) requeueForced(j job, wait time.Duration) {
	time.AfterFunc(wait, func() {
		if b.push(j) == queueFull {
			b.requeueForced(j, wait)
		}
	})
}

// announceReview posts the review-started notice (see announceNotice).
func (b *Bot) announceReview(ctx context.Context, cl *github.Client, pr *store.PR, n reviewNotice) int64 {
	return b.announceNotice(ctx, cl, pr, n.head, n.startedBody())
}

// announceNotice posts a review notice for commit head, or, when that commit
// already has one, edits it to body, so retries and reviews cut short by a
// restart don't pile up comments (or leave one saying a review is queued or
// under way). It returns the comment's ID (0 if it couldn't be posted).
func (b *Bot) announceNotice(ctx context.Context, cl *github.Client, pr *store.PR, head, body string) int64 {
	cs, err := cl.IssueComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		// This commit may have a notice already (from before a restart), and
		// a second would leave that one saying a review is under way. The
		// review goes ahead without one.
		b.logf("warn", pr.Repo, pr.Number, "could not check for an earlier review notice, so none is posted: %v", err)
		return 0
	}
	if id := findNotice(cs, b.BotLogin(), head); id != 0 {
		// Even if it can't be edited now, it's this commit's notice: a
		// second one would leave this one behind, never updated again.
		// The edit when the review ends tries again.
		if err := cl.UpdateIssueComment(ctx, pr.Repo, id, body); err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not update the review notice: %v", err)
		}
		return id
	}
	c, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, body)
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not post the review notice: %v", err)
		return 0
	}
	return c.ID
}

// overRoundLimit reports how many reviews the bot has posted on the PR, and
// whether that's as many as it may post automatically. If they can't be
// counted, it isn't over: a review too many beats a PR never reviewed.
func (b *Bot) overRoundLimit(pr *store.PR, st settings.Settings) (rounds int, over bool) {
	rounds, err := b.store.PostedReviews(pr.AccountID, pr.Repo, pr.Number)
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not count the bot's reviews of this PR: %v", err)
		return 0, false
	}
	return rounds, rounds >= st.Review.MaxRounds
}

// announceLimit posts the review-limit notice, once per limit: a limit that
// is raised and reached again gets a notice of its own.
func (b *Bot) announceLimit(ctx context.Context, st settings.Settings, pr *store.PR, rounds int) {
	app, err := b.getApp(ctx, st)
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not post the review-limit comment: %v", err)
		return
	}
	cl := app.Installation(pr.InstallationID)
	cs, err := cl.IssueComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		// Without the list, a second notice could be posted; leave it out.
		b.logf("warn", pr.Repo, pr.Number, "could not check for an earlier review-limit comment, so none is posted: %v", err)
		return
	}
	marker := limitMarker(st.Review.MaxRounds)
	for _, c := range cs {
		if strings.EqualFold(c.User.Login, b.BotLogin()) && strings.Contains(c.Body, marker) {
			return
		}
	}
	if _, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, limitBody(rounds, st.Review.MaxRounds)); err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not post the review-limit comment: %v", err)
	}
}

// updateNotice edits the review notice, even if the review was cancelled.
func (b *Bot) updateNotice(cl *github.Client, pr *store.PR, id int64, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.UpdateIssueComment(ctx, pr.Repo, id, body); err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not update the review notice: %v", err)
	}
}

type batch struct {
	text  string
	paths []string
}

type plan struct {
	files    map[string]*diff.File
	batches  []batch
	skipped  []string
	reviewed int
}

// buildPlan filters the PR files and packs their annotated hunks into
// batches that each fit the model's context budget (in characters).
func buildPlan(st settings.Settings, files []github.PRFile, budget int) plan {
	p := plan{files: map[string]*diff.File{}}
	ignore := compileGlobs(st.Review.IgnorePatterns)
	type unit struct{ path, text string }
	var units []unit
	for _, f := range files {
		switch {
		case f.Status == "removed":
			p.skipped = append(p.skipped, f.Filename+" (deleted)")
			continue
		case matchAny(ignore, f.Filename):
			p.skipped = append(p.skipped, f.Filename+" (ignored by pattern)")
			continue
		case f.Patch == "":
			p.skipped = append(p.skipped, f.Filename+" (binary or too large for a diff)")
			continue
		case p.reviewed >= st.Review.MaxFiles:
			p.skipped = append(p.skipped, f.Filename+" (file limit reached)")
			continue
		}
		df := diff.Parse(f.Filename, f.Patch)
		if df.Empty() {
			continue
		}
		p.files[f.Filename] = df
		p.reviewed++
		header := fmt.Sprintf("### File: %s (%s, +%d -%d)\n", f.Filename, f.Status, f.Additions, f.Deletions)
		if f.PreviousFilename != "" {
			header = fmt.Sprintf("### File: %s (renamed from %s, +%d -%d)\n", f.Filename, f.PreviousFilename, f.Additions, f.Deletions)
		}
		cur, has := header, false
		for i := range df.Hunks {
			ht := df.Hunks[i].Render()
			if len(header)+len(ht) > budget {
				ht = textutil.Truncate(ht, budget-len(header)-64) + "\n(hunk truncated)\n"
			}
			if has && len(cur)+len(ht) > budget {
				units = append(units, unit{f.Filename, cur})
				cur, has = header+"(continued)\n", false
			}
			cur += ht
			has = true
		}
		if has {
			units = append(units, unit{f.Filename, cur})
		}
	}
	var cur batch
	for _, u := range units {
		if cur.text != "" && len(cur.text)+len(u.text) > budget {
			p.batches = append(p.batches, cur)
			cur = batch{}
		}
		cur.text += u.text + "\n"
		if len(cur.paths) == 0 || cur.paths[len(cur.paths)-1] != u.path {
			cur.paths = append(cur.paths, u.path)
		}
	}
	if cur.text != "" {
		p.batches = append(p.batches, cur)
	}
	return p
}

// reviewablePaths lists the PR files the model will actually see, which
// decides the language-specific skill references to include.
func reviewablePaths(st settings.Settings, files []github.PRFile) []string {
	ignore := compileGlobs(st.Review.IgnorePatterns)
	paths := []string{} // non-nil: an empty list means "no languages", not "all"
	for _, f := range files {
		if f.Status != "removed" && f.Patch != "" && !matchAny(ignore, f.Filename) {
			paths = append(paths, f.Filename)
		}
	}
	return paths
}

func listOrNone(ss []string) string {
	if len(ss) == 0 {
		return "(none)"
	}
	return strings.Join(ss, ", ")
}

// reviewPR fetches the current PR and reviews it. Automatic reviews skip an
// already reviewed head and return the fetch so the tracker can catch up.
// qn is the review's queued comment (nil if none): reviewPR claims it once it
// can see the review through, and turns it into the review notice.
func (b *Bot) reviewPR(ctx context.Context, st settings.Settings, pr *store.PR, qn *queuedNotice, force bool) (reviewed *github.PullRequest, err error) {
	app, err := b.getApp(ctx, st)
	if err != nil {
		return nil, err
	}
	cl := app.Installation(pr.InstallationID)
	gp, err := cl.Pull(ctx, pr.Repo, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("fetch PR: %w", err)
	}
	if gp.State != "open" {
		return nil, errSkip
	}
	head := gp.Head.SHA
	if head == "" {
		return nil, errors.New("fetched PR has no head SHA")
	}
	if head != pr.HeadSHA && newerOnGitHub(pr.GHUpdatedAt, gp.UpdatedAt) {
		return nil, fmt.Errorf("fetched PR head %s is older than the tracked head %s", short(head), short(pr.HeadSHA))
	}
	if !force && head == pr.ReviewedSHA {
		b.logf("info", pr.Repo, pr.Number, "skipping automatic review of %s: commit already reviewed", short(head))
		return gp, errAlreadyReviewed
	}
	sk := b.loadSkill(st)

	rec := &store.Review{Repo: pr.Repo, Number: pr.Number, HeadSHA: head, Model: st.LLM.Model, Skill: sk.Name,
		Status: "running", StartedAt: time.Now().Unix(), AccountID: pr.AccountID}
	if id, err := b.store.CreateReview(rec); err == nil {
		rec.ID = id
	}
	notice := reviewNotice{head: head, model: st.LLM.Model, skill: sk.Name, started: time.Unix(rec.StartedAt, 0)}
	var noticeID int64
	if qn != nil {
		noticeID, qn.claimed = qn.id, true // from here on, the deferred edit below keeps it current
	}
	var raw strings.Builder
	defer func() {
		switch {
		case noticeID == 0:
		case err == nil && rec.Status == "nothing to review":
			b.updateNotice(cl, pr, noticeID, notice.nothingBody())
		case err == nil:
			// So the notice doesn't go on saying a review is under way.
			b.updateNotice(cl, pr, noticeID, notice.postedBody(rec.URL))
		case errors.Is(err, errSkip):
			// Not returned once the notice is up today, but if it ever is,
			// the notice shouldn't stay "under way" either.
			b.updateNotice(cl, pr, noticeID, notice.skippedBody())
		default:
			// Same count handleReview uses to decide on a retry, from the PR
			// as stored now: pr is from before the fetch, and a push since
			// may have changed the head the attempts are counted against.
			attempts := 1
			if cur, _ := b.store.GetPR(pr.Repo, pr.Number); cur != nil && cur.AttemptSHA == cur.HeadSHA {
				attempts += cur.Attempts
			}
			b.updateNotice(cl, pr, noticeID, notice.failedBody(attempts < maxAttempts))
		}
		rec.FinishedAt = time.Now().Unix()
		rec.Raw = textutil.Truncate(raw.String(), 500_000)
		switch {
		case errors.Is(err, errSkip):
			rec.Status = "skipped"
		case err != nil:
			rec.Status, rec.Error = "error", err.Error()
		}
		if rec.ID != 0 {
			_ = b.store.UpdateReview(rec)
		}
	}()

	files, err := cl.PullFiles(ctx, pr.Repo, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("list PR files: %w", err)
	}
	lc, err := llm.New(llmConfig(st))
	if err != nil {
		return nil, err
	}
	existing, err := cl.ReviewComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("list review comments: %w", err)
	}
	login := b.BotLogin()
	mine := botRoots(existing, login)
	threads, threadsKnown := b.loadBotThreads(ctx, cl, pr, mine)

	// Context budget: the skill gets up to 35% of the input window and the
	// diff gets what's left after the system prompt and PR header.
	totalChars := (st.LLM.ContextTokens - st.LLM.MaxOutputTokens - 256) * charsPerToken
	skillText, refs, truncated := sk.Prompt(reviewablePaths(st, files), totalChars*35/100)
	if truncated {
		b.logf("warn", pr.Repo, pr.Number, "skill %q was truncated to fit a %d-token context", sk.Name, st.LLM.ContextTokens)
	}
	// The project's own docs come from the base commit: a PR can't change the rules it is judged by.
	changedPaths := make([]string, 0, len(files))
	for _, f := range files {
		changedPaths = append(changedPaths, f.Filename)
	}
	baseRef := gp.Base.SHA
	if baseRef == "" {
		baseRef = gp.Base.Ref
	}
	guide, docsUsed := b.guidelinesFor(ctx, st, cl, pr.Repo, pr.Number, baseRef,
		fmt.Sprintf("%s at %s", gp.Base.Ref, short(baseRef)), changedPaths, 15)
	fmt.Fprintf(&raw, "skill references sent: %s\nproject guidelines sent: %s\n\n", listOrNone(refs), listOrNone(docsUsed))
	system := reviewSystemPrompt(sk.Name, skillText) + guide
	header := prHeader(gp)
	budget := totalChars - len(system) - len(header) - 3000
	if budget < 2000 {
		return nil, fmt.Errorf("model context (%d tokens) is too small for the skill and PR description; raise 'Context tokens' in Settings or shorten the skill", st.LLM.ContextTokens)
	}
	p := buildPlan(st, files, budget)
	rec.Batches = len(p.batches)
	if len(p.batches) > 0 {
		// Only when there's code to review, so a PR with nothing reviewable
		// doesn't get a notice that's never followed by a review. A queued
		// review's comment becomes the notice.
		if noticeID != 0 {
			b.updateNotice(cl, pr, noticeID, notice.startedBody())
		} else {
			noticeID = b.announceReview(ctx, cl, pr, notice)
		}
	}

	// First check whether earlier findings have since been fixed: confirmed
	// ones get a reply and are resolved, and no longer block approval.
	fixed := 0
	if threadsKnown {
		vu := b.verifyFixes(ctx, st, lc, cl, pr, head, threads, files, &raw)
		fixed = vu.fixed
		rec.Fixed = fixed
		rec.PromptTokens += vu.usage.PromptTokens
		rec.CompletionTokens += vu.usage.CompletionTokens
	}

	if len(p.batches) == 0 {
		rec.Status = "nothing to review"
		rec.Summary = "No reviewable changes (all files were deleted, binary or ignored)."
		b.logf("info", pr.Repo, pr.Number, "nothing to review at %s", short(head))
		if threadsKnown {
			// No review is posted, but the fix check may have resolved what
			// the bot's change request was about (say, by the PR deleting the
			// file), and nothing else would lift it: syncApproval only acts
			// on a commit the bot posted a review for.
			b.dismissStanding(ctx, st, cl, pr, login, standing{
				liftRequest: countOpen(st.Review.RequestChangesLevel, threads) == 0 && pr.ReviewedSHA != "",
			}, nil)
		}
		return gp, nil
	}

	var all []finding
	var summaries []string
	for i, bt := range p.batches {
		if len(p.batches) > 1 {
			b.setCurrent(ctx, pr, "Reviewing %s#%d (part %d/%d)", pr.Repo, pr.Number, i+1, len(p.batches))
		}
		user := batchPrompt(header, i, len(p.batches), bt, previousFor(mine, bt.paths))
		var out modelOutput
		usage, rawOut, err := b.chatJSON(ctx, lc, st, reviewSchema, "code_review", system, user, &out)
		fmt.Fprintf(&raw, "===== part %d/%d =====\n%s\n\n", i+1, len(p.batches), rawOut)
		rec.PromptTokens += usage.PromptTokens
		rec.CompletionTokens += usage.CompletionTokens
		if err != nil {
			return nil, fmt.Errorf("model (part %d/%d): %w", i+1, len(p.batches), err)
		}
		if s := strings.TrimSpace(out.Summary); s != "" {
			summaries = append(summaries, s)
		}
		all = append(all, out.Comments...)
	}

	inline, notes, dropped := prepareFindings(st, p, all, mine)
	var summary string
	switch len(summaries) {
	case 0:
		summary = "_The model did not produce a summary._"
	case 1:
		summary = summaries[0]
	default:
		summary = b.synthesizeSummary(ctx, lc, st, gp, summaries, append(append([]finding{}, inline...), notes...))
	}
	summary = sanitizeModelText(summary)

	// Request changes while anything at or above the request-changes level is
	// open; otherwise approve when nothing at or above the approval level is:
	// no unresolved earlier thread and no new finding.
	open := func(level string) int {
		return countOpen(level, threads) + countFindings(level, inline) + countFindings(level, notes)
	}
	blocking, requesting := open(st.Review.ApprovalLevel), open(st.Review.RequestChangesLevel)
	event := reviewEvent(st, blocking, requesting)
	ri := reviewInfo{summary: summary, head: head, inline: inline, notes: notes, plan: p, dropped: dropped,
		model: st.LLM.Model, skill: sk.Name, rereview: pr.ReviewedSHA != "" && pr.ReviewedSHA != head, fixed: fixed,
		maxRounds: st.Review.MaxRounds}
	if n, err := b.store.PostedReviews(pr.AccountID, pr.Repo, pr.Number); err == nil {
		ri.round = n + 1 // this one isn't "posted" yet
	}
	post := func(ri reviewInfo, event string) (*github.Review, []github.DraftComment, error) {
		ri.verdict = verdictLine(st, event, blocking, requesting)
		drafts := make([]github.DraftComment, 0, len(ri.inline))
		for _, f := range ri.inline {
			drafts = append(drafts, github.DraftComment{Path: f.Path, Line: f.Line, Side: "RIGHT", Body: commentBody(f)})
		}
		req := github.ReviewRequest{CommitID: head, Body: reviewBody(ri), Event: event, Comments: drafts}
		rv, err := cl.CreateReview(ctx, pr.Repo, pr.Number, req)
		return rv, drafts, err
	}
	rv, drafts, err := post(ri, event)
	if err != nil && github.IsStatus(err, 422) && len(ri.inline) > 0 {
		// GitHub rejected an inline position (e.g. the PR moved on). Post everything in the body instead.
		b.logf("warn", pr.Repo, pr.Number, "inline comments rejected (%v); posting findings in the review body", err)
		ri.notes = append(append([]finding{}, ri.inline...), ri.notes...)
		ri.inline = nil
		rv, drafts, err = post(ri, event)
	}
	if err != nil && event != "COMMENT" && (github.IsStatus(err, 422) || github.IsStatus(err, 403)) {
		// e.g. GitHub doesn't let an account approve or request changes on its own PR.
		what := map[string]string{"APPROVE": "the approval", "REQUEST_CHANGES": "the change request"}[event]
		b.logf("warn", pr.Repo, pr.Number, "GitHub refused %s (%v); posting the review as a comment", what, err)
		event = "COMMENT"
		rv, drafts, err = post(ri, event)
	}
	if err != nil {
		return nil, fmt.Errorf("post review: %w", err)
	}
	// A change request can only be standing if the bot reviewed the PR before,
	// so a first review skips listing the reviews just to find none.
	b.dismissStanding(ctx, st, cl, pr, login, standing{
		withdrawApproval: st.Review.ApprovalOn() && blocking > 0, blocking: blocking,
		liftRequest: requesting == 0 && event != "APPROVE" && pr.ReviewedSHA != "",
	}, nil)
	rec.GHReviewID, rec.URL = rv.ID, rv.HTMLURL
	rec.Summary, rec.Comments, rec.Notes, rec.Status = summary, len(drafts), len(ri.notes), "posted"
	rec.Event = event
	for _, f := range append(append([]finding{}, ri.inline...), ri.notes...) {
		if r := settings.SeverityRank(f.Severity); r > 0 {
			rec.Sev[r-1]++
		}
	}
	b.logf("info", pr.Repo, pr.Number, "posted review of %s (%s): %d inline comments, %d notes, %d earlier findings resolved as fixed (%d parts, %d+%d tokens)",
		short(head), strings.ToLower(event), len(drafts), len(ri.notes), fixed, len(p.batches), rec.PromptTokens, rec.CompletionTokens)
	return gp, nil
}

// reviewEvent picks the review's event from the open findings: blocking at
// the approval level, requesting at the request-changes level.
func reviewEvent(st settings.Settings, blocking, requesting int) string {
	switch {
	case st.Review.RequestChangesOn() && requesting > 0:
		return "REQUEST_CHANGES"
	case st.Review.ApprovalOn() && blocking == 0:
		return "APPROVE"
	}
	return "COMMENT"
}

// verdictLine explains the approval or change-request decision in the review
// body; "" when there's nothing to say.
func verdictLine(st settings.Settings, event string, blocking, requesting int) string {
	switch {
	case event == "REQUEST_CHANGES":
		return fmt.Sprintf("🛑 **Changes requested:** %d unresolved finding(s) at %s or above. I'll withdraw this once they're fixed or resolved.",
			requesting, settings.SeverityInfo(st.Review.RequestChangesLevel).Label)
	case event == "APPROVE":
		return fmt.Sprintf("✅ **Approved:** no unresolved findings at %s or above.", settings.SeverityInfo(st.Review.ApprovalLevel).Label)
	case st.Review.ApprovalOn() && blocking > 0:
		return fmt.Sprintf("⛔ **Not approved:** %d unresolved finding(s) at %s or above. I'll approve once they're fixed or resolved.",
			blocking, settings.SeverityInfo(st.Review.ApprovalLevel).Label)
	}
	return "" // nothing blocks, or GitHub refused the approval or change request (the log says why)
}

// chatJSON asks for structured output, retrying once with a nudge if the
// model's reply isn't valid JSON.
func (b *Bot) chatJSON(ctx context.Context, lc llm.Client, st settings.Settings, schema map[string]any, schemaName, system, user string, out any) (llm.Response, string, error) {
	msgs := []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	var total llm.Response
	var rawAll strings.Builder
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := lc.Chat(ctx, llm.Request{
			Messages: msgs, Schema: schema, SchemaName: schemaName,
			Temperature: st.LLM.Temperature, MaxTokens: st.LLM.MaxOutputTokens,
		})
		if err != nil {
			return total, rawAll.String(), err
		}
		total.PromptTokens += resp.PromptTokens
		total.CompletionTokens += resp.CompletionTokens
		rawAll.WriteString(resp.Content)
		if lastErr = llm.DecodeJSON(resp.Content, out); lastErr == nil {
			return total, rawAll.String(), nil
		}
		hint := "Your reply was not a valid JSON object in the required format."
		if resp.Truncated {
			hint = "Your reply was cut off because it was too long. Be briefer: report at most the 8 most important items, with short text."
		}
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: textutil.Truncate(resp.Content, 2000)},
			llm.Message{Role: "user", Content: hint + " Respond again with ONLY the JSON object, nothing else."})
		rawAll.WriteString("\n----- retry -----\n")
	}
	return total, rawAll.String(), fmt.Errorf("model did not return valid JSON: %w", lastErr)
}

func (b *Bot) synthesizeSummary(ctx context.Context, lc llm.Client, st settings.Settings, gp *github.PullRequest, parts []string, fs []finding) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pull request: %s\n\nThe diff was reviewed in %d parts. Part summaries:\n", gp.Title, len(parts))
	for i, s := range parts {
		fmt.Fprintf(&sb, "\nPart %d: %s\n", i+1, s)
	}
	if len(fs) > 0 {
		sb.WriteString("\nIssues found:\n")
		for _, f := range fs {
			fmt.Fprintf(&sb, "- %s %s (%s)\n", settings.SeverityInfo(f.Severity).Label, f.Title, f.Path)
		}
	}
	sb.WriteString("\nWrite one overall summary of the pull request in 2-5 sentences: what it changes and the main risks. Plain markdown, no headings, no preamble.")
	resp, err := lc.Chat(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: "You write the overall summary for an automated pull request review."},
			{Role: "user", Content: sb.String()},
		},
		Temperature: st.LLM.Temperature, MaxTokens: 1024,
	})
	if err == nil {
		if s := llm.StripThinking(resp.Content); s != "" {
			return s
		}
	}
	return strings.Join(parts, "\n\n")
}

// prepareFindings normalizes, filters, de-duplicates and places findings:
// inline ones target a commentable diff line; the rest become notes.
func prepareFindings(st settings.Settings, p plan, all []finding, mine []github.ReviewComment) (inline, notes []finding, dropped int) {
	maxRank := settings.SeverityRank(st.Review.CommentLevel)
	seen := map[string]bool{}
	var kept []finding
	for _, f := range all {
		f.Severity = normSeverity(f.Severity)
		f.Body = sanitizeModelText(f.Body)
		if strings.TrimSpace(f.Title) == "" {
			f.Title = textutil.Truncate(textutil.FirstLine(f.Body), 100)
		}
		f.Title = sanitizeModelLine(f.Title)
		if f.Title == "" && f.Body == "" {
			continue
		}
		if settings.SeverityRank(f.Severity) > maxRank {
			dropped++
			continue
		}
		f.Path = matchPath(p.files, f.Path)
		key := f.Path + ":" + strconv.Itoa(f.Line) + ":" + strings.ToLower(f.Title)
		if seen[key] {
			continue
		}
		seen[key] = true
		if isDuplicate(f, mine) {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		return settings.SeverityRank(kept[i].Severity) < settings.SeverityRank(kept[j].Severity)
	})
	if max := st.Review.MaxComments; len(kept) > max {
		dropped += len(kept) - max
		kept = kept[:max]
	}
	for _, f := range kept {
		if df := p.files[f.Path]; df != nil {
			if df.Valid(f.Line) {
				inline = append(inline, f)
				continue
			}
			if n, ok := df.Nearest(f.Line, 3); ok {
				f.Line = n
				inline = append(inline, f)
				continue
			}
		}
		notes = append(notes, f)
	}
	return inline, notes, dropped
}

// matchPath maps a model-provided path onto a real file in the diff.
func matchPath(files map[string]*diff.File, p string) string {
	p = strings.TrimSpace(strings.Trim(p, "`"))
	if _, ok := files[p]; ok {
		return p
	}
	for _, pre := range []string{"a/", "b/", "./", "/"} {
		if q := strings.TrimPrefix(p, pre); q != p {
			if _, ok := files[q]; ok {
				return q
			}
		}
	}
	var match string
	for name := range files {
		if strings.HasSuffix(name, "/"+p) || strings.HasSuffix(p, "/"+name) {
			if match != "" {
				return p // ambiguous
			}
			match = name
		}
	}
	if match != "" {
		return match
	}
	return p
}

// isDuplicate reports whether a previous bot comment already raised this issue.
func isDuplicate(f finding, mine []github.ReviewComment) bool {
	for _, c := range mine {
		if c.Path != f.Path {
			continue
		}
		d := c.AnyLine() - f.Line
		if d < 0 {
			d = -d
		}
		if d <= 10 && similarity(f.Title, findingTitle(c.Body)) >= 0.5 {
			return true
		}
	}
	return false
}

func similarity(a, b string) float64 {
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(wa)+len(wb)-inter)
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	}) {
		if len(w) > 2 {
			out[w] = true
		}
	}
	return out
}

var pLevelRE = regexp.MustCompile(`\bp([1-5])\b`)

func severityIDs() []string {
	ids := make([]string, 0, len(settings.Severities))
	for _, s := range settings.Severities {
		ids = append(ids, s.ID)
	}
	return ids
}

// normSeverity maps whatever the model wrote onto P1..P5. Besides the IDs it
// accepts full labels ("[P3] MINOR (Defect)") and common words for each level.
func normSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if m := pLevelRE.FindStringSubmatch(s); m != nil {
		return "P" + m[1]
	}
	switch {
	case strings.Contains(s, "quality"):
		return "P4"
	case strings.Contains(s, "defect"):
		return "P3"
	}
	switch strings.Trim(s, "[]() ") {
	case "critical", "blocker", "severe":
		return "P1"
	case "major", "high", "error", "important":
		return "P2"
	case "maintainability", "refactor", "readability", "suggestion", "improvement", "test", "tests":
		return "P4"
	case "nit", "nitpick", "low", "info", "style", "trivial", "cosmetic":
		return "P5"
	default: // "minor", "medium", "bug", unknown
		return "P3"
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
