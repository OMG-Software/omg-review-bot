package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/skill"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// ---------- auth pages ----------

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
	h, err := hashPassword(pw)
	if err == nil {
		err = s.store.Set(pwKey, h)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.issueSession(w, r)
	redirectMsg(w, r, "/settings", "Admin password set. Next: connect your GitHub App and model.")
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.validSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", page{Title: "Sign in"})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	stored, _, _ := s.store.Get(pwKey)
	if !checkPassword(stored, r.FormValue("password")) {
		time.Sleep(700 * time.Millisecond) // slow down guessing
		s.render(w, r, "login.html", page{Title: "Sign in", Err: "Wrong password."})
		return
	}
	s.issueSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "account.html", page{Title: "Account", Nav: "account"})
}

func (s *Server) accountSubmit(w http.ResponseWriter, r *http.Request) {
	stored, _, _ := s.store.Get(pwKey)
	if !checkPassword(stored, r.FormValue("current")) {
		s.render(w, r, "account.html", page{Title: "Account", Nav: "account", Err: "Current password is wrong."})
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 || pw != r.FormValue("confirm") {
		s.render(w, r, "account.html", page{Title: "Account", Nav: "account", Err: "New passwords must match and be at least 10 characters."})
		return
	}
	h, err := hashPassword(pw)
	if err == nil {
		err = s.store.Set(pwKey, h)
	}
	if err == nil {
		err = s.rotateSecret()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.issueSession(w, r)
	redirectMsg(w, r, "/account", "Password changed. Other sessions have been signed out.")
}

// ---------- dashboard, PRs, reviews ----------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	reviews, _ := s.store.RecentReviews(10)
	acts, _ := s.store.RecentActivity(15, "", 0)
	counts, _ := s.store.CountPRsByStatus()
	s.render(w, r, "dashboard.html", page{Title: "Dashboard", Nav: "dash", D: map[string]any{
		"Reviews": reviews, "Activity": acts, "Counts": counts,
	}})
}

func (s *Server) prsPage(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	prs, err := s.store.ListPRs(all, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "prs.html", page{Title: "Pull requests", Nav: "prs", D: map[string]any{"PRs": prs, "All": all}})
}

func prKey(r *http.Request) (string, int, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return "", 0, errors.New("bad PR number")
	}
	return r.PathValue("owner") + "/" + r.PathValue("repo"), n, nil
}

func prPath(repo string, n int) string { return fmt.Sprintf("/prs/%s/%d", repo, n) }

func (s *Server) prPage(w http.ResponseWriter, r *http.Request) {
	repo, n, err := prKey(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pr, err := s.store.GetPR(repo, n)
	if err != nil || pr == nil {
		http.NotFound(w, r)
		return
	}
	reviews, _ := s.store.ListReviews(repo, n, 50)
	acts, _ := s.store.RecentActivity(50, repo, n)
	s.render(w, r, "pr.html", page{Title: fmt.Sprintf("%s#%d", repo, n), Nav: "prs", D: map[string]any{
		"PR": pr, "Reviews": reviews, "Activity": acts,
	}})
}

func (s *Server) prReview(w http.ResponseWriter, r *http.Request) {
	repo, n, err := prKey(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.bot.ReviewNow(repo, n); err != nil {
		redirectErr(w, r, prPath(repo, n), err)
		return
	}
	redirectMsg(w, r, prPath(repo, n), "Review queued.")
}

func (s *Server) prReplies(w http.ResponseWriter, r *http.Request) {
	repo, n, err := prKey(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.bot.CheckReplies(repo, n)
	redirectMsg(w, r, prPath(repo, n), "Checking for replies and mentions.")
}

func (s *Server) prSkip(w http.ResponseWriter, r *http.Request) {
	repo, n, err := prKey(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	skip := r.FormValue("skip") == "1"
	if err := s.bot.SetSkip(repo, n, skip); err != nil {
		redirectErr(w, r, prPath(repo, n), err)
		return
	}
	msg := "Automatic reviews resumed."
	if skip {
		msg = "Automatic reviews disabled for this PR."
	}
	redirectMsg(w, r, prPath(repo, n), msg)
}

func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rv, err := s.store.GetReview(id)
	if err != nil || rv == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "review.html", page{Title: fmt.Sprintf("Review %d", id), Nav: "prs", D: rv})
}

// ---------- repos ----------

func (s *Server) reposPage(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepos()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	enabled := 0
	for _, rp := range repos {
		if rp.Enabled {
			enabled++
		}
	}
	s.render(w, r, "repos.html", page{Title: "Repositories", Nav: "repos", D: map[string]any{"Repos": repos, "Enabled": enabled}})
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
	if err := s.store.SetAllReposEnabled(on); err != nil {
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
	s.render(w, r, "settings.html", page{Title: "Settings", Nav: "settings", D: map[string]any{
		"S": st, "HasKey": st.GitHub.PrivateKey != "", "HasAPIKey": st.LLM.APIKey != "",
		"Providers": settings.Providers, "Severities": settings.Severities,
		"SkillDir": s.bot.SkillDir(st),
	}})
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

	v.Review.Enabled = on("rv_enabled")
	v.Review.SkillDir = f("rv_skill_dir")
	v.Review.SkipDrafts = on("rv_skip_drafts")
	v.Review.SkipLabels = textutil.SplitList(r.FormValue("rv_skip_labels"))
	v.Review.IgnorePatterns = textutil.SplitList(r.FormValue("rv_ignore"))
	v.Review.DebounceSeconds = num("rv_debounce", v.Review.DebounceSeconds)
	v.Review.MaxComments = num("rv_max_comments", v.Review.MaxComments)
	v.Review.MinSeverity = f("rv_min_severity")
	v.Review.MaxFiles = num("rv_max_files", v.Review.MaxFiles)
	v.Review.ReviewExisting = on("rv_existing")

	v.Replies.Enabled = on("rp_enabled")
	v.Replies.Mentions = on("rp_mentions")
	v.Replies.MaxPerThread = num("rp_max", v.Replies.MaxPerThread)

	v.Poll.IntervalSeconds = num("poll_interval", v.Poll.IntervalSeconds)
	v.Poll.RepoRefreshMinutes = num("poll_refresh", v.Poll.RepoRefreshMinutes)
	v.Normalize()
	return v
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	v := parseSettings(r, s.bot.Settings())
	if err := settings.Save(s.store, v); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	s.bot.Wake()
	redirectMsg(w, r, "/settings", "Settings saved.")
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.bot.Status())
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
	dir := s.bot.SkillDir(s.bot.Settings())
	switch strings.ToLower(filepath.Ext(hdr.Filename)) {
	case ".zip":
		err = skill.InstallZip(data, dir)
	case ".md", ".markdown":
		err = skill.WriteSkillFile(dir, string(data))
	default:
		err = errors.New("upload a .zip of the skill folder or a SKILL.md file")
	}
	if err != nil {
		redirectErr(w, r, "/skill", err)
		return
	}
	redirectMsg(w, r, "/skill", "Skill installed from "+hdr.Filename+".")
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
	acts, _ := s.store.RecentActivity(300, "", 0)
	s.render(w, r, "activity.html", page{Title: "Activity", Nav: "activity", D: acts})
}
