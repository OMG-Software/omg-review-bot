package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/skill"
	"github.com/OMG-Software/omg-review-bot/internal/store"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// ---------- sign-in pages ----------

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "setup.html", page{Title: "First-time setup"})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	pw, confirm := r.FormValue("password"), r.FormValue("confirm")
	if len(pw) < 10 {
		s.render(w, r, "setup.html", page{Title: "First-time setup", Err: "Use at least 10 characters."})
		return
	}
	if pw != confirm {
		s.render(w, r, "setup.html", page{Title: "First-time setup", Err: "The passwords don't match."})
		return
	}
	// Setup also follows `prbot -reset-password`, usually done because the
	// password (or a session) leaked: a new signing key ends every session
	// made before, as changing the password on the Account page does. The
	// key goes first: if saving the password then fails, everyone is still
	// signed out and setup can simply be tried again.
	h, err := hashPassword(pw)
	if err == nil {
		err = s.rotateSecret()
	}
	if err == nil {
		err = s.store.Set(pwKey, h)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.issueSession(w, r, localAdminID, 0)
	redirectMsg(w, r, "/settings", "Administrator password set. Next: connect your GitHub App and model.")
}

func (s *Server) loginData(r *http.Request) map[string]any {
	return map[string]any{"SignIn": s.bot.Settings().GitHub.SignInOn(), "Next": safeNext(r.FormValue("next"))}
}

// loginPage is the sign-in page for everyone: GitHub only. The administrator
// password has its own page, /admin-login, which nothing here links to. Until
// GitHub sign-in is set up the administrator is the only one who can sign in,
// so /login sends them there.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if s.sessionViewer(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	if !s.bot.Settings().GitHub.SignInOn() {
		http.Redirect(w, r, adminLoginPath(next), http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", page{Title: "Sign in", D: s.loginData(r)})
}

func adminLoginPath(next string) string {
	if next == "/" {
		return "/admin-login"
	}
	return "/admin-login?next=" + url.QueryEscape(next)
}

func (s *Server) adminLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.sessionViewer(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, "admin_login.html", page{Title: "Administrator sign-in", D: s.loginData(r)})
}

func (s *Server) adminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	stored, _, _ := s.store.Get(pwKey)
	if !checkPassword(stored, r.FormValue("password")) {
		time.Sleep(700 * time.Millisecond) // slow down guessing
		s.render(w, r, "admin_login.html", page{Title: "Administrator sign-in", Err: "Wrong password.", D: s.loginData(r)})
		return
	}
	s.issueSession(w, r, localAdminID, 0)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

// logout sends the password administrator back to their own sign-in page and
// everyone else to the GitHub one.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	to := "/login"
	if v := s.sessionViewer(r); v != nil && v.Local {
		to = "/admin-login"
	}
	s.clearSession(w, r)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "account.html", page{Title: "Account", Nav: "account", D: s.loginData(r)})
}

// accountSubmit changes the administrator password.
func (s *Server) accountSubmit(w http.ResponseWriter, r *http.Request) {
	if !me(r).Local {
		s.forbidden(w, r, "Only the password administrator has a password here; GitHub users sign in with GitHub.")
		return
	}
	fail := func(msg string) {
		s.render(w, r, "account.html", page{Title: "Account", Nav: "account", Err: msg, D: s.loginData(r)})
	}
	stored, _, _ := s.store.Get(pwKey)
	if !checkPassword(stored, r.FormValue("current")) {
		fail("Current password is wrong.")
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 || pw != r.FormValue("confirm") {
		fail("New passwords must match and be at least 10 characters.")
		return
	}
	// The new key first, so old sessions never outlive the old password.
	h, err := hashPassword(pw)
	if err == nil {
		err = s.rotateSecret()
	}
	if err == nil {
		err = s.store.Set(pwKey, h)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.issueSession(w, r, localAdminID, 0)
	redirectMsg(w, r, "/account", "Password changed. Everyone has been signed out, including GitHub users; they sign in again with GitHub.")
}

// accountSignOutAll ends every session of the signed-in GitHub user.
func (s *Server) accountSignOutAll(w http.ResponseWriter, r *http.Request) {
	v := me(r)
	if v.Local {
		s.forbidden(w, r, "Change the administrator password to sign out every administrator session.")
		return
	}
	if err := s.store.BumpUserEpoch(v.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.clearSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- home and administrator dashboard ----------

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	v := me(r)
	if !v.Admin {
		if len(v.Orgs) == 1 {
			http.Redirect(w, r, "/orgs/"+url.PathEscape(v.Orgs[0].OrgLogin), http.StatusSeeOther)
			return
		}
		s.orgsPage(w, r)
		return
	}
	now := time.Now()
	stats, _ := s.store.ReviewStatsSince(store.AllAccounts, store.DayWindowStart(30, now)) // the chart's days
	days, _ := s.store.ReviewsPerDay(store.AllAccounts, 30, now)
	reviews, _ := s.store.RecentReviews(store.AllAccounts, 10)
	acts, _ := s.store.RecentActivity(store.AllAccounts, 15, "", 0)
	counts, _ := s.store.CountPRsByStatus(store.AllAccounts)
	orgs, _ := s.store.OrgSummaries(nil)
	users, _ := s.store.CountUsers()
	st := s.bot.Settings()
	s.render(w, r, "dashboard.html", page{Title: "Dashboard", Nav: "dash", D: map[string]any{
		"Reviews": reviews, "Activity": acts, "Counts": counts, "Stats": stats, "Days": days, "Orgs": orgs,
		"Users": users, "SignIn": st.GitHub.SignInOn(), "WebhookOn": st.GitHub.WebhookSecret != "",
		"Deliveries24h": s.store.CountDeliveriesSince(time.Now().Add(-24 * time.Hour).Unix()),
	}})
}

// ---------- pull requests and reviews ----------

func (s *Server) prsPage(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	prs, err := s.store.ListPRs(store.AllAccounts, all, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "prs.html", page{Title: "Pull requests", Nav: "prs", D: map[string]any{"PRs": prs, "All": all, "Base": "/prs"}})
}

func prKey(r *http.Request) (string, int, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return "", 0, errors.New("bad PR number")
	}
	return r.PathValue("owner") + "/" + r.PathValue("repo"), n, nil
}

func prPath(repo string, n int) string { return fmt.Sprintf("/prs/%s/%d", repo, n) }

// prFor loads the PR in the URL if the viewer may see it; otherwise it
// answers 404 and returns nil.
func (s *Server) prFor(w http.ResponseWriter, r *http.Request) *store.PR {
	repo, n, err := prKey(r)
	if err != nil {
		s.notFound(w, r)
		return nil
	}
	pr, err := s.store.GetPR(repo, n)
	if err != nil || pr == nil || me(r).Role(pr.AccountID) == "" {
		s.notFound(w, r)
		return nil
	}
	return pr
}

func (s *Server) prPage(w http.ResponseWriter, r *http.Request) {
	pr := s.prFor(w, r)
	if pr == nil {
		return
	}
	// Only this organization's history: a repository name can be reused by
	// another organization later.
	reviews, _ := s.store.ListReviews(pr.AccountID, pr.Repo, pr.Number, 50)
	acts, _ := s.store.RecentActivity(pr.AccountID, 50, pr.Repo, pr.Number)
	org, _ := s.store.GetOrg(pr.AccountID)
	s.render(w, r, "pr.html", page{Title: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Nav: "prs", Org: org, Tab: "prs", D: map[string]any{
		"PR": pr, "Reviews": reviews, "Activity": acts, "CanManage": me(r).CanManage(pr.AccountID),
	}})
}

func (s *Server) prReview(w http.ResponseWriter, r *http.Request) {
	pr := s.prFor(w, r)
	if pr == nil {
		return
	}
	if err := s.bot.ReviewNow(pr.Repo, pr.Number, pr.AccountID); err != nil {
		redirectErr(w, r, prPath(pr.Repo, pr.Number), err)
		return
	}
	redirectMsg(w, r, prPath(pr.Repo, pr.Number), "Review queued.")
}

func (s *Server) prReplies(w http.ResponseWriter, r *http.Request) {
	pr := s.prFor(w, r)
	if pr == nil {
		return
	}
	if err := s.bot.CheckReplies(pr.Repo, pr.Number, pr.AccountID); err != nil {
		redirectErr(w, r, prPath(pr.Repo, pr.Number), err)
		return
	}
	redirectMsg(w, r, prPath(pr.Repo, pr.Number), "Checking for replies and mentions.")
}

func (s *Server) prSkip(w http.ResponseWriter, r *http.Request) {
	pr := s.prFor(w, r)
	if pr == nil {
		return
	}
	if !me(r).CanManage(pr.AccountID) {
		s.forbidden(w, r, "Only the organization's owners can stop or resume automatic reviews.")
		return
	}
	skip := r.FormValue("skip") == "1"
	if err := s.bot.SetSkip(pr.Repo, pr.Number, pr.AccountID, skip); err != nil {
		redirectErr(w, r, prPath(pr.Repo, pr.Number), err)
		return
	}
	msg := "Automatic reviews resumed."
	if skip {
		msg = "Automatic reviews disabled for this PR."
	}
	redirectMsg(w, r, prPath(pr.Repo, pr.Number), msg)
}

func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rv, err := s.store.GetReview(id)
	if err != nil || rv == nil || me(r).Role(rv.AccountID) == "" {
		s.notFound(w, r)
		return
	}
	org, _ := s.store.GetOrg(rv.AccountID)
	s.render(w, r, "review.html", page{Title: fmt.Sprintf("Review %d", id), Nav: "reviews", Org: org, Tab: "reviews", D: rv})
}

// reviewList renders the review history for one organization or all.
func (s *Server) reviewList(w http.ResponseWriter, r *http.Request, p page, accountID int64, base string) {
	q := r.URL.Query()
	pageNo, _ := strconv.Atoi(q.Get("page"))
	pageNo = max(1, pageNo)
	const perPage = 50
	f := store.ReviewFilter{AccountID: accountID, Repo: q.Get("repo"), Status: q.Get("status"), Limit: perPage, Offset: (pageNo - 1) * perPage}
	reviews, total, err := s.store.QueryReviews(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The filter offers the repositories and the names reviews are filed
	// under, so an earlier repository's reviews can be picked too.
	var repos []string
	seen := map[string]bool{}
	stored, _ := s.store.ListRepos(accountID)
	named, _ := s.store.ReviewRepoNames(accountID)
	for _, n := range named {
		seen[n] = true
	}
	for _, rp := range stored {
		if !seen[rp.FullName] {
			named = append(named, rp.FullName)
			seen[rp.FullName] = true
		}
	}
	repos = named
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i]) < strings.ToLower(repos[j]) })
	orgs := map[int64]string{}
	if accountID == store.AllAccounts {
		if all, err := s.store.ListOrgs(); err == nil {
			for _, o := range all {
				orgs[o.ID] = o.Login
			}
		}
	}
	p.D = map[string]any{
		"Reviews": reviews, "Total": total, "Page": pageNo, "Pages": (total + perPage - 1) / perPage,
		"Repo": f.Repo, "Status": f.Status, "Repos": repos, "Base": base, "OrgNames": orgs,
		"Statuses": []string{"posted", "error", "running", "skipped", "nothing to review"},
	}
	s.render(w, r, "reviews.html", p)
}

func (s *Server) reviewsPage(w http.ResponseWriter, r *http.Request) {
	s.reviewList(w, r, page{Title: "Reviews", Nav: "reviews"}, store.AllAccounts, "/reviews")
}

// ---------- repositories (administrator view) ----------

func (s *Server) reposPage(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepos(store.AllAccounts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "repos.html", page{Title: "Repositories", Nav: "repos", D: reposData(repos, "/repos", true)})
}

func reposData(repos []store.Repo, base string, canManage bool) map[string]any {
	enabled := 0
	for _, rp := range repos {
		if rp.Enabled {
			enabled++
		}
	}
	return map[string]any{"Repos": repos, "Enabled": enabled, "Base": base, "CanManage": canManage}
}

func (s *Server) reposSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	n, err := s.bot.SyncRepos(ctx)
	if err != nil {
		redirectErr(w, r, "/repos", err)
		return
	}
	s.bot.Wake()
	redirectMsg(w, r, "/repos", fmt.Sprintf("Found %d repositories.", n))
}

func (s *Server) reposAll(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("enabled") == "1"
	if err := s.store.SetAllReposEnabled(store.AllAccounts, on); err != nil {
		redirectErr(w, r, "/repos", err)
		return
	}
	s.bot.Wake()
	redirectMsg(w, r, "/repos", "Updated all repositories.")
}

func (s *Server) repoToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.SetRepoEnabled(id, r.FormValue("enabled") == "1"); err != nil {
		redirectErr(w, r, "/repos", err)
		return
	}
	s.bot.Wake()
	http.Redirect(w, r, "/repos", http.StatusSeeOther)
}

// ---------- settings ----------

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	st := s.bot.Settings()
	base := s.publicURL(r)
	s.render(w, r, "settings.html", page{Title: "Settings", Nav: "settings", D: map[string]any{
		"S": st, "P": st.Policy(), "HasKey": st.GitHub.PrivateKey != "", "HasAPIKey": st.LLM.APIKey != "",
		"HasClientSecret": st.GitHub.ClientSecret != "", "HasWebhookSecret": st.GitHub.WebhookSecret != "",
		"CallbackURL": base + "/auth/github/callback", "WebhookURL": base + "/webhooks/github",
		"Providers": settings.Providers, "Severities": settings.Severities,
		"SkillDir": s.bot.SkillDir(st), "Instance": true,
	}})
}

// parsePolicy merges the review policy fields of a form into cur.
func parsePolicy(r *http.Request, cur settings.Policy) settings.Policy {
	v := cur
	f := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	on := func(k string) bool { return r.FormValue(k) != "" }
	num := func(k string, def int) int {
		if n, err := strconv.Atoi(f(k)); err == nil {
			return n
		}
		return def
	}
	v.Review.Enabled = on("rv_enabled")
	v.Review.SkipDrafts = on("rv_skip_drafts")
	v.Review.SkipLabels = textutil.SplitList(r.FormValue("rv_skip_labels"))
	v.Review.IgnorePatterns = textutil.SplitList(r.FormValue("rv_ignore"))
	v.Review.GuidelineFiles = textutil.SplitList(r.FormValue("rv_guidelines"))
	v.Review.DebounceSeconds = num("rv_debounce", v.Review.DebounceSeconds)
	v.Review.MaxComments = num("rv_max_comments", v.Review.MaxComments)
	v.Review.CommentLevel = f("rv_comment_level")
	v.Review.ApprovalLevel = f("rv_approval_level")
	if l := f("rv_request_changes_level"); l != "" {
		v.Review.RequestChangesLevel = l
	}
	v.Review.MaxFiles = num("rv_max_files", v.Review.MaxFiles)
	v.Review.MaxRounds = num("rv_max_rounds", v.Review.MaxRounds)
	v.Review.ReviewExisting = on("rv_existing")

	v.Replies.Enabled = on("rp_enabled")
	v.Replies.Mentions = on("rp_mentions")
	v.Replies.MaxPerThread = num("rp_max", v.Replies.MaxPerThread)
	return v
}

// parseSettings merges the settings form into cur. Secrets left blank keep their saved value.
func parseSettings(r *http.Request, cur settings.Settings) settings.Settings {
	v := cur
	f := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	on := func(k string) bool { return r.FormValue(k) != "" }
	num := func(k string, def int) int {
		if n, err := strconv.Atoi(f(k)); err == nil {
			return n
		}
		return def
	}

	if id, err := strconv.ParseInt(f("gh_app_id"), 10, 64); err == nil {
		v.GitHub.AppID = id
	}
	if k := f("gh_private_key"); k != "" {
		v.GitHub.PrivateKey = k
	}
	v.GitHub.APIBaseURL = f("gh_api_base")
	v.GitHub.Accounts = textutil.SplitList(r.FormValue("gh_accounts"))
	v.GitHub.NewReposEnabled = on("gh_new_repos")
	v.GitHub.ClientID = f("gh_client_id")
	if k := f("gh_client_secret"); k != "" {
		v.GitHub.ClientSecret = k
	}
	if k := f("gh_webhook_secret"); k != "" {
		v.GitHub.WebhookSecret = k
	}
	if on("gh_webhook_secret_clear") {
		v.GitHub.WebhookSecret = ""
	}
	v.GitHub.PublicURL = f("gh_public_url")

	v.LLM.Provider = f("llm_provider")
	v.LLM.BaseURL = f("llm_base_url")
	if k := f("llm_api_key"); k != "" {
		v.LLM.APIKey = k
	}
	if on("llm_api_key_clear") {
		v.LLM.APIKey = ""
	}
	v.LLM.Model = f("llm_model")
	if t, err := strconv.ParseFloat(f("llm_temperature"), 64); err == nil {
		v.LLM.Temperature = t
	}
	v.LLM.ContextTokens = num("llm_context", v.LLM.ContextTokens)
	v.LLM.MaxOutputTokens = num("llm_max_output", v.LLM.MaxOutputTokens)
	v.LLM.StructuredOutput = on("llm_structured")
	v.LLM.TimeoutSeconds = num("llm_timeout", v.LLM.TimeoutSeconds)

	p := parsePolicy(r, v.Policy())
	v.Review, v.Replies = p.Review, p.Replies
	v.Review.SkillDir = f("rv_skill_dir")

	v.Poll.IntervalSeconds = num("poll_interval", v.Poll.IntervalSeconds)
	v.Poll.RepoRefreshMinutes = num("poll_refresh", v.Poll.RepoRefreshMinutes)
	v.Poll.WebhookIntervalSeconds = num("poll_webhook_interval", v.Poll.WebhookIntervalSeconds)
	v.Normalize()
	return v
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	v := parseSettings(r, s.bot.Settings())
	if err := settings.CheckPublicURL(v.GitHub.PublicURL); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	if err := settings.Save(s.store, v); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	s.bot.Wake()
	redirectMsg(w, r, "/settings", "Settings saved.")
}

// apiStatus reports the bot's status, without other organizations' repositories.
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusFor(me(r)))
}

// statusFor is the bot's status as v may see it: anyone but an administrator
// sees neither another organization's repository nor the instance's
// configuration problems and poll errors (which can name any repository).
func (s *Server) statusFor(v *viewer) bot.Status { return visibleStatus(s.bot.Status(), v) }

// visibleStatus is the part of the bot's status v may see. A running job is
// shown only to members of the account it was started for.
func visibleStatus(st bot.Status, v *viewer) bot.Status {
	if v != nil && v.Admin {
		return st
	}
	working := make([]bot.Activity, len(st.Working))
	for i, a := range st.Working {
		if a.Repo != "" && (v == nil || a.Account == 0 || v.Role(a.Account) == "") {
			a.What, a.Repo, a.Account = "a review for another organization", "", 0
		}
		working[i] = a
	}
	st.Working = working
	st.ConfigIssues, st.LastPollErr = nil, ""
	return st
}

func (s *Server) apiTestGitHub(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	msg, err := s.bot.TestGitHub(ctx, parseSettings(r, s.bot.Settings()))
	respond(w, msg, err, nil)
}

func (s *Server) apiTestLLM(w http.ResponseWriter, r *http.Request) {
	st := parseSettings(r, s.bot.Settings())
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(st.LLM.TimeoutSeconds)*time.Second)
	defer cancel()
	msg, err := s.bot.TestLLM(ctx, st)
	respond(w, msg, err, nil)
}

func (s *Server) apiModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := s.bot.Models(ctx, parseSettings(r, s.bot.Settings()))
	msg := fmt.Sprintf("%d models available.", len(models))
	if err == nil && len(models) == 0 {
		msg = "The server returned no models. Load or pull one first."
	}
	respond(w, msg, err, models)
}

func respond(w http.ResponseWriter, msg string, err error, models []string) {
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "models": models})
}

// ---------- skill ----------

func (s *Server) skillPage(w http.ResponseWriter, r *http.Request) {
	st := s.bot.Settings()
	dir := s.bot.SkillDir(st)
	sk, loadErr := skill.Load(dir)
	raw, _ := skill.ReadRaw(dir)
	if loadErr != nil {
		sk = skill.Default()
		raw = skill.DefaultText()
	}
	tokens, base := sk.Size()/3, sk.BaseSize()/3
	d := map[string]any{
		"Skill": sk, "Dir": dir, "Raw": raw, "Tokens": tokens, "BaseTokens": base, "Context": st.LLM.ContextTokens,
		"Percent": base * 100 / max(1, st.LLM.ContextTokens), "HasLang": base != tokens, "LoadErr": "",
	}
	if loadErr != nil {
		d["LoadErr"] = loadErr.Error()
	}
	s.render(w, r, "skill.html", page{Title: "Review skill", Nav: "skill", D: d})
}

func (s *Server) skillUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		redirectErr(w, r, "/skill", fmt.Errorf("upload failed: %w", err))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		redirectErr(w, r, "/skill", err)
		return
	}
	st := s.bot.Settings()
	dir := s.bot.SkillDir(st)
	// A Skill folder the user pointed at a git clone is theirs to manage
	// (updated with git pull): an upload must not replace it.
	clone := st.Review.SkillDir != "" && inGitCheckout(dir)
	note := ""
	switch ext := strings.ToLower(filepath.Ext(hdr.Filename)); {
	case ext == ".zip" && clone:
		def := st
		def.Review.SkillDir = ""
		dir = s.bot.SkillDir(def) // the bot's own <data>/skill folder
		if err = skill.InstallZip(data, dir); err == nil {
			note = fmt.Sprintf(" Settings → Skill folder pointed at a git clone (%s), which was left untouched; it now points at the uploaded skill.", st.Review.SkillDir)
			err = settings.Save(s.store, def)
		}
	case ext == ".zip":
		err = skill.InstallZip(data, dir)
	case (ext == ".md" || ext == ".markdown") && clone:
		err = fmt.Errorf("the skill folder is a git clone (%s): edit SKILL.md there, or upload a .zip of a whole skill to install it separately", dir)
	case ext == ".md" || ext == ".markdown":
		err = skill.WriteSkillFile(dir, string(data))
	default:
		err = errors.New("upload a .zip of the skill folder or a SKILL.md file")
	}
	if err != nil {
		redirectErr(w, r, "/skill", err)
		return
	}
	sk, err := skill.Load(dir)
	if err != nil {
		redirectErr(w, r, "/skill", fmt.Errorf("installed %s, but it doesn't load: %w", hdr.Filename, err))
		return
	}
	s.store.Log("info", "", 0, fmt.Sprintf("installed skill %q from %s into %s (%d reference files)", sk.Name, hdr.Filename, dir, len(sk.References)))
	redirectMsg(w, r, "/skill", fmt.Sprintf("Installed skill %q from %s into %s, with %d reference files. It applies from the next review.%s",
		sk.Name, hdr.Filename, dir, len(sk.References), note))
}

// inGitCheckout reports whether dir is inside a git working tree.
func inGitCheckout(dir string) bool {
	d, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

func (s *Server) skillSave(w http.ResponseWriter, r *http.Request) {
	content := r.FormValue("content")
	if strings.TrimSpace(content) == "" {
		redirectErr(w, r, "/skill", errors.New("SKILL.md can't be empty"))
		return
	}
	if err := skill.WriteSkillFile(s.bot.SkillDir(s.bot.Settings()), content); err != nil {
		redirectErr(w, r, "/skill", err)
		return
	}
	redirectMsg(w, r, "/skill", "SKILL.md saved.")
}

// ---------- activity ----------

func (s *Server) activityPage(w http.ResponseWriter, r *http.Request) {
	acts, _ := s.store.RecentActivity(store.AllAccounts, 300, "", 0)
	s.render(w, r, "activity.html", page{Title: "Activity", Nav: "activity", D: acts})
}
