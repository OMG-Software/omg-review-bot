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
	Configured    bool
	ConfigIssues  []string
	BotLogin      string
	AppName       string
	LastPoll      time.Time
	LastPollDur   time.Duration
	LastPollErr   string
	Current       string
	CurrentSince  time.Time
	QueueLen      int
	RateRemaining int64
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
	lastRepoSync time.Time
	lastSweep    time.Time
	queued       map[string]bool

	jobs chan job
	wake chan struct{}
}

func New(st *store.Store, dataDir string, log *slog.Logger) *Bot {
	return &Bot{
		store:   st,
		dataDir: dataDir,
		log:     log,
		queued:  map[string]bool{},
		jobs:    make(chan job, 4096),
		wake:    make(chan struct{}, 1),
		status:  Status{RateRemaining: -1},
	}
}

// Run polls until ctx is cancelled. A single worker processes the queue so
// the (often local, single-GPU) model only ever handles one request at a time.
func (b *Bot) Run(ctx context.Context) {
	if err := b.store.ResetReviewing(); err != nil {
		b.log.Warn("reset interrupted reviews", "err", err)
	}
	go b.worker(ctx)
	for {
		b.pollOnce(ctx)
		t := time.NewTimer(time.Duration(b.Settings().Poll.IntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-b.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Wake triggers a poll now (e.g. after settings change).
func (b *Bot) Wake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Bot) Settings() settings.Settings {
	st, err := settings.Load(b.store)
	if err != nil {
		b.log.Error("load settings", "err", err)
	}
	return st
}

func (b *Bot) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.status
	s.QueueLen = len(b.queued)
	if b.app != nil {
		s.RateRemaining = b.app.RateRemaining()
	}
	return s
}

func (b *Bot) setCurrent(format string, args ...any) {
	b.mu.Lock()
	b.status.Current = fmt.Sprintf(format, args...)
	b.status.CurrentSince = time.Now()
	b.mu.Unlock()
}

// logf writes to both the process log and the activity log shown in the UI.
func (b *Bot) logf(level, repo string, number int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.store.Log(level, repo, number, msg)
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

func (b *Bot) enqueue(j job) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queued[j.key()] {
		return false
	}
	select {
	case b.jobs <- j:
		b.queued[j.key()] = true
		return true
	default:
		b.log.Warn("job queue full, dropping", "job", j.key())
		return false
	}
}

func (b *Bot) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-b.jobs:
			b.mu.Lock()
			delete(b.queued, j.key())
			b.mu.Unlock()
			b.runJob(ctx, j)
			b.mu.Lock()
			b.status.Current = ""
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
	b.lastRepoSync = time.Time{}
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
func (b *Bot) ReviewNow(repo string, number int) error {
	b.prMu.Lock()
	pr, err := b.store.GetPR(repo, number)
	if err != nil || pr == nil {
		b.prMu.Unlock()
		return fmt.Errorf("PR %s#%d is not tracked", repo, number)
	}
	if pr.Status == "reviewing" {
		b.prMu.Unlock()
		return fmt.Errorf("a review of %s#%d is already in progress", repo, number)
	}
	pr.Status = "pending"
	pr.NotBefore = 0
	pr.Attempts = 0
	err = b.store.SavePR(pr)
	b.prMu.Unlock()
	if err != nil {
		return err
	}
	if !b.enqueue(job{kind: jobReview, repo: repo, number: number, force: true}) {
		return fmt.Errorf("a review of %s#%d is already queued", repo, number)
	}
	b.logf("info", repo, number, "manual review requested")
	return nil
}

// CheckReplies queues an immediate scan for replies/mentions on a PR.
func (b *Bot) CheckReplies(repo string, number int) {
	b.enqueue(job{kind: jobReplies, repo: repo, number: number})
}

func (b *Bot) SetSkip(repo string, number int, skip bool) error {
	b.prMu.Lock()
	defer b.prMu.Unlock()
	pr, err := b.store.GetPR(repo, number)
	if err != nil || pr == nil {
		return fmt.Errorf("PR %s#%d is not tracked", repo, number)
	}
	pr.Skip = skip
	b.recompute(pr, b.Settings())
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
