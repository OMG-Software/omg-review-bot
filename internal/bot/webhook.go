package bot

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// HandleWebhook acts on one verified webhook delivery. It only updates state
// and queues work, so it returns quickly (GitHub times out after 10 seconds).
// It returns the organization the delivery belongs to (0 if unknown) and a
// short description of what was done, for the delivery log.
func (b *Bot) HandleWebhook(event string, body []byte) (accountID int64, result string, err error) {
	b.mu.Lock()
	b.lastWebhook = time.Now()
	b.mu.Unlock()

	var ev github.WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return 0, "", fmt.Errorf("unreadable payload: %w", err)
	}
	if ev.Installation != nil && ev.Installation.Account.ID != 0 {
		accountID = ev.Installation.Account.ID
	}
	if ev.Repository != nil && ev.Repository.Owner.ID != 0 {
		accountID = ev.Repository.Owner.ID
	}
	if event == "ping" {
		return accountID, "pong", nil
	}
	st := b.Settings()
	if len(st.GitHubIssues()) > 0 {
		return accountID, "ignored: the bot isn't configured yet", nil
	}
	switch event {
	case "installation", "installation_repositories", "installation_target", "repository":
		b.requestSync()
		return accountID, "refreshing organizations and repositories", nil
	case "pull_request":
		return accountID, b.webhookPull(st, &ev), nil
	case "pull_request_review_comment", "pull_request_review", "pull_request_review_thread", "issue_comment":
		return accountID, b.webhookConversation(st, event, &ev), nil
	}
	return accountID, "ignored: event not used", nil
}

// requestSync asks for the repository list to be refreshed. Syncs are at
// most a minute apart (events for a repository the bot doesn't watch would
// otherwise each trigger one); a request inside that minute is kept and runs
// when it ends.
func (b *Bot) requestSync() {
	b.mu.Lock()
	wait := b.syncGap - time.Since(b.syncTried)
	switch {
	case wait <= 0:
		b.syncAsked++
	case !b.syncPending:
		b.syncPending = true
		time.AfterFunc(wait, func() {
			b.mu.Lock()
			b.syncPending = false
			b.mu.Unlock()
			b.requestSync()
		})
	}
	b.mu.Unlock()
	if wait <= 0 {
		b.Wake()
	}
}

func (b *Bot) webhookPull(st settings.Settings, ev *github.WebhookEvent) string {
	if ev.PullRequest == nil || ev.Repository == nil {
		return "ignored: no pull request in payload"
	}
	r, err := b.store.GetRepo(ev.Repository.ID)
	switch {
	case err != nil:
		return "error: " + err.Error()
	case r == nil:
		b.requestSync()
		return "unknown repository; refreshing the repository list"
	case !r.Enabled || !allowedRepo(st, r.FullName):
		return "ignored: repository isn't watched"
	case !r.Synced:
		// The first poll decides which PRs are a baseline; let it run.
		b.Wake()
		return "repository not polled yet; polling now"
	}
	st = b.settingsFor(st, r.AccountID)
	b.prMu.Lock()
	u, err := b.applyPull(st, *r, ev.PullRequest, false, 0)
	b.prMu.Unlock()
	if err != nil {
		return "error: " + err.Error()
	}
	if u.isNew {
		b.logf("info", r.FullName, ev.PullRequest.Number, "new pull request (webhook)")
	}
	// Any change can make a review due now (a push, a new PR, a draft marked
	// ready, a skip label removed); the run loop decides, and it's cheap. It
	// starts nothing itself: enqueueDue also skips an organization whose
	// models aren't set up (see modelsUnset).
	b.kickDue()
	if u.stale {
		return fmt.Sprintf("%s#%d %s: ignored, older than the state already recorded", r.FullName, ev.PullRequest.Number, ev.Action)
	}
	return fmt.Sprintf("%s#%d %s: %s", r.FullName, ev.PullRequest.Number, ev.Action, u.pr.Status)
}

func (b *Bot) webhookConversation(st settings.Settings, event string, ev *github.WebhookEvent) string {
	if ev.Repository == nil {
		return "ignored: no repository in payload"
	}
	number := 0
	switch {
	case ev.PullRequest != nil:
		number = ev.PullRequest.Number
	case ev.Issue != nil && ev.Issue.PullRequest != nil:
		number = ev.Issue.Number
	default:
		return "ignored: not a pull request"
	}
	if ev.Action == "deleted" {
		return "ignored: deletion"
	}
	if login := b.BotLogin(); login != "" && ev.Sender.Login == login {
		return "ignored: the bot's own activity"
	}
	r, err := b.store.GetRepo(ev.Repository.ID)
	if err != nil || r == nil || !r.Enabled || !allowedRepo(st, r.FullName) {
		return "ignored: repository isn't watched"
	}
	pr, err := b.store.GetPR(r.FullName, number)
	if err != nil || pr == nil || !pr.Open || pr.AccountID != r.AccountID {
		return "ignored: pull request isn't tracked"
	}
	if !conversationsOn(b.settingsFor(st, pr.AccountID)) {
		return "ignored: conversations and approvals are off"
	}
	if b.modelsUnset(st, pr.AccountID) {
		return "ignored: no model is set up for this organization"
	}
	if !b.enqueue(job{kind: jobReplies, repo: pr.Repo, number: pr.Number, org: pr.AccountID}) {
		return fmt.Sprintf("%s#%d: reply check already queued", pr.Repo, pr.Number)
	}
	return fmt.Sprintf("%s#%d: reply check queued (%s %s)", pr.Repo, pr.Number, event, ev.Action)
}
