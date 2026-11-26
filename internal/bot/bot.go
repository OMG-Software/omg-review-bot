// Package bot is the engine: it polls GitHub, queues reviews and replies, and
// runs them against the configured model, two at a time (never two for the
// same PR).
package bot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/secret"
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
	// Working is what the workers are doing, longest-running first (empty
	// when idle).
	Working []Activity
	// Slots has one entry per worker, in worker order; an idle worker's
	// entry has no What.
	Slots    []Activity
	QueueLen int
	// Stopped: an administrator stopped new work (StopReviews); the jobs in
	// Working still run to the end.
	Stopped       bool
	RateRemaining int64
	LastWebhook   time.Time
	// WebhooksActive: deliveries arrived recently, so polling has slowed to a safety net.
	WebhooksActive bool
}

// Activity is the job one worker is running.
type Activity struct {
	What   string
	Repo   string // repository of the job
	Number int    // the job's PR
	Title  string // the PR's title when the job started this step
	// Account owns the job's PR, so org views can hide others' work. It's
	// recorded with the job, not looked up from Repo: the name can pass to
	// another account while the job runs.
	Account int64
	Since   time.Time // when What started (each step of a job restarts it)
	started time.Time // when the job started, which orders Working
}

// workers is how many jobs run at once. Jobs for the same PR still run one
// after another (see claimPR).
const workers = 4

// Workers is how many jobs the bot runs at once (its review slots).
const Workers = workers

// workerSlot is the context key of the running worker's index, so a job's
// progress (setCurrent) is shown against the worker running it.
type workerSlot struct{}

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
	// org is the account whose share of the workers the job counts against
	// (0 = none: its PR isn't tracked). Set as a worker takes the job.
	org int64
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
	// dailyNotices holds the held PRs whose daily-limit comment couldn't be
	// posted, with when to try again; dailyNoticeWait is the wait between
	// tries (tests shorten it).
	dailyNotices    map[prRef]time.Time
	dailyNoticeWait time.Duration
	// dailyWake is when a PR held by its organization's daily review limit
	// may next be let through (zero if none waits on the clock).
	dailyWake   time.Time
	lastPoll    time.Time
	lastWebhook time.Time
	queued      map[string]bool // key → whether the queued job is a manual review
	// upgrades holds a manual review asked for while an automatic one for
	// the same PR was queued: that queue entry is run as the manual review.
	upgrades map[string]job
	// notices holds the "Queued for review" comment of each queued review
	// job, until the worker takes it (see queued.go).
	notices map[string]*queuedNotice
	// running holds the review jobs the worker has taken, until they finish.
	// A review asked for meanwhile waits in after (a manual one wins), so it
	// can't take over the running review's comment.
	running map[string]bool
	after   map[string]job
	// repliesWaiting holds the PRs (prKey) whose thread replies wait for the
	// review of a new push (see replyThreads), so that review's end answers them.
	repliesWaiting map[string]bool
	// commandsTaken holds the review-request comments (by ID) whose review
	// was queued but that couldn't be recorded as handled: a later scan
	// records them instead of queuing the review again (see markCommand).
	commandsTaken map[int64]bool
	// active holds each PR (prKey) a worker is running a job for, with the
	// jobs for that PR taken off the queue meanwhile. Those run next on the
	// same worker, in order, so no two jobs for one PR ever overlap.
	active map[string][]job
	takeMu sync.Mutex // held by the worker taking a job off the queue
	// working is what each worker is doing (zero when idle).
	working [workers]Activity
	// stopped holds back new jobs (StopReviews) until resumed is closed. It's
	// not saved: a restart, the reason to stop, starts work again.
	stopMu  sync.Mutex
	stopped bool
	resumed chan struct{}

	docsMu    sync.Mutex
	docsCache map[string]docsEntry // project guideline docs by repo@ref

	// box seals organizations' own models (see secretBox; tests may set it).
	boxMu sync.Mutex
	box   *secret.Box
	// roundRobin counts the jobs each set of models has started (key: the
	// organization whose own set it is, 0 for the instance's).
	roundRobin map[int64]int
	// orgBusy counts the workers running each organization's jobs, and
	// orgHeld holds jobs taken off the queue while their organization had
	// its share of the workers (see claimPR), oldest first.
	orgBusy map[int64]int
	orgHeld map[int64][]job
	// slotLimit is how many workers one organization may hold (orgSlots),
	// kept from the settings so a worker taking a job reads it without the
	// database. It's refreshed as soon as settings are saved (SettingsSaved)
	// and on each pass of the run loop; slotMu makes each refresh's read
	// and store one step, so an older read can't replace a newer one.
	slotLimit atomic.Int64
	slotMu    sync.Mutex

	jobs chan job
	wake chan struct{} // poll GitHub now
	kick chan struct{} // re-check due reviews without polling
}

func New(st *store.Store, dataDir string, log *slog.Logger) *Bot {
	return &Bot{
		store:           st,
		dataDir:         dataDir,
		log:             log,
		queued:          map[string]bool{},
		upgrades:        map[string]job{},
		notices:         map[string]*queuedNotice{},
		running:         map[string]bool{},
		after:           map[string]job{},
		repliesWaiting:  map[string]bool{},
		commandsTaken:   map[int64]bool{},
		active:          map[string][]job{},
		roundRobin:      map[int64]int{},
		orgBusy:         map[int64]int{},
		orgHeld:         map[int64][]job{},
		jobs:            make(chan job, 4096),
		wake:            make(chan struct{}, 1),
		kick:            make(chan struct{}, 1),
		status:          Status{RateRemaining: -1},
		syncGap:         time.Minute,
		policyRetry:     time.Minute,
		dailyNotices:    map[prRef]time.Time{},
		dailyNoticeWait: 5 * time.Minute,
		// Deliveries are stored (only signed ones), so whether webhooks are
		// arriving doesn't start over at every restart.
		lastWebhook: st.LastDeliveryAt(),
	}
}

// Run polls until ctx is cancelled. Four workers process the queue, so the
// model handles at most four requests at a time.
func (b *Bot) Run(ctx context.Context) {
	if err := b.store.ResetReviewing(); err != nil {
		b.log.Warn("reset interrupted reviews", "err", err)
	}
	for i := range workers {
		go b.worker(ctx, i)
	}
	pollNow := true
	for {
		st := b.Settings()
		b.refreshSlotLimit()
		interval := b.pollInterval(st)
		if pollNow || time.Since(b.lastPollTime()) >= interval {
			b.pollOnce(ctx)
			b.mu.Lock()
			b.lastPoll = time.Now()
			b.mu.Unlock()
		} else if len(st.GitHubIssues()) == 0 {
			b.enqueueDue(st)
			b.maybeSweep(st)
		}
		// Sleep until the next poll, or sooner when a debounced review falls due.
		wait := interval - time.Since(b.lastPollTime())
		if due := b.store.NextDue(time.Now().Unix()); due > 0 {
			wait = min(wait, time.Until(time.Unix(due, 0))+time.Second)
		}
		if at := b.nextDailyRelease(); !at.IsZero() {
			wait = min(wait, time.Until(at)+time.Second)
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
	s.Slots = make([]Activity, workers)
	for i, a := range b.working {
		if a.What != "" {
			s.Working = append(s.Working, a)
			s.Slots[i] = a
		}
	}
	sort.SliceStable(s.Working, func(i, j int) bool { return s.Working[i].started.Before(s.Working[j].started) })
	s.QueueLen = len(b.queued)
	s.Stopped = b.Stopped()
	if b.app != nil {
		s.RateRemaining = b.app.RateRemaining()
	}
	s.LastWebhook = b.lastWebhook
	s.WebhooksActive = active
	return s
}

// setCurrent records what the worker running ctx's job is doing.
func (b *Bot) setCurrent(ctx context.Context, pr *store.PR, format string, args ...any) {
	slot, ok := ctx.Value(workerSlot{}).(int)
	if !ok || slot < 0 || slot >= workers {
		return // not run by a worker, so there's no worker's progress to show
	}
	now := time.Now()
	b.mu.Lock()
	started := b.working[slot].started
	if started.IsZero() {
		started = now
	}
	b.working[slot] = Activity{What: fmt.Sprintf(format, args...), Repo: pr.Repo, Number: pr.Number, Title: pr.Title,
		Account: pr.AccountID, Since: now, started: started}
	b.mu.Unlock()
}

// admit starts j in worker slot unless new work is stopped, in which case it
// waits until it isn't (false: ctx ended first). The check and marking the
// slot busy are one step under both locks (b.mu first, as in Status), so
// once StopReviews returns no job starts, and every job that did shows as
// running until it's done.
func (b *Bot) admit(ctx context.Context, slot int, j job) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		b.mu.Lock()
		b.stopMu.Lock()
		stopped, ch := b.stopped, b.resumed
		if !stopped {
			now := time.Now()
			b.working[slot] = Activity{What: "Starting", Repo: j.repo, Number: j.number, Account: j.org, Since: now, started: now}
		}
		b.stopMu.Unlock()
		b.mu.Unlock()
		if !stopped {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
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
	if j.kind == jobReview && b.running[j.key()] {
		// Until the PR's review has finished, its status may still say it's
		// due, so an automatic review is asked for again. Queued now, it would
		// edit the running review's comment and then close it as skipped.
		if prev, ok := b.after[j.key()]; !ok || !prev.force {
			b.after[j.key()] = j
		}
		return pushed
	}
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
		if j.kind == jobReview {
			b.queueNotice(j)
		}
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
	if j.kind == jobReview {
		b.running[j.key()] = true // in the same step, so nothing slips in between
	}
	if u, ok := b.upgrades[j.key()]; ok {
		delete(b.upgrades, j.key())
		return u
	}
	return j
}

// reviewDone ends a running review job, and queues what was asked for
// meanwhile: a manual review as asked, while an automatic one only goes
// ahead if the PR is still due once its status is up to date.
func (b *Bot) reviewDone(j job) {
	b.mu.Lock()
	delete(b.running, j.key())
	next, ok := b.after[j.key()]
	delete(b.after, j.key())
	b.mu.Unlock()
	switch {
	case !ok:
	case next.force:
		if b.push(next) == queueFull {
			b.requeueForced(next, b.policyRetry)
		}
	default:
		b.kickDue()
	}
}

// StopReviews lets the jobs under way finish but starts no more, so the bot
// can be upgraded or restarted without cutting a review short. Due reviews
// aren't queued meanwhile; they stay pending, and are queued once work
// resumes (ResumeReviews, or the next start).
func (b *Bot) StopReviews() {
	b.stopMu.Lock()
	defer b.stopMu.Unlock()
	if !b.stopped {
		b.stopped, b.resumed = true, make(chan struct{})
		b.log.Info("new reviews stopped; the ones under way will finish")
	}
}

// ResumeReviews undoes StopReviews.
func (b *Bot) ResumeReviews() {
	b.stopMu.Lock()
	if b.stopped {
		b.stopped = false
		close(b.resumed)
		b.log.Info("reviews resumed")
	}
	b.stopMu.Unlock()
	b.kickDue()
}

// Stopped reports whether new work is stopped.
func (b *Bot) Stopped() bool {
	b.stopMu.Lock()
	defer b.stopMu.Unlock()
	return b.stopped
}

// waitResumed waits while new work is stopped. It's false if ctx ended first.
func (b *Bot) waitResumed(ctx context.Context) bool {
	for {
		b.stopMu.Lock()
		stopped, ch := b.stopped, b.resumed
		b.stopMu.Unlock()
		if !stopped {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

// worker runs queued jobs as worker slot. A job for a PR another worker is
// busy with is handed to that worker instead, to run after its current one.
func (b *Bot) worker(ctx context.Context, slot int) { b.work(ctx, slot, b.runJob) }

// work is worker with the job runner given (tests pass their own).
func (b *Bot) work(ctx context.Context, slot int, run func(context.Context, job)) {
	ctx = context.WithValue(ctx, workerSlot{}, slot)
	for {
		if !b.waitResumed(ctx) { // while stopped, jobs stay queued
			return
		}
		// A job is taken and its PR claimed in one step, so a PR's jobs are
		// claimed in queue order and run in it. Its organization is normally
		// stamped when it was queued, so the take is in memory only.
		b.takeMu.Lock()
		var j job
		select {
		case <-ctx.Done():
			b.takeMu.Unlock()
			return
		case j = <-b.jobs:
		}
		if j.org == 0 { // stamped when queued; looked up only if it wasn't
			j.org = b.jobOrg(j)
		}
		ok := b.claimPR(j, b.orgSlots()) // read now, so the share is the current one
		b.takeMu.Unlock()
		for ok {
			// A job taken just before work stopped, or the PR's next one,
			// waits rather than starting.
			if !b.admit(ctx, slot, j) {
				b.releasePR(j)
				return
			}
			run(ctx, b.dequeued(j))
			b.mu.Lock()
			b.working[slot] = Activity{}
			b.mu.Unlock()
			j, ok = b.nextForPR(j)
		}
	}
}

// prKey names the PR a job is for, whatever its kind.
func prKey(j job) string { return fmt.Sprintf("%s#%d", strings.ToLower(j.repo), j.number) }

// jobOrg returns the account j's PR belongs to (0 if it isn't tracked).
func (b *Bot) jobOrg(j job) int64 {
	if j.account != 0 {
		return j.account
	}
	if pr, err := b.store.GetPR(j.repo, j.number); err == nil && pr != nil {
		return pr.AccountID
	}
	return 0
}

// orgSlots is how many workers one organization's jobs may hold at once.
func (b *Bot) orgSlots() int {
	if n := b.slotLimit.Load(); n > 0 {
		return int(n)
	}
	return b.refreshSlotLimit()
}

// refreshSlotLimit reads the slot share from the stored settings and keeps
// it for orgSlots, returning it.
func (b *Bot) refreshSlotLimit() int {
	b.slotMu.Lock()
	defer b.slotMu.Unlock()
	n := b.Settings().Limits.OrgSlots(workers)
	b.slotLimit.Store(int64(n))
	return n
}

// SettingsSaved is called once the settings are saved. What the bot keeps
// of them in memory (the slot share) takes effect at once, for the next job
// a worker takes, even while the run loop is busy polling; then the loop is
// woken for the rest.
func (b *Bot) SettingsSaved() {
	b.refreshSlotLimit()
	b.Wake()
}

// claimPR makes the calling worker the one running j's PR's jobs, and reports
// whether it may run j now. If another worker has the PR, j waits for that
// worker (it stays queued until then, as it would in the queue). If j's
// organization already has limit workers, j waits for one of them to finish
// instead, so the rest stay free for other organizations.
func (b *Bot) claimPR(j job, limit int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := prKey(j)
	if waiting, busy := b.active[k]; busy {
		b.active[k] = append(waiting, j)
		return false
	}
	if j.org != 0 && b.orgBusy[j.org] >= limit {
		b.orgHeld[j.org] = append(b.orgHeld[j.org], j)
		return false
	}
	b.active[k] = nil
	if j.org != 0 {
		b.orgBusy[j.org]++
	}
	return true
}

// nextForPR returns the next job for j's PR that was taken off the queue
// while j ran. With none, it releases the PR, and the worker takes a job its
// organization's share held back, if any: the worker stays the
// organization's, so its share isn't exceeded.
//
// The worker's place in a share is j.org's, the account its PR was claimed
// for, and every job it runs until it's free counts there: a job that waited
// for the PR takes j.org even if its PR has since moved to another account
// (a transferred repository), so the place it gives back is the one it took.
func (b *Bot) nextForPR(j job) (job, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := prKey(j)
	if waiting := b.active[k]; len(waiting) > 0 {
		b.active[k] = waiting[1:]
		next := waiting[0]
		next.org = j.org
		return next, true
	}
	delete(b.active, k)
	for j.org != 0 && len(b.orgHeld[j.org]) > 0 {
		h := b.orgHeld[j.org][0]
		b.orgHeld[j.org] = b.orgHeld[j.org][1:]
		hk := prKey(h)
		if waiting, busy := b.active[hk]; busy {
			b.active[hk] = append(waiting, h) // its PR's worker runs it next
			continue
		}
		// The PR's other held jobs wait for this worker now, ahead of any
		// that come later, so they run in the order they were taken.
		var rest, same []job
		for _, o := range b.orgHeld[j.org] {
			if prKey(o) == hk {
				same = append(same, o)
			} else {
				rest = append(rest, o)
			}
		}
		b.orgHeld[j.org] = rest
		b.active[hk] = same
		return h, true
	}
	b.orgDone(j.org)
	return job{}, false
}

// orgDone gives back a worker org's jobs held. The caller holds mu.
func (b *Bot) orgDone(org int64) {
	if len(b.orgHeld[org]) == 0 {
		delete(b.orgHeld, org)
	}
	if org == 0 {
		return
	}
	if b.orgBusy[org]--; b.orgBusy[org] <= 0 {
		delete(b.orgBusy, org)
	}
}

// releasePR gives up j's PR without running j or the jobs waiting for it,
// as a worker stops. j hasn't been run, so it and the waiting jobs go back on
// the queue, where they're still counted: nothing is left claimed or lost.
func (b *Bot) releasePR(j job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := prKey(j)
	b.orgDone(j.org)
	back := append([]job{j}, b.active[k]...)
	delete(b.active, k)
	// With none of its jobs left running, nothing would hand on the jobs its
	// share held back (see nextForPR): they go back on the queue too.
	if j.org != 0 && b.orgBusy[j.org] == 0 {
		back = append(back, b.orgHeld[j.org]...)
		delete(b.orgHeld, j.org)
	}
	for _, w := range back {
		select {
		case b.jobs <- w:
		default:
			// No room: forgotten, so asking for it again queues it.
			delete(b.queued, w.key())
			delete(b.upgrades, w.key())
			b.log.Warn("job queue full, dropping", "job", w.key())
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
		defer b.reviewDone(j)
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
	if b.Stopped() {
		return errors.New("reviews are stopped for now (an administrator is about to restart the bot); ask again once they're back")
	}
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
	// Unlike the per-PR round limit, the daily limit binds reviews asked for
	// by hand too: it's the administrator's, not the organization's.
	if q, full := b.dailyCapped(pr); full {
		b.prMu.Unlock()
		return dailyLimitError(q)
	}
	pr.Status = "pending"
	pr.NotBefore = 0
	pr.Attempts = 0
	err = b.store.SavePR(pr)
	b.prMu.Unlock()
	if err != nil {
		return err
	}
	if !b.enqueue(job{kind: jobReview, repo: repo, number: number, force: true, account: accountID, org: accountID}) {
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
	b.enqueue(job{kind: jobReplies, repo: repo, number: number, org: pr.AccountID})
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
