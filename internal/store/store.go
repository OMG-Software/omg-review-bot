// Package store persists bot state (settings, repos, PRs, reviews, activity) in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO needed on Windows or Linux
)

const schema = `
CREATE TABLE IF NOT EXISTS kv (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS repos (
  id              INTEGER PRIMARY KEY,
  installation_id INTEGER NOT NULL,
  full_name       TEXT NOT NULL,
  enabled         INTEGER NOT NULL DEFAULT 1,
  synced          INTEGER NOT NULL DEFAULT 0,
  etag            TEXT NOT NULL DEFAULT '',
  last_polled     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_repos_name ON repos(full_name);
CREATE TABLE IF NOT EXISTS pulls (
  repo            TEXT NOT NULL,
  number          INTEGER NOT NULL,
  installation_id INTEGER NOT NULL DEFAULT 0,
  title           TEXT NOT NULL DEFAULT '',
  author          TEXT NOT NULL DEFAULT '',
  url             TEXT NOT NULL DEFAULT '',
  base_ref        TEXT NOT NULL DEFAULT '',
  head_ref        TEXT NOT NULL DEFAULT '',
  draft           INTEGER NOT NULL DEFAULT 0,
  open            INTEGER NOT NULL DEFAULT 1,
  labels          TEXT NOT NULL DEFAULT '',
  head_sha        TEXT NOT NULL DEFAULT '',
  reviewed_sha    TEXT NOT NULL DEFAULT '',
  attempt_sha     TEXT NOT NULL DEFAULT '',
  attempts        INTEGER NOT NULL DEFAULT 0,
  gh_updated_at   TEXT NOT NULL DEFAULT '',
  comments_seen   TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'new',
  last_error      TEXT NOT NULL DEFAULT '',
  skip            INTEGER NOT NULL DEFAULT 0,
  not_before      INTEGER NOT NULL DEFAULT 0,
  tracked_at      INTEGER NOT NULL DEFAULT 0,
  updated_at      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo, number)
);
CREATE INDEX IF NOT EXISTS idx_pulls_status ON pulls(status, open);
CREATE TABLE IF NOT EXISTS reviews (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  repo              TEXT NOT NULL,
  number            INTEGER NOT NULL,
  head_sha          TEXT NOT NULL DEFAULT '',
  gh_review_id      INTEGER NOT NULL DEFAULT 0,
  url               TEXT NOT NULL DEFAULT '',
  summary           TEXT NOT NULL DEFAULT '',
  comments          INTEGER NOT NULL DEFAULT 0,
  notes             INTEGER NOT NULL DEFAULT 0,
  model             TEXT NOT NULL DEFAULT '',
  skill             TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT '',
  error             TEXT NOT NULL DEFAULT '',
  raw               TEXT NOT NULL DEFAULT '',
  batches           INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  started_at        INTEGER NOT NULL DEFAULT 0,
  finished_at       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_reviews_pr ON reviews(repo, number);
CREATE TABLE IF NOT EXISTS handled_comments (
  gh_comment_id INTEGER PRIMARY KEY,
  kind          TEXT NOT NULL,
  repo          TEXT NOT NULL,
  number        INTEGER NOT NULL,
  reply_id      INTEGER NOT NULL DEFAULT 0,
  handled_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS activity (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  level   TEXT NOT NULL,
  repo    TEXT NOT NULL DEFAULT '',
  number  INTEGER NOT NULL DEFAULT 0,
  message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_activity_pr ON activity(repo, number);
`

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	logs atomic.Int64
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- key/value ----------

func (s *Store) Get(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) Delete(key string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE key=?`, key)
	return err
}

func (s *Store) GetJSON(key string, v any) (bool, error) {
	raw, ok, err := s.Get(key)
	if err != nil || !ok {
		return ok, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (s *Store) SetJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Set(key, string(b))
}

// ---------- repos ----------

type Repo struct {
	ID             int64
	InstallationID int64
	FullName       string
	Enabled        bool
	Synced         bool
	ETag           string
	LastPolled     int64
}

func (s *Store) UpsertRepo(id, installationID int64, fullName string, defaultEnabled bool) error {
	_, err := s.db.Exec(`INSERT INTO repos(id, installation_id, full_name, enabled) VALUES(?,?,?,?)
ON CONFLICT(id) DO UPDATE SET installation_id=excluded.installation_id, full_name=excluded.full_name`,
		id, installationID, fullName, b2i(defaultEnabled))
	return err
}

func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.db.Query(`SELECT id, installation_id, full_name, enabled, synced, etag, last_polled FROM repos ORDER BY full_name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		var en, sy int
		if err := rows.Scan(&r.ID, &r.InstallationID, &r.FullName, &en, &sy, &r.ETag, &r.LastPolled); err != nil {
			return nil, err
		}
		r.Enabled, r.Synced = en == 1, sy == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetRepoEnabled(id int64, enabled bool) error {
	// Clearing the ETag forces a full fetch next poll so changes made while disabled are seen.
	_, err := s.db.Exec(`UPDATE repos SET enabled=?, etag='' WHERE id=?`, b2i(enabled), id)
	return err
}

func (s *Store) SetAllReposEnabled(enabled bool) error {
	_, err := s.db.Exec(`UPDATE repos SET enabled=?, etag=''`, b2i(enabled))
	return err
}

func (s *Store) SetRepoPolled(id int64, etag string) error {
	_, err := s.db.Exec(`UPDATE repos SET etag=?, synced=1, last_polled=? WHERE id=?`, etag, time.Now().Unix(), id)
	return err
}

// PruneRepos deletes repos whose IDs are not in keep (e.g. removed from the App installation).
func (s *Store) PruneRepos(keep []int64) error {
	set := make(map[int64]bool, len(keep))
	for _, id := range keep {
		set[id] = true
	}
	repos, err := s.ListRepos()
	if err != nil {
		return err
	}
	for _, r := range repos {
		if !set[r.ID] {
			if _, err := s.db.Exec(`DELETE FROM repos WHERE id=?`, r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- pull requests ----------

type PR struct {
	Repo           string
	Number         int
	InstallationID int64
	Title          string
	Author         string
	URL            string
	BaseRef        string
	HeadRef        string
	Draft          bool
	Open           bool
	Labels         []string
	HeadSHA        string
	ReviewedSHA    string
	AttemptSHA     string
	Attempts       int
	GHUpdatedAt    string
	CommentsSeen   string
	Status         string
	LastError      string
	Skip           bool
	NotBefore      int64
	TrackedAt      int64
	UpdatedAt      int64
}

const prCols = `repo, number, installation_id, title, author, url, base_ref, head_ref, draft, open, labels,
head_sha, reviewed_sha, attempt_sha, attempts, gh_updated_at, comments_seen, status, last_error, skip,
not_before, tracked_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanPR(sc scanner) (*PR, error) {
	var p PR
	var draft, open, skip int
	var labels string
	err := sc.Scan(&p.Repo, &p.Number, &p.InstallationID, &p.Title, &p.Author, &p.URL, &p.BaseRef, &p.HeadRef,
		&draft, &open, &labels, &p.HeadSHA, &p.ReviewedSHA, &p.AttemptSHA, &p.Attempts, &p.GHUpdatedAt,
		&p.CommentsSeen, &p.Status, &p.LastError, &skip, &p.NotBefore, &p.TrackedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.Draft, p.Open, p.Skip = draft == 1, open == 1, skip == 1
	if labels != "" {
		p.Labels = strings.Split(labels, "\n")
	}
	return &p, nil
}

func (s *Store) queryPRs(q string, args ...any) ([]PR, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PR
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPR returns nil, nil when the PR is not tracked.
func (s *Store) GetPR(repo string, number int) (*PR, error) {
	p, err := scanPR(s.db.QueryRow(`SELECT `+prCols+` FROM pulls WHERE repo=? AND number=?`, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) SavePR(p *PR) error {
	p.UpdatedAt = time.Now().Unix()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO pulls(`+prCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Repo, p.Number, p.InstallationID, p.Title, p.Author, p.URL, p.BaseRef, p.HeadRef,
		b2i(p.Draft), b2i(p.Open), strings.Join(p.Labels, "\n"), p.HeadSHA, p.ReviewedSHA, p.AttemptSHA,
		p.Attempts, p.GHUpdatedAt, p.CommentsSeen, p.Status, p.LastError, b2i(p.Skip), p.NotBefore,
		p.TrackedAt, p.UpdatedAt)
	return err
}

func (s *Store) ListPRs(includeClosed bool, limit int) ([]PR, error) {
	where := `WHERE open=1`
	if includeClosed {
		where = ``
	}
	return s.queryPRs(`SELECT `+prCols+` FROM pulls `+where+` ORDER BY updated_at DESC LIMIT ?`, limit)
}

func (s *Store) OpenPRs(repo string) ([]PR, error) {
	return s.queryPRs(`SELECT `+prCols+` FROM pulls WHERE repo=? AND open=1`, repo)
}

// DuePRs returns PRs waiting for review whose debounce/backoff window has passed.
func (s *Store) DuePRs(now int64) ([]PR, error) {
	return s.queryPRs(`SELECT `+prCols+` FROM pulls WHERE status='pending' AND open=1 AND skip=0 AND not_before<=? ORDER BY not_before`, now)
}

// ReviewedOpenPRs returns open PRs the bot has reviewed (candidates for the reply sweep).
func (s *Store) ReviewedOpenPRs() ([]PR, error) {
	return s.queryPRs(`SELECT ` + prCols + ` FROM pulls WHERE open=1 AND status IN ('reviewed','pending')`)
}

// ResetReviewing re-queues reviews interrupted by a shutdown.
func (s *Store) ResetReviewing() error {
	_, err := s.db.Exec(`UPDATE pulls SET status='pending', not_before=0 WHERE status='reviewing'`)
	return err
}

func (s *Store) CountPRsByStatus() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM pulls WHERE open=1 GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ---------- reviews ----------

type Review struct {
	ID               int64
	Repo             string
	Number           int
	HeadSHA          string
	GHReviewID       int64
	URL              string
	Summary          string
	Comments         int
	Notes            int
	Model            string
	Skill            string
	Status           string
	Error            string
	Raw              string
	Batches          int
	PromptTokens     int
	CompletionTokens int
	StartedAt        int64
	FinishedAt       int64
}

const reviewCols = `id, repo, number, head_sha, gh_review_id, url, summary, comments, notes, model, skill,
status, error, raw, batches, prompt_tokens, completion_tokens, started_at, finished_at`

func scanReview(sc scanner) (*Review, error) {
	var r Review
	err := sc.Scan(&r.ID, &r.Repo, &r.Number, &r.HeadSHA, &r.GHReviewID, &r.URL, &r.Summary, &r.Comments,
		&r.Notes, &r.Model, &r.Skill, &r.Status, &r.Error, &r.Raw, &r.Batches, &r.PromptTokens,
		&r.CompletionTokens, &r.StartedAt, &r.FinishedAt)
	return &r, err
}

func (s *Store) CreateReview(r *Review) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO reviews(repo, number, head_sha, model, skill, status, started_at) VALUES(?,?,?,?,?,?,?)`,
		r.Repo, r.Number, r.HeadSHA, r.Model, r.Skill, r.Status, r.StartedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateReview(r *Review) error {
	_, err := s.db.Exec(`UPDATE reviews SET head_sha=?, gh_review_id=?, url=?, summary=?, comments=?, notes=?, model=?,
skill=?, status=?, error=?, raw=?, batches=?, prompt_tokens=?, completion_tokens=?, finished_at=? WHERE id=?`,
		r.HeadSHA, r.GHReviewID, r.URL, r.Summary, r.Comments, r.Notes, r.Model, r.Skill, r.Status, r.Error,
		r.Raw, r.Batches, r.PromptTokens, r.CompletionTokens, r.FinishedAt, r.ID)
	return err
}

func (s *Store) GetReview(id int64) (*Review, error) {
	r, err := scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) queryReviews(q string, args ...any) ([]Review, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		r.Raw = "" // lists never need the (large) raw model output
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) ListReviews(repo string, number, limit int) ([]Review, error) {
	return s.queryReviews(`SELECT `+reviewCols+` FROM reviews WHERE repo=? AND number=? ORDER BY id DESC LIMIT ?`, repo, number, limit)
}

func (s *Store) RecentReviews(limit int) ([]Review, error) {
	return s.queryReviews(`SELECT `+reviewCols+` FROM reviews ORDER BY id DESC LIMIT ?`, limit)
}

// LatestPostedReview returns the most recent review that was posted to GitHub, or nil.
func (s *Store) LatestPostedReview(repo string, number int) (*Review, error) {
	r, err := scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE repo=? AND number=? AND status='posted' ORDER BY id DESC LIMIT 1`, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ---------- handled comments (reply idempotency) ----------

func (s *Store) IsHandled(commentID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM handled_comments WHERE gh_comment_id=?`, commentID).Scan(&n)
	return n > 0
}

func (s *Store) MarkHandled(commentID int64, kind, repo string, number int, replyID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO handled_comments(gh_comment_id, kind, repo, number, reply_id, handled_at) VALUES(?,?,?,?,?,?)`,
		commentID, kind, repo, number, replyID, time.Now().Unix())
	return err
}

// ---------- activity log ----------

type Activity struct {
	ID      int64
	TS      int64
	Level   string
	Repo    string
	Number  int
	Message string
}

func (s *Store) Log(level, repo string, number int, msg string) {
	_, _ = s.db.Exec(`INSERT INTO activity(ts, level, repo, number, message) VALUES(?,?,?,?,?)`,
		time.Now().Unix(), level, repo, number, msg)
	if s.logs.Add(1)%200 == 0 {
		_, _ = s.db.Exec(`DELETE FROM activity WHERE id <= (SELECT MAX(id) FROM activity) - 5000`)
	}
}

func (s *Store) RecentActivity(limit int, repo string, number int) ([]Activity, error) {
	q := `SELECT id, ts, level, repo, number, message FROM activity`
	var args []any
	if repo != "" {
		q += ` WHERE repo=? AND number=?`
		args = append(args, repo, number)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.TS, &a.Level, &a.Repo, &a.Number, &a.Message); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
