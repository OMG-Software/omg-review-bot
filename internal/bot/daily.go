package bot

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// Each organization gets at most its daily review limit (set by an
// administrator, store.DefaultDailyReviews unless changed) of reviews in any
// 24 hours. A review counts from when it started, so the window rolls: a
// place frees up 24 hours after the review that took it.
//
// A review the limit stops leaves its PR at the "daily" status, with one
// comment saying why and when reviews resume. Nothing more is reviewed on
// that PR, pushes included, until releaseDailyCapped finds room again.
// Replies in its threads go on as usual.

// dailyWindow is the period the daily review limit covers.
const dailyWindow = 24 * time.Hour

// DailyQuota is an organization's daily review limit and how much of it is taken.
type DailyQuota struct {
	Limit int
	store.DailyUsage
	// ResetAt is when the oldest review in the window drops out of it,
	// freeing a place (zero if no review was posted in the window).
	ResetAt time.Time
}

// Used counts the posted reviews and those under way.
func (q DailyQuota) Used() int { return q.Posted + q.Reviewing }

// Full reports whether no review can start now.
func (q DailyQuota) Full() bool { return q.Used() >= q.Limit }

// Free is how many held PRs can be let through now: the places left, less
// the PRs already waiting for one.
func (q DailyQuota) Free() int { return q.Limit - q.Used() - q.Due }

// DailyQuota returns accountID's daily review limit and usage at now.
func (b *Bot) DailyQuota(accountID int64, now time.Time) (DailyQuota, error) {
	limit, err := b.store.OrgDailyLimit(accountID)
	if err != nil {
		return DailyQuota{}, err
	}
	u, err := b.store.DailyReviewUsage(accountID, now.Add(-dailyWindow).Unix(), now.Unix())
	if err != nil {
		return DailyQuota{}, err
	}
	q := DailyQuota{Limit: limit, DailyUsage: u}
	if u.Oldest > 0 {
		q.ResetAt = time.Unix(u.Oldest, 0).Add(dailyWindow)
	}
	return q, nil
}

// dailyCapped reports whether pr's organization has no review left for now.
// The caller holds prMu, so two workers can't both take the last place. If
// the usage can't be read, the review goes ahead, as with the round limit.
func (b *Bot) dailyCapped(pr *store.PR) (DailyQuota, bool) {
	q, err := b.DailyQuota(pr.AccountID, time.Now())
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not count the organization's reviews today: %v", err)
		return q, false
	}
	return q, q.Full()
}

// releaseDailyCapped lets PRs held by the daily review limit be reviewed
// again, longest held first, as far as their organization has room. Freeing
// only as many as fit keeps the others from being queued and held again
// (and told about it again). It also notes when the next place frees up, so
// the run loop wakes then.
func (b *Bot) releaseDailyCapped(st settings.Settings) {
	prs, err := b.store.DailyCappedPRs()
	if err != nil {
		b.log.Warn("PRs at their daily review limit", "err", err)
		return
	}
	now := time.Now()
	var wake time.Time
	free := map[int64]int{}
	quota := map[int64]DailyQuota{}
	for _, p := range prs {
		q, ok := quota[p.AccountID]
		if !ok {
			if q, err = b.DailyQuota(p.AccountID, now); err != nil {
				b.log.Warn("daily review usage", "account", p.AccountID, "err", err)
				continue
			}
			quota[p.AccountID], free[p.AccountID] = q, q.Free()
		}
		if free[p.AccountID] <= 0 {
			if at := q.ResetAt; !at.IsZero() && (wake.IsZero() || at.Before(wake)) {
				wake = at
			}
			continue
		}
		b.prMu.Lock()
		if cur, err := b.store.GetPR(p.Repo, p.Number); err == nil && cur != nil && cur.Status == "daily" {
			cur.Status, cur.HeldAt = "", 0 // recompute keeps "daily" as it is: decide afresh
			b.recompute(cur, b.settingsFor(st, cur.AccountID))
			if cur.Status == "pending" {
				free[p.AccountID]--
			}
			if err := b.store.SavePR(cur); err == nil && cur.Status == "pending" {
				b.logf("info", cur.Repo, cur.Number, "the organization's daily review limit has room again; reviewing")
			}
		}
		b.prMu.Unlock()
	}
	b.mu.Lock()
	b.dailyWake = wake
	b.mu.Unlock()
}

// nextDailyRelease is when a PR held by the daily limit may next be let
// through (zero if none is waiting on the clock).
func (b *Bot) nextDailyRelease() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dailyWake
}

// holdForDailyLimit parks pr at the "daily" status. The caller holds prMu
// and releases it after.
func (b *Bot) holdForDailyLimit(pr *store.PR, q DailyQuota) {
	pr.Status, pr.HeldAt = "daily", time.Now().Unix()
	_ = b.store.SavePR(pr)
	b.logf("info", pr.Repo, pr.Number, "not reviewing %s: the organization has had %d of its %d reviews in the last 24 hours",
		short(pr.HeadSHA), q.Used(), q.Limit)
	b.mu.Lock()
	if at := q.ResetAt; !at.IsZero() && (b.dailyWake.IsZero() || at.Before(b.dailyWake)) {
		b.dailyWake = at
	}
	b.mu.Unlock()
}

var dailyMarkerRE = regexp.MustCompile(`<!-- prbot:daily-limit at=(\d+) -->`)

func dailyMarker(at time.Time) string {
	return fmt.Sprintf("<!-- prbot:daily-limit at=%d -->", at.Unix())
}

// dailyBody is the PR comment posted when the daily review limit stops a review.
func dailyBody(q DailyQuota, now time.Time) string {
	when := "once the organization's reviews from the last 24 hours start to age out"
	if !q.ResetAt.IsZero() {
		when = "from about " + q.ResetAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf("🛑 **Daily review limit reached.** This organization has had %d of its %d reviews for the last 24 hours, "+
		"so I won't review this pull request, or further pushes to it, for now. Reviews resume automatically %s. "+
		"I'll still answer replies in my review threads. The limit is set by the bot's administrator.\n%s",
		q.Used(), q.Limit, when, dailyMarker(now))
}

// announceDailyLimit posts the daily-limit comment on pr, unless the bot
// posted one there in the last 24 hours.
func (b *Bot) announceDailyLimit(ctx context.Context, st settings.Settings, pr *store.PR, q DailyQuota) {
	app, err := b.getApp(ctx, st)
	if err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not post the daily-limit comment: %v", err)
		return
	}
	cl := app.Installation(pr.InstallationID)
	cs, err := cl.IssueComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		// Without the list, a second notice could be posted; leave it out.
		b.logf("warn", pr.Repo, pr.Number, "could not check for an earlier daily-limit comment, so none is posted: %v", err)
		return
	}
	now := time.Now()
	for _, c := range cs {
		if !strings.EqualFold(c.User.Login, b.BotLogin()) {
			continue
		}
		if m := lastMatch(dailyMarkerRE, c.Body); m != nil {
			if at, err := strconv.ParseInt(m[1], 10, 64); err == nil && now.Sub(time.Unix(at, 0)) < dailyWindow {
				return
			}
		}
	}
	if _, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, dailyBody(q, now)); err != nil {
		b.logf("warn", pr.Repo, pr.Number, "could not post the daily-limit comment: %v", err)
	}
}

// dailyLimitError explains a review asked for by hand that the limit refuses.
func dailyLimitError(q DailyQuota) error {
	msg := fmt.Sprintf("this organization has had %d of its %d reviews for the last 24 hours", q.Used(), q.Limit)
	if !q.ResetAt.IsZero() {
		msg += "; the next can start from about " + q.ResetAt.UTC().Format("15:04 UTC")
	}
	return fmt.Errorf("%s", msg)
}
