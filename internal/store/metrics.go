package store

import (
	"sort"
	"time"
)

// ---------- model calls ----------

// Kinds of job a model call is recorded under.
const (
	CallReview = "review" // part of a review: the diff, fix checks and the summary
	CallReply  = "reply"  // answering a review thread or a mention
	CallOther  = "other"
)

// ModelCall is one call a job made to a configured model.
type ModelCall struct {
	TS        int64
	AccountID int64 // the organization the job was for
	ReviewID  int64 // the review the call was part of (0 if none)
	Kind      string
	// ModelSet is whose model answered: 0 for the instance's, else the
	// organization (account) whose own model it is. Configuration IDs are
	// only unique within a set.
	ModelSet         int64
	ConfigID         string
	Label            string
	Provider         string
	Model            string
	OK               bool
	PromptTokens     int
	CompletionTokens int
	DurationMS       int64
}

// modelCallRetention is how long model calls are kept: past the longest
// period the metrics pages offer.
const modelCallRetention = 400 * 24 * time.Hour

// RecordModelCall stores one model call. Calls older than the retention are
// pruned every 500 writes.
func (s *Store) RecordModelCall(c ModelCall) error {
	if c.TS == 0 {
		c.TS = time.Now().Unix()
	}
	if c.Kind == "" {
		c.Kind = CallOther
	}
	_, err := s.db.Exec(`INSERT INTO model_calls(ts, account_id, review_id, kind, model_set, config_id, label, provider, model,
  ok, prompt_tokens, completion_tokens, duration_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.TS, c.AccountID, c.ReviewID, c.Kind, c.ModelSet, c.ConfigID, c.Label, c.Provider, c.Model,
		b2i(c.OK), c.PromptTokens, c.CompletionTokens, c.DurationMS)
	if err == nil && s.calls.Add(1)%500 == 0 {
		_, _ = s.db.Exec(`DELETE FROM model_calls WHERE ts<?`, time.Now().Add(-modelCallRetention).Unix())
	}
	return err
}

// ModelUsage is how much one configured model was used over a period.
type ModelUsage struct {
	ModelSet int64  // 0 = the instance's model, else the organization whose own it is
	SetOrg   string // that organization's login
	ConfigID string
	Label    string // its name when last used
	Provider string
	Model    string
	// Reviews counts the reviews that sent work to the model, and Answered
	// those it answered at least once: with failover a review can be sent
	// to a model that fails and then answered by the next; with collate,
	// every review goes to every model.
	Reviews          int
	Answered         int
	Replies          int // reply calls answered
	Calls            int
	Failed           int
	PromptTokens     int64
	CompletionTokens int64
	AvgMS            int64 // average duration of an answered call
}

// FailRate is the share of the model's calls that failed, in percent.
func (m ModelUsage) FailRate() int {
	if m.Calls == 0 {
		return 0
	}
	return m.Failed * 100 / m.Calls
}

// ModelUsageSince sums up the model calls made for one organization's jobs
// (or everyone's, with AllAccounts) at or after since, one row per model,
// the most used first. A model is its set, configuration and model name, so
// a configuration switched to another model starts a row of its own.
func (s *Store) ModelUsageSince(accountID, since int64) ([]ModelUsage, error) {
	// label and provider come from the model's newest call (SQLite takes bare
	// columns from the row MAX picks).
	rows, err := s.db.Query(`SELECT c.model_set, COALESCE(o.login,''), c.config_id, c.model, MAX(c.id), c.label, c.provider,
  COUNT(DISTINCT CASE WHEN c.kind='review' AND c.review_id<>0 THEN c.review_id END),
  COUNT(DISTINCT CASE WHEN c.kind='review' AND c.review_id<>0 AND c.ok=1 THEN c.review_id END),
  COALESCE(SUM(c.kind='reply' AND c.ok=1),0), COUNT(*), COALESCE(SUM(c.ok=0),0),
  COALESCE(SUM(c.prompt_tokens),0), COALESCE(SUM(c.completion_tokens),0),
  COALESCE(AVG(CASE WHEN c.ok=1 THEN c.duration_ms END),0)
FROM model_calls c LEFT JOIN orgs o ON o.id=c.model_set
WHERE c.ts>=? AND (?=-1 OR c.account_id=?)
GROUP BY c.model_set, c.config_id, c.model ORDER BY 8 DESC, 11 DESC`, since, accountID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelUsage
	for rows.Next() {
		var m ModelUsage
		var last int64
		var avg float64
		if err := rows.Scan(&m.ModelSet, &m.SetOrg, &m.ConfigID, &m.Model, &last, &m.Label, &m.Provider,
			&m.Reviews, &m.Answered, &m.Replies, &m.Calls, &m.Failed, &m.PromptTokens, &m.CompletionTokens, &avg); err != nil {
			return nil, err
		}
		m.AvgMS = int64(avg)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ConfigUsage is one configuration's use, for the models page. Reviews and
// Answered mean what ModelUsage's do, so both pages agree.
type ConfigUsage struct {
	Reviews  int // reviews sent to it
	Answered int // of those, ones it answered at least once
	Replies  int // reply calls answered
	Failed   int // calls that failed
}

// ConfigUsageSince sums up the use of a set's models (0 = the instance's,
// else an organization's own) at or after since, by configuration ID: a
// configuration's card counts every model it has used in the period, so
// editing its model doesn't hide the reviews it already sent.
func (s *Store) ConfigUsageSince(set, since int64) (map[string]ConfigUsage, error) {
	rows, err := s.db.Query(`SELECT config_id,
  COUNT(DISTINCT CASE WHEN kind='review' AND review_id<>0 THEN review_id END),
  COUNT(DISTINCT CASE WHEN kind='review' AND review_id<>0 AND ok=1 THEN review_id END),
  COALESCE(SUM(kind='reply' AND ok=1),0), COALESCE(SUM(ok=0),0)
FROM model_calls WHERE model_set=? AND ts>=? GROUP BY config_id`, set, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ConfigUsage{}
	for rows.Next() {
		var id string
		var u ConfigUsage
		if err := rows.Scan(&id, &u.Reviews, &u.Answered, &u.Replies, &u.Failed); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// ---------- organizations ----------

// OrgMetrics is one organization's reviews over a period, for the
// administrators' metrics.
type OrgMetrics struct {
	AccountID        int64
	Login            string // "" for reviews not linked to an organization
	Posted           int
	PRs              int
	Repos            int
	Findings         int
	Sev              [5]int
	Fixed            int
	Errors           int
	PromptTokens     int64
	CompletionTokens int64
}

// FixRate is the share of the period's findings confirmed fixed in it, in percent.
func (o OrgMetrics) FixRate() int { return fixRate(o.Fixed, o.Findings) }

// OrgMetricsSince sums up every organization's reviews started at or after
// since, the most reviewed first.
func (s *Store) OrgMetricsSince(since int64) ([]OrgMetrics, error) {
	rows, err := s.db.Query(`SELECT r.account_id, COALESCE(o.login,''), COALESCE(SUM(r.status='posted'),0),
  COUNT(DISTINCT CASE WHEN r.status='posted' THEN r.repo||'#'||r.number END),
  COUNT(DISTINCT CASE WHEN r.status='posted' THEN r.repo END),
  COALESCE(SUM(r.comments+r.notes),0),
  COALESCE(SUM(r.sev_p1),0), COALESCE(SUM(r.sev_p2),0), COALESCE(SUM(r.sev_p3),0), COALESCE(SUM(r.sev_p4),0), COALESCE(SUM(r.sev_p5),0),
  COALESCE(SUM(r.fixed),0), COALESCE(SUM(r.status='error'),0),
  COALESCE(SUM(r.prompt_tokens),0), COALESCE(SUM(r.completion_tokens),0)
FROM reviews r LEFT JOIN orgs o ON o.id=r.account_id
WHERE r.started_at>=? GROUP BY r.account_id ORDER BY 3 DESC, 6 DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMetrics
	for rows.Next() {
		var o OrgMetrics
		if err := rows.Scan(&o.AccountID, &o.Login, &o.Posted, &o.PRs, &o.Repos, &o.Findings,
			&o.Sev[0], &o.Sev[1], &o.Sev[2], &o.Sev[3], &o.Sev[4], &o.Fixed, &o.Errors,
			&o.PromptTokens, &o.CompletionTokens); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------- conversations and latency ----------

// ReplyStats counts the bot's answers to people over a period.
type ReplyStats struct {
	Threads  int // replies in the bot's review threads
	Mentions int // answers to @mentions in PR conversations
}

// Total is every answer posted.
func (r ReplyStats) Total() int { return r.Threads + r.Mentions }

// RepliesSince counts the replies the bot posted on one organization's PRs
// (or everyone's, with AllAccounts) at or after since. One reply can answer
// several waiting messages, each marked with its ID, so replies are counted
// by ID.
func (s *Store) RepliesSince(accountID, since int64) (ReplyStats, error) {
	var r ReplyStats
	err := s.db.QueryRow(`SELECT COUNT(DISTINCT CASE WHEN h.kind='thread' THEN h.reply_id END),
  COUNT(DISTINCT CASE WHEN h.kind='mention' THEN h.reply_id END)
FROM handled_comments h WHERE h.handled_at>=? AND h.reply_id<>0 AND (?=-1
  OR EXISTS(SELECT 1 FROM pulls p WHERE p.repo=h.repo AND p.number=h.number AND p.account_id=?))`,
		since, accountID, accountID).Scan(&r.Threads, &r.Mentions)
	return r, err
}

// Latency describes how long pull requests waited for their first review.
type Latency struct {
	PRs    int // pull requests whose first review was posted in the period
	Median int // seconds
	P90    int // seconds
}

// FirstReviewLatency measures, for one organization's pull requests (or
// everyone's, with AllAccounts) whose first review was posted at or after
// since, the time from the bot first seeing the PR to that review. It
// includes the wait after a push and any queueing: what an author waits.
//
// Only the period's reviews are read (by start, with a day's margin, as a
// job runs at most 45 minutes), and a PR with a posted review before the
// period is dropped by an indexed lookup, so the cost follows the period
// rather than the whole history.
func (s *Store) FirstReviewLatency(accountID, since int64) (Latency, error) {
	rows, err := s.db.Query(`SELECT f.first-p.tracked_at FROM
  (SELECT account_id, repo, number, MIN(finished_at) AS first FROM reviews
   WHERE status='posted' AND finished_at>0 AND finished_at>=? AND started_at>=? AND (?=-1 OR account_id=?)
   GROUP BY account_id, repo, number) f
  JOIN pulls p ON p.repo=f.repo AND p.number=f.number AND p.account_id=f.account_id
WHERE p.tracked_at>0 AND f.first>=p.tracked_at
  AND NOT EXISTS (SELECT 1 FROM reviews e WHERE e.repo=f.repo AND e.number=f.number AND e.account_id=f.account_id
    AND e.status='posted' AND e.finished_at>0 AND e.finished_at<?)`,
		since, since-86400, accountID, accountID, since)
	if err != nil {
		return Latency{}, err
	}
	defer rows.Close()
	var waits []int
	for rows.Next() {
		var w int
		if err := rows.Scan(&w); err != nil {
			return Latency{}, err
		}
		waits = append(waits, w)
	}
	if err := rows.Err(); err != nil {
		return Latency{}, err
	}
	l := Latency{PRs: len(waits)}
	if len(waits) > 0 {
		sort.Ints(waits)
		l.Median = percentile(waits, 50)
		l.P90 = percentile(waits, 90)
	}
	return l, nil
}

// percentile returns the p-th percentile of sorted (nearest rank).
func percentile(sorted []int, p int) int {
	i := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	return sorted[max(0, min(len(sorted), i)-1)]
}
