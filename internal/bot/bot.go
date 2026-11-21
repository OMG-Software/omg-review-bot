// Package bot is the engine: it polls GitHub, queues reviews and replies, and
// runs them one at a time against the configured model.
package bot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/skill"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// Status is a snapshot shown on the dashboard.
type Status struct {
	Configured   bool
	ConfigIssues []string
	BotLogin     string
	AppName      string
	LastPoll     time.Time
	LastPollDur  time.Duration
	LastPollErr  string
	Current      string
	CurrentRepo  string // repository of the running job
	// CurrentAccount owns the running job's PR, so org views can hide others'
	// work. It's recorded with the job, not looked up from CurrentRepo: the
	// name can pass to another account while the job runs.
	CurrentAccount int64
	CurrentSince   time.Time
	QueueLen       int
	RateRemaining  int64
	LastWebhook    time.Time
	// WebhooksActive: deliveries arrived recently, so polling has slowed to a safety net.
	WebhooksActive bool
}

type jobKind string

const (
	jobReview  jobKind = "review"
	jobReplies jobKind = "replies"
)

type job struct {
	kind   jobKind
	repo   string
	number int
	force  bool
	// account a manual review was requested for: if the PR's name and
	// number pass to another account first, it isn't reviewed there.
	account int64
	tries   int // policy reads failed so far (manual reviews; see retryForced)
}

func (j job) key() string { return fmt.Sprintf("%s:%s#%d", j.kind, j.repo, j.number) }

const (
	maxAttempts   = 3
	sweepInterval = 15 * time.Minute
)

type Bot struct {
	store   *store.Store
	dataDir string
	log     *slog.Logger

	// prMu serializes read-modify-write of PR rows between the poller, the
	// worker and web actions.
	prMu sync.Mutex

	mu           sync.Mutex
	app          *github.App
	appKey       string
	appInfo      *github.AppInfo
	status       Status
	lastRepoSync time.Time // the last successful repository sync
	// syncAsked counts requests for a repository sync; syncDone is the count
	// the last successful sync started after. A sync only answers requests
	// made before it started, so one made during it isn't lost.
	syncAsked, syncDone uint64
	syncGap             time.Duration // the least time between sync attempts (a minute; tests shorten it)
	// policyRetry is the first wait before a manual review whose policy couldn't
	// be read is tried again (tests shorten it).
	policyRetry time.Duration
	syncTried   time.Time // the last repository sync started, successful or not
	syncPending bool      // a repository sync is scheduled for when the minute since the last one ends
	lastSweep   time.Time
	lastPoll    time.Time
	lastWebhook time.Time
	queued      map[string]bool // key → whether the queued job is a manual review
	// upgrades holds a manual review asked for while an automatic one for
	// the same PR was queued: that queue entry is run as the manual review.
	upgrades map[string]job

	docsMu    sync.Mutex
	docsCache map[string]docsEntry // project guideline docs by repo@ref

	jobs chan job
	wake chan struct{} // poll GitHub now
	kick chan struct{} // re-check due reviews without polling
}

func New(st *store.Store, dataDir string, log *slog.Logger) *Bot {
	return &Bot{
		store:       st,
		dataDir:     dataDir,
		log:         log,
		queued:      map[string]bool{},
		upgrades:    map[string]job{},
		jobs:        make(chan job, 4096),
		wake:        make(chan struct{}, 1),
		kick:        make(chan struct{}, 1),
		status:      Status{RateRemaining: -1},
		syncGap:     time.Minute,
		policyRetry: time.Minute,
		// Deliveries are stored (only signed ones), so whether webhooks are
		// arriving doesn't start over at every restart.
		lastWebhook: st.LastDeliveryAt(),
	}
}

// Run polls until ctx is cancelled. A single worker processes the queue so
// the (often local, single-GPU) model only ever handles one request at a time.
func (b *Bot) Run(ctx context.Context) {
	if err := b.store.ResetReviewing(); err != nil {
		b.log.Warn("reset interrupted reviews", "err", err)
	}
	go b.worker(ctx)
	pollNow := true
	for {
		st := b.Settings()
		interval := b.pollInterval(st)
		if pollNow || time.Since(b.lastPollTime()) >= interval {
			b.pollOnce(ctx)
			b.mu.Lock()
			b.lastPoll = time.Now()
			b.mu.Unlock()
		} else if len(st.Issues()) == 0 {
			b.enqueueDue(st)
			b.maybeSweep(st)
		}
		// Sleep until the next poll, or sooner when a debounced review falls due.
		wait := interval - time.Since(b.lastPollTime())
		if due := b.store.NextDue(time.Now().Unix()); due > 0 {
			wait = min(wait, time.Until(time.Unix(due, 0))+time.Second)
		}
		t := time.NewTimer(max(wait, time.Second))
		pollNow = false
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-b.wake:
			t.Stop()
			pollNow = true
		case <-b.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

func (b *Bot) lastPollTime() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPoll
}

// pollInterval is how often GitHub is polled: the configured interval, or the
// slower webhook interval while webhook deliveries are arriving.
func (b *Bot) pollInterval(st settings.Settings) time.Duration {
	if b.webhooksActive(st) {
		return time.Duration(st.Poll.WebhookIntervalSeconds) * time.Second
	}
	return time.Duration(st.Poll.IntervalSeconds) * time.Second
}

// webhooksActive reports whether a verified delivery arrived in the last 24 hours.
func (b *Bot) webhooksActive(st settings.Settings) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return st.GitHub.WebhookSecret != "" && !b.lastWebhook.IsZero() && time.Since(b.lastWebhook) < 24*time.Hour
}

// Wake triggers a poll now (e.g. after settings change).
func (b *Bot) Wake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// kickDue re-checks for due reviews without polling GitHub.
func (b *Bot) kickDue() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// settingsFor returns the settings with an organization's own review policy
// applied, if it has one. If the policy can't be read, the organization's
// automatic reviews and replies are off until it can: falling back to the
// instance's settings could review an organization that turned them off.
func (b *Bot) settingsFor(st settings.Settings, accountID int64) settings.Settings {
	out, err := b.policySettings(st, accountID)
	if err != nil {
		b.log.Warn("read organization policy; skipping its automatic work", "account", accountID, "err", err)
		out.Review.Enabled, out.Review.ApprovalLevel, out.Review.RequestChangesLevel = false, "", ""
		out.Replies.Enabled, out.Replies.Mentions = false, false
	}
	return out
}

// policySettings is settingsFor without the fallback: an error means the
// organization's policy couldn't be read.
func (b *Bot) policySettings(st settings.Settings, accountID int64) (settings.Settings, error) {
	if accountID == 0 {
		return st, nil
	}
	raw, err := b.store.OrgPolicy(accountID)
	if err != nil {
		return st, err
	}
	if p, ok := settings.ParsePolicy(raw); ok {
		return st.WithPolicy(p), nil
	}
	return st, nil
}

// SettingsFor returns the settings that apply to an organization's repositories.
func (b *Bot) SettingsFor(accountID int64) settings.Settings {
	return b.settingsFor(b.Settings(), accountID)
}

func (b *Bot) Settings() settings.Settings {
	st, err := settings.Load(b.store)
	if err != nil {
		b.log.Error("load settings", "err", err)
	}
	return st
}

func (b *Bot) Status() Status {
	active := b.webhooksActive(b.Settings())
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.status
	s.QueueLen = len(b.queued)
	if b.app != nil {
		s.RateRemaining = b.app.RateRemaining()
	}
	s.LastWebhook = b.lastWebhook
	s.WebhooksActive = active
	return s
}

func (b *Bot) setCurrent(pr *store.PR, format string, args ...any) {
	b.mu.Lock()
	b.status.Current = fmt.Sprintf(format, args...)
	b.status.CurrentRepo, b.status.CurrentAccount = pr.Repo, pr.AccountID
	b.status.CurrentSince = time.Now()
	b.mu.Unlock()
}

// logf writes to both the process log and the activity log shown in the UI.
// The entry's organization is worked out from the PR, then the repository.
func (b *Bot) logf(level, repo string, number int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.store.Log(level, repo, number, msg)
	b.processLog(level, repo, number, msg)
}

// logAs is logf for an entry whose organization the caller knows: it's
// filed under exactly accountID, and 0 (not known) leaves it unassigned.
func (b *Bot) logAs(accountID int64, level, repo string, number int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.store.LogAs(accountID, level, repo, number, msg)
	b.processLog(level, repo, number, msg)
}

func (b *Bot) processLog(level, repo string, number int, msg string) {
	attrs := []any{"repo", repo, "pr", number}
	switch level {
	case "error":
		b.log.Error(msg, attrs...)
	case "warn":
		b.log.Warn(msg, attrs...)
	default:
		b.log.Info(msg, attrs...)
	}
}

func (b *Bot) enqueue(j job) bool { return b.push(j) == pushed }

type pushResult int

const (
	pushed        pushResult = iota // queued (or folded into the PR's queued job)
	alreadyQueued                   // an equivalent job is queued already
	queueFull
)

func (b *Bot) push(j job) pushResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if manual, ok := b.queued[j.key()]; ok {
		if j.force && !manual {
			// An automatic review of this PR is queued: it runs as this
			// manual one instead, so the request isn't lost to it.
			b.upgrades[j.key()] = j
			b.queued[j.key()] = true
			return pushed
		}
		return alreadyQueued
	}
	select {
	case b.jobs <- j:
		b.queued[j.key()] = j.force
		return pushed
	default:
		b.log.Warn("job queue full, dropping", "job", j.key())
		return queueFull
	}
}

// dequeued is a job the worker has taken off the queue, as it is to run.
func (b *Bot) dequeued(j job) job {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.queued, j.key())
	if u, ok := b.upgrades[j.key()]; ok {
		delete(b.upgrades, j.key())
		return u
	}
	return j
}

func (b *Bot) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-b.jobs:
			b.runJob(ctx, b.dequeued(j))
			b.mu.Lock()
			b.status.Current, b.status.CurrentRepo, b.status.CurrentAccount = "", "", 0
			b.mu.Unlock()
		}
	}
}

func (b *Bot) runJob(ctx context.Context, j job) {
	defer func() {
		if r := recover(); r != nil {
			b.logf("error", j.repo, j.number, "internal error in %s job: %v", j.kind, r)
			b.log.Error("panic", "stack", string(debug.Stack()))
		}
	}()
	jctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	switch j.kind {
	case jobReview:
		b.handleReview(jctx, j)
	case jobReplies:
		b.handleReplies(jctx, j)
	}
}

// getApp returns a cached GitHub App client, rebuilding it when credentials change.
func (b *Bot) getApp(ctx context.Context, st settings.Settings) (*github.App, error) {
	key := fmt.Sprintf("%d|%s|%x", st.GitHub.AppID, st.GitHub.APIBaseURL, sha256.Sum256([]byte(st.GitHub.PrivateKey)))
	b.mu.Lock()
	if b.app != nil && b.appKey == key {
		a := b.app
		b.mu.Unlock()
		return a, nil
	}
	b.mu.Unlock()

	app, err := github.NewApp(st.GitHub.AppID, st.GitHub.PrivateKey, st.GitHub.APIBaseURL)
	if err != nil {
		return nil, err
	}
	info, err := app.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("GitHub App authentication failed: %w", err)
	}
	b.mu.Lock()
	b.app, b.appKey, b.appInfo = app, key, info
	b.syncAsked++ // with the new App
	b.status.BotLogin = info.Slug + "[bot]"
	b.status.AppName = info.Name
	b.mu.Unlock()
	return app, nil
}

// BotLogin is the login the bot's comments are posted under ("<slug>[bot]").
func (b *Bot) BotLogin() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.appInfo == nil {
		return ""
	}
	return b.appInfo.Slug + "[bot]"
}

// InstallURL is where an organization owner installs the App on GitHub
// ("" until the App's credentials have been checked).
func (b *Bot) InstallURL() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.appInfo == nil || b.appInfo.HTMLURL == "" {
		return ""
	}
	return b.appInfo.HTMLURL + "/installations/new"
}

// LookupUser finds a GitHub user by login, as an organization's installation.
func (b *Bot) LookupUser(ctx context.Context, installationID int64, login string) (*github.User, error) {
	app, err := b.getApp(ctx, b.Settings())
	if err != nil {
		return nil, err
	}
	return app.Installation(installationID).UserByLogin(ctx, login)
}

func (b *Bot) botSlug() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.appInfo == nil {
		return ""
	}
	return b.appInfo.Slug
}

func llmConfig(st settings.Settings) llm.Config {
	return llm.Config{
		Provider:      st.LLM.Provider,
		BaseURL:       st.LLM.BaseURL,
		APIKey:        st.LLM.APIKey,
		Model:         st.LLM.Model,
		ContextTokens: st.LLM.ContextTokens,
		Structured:    st.LLM.StructuredOutput,
		Timeout:       time.Duration(st.LLM.TimeoutSeconds) * time.Second,
	}
}

// DataDir is the directory the bot keeps its files in.
func (b *Bot) DataDir() string { return b.dataDir }

// SkillDir is where the active review skill lives.
func (b *Bot) SkillDir(st settings.Settings) string {
	if st.Review.SkillDir != "" {
		return st.Review.SkillDir
	}
	return filepath.Join(b.dataDir, "skill")
}

func (b *Bot) loadSkill(st settings.Settings) *skill.Skill {
	sk, err := skill.Load(b.SkillDir(st))
	if err != nil {
		return skill.Default()
	}
	return sk
}

// ---------- actions used by the web UI ----------

// ReviewNow queues an immediate review, ignoring debounce, drafts and skip flags.
// ReviewNow queues a manual review of a PR of accountID's (the account the
// caller checked the person may act for).
func (b *Bot) ReviewNow(repo string, number int, accountID int64) error {
	b.prMu.Lock()
	pr, err := b.store.GetPR(repo, number)
	if err != nil || pr == nil || pr.AccountID != accountID {
		b.prMu.Unlock()
		return fmt.Errorf("PR %s#%d is not tracked", repo, number)
	}
	if pr.Status == "reviewing" {
		b.prMu.Unlock()
		return fmt.Errorf("a review of %s#%d is already in progress", repo, number)
	}
	if !allowedRepo(b.Settings(), repo) {
		b.prMu.Unlock()
		return fmt.Errorf("%s is paused: the \"Only these orgs\" setting excludes its owner", repo)
	}
	pr.Status = "pending"
	pr.NotBefore = 0
	pr.Attempts = 0
	err = b.store.SavePR(pr)
	b.prMu.Unlock()
	if err != nil {
		return err
	}
	if !b.enqueue(job{kind: jobReview, repo: repo, number: number, force: true, account: accountID}) {
		return fmt.Errorf("a review of %s#%d is already queued", repo, number)
	}
	b.logf("info", repo, number, "manual review requested")
	return nil
}

// CheckReplies queues an immediate scan for replies/mentions on a PR of
// accountID's.
func (b *Bot) CheckReplies(repo string, number int, accountID int64) error {
	b.prMu.Lock()
	pr, err := b.store.GetPR(repo, number)
	b.prMu.Unlock()
	if err != nil || pr == nil || pr.AccountID != accountID {
		return fmt.Errorf("PR %s#%d is not tracked", repo, number)
	}
	b.enqueue(job{kind: jobReplies, repo: repo, number: number})
	return nil
}

// SetSkip stops or resumes automatic reviews of a PR of accountID's.
func (b *Bot) SetSkip(repo string, number int, accountID int64, skip bool) error {
	b.prMu.Lock()
	defer b.prMu.Unlock()
	pr, err := b.store.GetPR(repo, number)
	if err != nil || pr == nil || pr.AccountID != accountID {
		return fmt.Errorf("PR %s#%d is not tracked", repo, number)
	}
	pr.Skip = skip
	b.recompute(pr, b.SettingsFor(pr.AccountID))
	return b.store.SavePR(pr)
}

// SyncRepos refreshes the repository list from the App installations now.
func (b *Bot) SyncRepos(ctx context.Context) (int, error) {
	st := b.Settings()
	app, err := b.getApp(ctx, st)
	if err != nil {
		return 0, err
	}
	return b.syncRepos(ctx, st, app)
}

// TestGitHub checks App credentials and lists installations.
func (b *Bot) TestGitHub(ctx context.Context, st settings.Settings) (string, error) {
	app, err := github.NewApp(st.GitHub.AppID, st.GitHub.PrivateKey, st.GitHub.APIBaseURL)
	if err != nil {
		return "", err
	}
	info, err := app.Info(ctx)
	if err != nil {
		return "", err
	}
	insts, err := app.Installations(ctx)
	if err != nil {
		return "", err
	}
	if len(insts) == 0 {
		return fmt.Sprintf("Authenticated as %q (%s[bot]), but the App isn't installed anywhere yet. Install it on your org.", info.Name, info.Slug), nil
	}
	var parts []string
	for _, in := range insts {
		n := "?"
		if repos, err := app.Installation(in.ID).Repos(ctx); err == nil {
			n = fmt.Sprint(len(repos))
		}
		parts = append(parts, fmt.Sprintf("%s (%s repos)", in.Account.Login, n))
	}
	return fmt.Sprintf("Authenticated as %q (%s[bot]). Installed on: %s.", info.Name, info.Slug, strings.Join(parts, ", ")), nil
}

// TestLLM sends a tiny prompt to the configured model.
func (b *Bot) TestLLM(ctx context.Context, st settings.Settings) (string, error) {
	lc, err := llm.New(llmConfig(st))
	if err != nil {
		return "", err
	}
	resp, err := lc.Chat(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: "You are a connectivity check. Follow instructions exactly."},
			{Role: "user", Content: "Reply with exactly the word: OK"},
		},
		MaxTokens: st.LLM.MaxOutputTokens, // same budget as reviews, so reasoning models are tested realistically
	})
	if err != nil {
		return "", err
	}
	reply := llm.StripThinking(resp.Content)
	if len(reply) > 80 {
		reply = reply[:80] + "…"
	}
	return fmt.Sprintf("%s replied %q in %s (%d prompt / %d completion tokens).",
		st.LLM.Model, reply, resp.Duration.Round(time.Millisecond), resp.PromptTokens, resp.CompletionTokens), nil
}

func (b *Bot) Models(ctx context.Context, st settings.Settings) ([]string, error) {
	lc, err := llm.New(llmConfig(st))
	if err != nil {
		return nil, err
	}
	return lc.Models(ctx)
}
