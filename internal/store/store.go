// Package store persists bot state (settings, repos, PRs, reviews, activity,
// users and organizations) in SQLite.
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
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY, -- GitHub user ID
  login         TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  avatar_url    TEXT NOT NULL DEFAULT '',
  is_admin      INTEGER NOT NULL DEFAULT 0,
  disabled      INTEGER NOT NULL DEFAULT 0,
  session_epoch INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL DEFAULT 0,
  last_login    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_users_login ON users(login COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS orgs (
  id              INTEGER PRIMARY KEY, -- GitHub account ID: stable across renames and reinstalls
  login           TEXT NOT NULL,
  type            TEXT NOT NULL DEFAULT 'Organization',
  installation_id INTEGER NOT NULL DEFAULT 0,
  active          INTEGER NOT NULL DEFAULT 1,
  auto_join       INTEGER NOT NULL DEFAULT 0,
  policy          TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL DEFAULT 0,
  daily_reviews   INTEGER NOT NULL DEFAULT 0 -- 0 = DefaultDailyReviews; set only by administrators
);
CREATE TABLE IF NOT EXISTS memberships (
  org_id     INTEGER NOT NULL,
  user_id    INTEGER NOT NULL,
  role       TEXT NOT NULL,                  -- owner | member
  source     TEXT NOT NULL DEFAULT 'manual', -- manual | github
  created_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_memberships_user ON memberships(user_id);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id          TEXT PRIMARY KEY,
  event       TEXT NOT NULL,
  action      TEXT NOT NULL DEFAULT '',
  repo        TEXT NOT NULL DEFAULT '',
  account_id  INTEGER NOT NULL DEFAULT 0,
  result      TEXT NOT NULL DEFAULT '',
  received_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_time ON webhook_deliveries(received_at);
`

// addedColumns were added after the first release. Opening an older database
// adds them with ALTER TABLE.
var addedColumns = []struct{ table, column, alter string }{
	{"repos", "account_id", `ALTER TABLE repos ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0`},
	{"pulls", "account_id", `ALTER TABLE pulls ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0`},
	{"pulls", "force_retry", `ALTER TABLE pulls ADD COLUMN force_retry INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "account_id", `ALTER TABLE reviews ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "event", `ALTER TABLE reviews ADD COLUMN event TEXT NOT NULL DEFAULT ''`},
	{"reviews", "fixed", `ALTER TABLE reviews ADD COLUMN fixed INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "sev_p1", `ALTER TABLE reviews ADD COLUMN sev_p1 INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "sev_p2", `ALTER TABLE reviews ADD COLUMN sev_p2 INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "sev_p3", `ALTER TABLE reviews ADD COLUMN sev_p3 INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "sev_p4", `ALTER TABLE reviews ADD COLUMN sev_p4 INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "sev_p5", `ALTER TABLE reviews ADD COLUMN sev_p5 INTEGER NOT NULL DEFAULT 0`},
	{"activity", "account_id", `ALTER TABLE activity ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0`},
	{"webhook_deliveries", "claim", `ALTER TABLE webhook_deliveries ADD COLUMN claim INTEGER NOT NULL DEFAULT 0`},
	// detailed: the review's severity and fixed counts were recorded (0 for
	// reviews from before they were, whose counts read as zero).
	{"reviews", "detailed", `ALTER TABLE reviews ADD COLUMN detailed INTEGER NOT NULL DEFAULT 0`},
	// account_given: the entry's account came from its caller (LogAs), even
	// 0, so BackfillAccounts leaves it. 0 for earlier entries.
	{"activity", "account_given", `ALTER TABLE activity ADD COLUMN account_given INTEGER NOT NULL DEFAULT 0`},
	// daily_reviews: the organization's daily review limit (0 = the default).
	{"orgs", "daily_reviews", `ALTER TABLE orgs ADD COLUMN daily_reviews INTEGER NOT NULL DEFAULT 0`},
	// held_at: when the daily review limit held the PR, so held PRs are let
	// through in the order they were held (updated_at moves on every save).
	{"pulls", "held_at", `ALTER TABLE pulls ADD COLUMN held_at INTEGER NOT NULL DEFAULT 0`},
}

// postMigrate indexes the added columns, so it runs after they exist.
const postMigrate = `
CREATE INDEX IF NOT EXISTS idx_repos_account ON repos(account_id);
CREATE INDEX IF NOT EXISTS idx_pulls_account ON pulls(account_id, open);
CREATE INDEX IF NOT EXISTS idx_reviews_account ON reviews(account_id, started_at);
CREATE INDEX IF NOT EXISTS idx_activity_account ON activity(account_id, id);

-- A login belongs to one organization at a time. UpsertOrg keeps it that way
-- by renaming an earlier account still stored under a login another has
-- taken; the unique index makes it a rule rather than a habit. Databases from
-- before the index may hold duplicates, so these get the same rename first:
-- the active one (else the newest) keeps the login.
UPDATE orgs SET login=login || ' (earlier account ' || id || ')'
WHERE EXISTS(SELECT 1 FROM orgs o WHERE o.login=orgs.login COLLATE NOCASE AND o.id<>orgs.id
  AND (o.active>orgs.active OR (o.active=orgs.active AND (o.created_at>orgs.created_at
  OR (o.created_at=orgs.created_at AND o.id>orgs.id)))));
DROP INDEX IF EXISTS idx_orgs_login;
CREATE UNIQUE INDEX IF NOT EXISTS idx_orgs_login_unique ON orgs(login COLLATE NOCASE);

-- The views fix each table's column order for the scan functions (scanRepo,
-- scanPR, scanReview, scanUser, scanOrg), so queries can select * from them.
-- They're recreated on every start, so a changed column list takes effect.
DROP VIEW IF EXISTS repos_v;
CREATE VIEW repos_v AS SELECT id, installation_id, account_id, full_name, enabled, synced, etag, last_polled FROM repos;
DROP VIEW IF EXISTS pulls_v;
CREATE VIEW pulls_v AS SELECT repo, number, installation_id, title, author, url, base_ref, head_ref, draft, open, labels,
  head_sha, reviewed_sha, attempt_sha, attempts, gh_updated_at, comments_seen, status, last_error, skip,
  not_before, tracked_at, updated_at, account_id, force_retry, held_at FROM pulls;
DROP VIEW IF EXISTS reviews_v;
CREATE VIEW reviews_v AS SELECT id, repo, number, head_sha, gh_review_id, url, summary, comments, notes, model, skill,
  status, error, raw, batches, prompt_tokens, completion_tokens, started_at, finished_at,
  account_id, event, fixed, sev_p1, sev_p2, sev_p3, sev_p4, sev_p5, detailed FROM reviews;
DROP VIEW IF EXISTS users_v;
CREATE VIEW users_v AS SELECT id, login, name, avatar_url, is_admin, disabled, session_epoch, created_at, last_login FROM users;
DROP VIEW IF EXISTS orgs_v;
CREATE VIEW orgs_v AS SELECT id, login, type, installation_id, active, auto_join, policy, created_at, daily_reviews FROM orgs;
`

// AllAccounts, passed as an account ID, selects the rows of every
// organization. Only administrator views use it. Account 0 matches rows not
// yet linked to an organization.
const AllAccounts int64 = -1

// acct returns the two arguments of the literal filter (?=-1 OR account_id=?)
// that queries use to select one organization's rows, or every row when the
// account is AllAccounts (-1). Keeping the condition in the SQL text, rather
// than building it, means every query is a single literal string.
func acct(accountID int64) []any { return []any{accountID, accountID} }

// Store wraps the SQLite database.
type Store struct {
	db         *sql.DB
	logs       atomic.Int64 // activity writes, for pruning
	deliveries atomic.Int64 // webhook deliveries recorded, for pruning
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	s := &Store{db: db}
	if err := s.BackfillAccounts(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return s, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	for _, c := range addedColumns {
		has, err := hasColumn(db, c.table, c.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(c.alter); err != nil {
				return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	_, err := db.Exec(postMigrate)
	return err
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=? COLLATE NOCASE`, table, column).Scan(&n)
	return n > 0, err
}

// BackfillAccounts links pull requests, reviews and activity that aren't yet
// linked to an organization (recorded before organizations existed, or before
// their repository was synced) to the organization that owns the repository.
func (s *Store) BackfillAccounts() error {
	for _, q := range []string{
		// A name alone isn't proof: it may since have passed to another
		// account. A PR must also have the repository's installation, and
		// reviews and activity must come after the PR they're matched to
		// was tracked (a later account's PR of the same number came after
		// them). What can't be matched stays unassigned: administrators only.
		`UPDATE pulls SET account_id=(SELECT r.account_id FROM repos r WHERE r.full_name=pulls.repo
		 AND r.installation_id=pulls.installation_id AND r.account_id<>0 LIMIT 1)
		 WHERE account_id=0 AND EXISTS(SELECT 1 FROM repos r WHERE r.full_name=pulls.repo
		 AND r.installation_id=pulls.installation_id AND r.account_id<>0)`,
		`UPDATE reviews SET account_id=(SELECT p.account_id FROM pulls p WHERE p.repo=reviews.repo AND p.number=reviews.number)
		 WHERE account_id=0 AND EXISTS(SELECT 1 FROM pulls p WHERE p.repo=reviews.repo AND p.number=reviews.number
		 AND p.account_id<>0 AND p.tracked_at<=reviews.started_at)`,
		// Activity is matched only to a PR that a stored repository of the
		// same installation and account backs, as PRs are above, and never
		// when its caller gave the account (LogAs, even 0: not known then).
		`UPDATE activity SET account_id=(SELECT p.account_id FROM pulls p WHERE ` + activityPR + ` LIMIT 1)
		 WHERE account_id=0 AND account_given=0 AND repo<>'' AND EXISTS(SELECT 1 FROM pulls p WHERE ` + activityPR + `)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// activityPR matches a PR (p) an activity entry can be filed under: see
// BackfillAccounts.
const activityPR = `p.repo=activity.repo AND (activity.number=0 OR p.number=activity.number)
 AND p.account_id<>0 AND p.tracked_at<=activity.ts
 AND EXISTS(SELECT 1 FROM repos r WHERE r.full_name=p.repo AND r.installation_id=p.installation_id AND r.account_id=p.account_id)`

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
	AccountID      int64 // the GitHub org or user that owns the repository
	FullName       string
	Enabled        bool
	Synced         bool
	ETag           string
	LastPolled     int64
}

// UpsertRepo records a repository. When one was renamed (same ID, new name),
// its PRs, reviews and activity move to the new name, as GitHub keeps a repository's PRs
// and their numbers through a rename; otherwise the PRs left under the old name
// would stay due, and the new name's would all be reviewed again. A repository
// moved to another account starts afresh there: its old PRs are closed.
// EarlierRepoSuffix marks the reviews and activity of a repository whose
// name another repository of the same account has since taken.
const EarlierRepoSuffix = " (earlier repository)"

// RepoTarget is a repository's name and owner as GitHub has them now.
type RepoTarget struct {
	FullName  string
	AccountID int64
}

// parkedName is where a renamed repository's records wait during a sync
// (see ParkRenamedRepos). It can't be a GitHub name: those have a slash.
func parkedName(id int64) string { return fmt.Sprintf("(repository %d, being renamed)", id) }

// ParkRenamedRepos, before a sync records the new names, moves each renamed
// repository (its row, PRs, reviews and activity) to a name of its own.
// Repositories can be renamed in a chain or a swap (a to b while b goes to
// c); recorded one at a time, the first would take the second's records
// along with its name. Parked first, each moves from its own placeholder to
// its new name, so a name then only holds records of a repository that's
// gone, which stay as history as before. A repository moved to another
// account isn't parked: it starts afresh there, and its records stay under
// the old name.
func (s *Store) ParkRenamedRepos(current map[int64]RepoTarget) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.Query(`SELECT id, full_name, account_id FROM repos`)
	if err != nil {
		return err
	}
	type renamed struct {
		id, account int64
		name        string
	}
	var park []renamed
	for rows.Next() {
		var r renamed
		if err = rows.Scan(&r.id, &r.name, &r.account); err != nil {
			rows.Close()
			return err
		}
		t, ok := current[r.id]
		if ok && t.FullName != r.name && r.name != parkedName(r.id) && (r.account == t.AccountID || r.account == 0) {
			park = append(park, r)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, r := range park {
		to := parkedName(r.id)
		if _, err = tx.Exec(`UPDATE repos SET full_name=? WHERE id=?`, to, r.id); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE pulls SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
			`UPDATE reviews SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
			`UPDATE activity SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
		} {
			if _, err = tx.Exec(q, to, r.name, r.account); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertRepo(id, installationID, accountID int64, fullName string, defaultEnabled bool) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var oldName string
	var oldAccount int64
	switch err = tx.QueryRow(`SELECT full_name, account_id FROM repos WHERE id=?`, id).Scan(&oldName, &oldAccount); {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
	case err != nil:
		return err
	}
	if _, err = tx.Exec(`INSERT INTO repos(id, installation_id, account_id, full_name, enabled) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET installation_id=excluded.installation_id, account_id=excluded.account_id, full_name=excluded.full_name`,
		id, installationID, accountID, fullName, b2i(defaultEnabled)); err != nil {
		return err
	}
	if oldName != "" && oldName != fullName {
		// Account 0: stored before organizations existed and not backfilled
		// yet. It's the same repository, so it's the same owner.
		if oldAccount == accountID || oldAccount == 0 {
			// A name belongs to one repository at a time, so PRs still stored
			// under the new one are left over from an earlier repository that
			// had it. They give way, so each PR moves together with its reviews.
			// That repository's reviews and activity stay as history, under a
			// name of their own, so they aren't taken for the renamed
			// repository's PRs, which reuse the numbers.
			if _, err = tx.Exec(`DELETE FROM pulls WHERE repo=?`, fullName); err != nil {
				return err
			}
			for _, q := range []string{
				`UPDATE reviews SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
				`UPDATE activity SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
			} {
				if _, err = tx.Exec(q, fullName+EarlierRepoSuffix, fullName, accountID); err != nil {
					return err
				}
			}
			for _, q := range []string{
				`UPDATE pulls SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
				`UPDATE reviews SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
				`UPDATE activity SET repo=? WHERE repo=? AND (account_id=? OR account_id=0)`,
			} {
				if _, err = tx.Exec(q, fullName, oldName, oldAccount); err != nil {
					return err
				}
			}
		}
		// Whatever is left under the old name (all of it after a move to
		// another account) is no longer this repository's.
		if err = closePRs(tx, oldName, oldAccount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// closePRs marks a repository's open PRs closed, so none stays due.
func closePRs(tx *sql.Tx, repo string, accountID int64) error {
	_, err := tx.Exec(`UPDATE pulls SET open=0 WHERE repo=? AND open=1 AND (account_id=? OR account_id=0)`, repo, accountID)
	return err
}

func scanRepo(sc scanner) (*Repo, error) {
	var r Repo
	var en, sy int
	if err := sc.Scan(&r.ID, &r.InstallationID, &r.AccountID, &r.FullName, &en, &sy, &r.ETag, &r.LastPolled); err != nil {
		return nil, err
	}
	r.Enabled, r.Synced = en == 1, sy == 1
	return &r, nil
}

// ListRepos lists one organization's repositories, or every one with AllAccounts.
func (s *Store) ListRepos(accountID int64) ([]Repo, error) {
	rows, err := s.db.Query(`SELECT * FROM repos_v WHERE (?=-1 OR account_id=?) ORDER BY full_name COLLATE NOCASE`, acct(accountID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetRepo returns nil, nil for an unknown repository.
func (s *Store) GetRepo(id int64) (*Repo, error) {
	r, err := scanRepo(s.db.QueryRow(`SELECT * FROM repos_v WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// GetRepoByName returns nil, nil for an unknown repository.
func (s *Store) GetRepoByName(fullName string) (*Repo, error) {
	r, err := scanRepo(s.db.QueryRow(`SELECT * FROM repos_v WHERE full_name=? COLLATE NOCASE LIMIT 1`, fullName))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) SetRepoEnabled(id int64, enabled bool) error {
	// Clearing the ETag forces a full fetch next poll so changes made while disabled are seen.
	_, err := s.db.Exec(`UPDATE repos SET enabled=?, etag='' WHERE id=?`, b2i(enabled), id)
	return err
}

// SetAllReposEnabled switches every repository of one organization (or of all, with AllAccounts).
func (s *Store) SetAllReposEnabled(accountID int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE repos SET enabled=?, etag='' WHERE (?=-1 OR account_id=?)`, b2i(enabled), accountID, accountID)
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
	repos, err := s.ListRepos(AllAccounts)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if set[r.ID] {
			continue
		}
		// Its PRs go too, or they'd stay due and be retried against a
		// repository the App can no longer see.
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if err := closePRs(tx, r.FullName, r.AccountID); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM repos WHERE id=?`, r.ID); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
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
	AccountID      int64
	ForceRetry     bool // retry a failed manual review with its original authorization
	// HeldAt is when the daily review limit last held the PR (0 if it
	// isn't held). Unlike UpdatedAt, later saves (pushes) don't move it.
	HeldAt int64
}

type scanner interface{ Scan(dest ...any) error }

func scanPR(sc scanner) (*PR, error) {
	var p PR
	var draft, open, skip, forceRetry int
	var labels string
	err := sc.Scan(&p.Repo, &p.Number, &p.InstallationID, &p.Title, &p.Author, &p.URL, &p.BaseRef, &p.HeadRef,
		&draft, &open, &labels, &p.HeadSHA, &p.ReviewedSHA, &p.AttemptSHA, &p.Attempts, &p.GHUpdatedAt,
		&p.CommentsSeen, &p.Status, &p.LastError, &skip, &p.NotBefore, &p.TrackedAt, &p.UpdatedAt, &p.AccountID, &forceRetry, &p.HeldAt)
	if err != nil {
		return nil, err
	}
	p.Draft, p.Open, p.Skip = draft == 1, open == 1, skip == 1
	p.ForceRetry = forceRetry == 1
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
	p, err := scanPR(s.db.QueryRow(`SELECT * FROM pulls_v WHERE repo=? AND number=?`, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) SavePR(p *PR) error {
	p.UpdatedAt = time.Now().Unix()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO pulls(repo, number, installation_id, title, author, url, base_ref, head_ref, draft, open, labels,
  head_sha, reviewed_sha, attempt_sha, attempts, gh_updated_at, comments_seen, status, last_error, skip,
  not_before, tracked_at, updated_at, account_id, force_retry, held_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Repo, p.Number, p.InstallationID, p.Title, p.Author, p.URL, p.BaseRef, p.HeadRef,
		b2i(p.Draft), b2i(p.Open), strings.Join(p.Labels, "\n"), p.HeadSHA, p.ReviewedSHA, p.AttemptSHA,
		p.Attempts, p.GHUpdatedAt, p.CommentsSeen, p.Status, p.LastError, b2i(p.Skip), p.NotBefore,
		p.TrackedAt, p.UpdatedAt, p.AccountID, b2i(p.ForceRetry), p.HeldAt)
	return err
}

// ListPRs lists one organization's PRs (or all, with AllAccounts), most recently updated first.
func (s *Store) ListPRs(accountID int64, includeClosed bool, limit int) ([]PR, error) {
	return s.queryPRs(`SELECT * FROM pulls_v WHERE (?=-1 OR account_id=?) AND (?=1 OR open=1) ORDER BY updated_at DESC LIMIT ?`,
		accountID, accountID, b2i(includeClosed), limit)
}

func (s *Store) OpenPRs(repo string) ([]PR, error) {
	return s.queryPRs(`SELECT * FROM pulls_v WHERE repo=? AND open=1`, repo)
}

// DuePRs returns PRs waiting for review whose debounce/backoff window has passed.
func (s *Store) DuePRs(now int64) ([]PR, error) {
	return s.queryPRs(`SELECT * FROM pulls_v WHERE status='pending' AND open=1 AND (skip=0 OR force_retry=1) AND not_before<=? ORDER BY not_before`, now)
}

// NextDue returns when the next pending review becomes due after the given
// time (0 if none). Reviews already due aren't counted: they're queued by
// every poll and wake-up, and may stay pending for a while (waiting in the
// queue, or in an organization with reviews switched off), so counting them
// would keep the run loop waking every second.
func (s *Store) NextDue(after int64) int64 {
	var n sql.NullInt64
	_ = s.db.QueryRow(`SELECT MIN(not_before) FROM pulls WHERE status='pending' AND open=1 AND (skip=0 OR force_retry=1) AND not_before>?`, after).Scan(&n)
	return n.Int64
}

// LimitedPRs returns the open PRs held at their review limit.
func (s *Store) LimitedPRs() ([]PR, error) {
	return s.queryPRs(`SELECT * FROM pulls_v WHERE status='limit' AND open=1`)
}

// DailyCappedPRs returns the open PRs held by their organization's daily
// review limit, longest held first.
func (s *Store) DailyCappedPRs() ([]PR, error) {
	return s.queryPRs(`SELECT * FROM pulls_v WHERE status='daily' AND open=1 ORDER BY held_at, repo, number`)
}

// ReviewedOpenPRs returns open PRs the bot has reviewed (candidates for the
// reply sweep), including those held by the daily review limit, which still
// get their replies.
func (s *Store) ReviewedOpenPRs() ([]PR, error) {
	return s.queryPRs(`SELECT * FROM pulls_v WHERE open=1 AND status IN ('reviewed','pending','daily')`)
}

// ResetReviewing re-queues reviews interrupted by a shutdown.
func (s *Store) ResetReviewing() error {
	_, err := s.db.Exec(`UPDATE pulls SET status='pending', not_before=0 WHERE status='reviewing'`)
	return err
}

func (s *Store) CountPRsByStatus(accountID int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM pulls WHERE open=1 AND (?=-1 OR account_id=?) GROUP BY status`, acct(accountID)...)
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
	AccountID        int64
	Event            string // APPROVE, REQUEST_CHANGES or COMMENT once posted
	Fixed            int    // earlier findings this review confirmed fixed and resolved
	Sev              [5]int // findings posted (inline and notes) at P1..P5
	Detailed         bool   // Sev and Fixed were recorded; false for reviews from before they were
}

func scanReview(sc scanner) (*Review, error) {
	var r Review
	err := sc.Scan(&r.ID, &r.Repo, &r.Number, &r.HeadSHA, &r.GHReviewID, &r.URL, &r.Summary, &r.Comments,
		&r.Notes, &r.Model, &r.Skill, &r.Status, &r.Error, &r.Raw, &r.Batches, &r.PromptTokens,
		&r.CompletionTokens, &r.StartedAt, &r.FinishedAt,
		&r.AccountID, &r.Event, &r.Fixed, &r.Sev[0], &r.Sev[1], &r.Sev[2], &r.Sev[3], &r.Sev[4], &r.Detailed)
	return &r, err
}

func (s *Store) CreateReview(r *Review) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO reviews(repo, number, head_sha, model, skill, status, started_at, account_id, detailed) VALUES(?,?,?,?,?,?,?,?,1)`,
		r.Repo, r.Number, r.HeadSHA, r.Model, r.Skill, r.Status, r.StartedAt, r.AccountID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateReview(r *Review) error {
	_, err := s.db.Exec(`UPDATE reviews SET head_sha=?, gh_review_id=?, url=?, summary=?, comments=?, notes=?, model=?,
skill=?, status=?, error=?, raw=?, batches=?, prompt_tokens=?, completion_tokens=?, finished_at=?,
event=?, fixed=?, sev_p1=?, sev_p2=?, sev_p3=?, sev_p4=?, sev_p5=? WHERE id=?`,
		r.HeadSHA, r.GHReviewID, r.URL, r.Summary, r.Comments, r.Notes, r.Model, r.Skill, r.Status, r.Error,
		r.Raw, r.Batches, r.PromptTokens, r.CompletionTokens, r.FinishedAt,
		r.Event, r.Fixed, r.Sev[0], r.Sev[1], r.Sev[2], r.Sev[3], r.Sev[4], r.ID)
	return err
}

func (s *Store) GetReview(id int64) (*Review, error) {
	r, err := scanReview(s.db.QueryRow(`SELECT * FROM reviews_v WHERE id=?`, id))
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

func (s *Store) ListReviews(accountID int64, repo string, number, limit int) ([]Review, error) {
	return s.queryReviews(`SELECT * FROM reviews_v WHERE account_id=? AND repo=? AND number=? ORDER BY id DESC LIMIT ?`,
		accountID, repo, number, limit)
}

// PostedReviews counts the reviews the bot posted on a PR of accountID's:
// its review rounds. Failed runs and runs with nothing to review don't count.
func (s *Store) PostedReviews(accountID int64, repo string, number int) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE account_id=? AND repo=? AND number=? AND status='posted'`,
		accountID, repo, number).Scan(&n)
	return n, err
}

// DailyUsage is how much of an organization's daily review limit is taken.
type DailyUsage struct {
	Posted    int   // reviews posted since the window began
	Oldest    int64 // when the oldest of them started (0 if none)
	Reviewing int   // reviews under way now
	Due       int   // PRs waiting for a review that is due now (as DuePRs counts them), and released ones
	// Reserved counts the PRs the daily limit held and has let through, still
	// waiting to be reviewed: their places are kept for them.
	Reserved int
}

// DailyReviewUsage counts accountID's reviews posted since since (a Unix
// time), the reviews under way and the PRs due for one at now. A review
// counts from when it started, so it holds its place for 24 hours from then.
func (s *Store) DailyReviewUsage(accountID, since, now int64) (DailyUsage, error) {
	var u DailyUsage
	var oldest sql.NullInt64
	err := s.db.QueryRow(`SELECT COUNT(*), MIN(started_at) FROM reviews WHERE account_id=? AND status='posted' AND started_at>=?`,
		accountID, since).Scan(&u.Posted, &oldest)
	if err != nil {
		return u, err
	}
	u.Oldest = oldest.Int64
	err = s.db.QueryRow(`SELECT COALESCE(SUM(status='reviewing'), 0),
  COALESCE(SUM(status='pending' AND (skip=0 OR force_retry=1) AND (held_at>0 OR not_before<=?)), 0),
  COALESCE(SUM(status='pending' AND (skip=0 OR force_retry=1) AND held_at>0), 0) FROM pulls WHERE account_id=? AND open=1`,
		now, accountID).Scan(&u.Reviewing, &u.Due, &u.Reserved)
	return u, err
}

// ReviewFilter selects reviews for the review history pages.
type ReviewFilter struct {
	AccountID int64  // AllAccounts for every organization
	Repo      string // "" = any
	Status    string // "" = any
	Limit     int
	Offset    int
}

// QueryReviews returns the reviews matching f, newest first, and how many match in total.
func (s *Store) QueryReviews(f ReviewFilter) ([]Review, int, error) {
	// An empty Repo or Status matches every review.
	args := []any{f.AccountID, f.AccountID, f.Repo, f.Repo, f.Status, f.Status}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reviews
WHERE (?=-1 OR account_id=?) AND (?='' OR repo=?) AND (?='' OR status=?)`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	out, err := s.queryReviews(`SELECT * FROM reviews_v
WHERE (?=-1 OR account_id=?) AND (?='' OR repo=?) AND (?='' OR status=?) ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, max(0, f.Offset))...)
	return out, total, err
}

// RecentReviews lists one organization's newest reviews (or all, with AllAccounts).
func (s *Store) RecentReviews(accountID int64, limit int) ([]Review, error) {
	out, _, err := s.QueryReviews(ReviewFilter{AccountID: accountID, Limit: limit})
	return out, err
}

// ReviewRepoNames lists the repository names one organization's reviews (or
// everyone's, with AllAccounts) are filed under, earlier repositories' too.
func (s *Store) ReviewRepoNames(accountID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT repo FROM reviews WHERE (?=-1 OR account_id=?) ORDER BY repo COLLATE NOCASE`, acct(accountID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// LatestPostedReview returns the most recent review that was posted to GitHub, or nil.
func (s *Store) LatestPostedReview(accountID int64, repo string, number int) (*Review, error) {
	r, err := scanReview(s.db.QueryRow(`SELECT * FROM reviews_v WHERE account_id=? AND repo=? AND number=? AND status='posted' ORDER BY id DESC LIMIT 1`,
		accountID, repo, number))
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
	ID        int64
	TS        int64
	Level     string
	Repo      string
	Number    int
	Message   string
	AccountID int64
}

// Log records an activity entry. An entry about a repository is linked to the
// organization that owns it; the rest (account 0) only administrators see.
func (s *Store) Log(level, repo string, number int, msg string) {
	s.LogFor(0, level, repo, number, msg)
}

// LogFor records an activity entry under an organization. With accountID 0
// it's worked out from the PR, then the repository (the most recently
// created one, if a name has belonged to two accounts); an entry about
// neither is instance-wide, shown only to administrators.
func (s *Store) LogFor(accountID int64, level, repo string, number int, msg string) {
	_, _ = s.db.Exec(`INSERT INTO activity(ts, level, repo, number, message, account_id)
VALUES(?,?,?,?,?,COALESCE(NULLIF(?,0),
  (SELECT account_id FROM pulls WHERE repo=? AND number=? AND account_id<>0),
  (SELECT account_id FROM repos WHERE full_name=? AND account_id<>0 ORDER BY id DESC LIMIT 1),
  0))`,
		time.Now().Unix(), level, repo, number, msg, accountID, repo, number, repo)
	s.trimActivity()
}

// LogAs records an activity entry under exactly accountID, worked out by the
// caller: 0 leaves it unassigned (administrators only), with no lookup by
// name, which could find another repository that has had the name. It stays
// as given: BackfillAccounts doesn't reassign it at the next start.
func (s *Store) LogAs(accountID int64, level, repo string, number int, msg string) {
	_, _ = s.db.Exec(`INSERT INTO activity(ts, level, repo, number, message, account_id, account_given) VALUES(?,?,?,?,?,?,1)`,
		time.Now().Unix(), level, repo, number, msg, accountID)
	s.trimActivity()
}

// trimActivity keeps the newest 20,000 entries, checking every 200 writes.
func (s *Store) trimActivity() {
	if s.logs.Add(1)%200 == 0 {
		_, _ = s.db.Exec(`DELETE FROM activity WHERE id <= (SELECT MAX(id) FROM activity) - 20000`)
	}
}

// RecentActivity lists one organization's activity (or all, with
// AllAccounts), optionally only one PR's.
func (s *Store) RecentActivity(accountID int64, limit int, repo string, number int) ([]Activity, error) {
	// An empty repo lists everything; otherwise only that PR's entries.
	rows, err := s.db.Query(`SELECT id, ts, level, repo, number, message, account_id FROM activity
WHERE (?=-1 OR account_id=?) AND (?='' OR (repo=? AND number=?)) ORDER BY id DESC LIMIT ?`,
		accountID, accountID, repo, repo, number, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.TS, &a.Level, &a.Repo, &a.Number, &a.Message, &a.AccountID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
