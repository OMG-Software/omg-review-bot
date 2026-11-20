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
	needSync := time.Since(b.lastRepoSync) > time.Duration(st.Poll.RepoRefreshMinutes)*time.Minute
	b.mu.Unlock()
	if needSync {
		if _, err := b.syncRepos(ctx, st, app); err != nil {
			return fmt.Errorf("sync repositories: %w", err)
		}
	}
	repos, err := b.store.ListRepos()
	if err != nil {
		return err
	}
	var firstErr error
	for _, r := range repos {
		if !r.Enabled {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := b.pollRepo(ctx, st, app, r); err != nil {
			b.log.Warn("poll repo", "repo", r.FullName, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", r.FullName, err)
			}
		}
	}
	return firstErr
}

func (b *Bot) syncRepos(ctx context.Context, st settings.Settings, app *github.App) (int, error) {
	insts, err := app.Installations(ctx)
	if err != nil {
		return 0, err
	}
	var keep []int64
	for _, in := range insts {
		if len(st.GitHub.Accounts) > 0 && !containsFold(st.GitHub.Accounts, in.Account.Login) {
			continue
		}
		repos, err := app.Installation(in.ID).Repos(ctx)
		if err != nil {
			return 0, fmt.Errorf("list repos for %s: %w", in.Account.Login, err)
		}
		for _, r := range repos {
			if r.Archived {
				continue
			}
			if err := b.store.UpsertRepo(r.ID, in.ID, r.FullName, st.GitHub.NewReposEnabled); err != nil {
				return 0, err
			}
			keep = append(keep, r.ID)
		}
	}
	if err := b.store.PruneRepos(keep); err != nil {
		return 0, err
	}
	b.mu.Lock()
	b.lastRepoSync = time.Now()
	b.mu.Unlock()
	return len(keep), nil
}

func (b *Bot) pollRepo(ctx context.Context, st settings.Settings, app *github.App, r store.Repo) error {
	cl := app.Installation(r.InstallationID)
	pulls, etag, notModified, err := cl.OpenPulls(ctx, r.FullName, r.ETag)
	if err != nil {
		return err
	}
	if notModified {
		return nil
	}
	firstSync := !r.Synced
	now := time.Now().Unix()
	open := map[int]bool{}
	var replyJobs []job
	var newPRs []string

	b.prMu.Lock()
	for _, p := range pulls {
		open[p.Number] = true
		pr, err := b.store.GetPR(r.FullName, p.Number)
		if err != nil {
			b.prMu.Unlock()
			return err
		}
		isNew := pr == nil
		if isNew {
			pr = &store.PR{Repo: r.FullName, Number: p.Number, TrackedAt: now, Status: "new", CommentsSeen: p.UpdatedAt}
			if firstSync && !st.Review.ReviewExisting {
				// First time we see this repo: record existing PRs as a baseline
				// instead of flooding the model with a backlog.
				pr.ReviewedSHA = p.Head.SHA
				pr.Status = "baseline"
			} else {
				newPRs = append(newPRs, fmt.Sprintf("#%d", p.Number))
			}
		}
		pr.InstallationID = r.InstallationID
		pr.Title, pr.Author, pr.URL = p.Title, p.User.Login, p.HTMLURL
		pr.BaseRef, pr.HeadRef = p.Base.Ref, p.Head.Ref
		pr.Draft, pr.Open = p.Draft, true
		pr.Labels = pr.Labels[:0]
		for _, l := range p.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		if pr.HeadSHA != p.Head.SHA {
			pr.HeadSHA = p.Head.SHA
			// Debounce: people often push several commits in a row.
			pr.NotBefore = now + int64(st.Review.DebounceSeconds)
		}
		pr.GHUpdatedAt = p.UpdatedAt
		if !isNew && pr.CommentsSeen != p.UpdatedAt {
			pr.CommentsSeen = p.UpdatedAt
			if st.Replies.Enabled || st.Replies.Mentions {
				replyJobs = append(replyJobs, job{kind: jobReplies, repo: pr.Repo, number: pr.Number})
			}
		}
		b.recompute(pr, st)
		if err := b.store.SavePR(pr); err != nil {
			b.prMu.Unlock()
			return err
		}
	}
	tracked, err := b.store.OpenPRs(r.FullName)
	if err == nil {
		for i := range tracked {
			pr := &tracked[i]
			if !open[pr.Number] {
				pr.Open = false
				b.recompute(pr, st)
				_ = b.store.SavePR(pr)
			}
		}
	}
	b.prMu.Unlock()

	if len(newPRs) > 0 {
		b.logf("info", r.FullName, 0, "new pull requests: %s", strings.Join(newPRs, ", "))
	}
	if firstSync {
		b.logf("info", r.FullName, 0, "started tracking repository (%d open PRs)", len(pulls))
	}
	for _, j := range replyJobs {
		b.enqueue(j)
	}
	return b.store.SetRepoPolled(r.ID, etag)
}

// recompute derives a PR's status from its facts and the settings.
func (b *Bot) recompute(pr *store.PR, st settings.Settings) {
	switch {
	case !pr.Open:
		pr.Status = "closed"
	case pr.Status == "reviewing":
		// the worker owns it until it finishes
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
	default:
		pr.Status = "pending"
	}
}

func (b *Bot) enqueueDue(st settings.Settings) {
	if !st.Review.Enabled {
		return
	}
	prs, err := b.store.DuePRs(time.Now().Unix())
	if err != nil {
		b.log.Warn("due PRs", "err", err)
		return
	}
	for _, p := range prs {
		b.enqueue(job{kind: jobReview, repo: p.Repo, number: p.Number})
	}
}

// maybeSweep periodically re-checks reviewed PRs for replies, as a safety net
// in case a reply didn't change the PR list's ETag.
func (b *Bot) maybeSweep(st settings.Settings) {
	if !st.Replies.Enabled && !st.Replies.Mentions {
		return
	}
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
		// Quiet PRs are skipped to save API calls; activity there changes the ETag anyway.
		if t, err := time.Parse(time.RFC3339, p.GHUpdatedAt); err == nil && t.Before(cutoff) {
			continue
		}
		b.enqueue(job{kind: jobReplies, repo: p.Repo, number: p.Number})
	}
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
