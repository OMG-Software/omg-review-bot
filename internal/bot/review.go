package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	// Seen is the commit Line was last confirmed on, for a held finding
	// (stored with it). Never taken from a model's answer.
	Seen string `json:"seen,omitempty"`
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
		if force {
			// Held, a review asked for by hand stays one: it runs once
			// released, even with automatic reviews off.
			pr.ForceRetry = true
		}
		b.holdForDailyLimit(pr, q)
		b.prMu.Unlock()
		b.closeQueued(qn, "This organization has reached its daily review limit. The review will run automatically once there's room.")
		b.announceDailyLimit(ctx, st, pr, q)
		return
	}
	pr.Status = "reviewing"
	pr.LastError = ""
	pr.HeldAt = 0 // any place the daily limit kept for it is taken now
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
	files string // full contents of the part's changed files, where they fit
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

// addFileContents gives each part the full contents of its changed files at
// head, as far as they fit in the part's budget beside its diff, so the
// model sees the code around each hunk, not just the hunk. Added files are
// left out: their diff is the whole file. It returns the paths sent.
func (b *Bot) addFileContents(ctx context.Context, rt *repoTools, p *plan, files []github.PRFile, budget int) []string {
	added := map[string]bool{}
	for _, f := range files {
		added[f.Filename] = f.Status == "added"
	}
	var sent []string
	for i := range p.batches {
		bt := &p.batches[i]
		for _, path := range bt.paths {
			room := budget - len(bt.text) - len(bt.files)
			if room < 1000 {
				break
			}
			if added[path] {
				continue
			}
			content, err := rt.file(ctx, rt.head, path)
			if err != nil || strings.ContainsRune(textutil.Truncate(content, 8000), 0) {
				continue
			}
			block := fmt.Sprintf("### Full file: %s\n```\n%s```\n\n", path, numberedFile(content))
			if len(block) > room {
				continue
			}
			bt.files += block
			sent = append(sent, path)
		}
	}
	return sent
}

// maxFindPasses bounds how often one part is reviewed again for findings
// the earlier passes missed.
const maxFindPasses = 5

// findingLine names a finding for the "already reported" list.
func findingLine(f finding) string {
	return fmt.Sprintf("%s:%d [%s] %s", f.Path, f.Line, f.Severity, textutil.Truncate(f.Title, 160))
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
	// The organization's own models if it has them, else the instance's.
	if st, err = b.modelSettings(st, pr.AccountID); err != nil {
		return nil, err
	}
	lcs, err := b.reviewClients(st, pr)
	if err != nil {
		return nil, err
	}
	// Fix checks and the overall summary are single calls: with several
	// models reviewing, they go to the first, failing over to the next.
	aux := lcs[0]
	if len(lcs) > 1 {
		if aux, err = b.chatClient(st, pr); err != nil {
			return nil, err
		}
	}

	rec := &store.Review{Repo: pr.Repo, Number: pr.Number, HeadSHA: head, Model: labels(lcs), Skill: sk.Name,
		Status: "running", StartedAt: time.Now().Unix(), AccountID: pr.AccountID, OwnModel: st.OwnModels != 0}
	if id, err := b.store.CreateReview(rec); err == nil {
		rec.ID = id
	}
	ctx = withCallTag(ctx, callTag{kind: store.CallReview, accountID: pr.AccountID, reviewID: rec.ID})
	notice := reviewNotice{head: head, model: rec.Model, skill: sk.Name, started: time.Unix(rec.StartedAt, 0)}
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
	rt := newRepoTools(cl, st, pr.Repo, head, gp.Base.SHA)
	if st.LLM.NoTools {
		rt.disable()
	}
	// The prompt says whether the tools are there; a server that refuses
	// them turns them off for the rest of that model's review.
	system := func(tools bool) string { return reviewSystemPrompt(sk.Name, skillText, tools) + guide }
	header := prHeader(gp)
	// Besides the system prompt and header, each part's prompt holds the
	// findings already reported (listRoom) and, with tools, what the model
	// reads through them (toolRoom). The diff and full files get the rest.
	listRoom := min(8000, totalChars/10)
	toolRoom := 0
	if rt.on() {
		toolRoom = totalChars / 4
	}
	budget := totalChars - len(system(rt.on())) - len(header) - 3000 - listRoom - toolRoom
	if budget < 2000 {
		return nil, fmt.Errorf("model context (%d tokens) is too small for the skill and PR description; raise 'Context tokens' in Settings or shorten the skill", st.LLM.ContextTokens)
	}
	p := buildPlan(st, files, budget)
	if sent := b.addFileContents(ctx, rt, &p, files, budget); len(sent) > 0 {
		fmt.Fprintf(&raw, "full files sent: %s\n\n", strings.Join(sent, ", "))
	}
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
		vu := b.verifyFixes(ctx, st, aux, cl, pr, head, threads, files, &raw)
		fixed = vu.fixed
		rec.Fixed = fixed
		rec.PromptTokens += vu.usage.PromptTokens
		rec.CompletionTokens += vu.usage.CompletionTokens
	}

	if len(p.batches) == 0 {
		rec.Status = "nothing to review"
		rec.Summary = "No reviewable changes (all files were deleted, binary or ignored)."
		b.logf("info", pr.Repo, pr.Number, "nothing to review at %s", short(head))
		// Findings held from an earlier review can't be checked without a
		// review: those on files the PR no longer changes go, the rest wait
		// for the next review (a file can be left out only because its
		// diff is too large to show, say).
		heldLeft, err := b.pruneHeld(ctx, cl, pr, head, files)
		if err != nil {
			return nil, err
		}
		if threadsKnown {
			// No review is posted, but the fix check may have resolved what
			// the bot's change request was about (say, by the PR deleting the
			// file), and nothing else would lift it: syncApproval only acts
			// on a commit the bot posted a review for.
			b.dismissStanding(ctx, st, cl, pr, login, standing{
				liftRequest: countOpen(st.Review.RequestChangesLevel, threads) == 0 && heldLeft == 0 && pr.ReviewedSHA != "",
			}, nil)
		}
		return gp, nil
	}

	if len(lcs) > 1 {
		b.setCurrent(ctx, pr, "Reviewing %s#%d with %d models", pr.Repo, pr.Number, len(lcs))
	}
	// Findings an earlier review held back go first, where they still apply.
	carried, hu, err := b.recheckHeld(ctx, st, aux, cl, rt, pr, head, files, p, &raw)
	rec.PromptTokens += hu.PromptTokens
	rec.CompletionTokens += hu.CompletionTokens
	if err != nil {
		// Going on without them would post a review that drops them.
		return nil, err
	}
	// Each model reviews the whole diff; with several (collate), side by side.
	passes := make([]reviewPass, len(lcs))
	var wg sync.WaitGroup
	for k, lc := range lcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			passes[k] = b.reviewPass(ctx, st, pr, lc, len(lcs) > 1, p, reviewInputs{rt: rt.fork(), system: system, header: header,
				mine: mine, seed: carried, listRoom: listRoom, toolRoom: toolRoom})
		}()
	}
	wg.Wait()
	var lists [][]finding
	var summaries, answered []string
	var failed error
	for _, ps := range passes {
		raw.WriteString(ps.raw)
		rec.PromptTokens += ps.usage.PromptTokens
		rec.CompletionTokens += ps.usage.CompletionTokens
		if ps.err != nil {
			if len(lcs) > 1 {
				b.logf("warn", pr.Repo, pr.Number, "model %s couldn't review %s, so the review goes ahead without it: %v", ps.label, short(head), ps.err)
			}
			if failed == nil {
				failed = ps.err
			}
			continue
		}
		answered = append(answered, ps.label)
		lists = append(lists, ps.findings)
		summaries = append(summaries, ps.summaries...)
	}
	if len(answered) == 0 {
		return nil, failed // every model failed
	}
	rec.Model = strings.Join(answered, " + ")

	// Past its comment limit a review holds the least severe findings for
	// the next one, except the last automatic review, which has no next.
	round := 0
	if n, err := b.store.PostedReviews(pr.AccountID, pr.Repo, pr.Number); err == nil {
		round = n + 1 // this one isn't "posted" yet
	}
	limit := st.Review.MaxComments
	if round >= st.Review.MaxRounds {
		limit = 0
	}
	inline, notes, held, dropped := prepareFindings(st, p, append(carried, collateFindings(p, lists)...), mine, limit)
	var summary string
	switch len(summaries) {
	case 0:
		summary = "_The model did not produce a summary._"
	case 1:
		summary = summaries[0]
	default:
		summary = b.synthesizeSummary(ctx, aux, st, gp, summaries, append(append([]finding{}, inline...), notes...))
	}
	summary = sanitizeModelText(summary)

	// Request changes while anything at or above the request-changes level is
	// open; otherwise approve when nothing at or above the approval level is:
	// no unresolved earlier thread and no new finding.
	// Held findings count too: they're real, just not posted yet.
	open := func(level string) int {
		return countOpen(level, threads) + countFindings(level, inline) + countFindings(level, notes) + countFindings(level, held)
	}
	blocking, requesting := open(st.Review.ApprovalLevel), open(st.Review.RequestChangesLevel)
	event := reviewEvent(st, blocking, requesting)
	ri := reviewInfo{summary: summary, head: head, inline: inline, notes: notes, held: held, plan: p, dropped: dropped,
		model: rec.Model, skill: sk.Name, rereview: pr.ReviewedSHA != "" && pr.ReviewedSHA != head, fixed: fixed,
		round: round, maxRounds: st.Review.MaxRounds, maxComments: limit}
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
	// The held findings are saved before the review that promises them is
	// posted: if they can't be, nothing is posted and the review is retried.
	// If posting fails, the findings held before are put back for the retry.
	prevHeld, prevHead, err := b.store.HeldFindings(pr.AccountID, pr.Repo, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("read the findings held for this review: %w", err)
	}
	if err := b.saveHeld(pr, head, held); err != nil {
		return nil, fmt.Errorf("keep %d finding(s) for the next review: %w", len(held), err)
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
		if rerr := b.store.SetHeldFindings(pr.AccountID, pr.Repo, pr.Number, prevHead, prevHeld); rerr != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not put back the findings held before this review: %v", rerr)
		}
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
	b.logf("info", pr.Repo, pr.Number, "posted review of %s (%s): %d inline comments, %d notes, %d held for the next review, %d earlier findings resolved as fixed (%d parts, %d+%d tokens)",
		short(head), strings.ToLower(event), len(drafts), len(ri.notes), len(held), fixed, len(p.batches), rec.PromptTokens, rec.CompletionTokens)
	return gp, nil
}

// reviewInputs is what every model's review of the diff shares.
type reviewInputs struct {
	rt       *repoTools // this model's tools (see repoTools.fork)
	system   func(tools bool) string
	header   string
	mine     []github.ReviewComment // the bot's findings from earlier reviews
	seed     []finding              // found already: held from the last review
	listRoom int                    // characters for the lists of findings so far
	toolRoom int                    // characters for tool results
}

// reviewPass is one model's review of the whole diff, batch by batch.
type reviewPass struct {
	label     string // the model(s) that answered
	findings  []finding
	summaries []string
	usage     llm.Response
	raw       string
	err       error
}

// reviewPass runs the plan's batches through lc. Each part is reviewed again
// and again, told what's been found so far, until a pass finds nothing new
// worth posting: one review should find everything there is, not leave it
// for the next push. many: other models review the same diff alongside, so
// the output names the model.
func (b *Bot) reviewPass(ctx context.Context, st settings.Settings, pr *store.PR, lc modelClient, many bool,
	p plan, in reviewInputs) (ps reviewPass) {
	var raw strings.Builder
	defer func() { ps.label, ps.raw = lc.Label(), raw.String() }()
	all := append([]finding(nil), in.seed...)
	maxRank := settings.SeverityRank(st.Review.CommentLevel)
	for i, bt := range p.batches {
		inPart := map[string]bool{}
		for _, path := range bt.paths {
			inPart[path] = true
		}
		for pass := 1; pass <= maxFindPasses; pass++ {
			label := fmt.Sprintf("part %d/%d, pass %d", i+1, len(p.batches), pass)
			if many {
				label = lc.Label() + ", " + label
			} else {
				b.setCurrent(ctx, pr, "Reviewing %s#%d (%s)", pr.Repo, pr.Number, label)
			}
			var found []string
			for _, f := range all {
				if inPart[f.Path] {
					found = append(found, findingLine(f))
				}
			}
			user := batchPrompt(in.header, i, len(p.batches), bt, previousFor(in.mine, bt.paths), found, pass, in.listRoom)
			var out modelOutput
			usage, rawOut, err := b.chatTools(ctx, lc, st, in.rt, reviewSchema, "code_review", in.system, user, in.toolRoom, &out)
			fmt.Fprintf(&raw, "===== %s =====\n%s\n\n", label, rawOut)
			ps.usage.PromptTokens += usage.PromptTokens
			ps.usage.CompletionTokens += usage.CompletionTokens
			if err != nil {
				if pass == 1 {
					ps.err = fmt.Errorf("model (part %d/%d): %w", i+1, len(p.batches), err)
					return ps
				}
				// What earlier passes found still stands.
				b.logf("warn", pr.Repo, pr.Number, "review pass %d of part %d/%d by %s failed, so the part ends there: %v", pass, i+1, len(p.batches), lc.Label(), err)
				break
			}
			if s := strings.TrimSpace(out.Summary); s != "" && pass == 1 {
				ps.summaries = append(ps.summaries, s)
			}
			fresh := 0
			for _, f := range out.Comments {
				f.Seen = "" // set when it's held, not by the model
				f, ok := normFinding(p, f)
				if !ok || isDuplicate(f, in.mine) {
					continue
				}
				if k := sameIndex(f, all); k >= 0 {
					// Found again, but more severe: passed on too, so the
					// review counts the issue at this level (see
					// prepareFindings). It's not news, so passes don't go on.
					if settings.SeverityRank(f.Severity) < settings.SeverityRank(all[k].Severity) {
						all[k].Severity = f.Severity
						ps.findings = append(ps.findings, f)
					}
					continue
				}
				all = append(all, f)
				ps.findings = append(ps.findings, f)
				if settings.SeverityRank(f.Severity) <= maxRank {
					fresh++
				}
			}
			if fresh == 0 {
				break
			}
		}
	}
	return ps
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
// model's reply isn't valid JSON. If it still isn't and lc fails over, the
// next model takes over, with the same tries.
func (b *Bot) chatJSON(ctx context.Context, lc llm.Client, st settings.Settings, schema map[string]any, schemaName, system, user string, out any) (llm.Response, string, error) {
	return b.chatJSONMsgs(ctx, lc, st, schema, schemaName, []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, out)
}

// chatJSONMsgs is chatJSON for a conversation already under way.
func (b *Bot) chatJSONMsgs(ctx context.Context, lc llm.Client, st settings.Settings, schema map[string]any, schemaName string, msgs []llm.Message, out any) (llm.Response, string, error) {
	var total llm.Response
	var rawAll strings.Builder
	for {
		usage, err := b.chatJSONOnce(ctx, lc, st, schema, schemaName, msgs, out, &rawAll)
		total.PromptTokens += usage.PromptTokens
		total.CompletionTokens += usage.CompletionTokens
		var bad badJSON
		if f, ok := lc.(*failover); ok && errors.As(err, &bad) && ctx.Err() == nil && f.skip(err) {
			rawAll.WriteString("\n----- next model -----\n")
			continue
		}
		return total, rawAll.String(), err
	}
}

// badJSON is a model's answers not being the JSON asked for, after its tries.
type badJSON struct{ err error }

func (e badJSON) Error() string { return "model did not return valid JSON: " + e.err.Error() }
func (e badJSON) Unwrap() error { return e.err }

// chatJSONOnce is chatJSON's tries with the model in use.
func (b *Bot) chatJSONOnce(ctx context.Context, lc llm.Client, st settings.Settings, schema map[string]any, schemaName string, start []llm.Message, out any, rawAll *strings.Builder) (llm.Response, error) {
	msgs := append([]llm.Message(nil), start...)
	var total llm.Response
	var lastErr error
	at := inUse(lc)
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := lc.Chat(ctx, llm.Request{
			// MaxTokens 0: the answering model's own output cap (see tuned).
			// The prompt was sized for the tightest model's input budget,
			// so it plus any model's own output fits that model's context.
			Messages: msgs, Schema: schema, SchemaName: schemaName,
			Temperature: st.LLM.Temperature,
		})
		if err != nil {
			return total, err
		}
		if now := inUse(lc); now != at {
			// The model in use failed and the next answered: that one gets
			// its own tries, starting from this answer.
			at, attempt, msgs = now, 1, msgs[:len(start)]
			rawAll.WriteString("\n----- next model -----\n")
		}
		total.PromptTokens += resp.PromptTokens
		total.CompletionTokens += resp.CompletionTokens
		rawAll.WriteString(resp.Content)
		if lastErr = llm.DecodeJSON(resp.Content, out); lastErr == nil {
			return total, nil
		}
		hint := "Your reply was not a valid JSON object in the required format."
		switch {
		case resp.Truncated && schemaName == "code_review":
			// Not a cap on findings: the review asks again for what's left out.
			hint = "Your reply was cut off because it was too long. Keep every text field short, and if it still won't fit, report the most severe issues first: you will be asked for the rest afterwards."
		case resp.Truncated:
			hint = "Your reply was cut off because it was too long. Be briefer: keep every text field short."
		}
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: textutil.Truncate(resp.Content, 2000)},
			llm.Message{Role: "user", Content: hint + " Respond again with ONLY the JSON object, nothing else."})
		rawAll.WriteString("\n----- retry -----\n")
	}
	return total, badJSON{lastErr}
}

func (b *Bot) synthesizeSummary(ctx context.Context, lc llm.Client, st settings.Settings, gp *github.PullRequest, parts []string, fs []finding) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pull request: %s\n\nThe review was done in %d parts (parts of the diff, or reviews by different models). Their summaries:\n", gp.Title, len(parts))
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

// normFinding cleans up a finding as the model wrote it: its level, text and
// path. ok is false when nothing is left of it.
func normFinding(p plan, f finding) (finding, bool) {
	f.Severity = normSeverity(f.Severity)
	f.Body = sanitizeModelText(f.Body)
	if strings.TrimSpace(f.Title) == "" {
		f.Title = textutil.Truncate(textutil.FirstLine(f.Body), 100)
	}
	f.Title = sanitizeModelLine(f.Title)
	f.Path = matchPath(p.files, f.Path)
	return f, f.Title != "" || f.Body != ""
}

// sameFinding reports whether two findings are the same issue: the same
// file and line, and much the same claim. Findings on different lines are
// never merged, however alike: two queries side by side can have the same
// flaw, described in the same words, and both need fixing. The cost is the
// odd duplicate when a later pass places one a line off.
func sameFinding(a, b finding) bool {
	return a.Path == b.Path && a.Line == b.Line &&
		(strings.EqualFold(a.Title, b.Title) || similarity(a.Title, b.Title) >= 0.5)
}

// seenFinding reports whether fs already has f. If it does, at a lower
// level, that one is raised to f's: the same issue found again counts at
// its most severe.
func seenFinding(f finding, fs []finding) bool {
	k := sameIndex(f, fs)
	if k >= 0 && settings.SeverityRank(f.Severity) < settings.SeverityRank(fs[k].Severity) {
		fs[k].Severity = f.Severity
	}
	return k >= 0
}

// sameIndex is the index of the finding in fs that is the same issue as f,
// or -1.
func sameIndex(f finding, fs []finding) int {
	for k := range fs {
		if sameFinding(f, fs[k]) {
			return k
		}
	}
	return -1
}

// prepareFindings normalizes, filters, de-duplicates and places findings:
// inline ones target a commentable diff line; the rest become notes. Past
// limit (0 for none), the least severe are held for the next review
// rather than dropped.
func prepareFindings(st settings.Settings, p plan, all []finding, mine []github.ReviewComment, limit int) (inline, notes, held []finding, dropped int) {
	maxRank := settings.SeverityRank(st.Review.CommentLevel)
	var kept []finding
	for _, f := range all {
		f, ok := normFinding(p, f)
		if !ok {
			continue
		}
		if settings.SeverityRank(f.Severity) > maxRank {
			dropped++
			continue
		}
		// The same issue again keeps the first, at the more severe level.
		if seenFinding(f, kept) {
			continue
		}
		if isDuplicate(f, mine) {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		return settings.SeverityRank(kept[i].Severity) < settings.SeverityRank(kept[j].Severity)
	})
	if limit > 0 && len(kept) > limit {
		held = append([]finding(nil), kept[limit:]...)
		kept = kept[:limit]
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
	return inline, notes, held, dropped
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
