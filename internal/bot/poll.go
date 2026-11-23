package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

func (b *Bot) pollOnce(ctx context.Context) {
	start := time.Now()
	st := b.Settings()
	issues := st.Issues()
	b.mu.Lock()
	b.status.Configured = len(issues) == 0
	b.status.ConfigIssues = issues
	b.mu.Unlock()
	if len(issues) > 0 {
		return
	}
	err := b.poll(ctx, st)
	b.mu.Lock()
	b.status.LastPoll = start
	b.status.LastPollDur = time.Since(start)
	b.status.LastPollErr = ""
	if err != nil {
		b.status.LastPollErr = err.Error()
	}
	b.mu.Unlock()
	if err != nil {
		b.log.Warn("poll", "err", err)
	}
	b.enqueueDue(st)
	b.maybeSweep(st)
}

func (b *Bot) poll(ctx context.Context, st settings.Settings) error {
	app, err := b.getApp(ctx, st)
	if err != nil {
		return err
	}
	b.mu.Lock()
	// At most one attempt a minute, failed ones included: a sync that keeps
	// failing would otherwise be retried at every wake.
	needSync := (time.Since(b.lastRepoSync) > time.Duration(st.Poll.RepoRefreshMinutes)*time.Minute ||
		b.syncAsked != b.syncDone) &&
		time.Since(b.syncTried) >= b.syncGap
	b.mu.Unlock()
	if needSync {
		if _, err := b.syncRepos(ctx, st, app); err != nil {
			return fmt.Errorf("sync repositories: %w", err)
		}
	}
	repos, err := b.store.ListRepos(store.AllAccounts)
	if err != nil {
		return err
	}
	var firstErr error
	for _, r := range repos {
		if !r.Enabled || !allowedRepo(st, r.FullName) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := b.pollRepo(ctx, b.settingsFor(st, r.AccountID), app, r); err != nil {
			b.log.Warn("poll repo", "repo", r.FullName, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", r.FullName, err)
			}
		}
	}
	return firstErr
}

// syncRepos refreshes the organizations and repositories from the App's
// installations. Each installation's account becomes an organization.
func (b *Bot) syncRepos(ctx context.Context, st settings.Settings, app *github.App) (int, error) {
	b.mu.Lock()
	b.syncTried = time.Now() // requestSync's minute counts failed attempts too
	asked := b.syncAsked     // requests from here on want another sync
	b.mu.Unlock()
	insts, err := app.Installations(ctx)
	if err != nil {
		return 0, err
	}
	stored, err := b.store.ListRepos(store.AllAccounts)
	if err != nil {
		return 0, err
	}
	type found struct {
		in    github.Installation
		repos []github.Repo
	}
	var all []found
	var keep, orgs []int64
	current := map[int64]store.RepoTarget{}
	for _, in := range insts {
		if in.Account.ID != 0 {
			// Still installed, even if the allow-list below skips it: only an
			// organization whose installation is gone is marked uninstalled.
			orgs = append(orgs, in.Account.ID)
		}
		if len(st.GitHub.Accounts) > 0 && !containsFold(st.GitHub.Accounts, in.Account.Login) {
			// Excluded by the allow-list, not uninstalled: its repositories
			// stay as they are (with their watch settings and PRs), paused,
			// until the account is allowed again. Those of its installation
			// too: a repository stored before organizations existed may not
			// have its account yet.
			for _, r := range stored {
				if r.InstallationID == in.ID || (in.Account.ID != 0 && r.AccountID == in.Account.ID) {
					keep = append(keep, r.ID)
				}
			}
			continue
		}
		repos, err := app.Installation(in.ID).Repos(ctx)
		if err != nil {
			return 0, fmt.Errorf("list repos for %s: %w", in.Account.Login, err)
		}
		all = append(all, found{in, repos})
		for _, r := range repos {
			if !r.Archived {
				current[r.ID] = store.RepoTarget{FullName: r.FullName, AccountID: in.Account.ID}
			}
		}
	}
	// Every rename first, then the names: see ParkRenamedRepos.
	if err := b.store.ParkRenamedRepos(current); err != nil {
		return 0, err
	}
	for _, f := range all {
		in := f.in
		accountID := in.Account.ID
		if accountID != 0 {
			if err := b.store.UpsertOrg(accountID, in.Account.Login, in.Account.Type, in.ID); err != nil {
				return 0, err
			}
		}
		for _, r := range f.repos {
			if r.Archived {
				continue
			}
			if err := b.store.UpsertRepo(r.ID, in.ID, accountID, r.FullName, st.GitHub.NewReposEnabled); err != nil {
				return 0, err
			}
			keep = append(keep, r.ID)
		}
	}
	if err := b.store.PruneRepos(keep); err != nil {
		return 0, err
	}
	if err := b.store.DeactivateOrgsExcept(orgs); err != nil {
		return 0, err
	}
	if err := b.store.BackfillAccounts(); err != nil {
		return 0, err
	}
	b.mu.Lock()
	b.lastRepoSync, b.syncDone = time.Now(), asked
	b.mu.Unlock()
	return len(keep), nil
}

func (b *Bot) pollRepo(ctx context.Context, st settings.Settings, app *github.App, r store.Repo) error {
	cl := app.Installation(r.InstallationID)
	fetched := time.Now().Unix()
	pulls, etag, notModified, err := cl.OpenPulls(ctx, r.FullName, r.ETag)
	if err != nil {
		return err
	}
	if notModified {
		return nil
	}
	firstSync := !r.Synced
	open := map[int]bool{}
	var replyJobs []job
	var newPRs []string

	b.prMu.Lock()
	for i := range pulls {
		p := &pulls[i]
		open[p.Number] = true
		u, err := b.applyPull(st, r, p, firstSync, fetched)
		if err != nil {
			b.prMu.Unlock()
			return err
		}
		if u.refetch {
			// With the new ETag, GitHub would answer the next poll with "not
			// modified" and the skipped snapshot would never be checked again.
			etag = r.ETag
		}
		if u.isNew && !u.baseline {
			newPRs = append(newPRs, fmt.Sprintf("#%d", p.Number))
		}
		if u.activity && conversationsOn(st) {
			replyJobs = append(replyJobs, job{kind: jobReplies, repo: r.FullName, number: p.Number})
		}
	}
	if b.closeMissing(st, r.FullName, open, fetched) {
		etag = r.ETag // as above: so a PR left open is checked again
	}
	b.prMu.Unlock()

	if len(newPRs) > 0 {
		// The repository's own account, even 0 (stored before organizations
		// and not linked yet): another stored under the name (one that's gone,
		// until the next sync prunes it) mustn't get its entries.
		b.logAs(r.AccountID, "info", r.FullName, 0, "new pull requests: %s", strings.Join(newPRs, ", "))
	}
	if firstSync {
		b.logAs(r.AccountID, "info", r.FullName, 0, "started tracking repository (%d open PRs)", len(pulls))
	}
	for _, j := range replyJobs {
		b.enqueue(j)
	}
	return b.store.SetRepoPolled(r.ID, etag)
}

// closeMissing marks tracked open PRs that aren't in a poll's list of open
// ones (fetched at fetched) as closed. It reports whether the poll must fetch
// the list again, because a PR was left as it is or the PRs couldn't be read.
// The caller holds prMu.
func (b *Bot) closeMissing(st settings.Settings, repo string, open map[int]bool, fetched int64) (refetch bool) {
	tracked, err := b.store.OpenPRs(repo)
	if err != nil {
		return true
	}
	for i := range tracked {
		pr := &tracked[i]
		if open[pr.Number] {
			continue
		}
		if pr.UpdatedAt >= fetched {
			// Saved since the list was fetched (a webhook for a newly opened
			// PR, say), so it may be newer than the list: leave it for now.
			refetch = true
			continue
		}
		pr.Open = false
		b.recompute(pr, st)
		if err := b.store.SavePR(pr); err != nil {
			refetch = true // still open here: check again next poll
		}
	}
	return refetch
}

// pullUpdate says what applyPull changed.
type pullUpdate struct {
	isNew    bool // first time the PR was seen
	baseline bool // recorded as already reviewed (open before the repo was watched)
	pushed   bool // the head commit changed
	activity bool // updated since last seen (possible new comments)
	stale    bool // older than what's stored; nothing was changed
	refetch  bool // maybe older; the poll mustn't save the ETag, so it fetches again
	pr       *store.PR
}

// newerOnGitHub reports whether GitHub timestamp a is later than b. An empty
// or unreadable one is never newer, so the update goes ahead.
func newerOnGitHub(a, b string) bool {
	ta, err1 := time.Parse(time.RFC3339, a)
	tb, err2 := time.Parse(time.RFC3339, b)
	return err1 == nil && err2 == nil && ta.After(tb)
}

// applyPull records the current state of an open or closed PR, from a poll
// or a webhook. fetched is when a poll fetched p (0 for a webhook). The caller
// holds prMu.
func (b *Bot) applyPull(st settings.Settings, r store.Repo, p *github.PullRequest, firstSync bool, fetched int64) (pullUpdate, error) {
	now := time.Now().Unix()
	pr, err := b.store.GetPR(r.FullName, p.Number)
	if err != nil {
		return pullUpdate{}, err
	}
	if pr != nil && (pr.AccountID != 0 && pr.AccountID != r.AccountID ||
		pr.AccountID == 0 && pr.InstallationID != 0 && pr.InstallationID != r.InstallationID) {
		// The name now belongs to another GitHub account (one renamed and
		// another took its login), or, for a PR recorded before accounts
		// were, to another installation. Nothing of the old PR carries over; its
		// reviews keep their own account, so they stay with the old one.
		pr = nil
	}
	if pr != nil && newerOnGitHub(pr.GHUpdatedAt, p.UpdatedAt) {
		// A webhook (or a later poll) already recorded a newer state of this
		// PR than this snapshot, which arrived out of order.
		return pullUpdate{pr: pr, stale: true}, nil
	}
	if pr != nil && fetched > 0 && pr.UpdatedAt >= fetched && pr.GHUpdatedAt == p.UpdatedAt {
		// GitHub's timestamps are to the second, so a webhook saved while
		// this poll was fetching can carry the same one and still be newer.
		// Keep what was saved; the poll fetches again next time.
		return pullUpdate{pr: pr, stale: true, refetch: true}, nil
	}
	var u pullUpdate
	if pr == nil {
		u.isNew = true
		pr = &store.PR{Repo: r.FullName, Number: p.Number, TrackedAt: now, Status: "new", CommentsSeen: p.UpdatedAt}
		if firstSync && !st.Review.ReviewExisting {
			// First time we see this repo: record existing PRs as a baseline
			// instead of flooding the model with a backlog.
			pr.ReviewedSHA = p.Head.SHA
			pr.Status = "baseline"
			u.baseline = true
		}
	}
	pr.InstallationID = r.InstallationID
	pr.AccountID = r.AccountID
	pr.Title, pr.Author, pr.URL = p.Title, p.User.Login, p.HTMLURL
	pr.BaseRef, pr.HeadRef = p.Base.Ref, p.Head.Ref
	pr.Draft, pr.Open = p.Draft, p.State != "closed"
	pr.Labels = pr.Labels[:0]
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	if p.Head.SHA != "" && pr.HeadSHA != p.Head.SHA {
		pr.HeadSHA = p.Head.SHA
		u.pushed = true
		// Debounce: people often push several commits in a row.
		pr.NotBefore = now + int64(st.Review.DebounceSeconds)
	}
	pr.GHUpdatedAt = p.UpdatedAt
	if !u.isNew && pr.CommentsSeen != p.UpdatedAt {
		pr.CommentsSeen = p.UpdatedAt
		u.activity = true
	}
	b.recompute(pr, st)
	u.pr = pr
	return u, b.store.SavePR(pr)
}

// recompute derives a PR's status from its facts and the settings.
func (b *Bot) recompute(pr *store.PR, st settings.Settings) {
	switch {
	case !pr.Open:
		pr.Status = "closed"
		pr.ForceRetry = false
	case pr.Status == "reviewing":
		// the worker owns it until it finishes
	case pr.ForceRetry:
		pr.Status = "pending"
	case pr.Skip:
		pr.Status = "skipped"
	case st.Review.SkipDrafts && pr.Draft:
		pr.Status = "draft"
	case hasAnyFold(pr.Labels, st.Review.SkipLabels):
		pr.Status = "skipped"
	case pr.HeadSHA != "" && pr.ReviewedSHA == pr.HeadSHA:
		if pr.Status != "baseline" {
			pr.Status = "reviewed"
		}
	case pr.Status == "error" && pr.AttemptSHA == pr.HeadSHA:
		// failed for this commit; waits for a new push or a manual retry
	case pr.Status == "limit" && b.stillOverLimit(pr, st):
		// reviewed as often as the bot may: later pushes wait for the limit
		// to be raised or a review asked for by hand
	case pr.Status == "daily":
		// the organization's daily review limit stopped it: only
		// releaseDailyCapped lets it go, once there's room
	default:
		pr.Status = "pending"
	}
}

// stillOverLimit reports whether a PR that reached its review limit is still
// at it. Only PRs in that state are counted, so recompute stays cheap. If the
// reviews can't be counted, it stays put: handleReview would only find out
// the same way.
func (b *Bot) stillOverLimit(pr *store.PR, st settings.Settings) bool {
	n, err := b.store.PostedReviews(pr.AccountID, pr.Repo, pr.Number)
	return err != nil || n >= st.Review.MaxRounds
}

// releaseLimited re-checks the PRs held at their review limit, so raising
// the limit lets them be reviewed at once. recompute does this too, but only
// runs when the PR list changes: pollRepo stops at an unchanged ETag.
func (b *Bot) releaseLimited(st settings.Settings) {
	prs, err := b.store.LimitedPRs()
	if err != nil {
		b.log.Warn("PRs at their review limit", "err", err)
		return
	}
	for _, p := range prs {
		b.prMu.Lock()
		if cur, err := b.store.GetPR(p.Repo, p.Number); err == nil && cur != nil && cur.Status == "limit" {
			b.recompute(cur, b.settingsFor(st, cur.AccountID))
			if cur.Status != "limit" {
				_ = b.store.SavePR(cur)
			}
		}
		b.prMu.Unlock()
	}
}

func (b *Bot) enqueueDue(st settings.Settings) {
	b.releaseLimited(st) // first, so a PR it frees is queued now
	b.releaseDailyCapped(st)
	prs, err := b.store.DuePRs(time.Now().Unix())
	if err != nil {
		b.log.Warn("due PRs", "err", err)
		return
	}
	for _, p := range prs {
		if !allowedRepo(st, p.Repo) || (!p.ForceRetry && !b.settingsFor(st, p.AccountID).Review.Enabled) {
			continue
		}
		b.enqueue(job{kind: jobReview, repo: p.Repo, number: p.Number})
	}
}

// maybeSweep periodically re-checks reviewed PRs for replies, as a safety net
// in case a reply didn't change the PR list's ETag (or its webhook was missed).
func (b *Bot) maybeSweep(st settings.Settings) {
	b.mu.Lock()
	due := time.Since(b.lastSweep) > sweepInterval
	if due {
		b.lastSweep = time.Now()
	}
	b.mu.Unlock()
	if !due {
		return
	}
	prs, err := b.store.ReviewedOpenPRs()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	for _, p := range prs {
		if !conversationsOn(b.settingsFor(st, p.AccountID)) || !allowedRepo(st, p.Repo) {
			continue
		}
		// Quiet PRs are skipped to save API calls; activity there changes the ETag anyway.
		if t, err := time.Parse(time.RFC3339, p.GHUpdatedAt); err == nil && t.Before(cutoff) {
			continue
		}
		b.enqueue(job{kind: jobReplies, repo: p.Repo, number: p.Number})
	}
}

// allowedRepo reports whether the "only these orgs/accounts" setting, if
// set, includes the repository's owner. One it excludes is paused: kept, but
// not polled, reviewed or replied in.
func allowedRepo(st settings.Settings, fullName string) bool {
	owner, _, _ := strings.Cut(fullName, "/")
	return len(st.GitHub.Accounts) == 0 || containsFold(st.GitHub.Accounts, owner)
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), s) {
			return true
		}
	}
	return false
}

func hasAnyFold(have, want []string) bool {
	for _, h := range have {
		if containsFold(want, h) {
			return true
		}
	}
	return false
}
