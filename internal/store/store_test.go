package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateOldDatabase opens a database made by the single-tenant release
// and checks the new columns are added and old rows linked to their organization.
func TestMigrateOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE repos (id INTEGER PRIMARY KEY, installation_id INTEGER NOT NULL, full_name TEXT NOT NULL,
		  enabled INTEGER NOT NULL DEFAULT 1, synced INTEGER NOT NULL DEFAULT 0, etag TEXT NOT NULL DEFAULT '', last_polled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE pulls (repo TEXT NOT NULL, number INTEGER NOT NULL, installation_id INTEGER NOT NULL DEFAULT 0,
		  title TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '',
		  head_ref TEXT NOT NULL DEFAULT '', draft INTEGER NOT NULL DEFAULT 0, open INTEGER NOT NULL DEFAULT 1, labels TEXT NOT NULL DEFAULT '',
		  head_sha TEXT NOT NULL DEFAULT '', reviewed_sha TEXT NOT NULL DEFAULT '', attempt_sha TEXT NOT NULL DEFAULT '',
		  attempts INTEGER NOT NULL DEFAULT 0, gh_updated_at TEXT NOT NULL DEFAULT '', comments_seen TEXT NOT NULL DEFAULT '',
		  status TEXT NOT NULL DEFAULT 'new', last_error TEXT NOT NULL DEFAULT '', skip INTEGER NOT NULL DEFAULT 0,
		  not_before INTEGER NOT NULL DEFAULT 0, tracked_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (repo, number))`,
		`CREATE TABLE reviews (id INTEGER PRIMARY KEY AUTOINCREMENT, repo TEXT NOT NULL, number INTEGER NOT NULL,
		  head_sha TEXT NOT NULL DEFAULT '', gh_review_id INTEGER NOT NULL DEFAULT 0, url TEXT NOT NULL DEFAULT '',
		  summary TEXT NOT NULL DEFAULT '', comments INTEGER NOT NULL DEFAULT 0, notes INTEGER NOT NULL DEFAULT 0,
		  model TEXT NOT NULL DEFAULT '', skill TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		  raw TEXT NOT NULL DEFAULT '', batches INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
		  completion_tokens INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE activity (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, level TEXT NOT NULL,
		  repo TEXT NOT NULL DEFAULT '', number INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL)`,
		`INSERT INTO repos(id, installation_id, full_name) VALUES(100, 10, 'acme/web')`,
		`INSERT INTO pulls(repo, number) VALUES('acme/web', 7)`,
		`INSERT INTO reviews(repo, number, status, comments, started_at) VALUES('acme/web', 7, 'posted', 2, strftime('%s','now'))`,
		`INSERT INTO activity(ts, level, repo, message) VALUES(1, 'info', 'acme/web', 'old entry')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer st.Close()
	// The repository sync records the organization; old rows follow.
	if err := st.UpsertRepo(100, 10, 500, "acme/web", true); err != nil {
		t.Fatal(err)
	}
	if err := st.BackfillAccounts(); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPR("acme/web", 7); p == nil || p.AccountID != 500 {
		t.Errorf("PR not linked: %+v", p)
	}
	stats, err := st.ReviewStatsSince(500, time.Now().Add(-time.Hour).Unix())
	if err != nil || stats.Posted != 1 || stats.Comments != 2 {
		t.Errorf("stats = %+v, %v", stats, err)
	}
	if acts, _ := st.RecentActivity(500, 10, "", 0); len(acts) != 1 {
		t.Errorf("activity not linked: %+v", acts)
	}
	if other, _ := st.ReviewStatsSince(600, 0); other.Reviews != 0 {
		t.Error("another organization sees the review")
	}
	days, _ := st.ReviewsPerDay(500, 7, time.Now())
	if len(days) != 7 || days[6].Posted != 1 {
		t.Errorf("per-day = %+v", days)
	}
}
