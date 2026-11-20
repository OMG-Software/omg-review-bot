package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ---------- users ----------

// User is a person who signs in with GitHub. ID is their GitHub user ID.
type User struct {
	ID        int64
	Login     string
	Name      string
	AvatarURL string
	Admin     bool
	Disabled  bool
	Epoch     int64 // bumped to sign the user out everywhere
	CreatedAt int64
	LastLogin int64 // 0 = added by an org owner but hasn't signed in yet
}

func scanUser(sc scanner) (*User, error) {
	var u User
	var admin, disabled int
	if err := sc.Scan(&u.ID, &u.Login, &u.Name, &u.AvatarURL, &admin, &disabled, &u.Epoch, &u.CreatedAt, &u.LastLogin); err != nil {
		return nil, err
	}
	u.Admin, u.Disabled = admin == 1, disabled == 1
	return &u, nil
}

func (s *Store) queryUsers(q string, args ...any) ([]User, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// RecordSignIn creates or refreshes a user from their GitHub profile.
func (s *Store) RecordSignIn(id int64, login, name, avatar string) (*User, error) {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO users(id, login, name, avatar_url, created_at, last_login) VALUES(?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET login=excluded.login, name=excluded.name, avatar_url=excluded.avatar_url, last_login=excluded.last_login`,
		id, login, name, avatar, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetUser(id)
}

// EnsureUser records a GitHub user who hasn't signed in yet (added to an org by login).
func (s *Store) EnsureUser(id int64, login string) error {
	_, err := s.db.Exec(`INSERT INTO users(id, login, created_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET login=excluded.login`,
		id, login, time.Now().Unix())
	return err
}

// GetUser returns nil, nil for an unknown user.
func (s *Store) GetUser(id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT * FROM users_v WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (s *Store) ListUsers() ([]User, error) {
	return s.queryUsers(`SELECT * FROM users_v ORDER BY login COLLATE NOCASE`)
}

func (s *Store) SetUserAdmin(id int64, admin bool) error {
	_, err := s.db.Exec(`UPDATE users SET is_admin=? WHERE id=?`, b2i(admin), id)
	return err
}

// SetUserDisabled blocks or unblocks sign-in; blocking also ends the user's sessions.
func (s *Store) SetUserDisabled(id int64, disabled bool) error {
	_, err := s.db.Exec(`UPDATE users SET disabled=?, session_epoch=session_epoch+? WHERE id=?`, b2i(disabled), b2i(disabled), id)
	return err
}

// BumpUserEpoch signs a user out of every session.
func (s *Store) BumpUserEpoch(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET session_epoch=session_epoch+1 WHERE id=?`, id)
	return err
}

// ---------- organizations ----------

// Org is a GitHub organization (or personal account) the App is installed on.
// ID is the GitHub account ID.
type Org struct {
	ID             int64
	Login          string
	Type           string // Organization | User
	InstallationID int64
	Active         bool // false once the App is uninstalled; its history is kept
	AutoJoin       bool // members of the GitHub org get access when they sign in
	Policy         string
	CreatedAt      int64
}

// Personal reports whether the account is a user, not an organization.
func (o Org) Personal() bool { return o.Type == "User" }

func scanOrg(sc scanner) (*Org, error) {
	var o Org
	var active, auto int
	if err := sc.Scan(&o.ID, &o.Login, &o.Type, &o.InstallationID, &active, &auto, &o.Policy, &o.CreatedAt); err != nil {
		return nil, err
	}
	o.Active, o.AutoJoin = active == 1, auto == 1
	return &o, nil
}

func (s *Store) queryOrgs(q string, args ...any) ([]Org, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// UpsertOrg records an App installation's account as an active organization.
func (s *Store) UpsertOrg(id int64, login, typ string, installationID int64) error {
	if typ == "" {
		typ = "Organization"
	}
	_, err := s.db.Exec(`INSERT INTO orgs(id, login, type, installation_id, active, created_at) VALUES(?,?,?,?,1,?)
ON CONFLICT(id) DO UPDATE SET login=excluded.login, type=excluded.type, installation_id=excluded.installation_id, active=1`,
		id, login, typ, installationID, time.Now().Unix())
	return err
}

// DeactivateOrgsExcept marks organizations whose installation is gone as inactive.
func (s *Store) DeactivateOrgsExcept(keep []int64) error {
	set := make(map[int64]bool, len(keep))
	for _, id := range keep {
		set[id] = true
	}
	orgs, err := s.ListOrgs()
	if err != nil {
		return err
	}
	for _, o := range orgs {
		if o.Active && !set[o.ID] {
			if _, err := s.db.Exec(`UPDATE orgs SET active=0 WHERE id=?`, o.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetOrg returns nil, nil for an unknown organization.
func (s *Store) GetOrg(id int64) (*Org, error) {
	o, err := scanOrg(s.db.QueryRow(`SELECT * FROM orgs_v WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

// GetOrgByLogin returns nil, nil for an unknown organization.
func (s *Store) GetOrgByLogin(login string) (*Org, error) {
	o, err := scanOrg(s.db.QueryRow(`SELECT * FROM orgs_v WHERE login=? COLLATE NOCASE ORDER BY active DESC LIMIT 1`, login))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

func (s *Store) ListOrgs() ([]Org, error) {
	return s.queryOrgs(`SELECT * FROM orgs_v ORDER BY login COLLATE NOCASE`)
}

func (s *Store) SetOrgAutoJoin(id int64, on bool) error {
	_, err := s.db.Exec(`UPDATE orgs SET auto_join=? WHERE id=?`, b2i(on), id)
	return err
}

// SetOrgPolicy stores the organization's review policy as JSON ("" = use the instance defaults).
func (s *Store) SetOrgPolicy(id int64, policy string) error {
	_, err := s.db.Exec(`UPDATE orgs SET policy=? WHERE id=?`, policy, id)
	return err
}

// OrgPolicy returns the organization's policy JSON ("" when it uses the defaults or is unknown).
func (s *Store) OrgPolicy(id int64) string {
	var p string
	_ = s.db.QueryRow(`SELECT policy FROM orgs WHERE id=?`, id).Scan(&p)
	return p
}

// ---------- memberships ----------

const (
	RoleOwner  = "owner"
	RoleMember = "member"

	SourceManual = "manual" // added by an org owner or administrator
	SourceGitHub = "github" // derived from the person's GitHub org role at sign-in
)

// Member is one person's access to an organization.
type Member struct {
	User
	Role      string
	Source    string
	MemberAt  int64
	OrgID     int64
	OrgLogin  string
	OrgActive bool
}

// SetMembership adds a person to an organization or changes their role.
func (s *Store) SetMembership(orgID, userID int64, role, source string) error {
	_, err := s.db.Exec(`INSERT INTO memberships(org_id, user_id, role, source, created_at) VALUES(?,?,?,?,?)
ON CONFLICT(org_id, user_id) DO UPDATE SET role=excluded.role, source=excluded.source`,
		orgID, userID, role, source, time.Now().Unix())
	return err
}

// AddMember gives someone access by hand, only if they don't have it yet: it
// never changes an existing member's role (that goes through SetMemberRole,
// which keeps an owner). It reports whether they were added.
func (s *Store) AddMember(orgID, userID int64, role string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO memberships(org_id, user_id, role, source, created_at) VALUES(?,?,?,?,?)
ON CONFLICT(org_id, user_id) DO NOTHING`, orgID, userID, role, SourceManual, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) RemoveMembership(orgID, userID int64) error {
	_, err := s.db.Exec(`DELETE FROM memberships WHERE org_id=? AND user_id=?`, orgID, userID)
	return err
}

// ErrLastOwner is returned when a change would leave an organization without an owner.
var ErrLastOwner = errors.New("an organization needs at least one owner")

// SetMemberRole sets a role chosen by hand (so sign-in won't change it).
// Demoting an owner only succeeds while another owner remains. The check and
// the change are one statement, which SQLite runs under its write lock, so two
// owners demoting each other at once can't both succeed.
func (s *Store) SetMemberRole(orgID, userID int64, role string) error {
	res, err := s.db.Exec(`UPDATE memberships SET role=?, source='manual' WHERE org_id=? AND user_id=?
AND (?='owner' OR role<>'owner' OR (SELECT COUNT(*) FROM memberships WHERE org_id=? AND role='owner')>1)`,
		role, orgID, userID, role, orgID)
	return s.lastOwnerCheck(res, err, orgID, userID)
}

// RemoveMember removes someone's access, refusing to remove the last owner,
// with the check and the change in one statement (see SetMemberRole).
func (s *Store) RemoveMember(orgID, userID int64) error {
	res, err := s.db.Exec(`DELETE FROM memberships WHERE org_id=? AND user_id=?
AND (role<>'owner' OR (SELECT COUNT(*) FROM memberships WHERE org_id=? AND role='owner')>1)`,
		orgID, userID, orgID)
	return s.lastOwnerCheck(res, err, orgID, userID)
}

// lastOwnerCheck turns "no row changed" into ErrLastOwner while the
// membership still exists (if it's gone, someone else removed it: no error).
func (s *Store) lastOwnerCheck(res sql.Result, err error, orgID, userID int64) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	if m, err := s.GetMembership(orgID, userID); err != nil || m == nil {
		return err
	}
	return ErrLastOwner
}

// GetMembership returns nil, nil when the person has no access to the organization.
func (s *Store) GetMembership(orgID, userID int64) (*Member, error) {
	if orgID <= 0 || userID <= 0 {
		return nil, nil // 0 means "any" to queryMembers, never a person
	}
	ms, err := s.queryMembers(orgID, userID)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	return &ms[0], nil
}

// ListMembers lists who has access to an organization, owners first.
func (s *Store) ListMembers(orgID int64) ([]Member, error) {
	return s.queryMembers(orgID, 0)
}

// UserMemberships lists the organizations a person has access to.
func (s *Store) UserMemberships(userID int64) ([]Member, error) {
	return s.queryMembers(0, userID)
}

// MembershipsByUser lists everyone's organizations in one query, by user ID.
func (s *Store) MembershipsByUser() (map[int64][]Member, error) {
	ms, err := s.queryMembers(0, 0)
	if err != nil {
		return nil, err
	}
	out := map[int64][]Member{}
	for _, m := range ms {
		out[m.ID] = append(out[m.ID], m)
	}
	return out, nil
}

// queryMembers lists one organization's members (userID 0: owners first,
// then by login), one person's organizations (orgID 0: by name), or one
// membership (both set).
func (s *Store) queryMembers(orgID, userID int64) ([]Member, error) {
	rows, err := s.db.Query(`SELECT u.id, u.login, u.name, u.avatar_url, u.is_admin, u.disabled, u.session_epoch, u.created_at, u.last_login,
m.role, m.source, m.created_at, o.id, o.login, o.active
FROM memberships m JOIN users u ON u.id=m.user_id JOIN orgs o ON o.id=m.org_id
WHERE (?=0 OR m.org_id=?) AND (?=0 OR m.user_id=?)
ORDER BY o.login COLLATE NOCASE, m.role='owner' DESC, u.login COLLATE NOCASE`, orgID, orgID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var admin, disabled, active int
		if err := rows.Scan(&m.ID, &m.Login, &m.Name, &m.AvatarURL, &admin, &disabled, &m.Epoch, &m.CreatedAt, &m.LastLogin,
			&m.Role, &m.Source, &m.MemberAt, &m.OrgID, &m.OrgLogin, &active); err != nil {
			return nil, err
		}
		m.Admin, m.Disabled, m.OrgActive = admin == 1, disabled == 1, active == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- webhook deliveries ----------

// Delivery is one webhook request GitHub sent.
type Delivery struct {
	ID         string
	Event      string
	Action     string
	Repo       string
	AccountID  int64
	Result     string
	ReceivedAt int64
}

// RecordDelivery stores a delivery. It returns false if this delivery ID was
// already received (GitHub redelivers on timeouts, and people can redeliver by hand).
func (s *Store) RecordDelivery(d Delivery) (bool, error) {
	if d.ReceivedAt == 0 {
		d.ReceivedAt = time.Now().Unix()
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO webhook_deliveries(id, event, action, repo, account_id, result, received_at) VALUES(?,?,?,?,?,?,?)`,
		d.ID, d.Event, d.Action, d.Repo, d.AccountID, d.Result, d.ReceivedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 && s.deliveries.Add(1)%100 == 0 {
		_, _ = s.db.Exec(`DELETE FROM webhook_deliveries WHERE received_at < ?`, time.Now().Add(-30*24*time.Hour).Unix())
	}
	return n > 0, nil
}

// SetDeliveryResult records what the bot did with a delivery.
func (s *Store) SetDeliveryResult(id string, accountID int64, result string) error {
	_, err := s.db.Exec(`UPDATE webhook_deliveries SET result=?, account_id=? WHERE id=?`, result, accountID, id)
	return err
}

func (s *Store) RecentDeliveries(limit int) ([]Delivery, error) {
	rows, err := s.db.Query(`SELECT id, event, action, repo, account_id, result, received_at FROM webhook_deliveries ORDER BY received_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.Event, &d.Action, &d.Repo, &d.AccountID, &d.Result, &d.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDeliveriesSince counts webhook deliveries received since a time.
func (s *Store) CountDeliveriesSince(since int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE received_at>=?`, since).Scan(&n)
	return n
}

// ---------- statistics ----------

// ReviewStats summarizes reviews over a period.
type ReviewStats struct {
	Reviews          int    // every review run, whatever the outcome
	Posted           int    // posted to GitHub
	Errors           int    // failed
	Approvals        int    // posted as an approval
	PRs              int    // distinct pull requests reviewed
	Repos            int    // distinct repositories reviewed
	Comments         int    // inline comments posted
	Notes            int    // findings posted in the review body
	Fixed            int    // findings later confirmed fixed by the bot
	Sev              [5]int // findings by level P1..P5
	PromptTokens     int64
	CompletionTokens int64
	AvgSeconds       int // average duration of a posted review
}

// Findings is the number of findings posted.
func (r ReviewStats) Findings() int { return r.Comments + r.Notes }

// ReviewStatsSince summarizes one organization's reviews (or all, with AllAccounts) started at or after since.
func (s *Store) ReviewStatsSince(accountID, since int64) (ReviewStats, error) {
	var r ReviewStats
	var avg sql.NullFloat64
	err := s.db.QueryRow(`SELECT COUNT(*),
  COALESCE(SUM(status='posted'),0), COALESCE(SUM(status='error'),0), COALESCE(SUM(event='APPROVE'),0),
  COUNT(DISTINCT CASE WHEN status='posted' THEN repo||'#'||number END),
  COUNT(DISTINCT CASE WHEN status='posted' THEN repo END),
  COALESCE(SUM(comments),0), COALESCE(SUM(notes),0), COALESCE(SUM(fixed),0),
  COALESCE(SUM(sev_p1),0), COALESCE(SUM(sev_p2),0), COALESCE(SUM(sev_p3),0), COALESCE(SUM(sev_p4),0), COALESCE(SUM(sev_p5),0),
  COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
  AVG(CASE WHEN status='posted' AND finished_at>=started_at THEN finished_at-started_at END)
FROM reviews WHERE started_at>=? AND (?=-1 OR account_id=?)`, since, accountID, accountID).Scan(
		&r.Reviews, &r.Posted, &r.Errors, &r.Approvals, &r.PRs, &r.Repos, &r.Comments, &r.Notes, &r.Fixed,
		&r.Sev[0], &r.Sev[1], &r.Sev[2], &r.Sev[3], &r.Sev[4], &r.PromptTokens, &r.CompletionTokens, &avg)
	r.AvgSeconds = int(avg.Float64)
	return r, err
}

// DayStats is one day's reviews (UTC).
type DayStats struct {
	Day      time.Time
	Posted   int
	Errors   int
	Findings int
	Serious  int // P1 and P2 findings
}

// ReviewsPerDay returns one entry per day for the last `days` days (UTC), oldest first.
func (s *Store) ReviewsPerDay(accountID int64, days int, now time.Time) ([]DayStats, error) {
	today := now.UTC().Truncate(24 * time.Hour)
	start := time.Unix(DayWindowStart(days, now), 0).UTC()
	rows, err := s.db.Query(`SELECT date(started_at,'unixepoch'), COALESCE(SUM(status='posted'),0), COALESCE(SUM(status='error'),0),
  COALESCE(SUM(comments+notes),0), COALESCE(SUM(sev_p1+sev_p2),0)
FROM reviews WHERE started_at>=? AND (?=-1 OR account_id=?) GROUP BY 1`, start.Unix(), accountID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[string]DayStats{}
	for rows.Next() {
		var day string
		var d DayStats
		if err := rows.Scan(&day, &d.Posted, &d.Errors, &d.Findings, &d.Serious); err != nil {
			return nil, err
		}
		byDay[day] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DayStats, 0, days)
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		ds := byDay[d.Format("2006-01-02")]
		ds.Day = d
		out = append(out, ds)
	}
	return out, nil
}

// RepoStats summarizes one repository's reviews.
type RepoStats struct {
	Repo     string
	Reviews  int
	PRs      int
	Findings int
	Serious  int // P1 and P2
	Errors   int
	Last     int64
}

// RepoStatsSince ranks repositories by reviews since a time.
func (s *Store) RepoStatsSince(accountID, since int64, limit int) ([]RepoStats, error) {
	rows, err := s.db.Query(`SELECT repo, COALESCE(SUM(status='posted'),0), COUNT(DISTINCT CASE WHEN status='posted' THEN number END),
  COALESCE(SUM(comments+notes),0), COALESCE(SUM(sev_p1+sev_p2),0), COALESCE(SUM(status='error'),0), MAX(started_at)
FROM reviews WHERE started_at>=? AND (?=-1 OR account_id=?) GROUP BY repo ORDER BY 2 DESC, 7 DESC LIMIT ?`,
		since, accountID, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoStats
	for rows.Next() {
		var r RepoStats
		if err := rows.Scan(&r.Repo, &r.Reviews, &r.PRs, &r.Findings, &r.Serious, &r.Errors, &r.Last); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OrgSummary is one organization's row on the administrator's overview.
type OrgSummary struct {
	Org
	Members      int
	Owners       int
	Repos        int
	EnabledRepos int
	OpenPRs      int
	Reviews30d   int
	Findings30d  int
	LastReview   int64
}

// OrgSummaries summarizes every organization (for administrators) or the given ones.
func (s *Store) OrgSummaries(only []int64) ([]OrgSummary, error) {
	orgs, err := s.ListOrgs()
	if err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, id := range only {
		want[id] = true
	}
	since := time.Now().Add(-30 * 24 * time.Hour).Unix()
	var out []OrgSummary
	for _, o := range orgs {
		if only != nil && !want[o.ID] {
			continue
		}
		sm := OrgSummary{Org: o}
		_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(role='owner'),0) FROM memberships WHERE org_id=?`, o.ID).Scan(&sm.Members, &sm.Owners)
		_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM repos WHERE account_id=?`, o.ID).Scan(&sm.Repos, &sm.EnabledRepos)
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM pulls WHERE account_id=? AND open=1`, o.ID).Scan(&sm.OpenPRs)
		var last sql.NullInt64
		_ = s.db.QueryRow(`SELECT COALESCE(SUM(status='posted'),0), COALESCE(SUM(comments+notes),0) FROM reviews WHERE account_id=? AND started_at>=?`,
			o.ID, since).Scan(&sm.Reviews30d, &sm.Findings30d)
		_ = s.db.QueryRow(`SELECT MAX(started_at) FROM reviews WHERE account_id=?`, o.ID).Scan(&last)
		sm.LastReview = last.Int64
		out = append(out, sm)
	}
	return out, nil
}

// DayWindowStart is the start (midnight UTC) of the last `days` whole UTC
// days up to now, today included: the range ReviewsPerDay charts. Totals
// shown beside a chart use it too, so they add up to the chart's bars.
func DayWindowStart(days int, now time.Time) int64 {
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1)).Unix()
}

// NormLogin lowercases a GitHub login for comparisons.
func NormLogin(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@")))
}
