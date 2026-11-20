package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// orgFor loads the organization in the URL if the viewer has access to it;
// otherwise it answers 404 and returns nil. With manage set, it also requires
// an owner (or administrator) and answers 403 to members.
func (s *Server) orgFor(w http.ResponseWriter, r *http.Request, manage bool) *store.Org {
	org, err := s.store.GetOrgByLogin(r.PathValue("org"))
	v := me(r)
	if err != nil || org == nil || v.Role(org.ID) == "" {
		s.notFound(w, r)
		return nil
	}
	if manage && !v.CanManage(org.ID) {
		s.forbidden(w, r, fmt.Sprintf("Only owners of %s can change this.", org.Login))
		return nil
	}
	return org
}

func orgBase(o *store.Org) string { return "/orgs/" + o.Login }

// orgsPage lists the organizations the viewer can see.
func (s *Server) orgsPage(w http.ResponseWriter, r *http.Request) {
	v := me(r)
	var only []int64
	if !v.Admin {
		only = []int64{}
		for _, m := range v.Orgs {
			only = append(only, m.OrgID)
		}
	}
	sums, err := s.store.OrgSummaries(only)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	roles := map[int64]string{}
	for _, sm := range sums {
		roles[sm.ID] = v.Role(sm.ID)
	}
	st := s.bot.Settings()
	s.render(w, r, "orgs.html", page{Title: "Organizations", Nav: "orgs", D: map[string]any{
		"Orgs": sums, "Roles": roles, "InstallURL": s.bot.InstallURL(), "SignIn": st.GitHub.SignInOn(),
	}})
}

// statsRanges are the periods the overview can show, in days.
var statsRanges = []int{7, 30, 90}

func (s *Server) orgOverview(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil {
		for _, ok := range statsRanges {
			if d == ok {
				days = d
			}
		}
	}
	now := time.Now()
	since := store.DayWindowStart(days, now) // the chart's first day, so totals match it
	stats, _ := s.store.ReviewStatsSince(org.ID, since)
	perDay, _ := s.store.ReviewsPerDay(org.ID, days, now)
	repoStats, _ := s.store.RepoStatsSince(org.ID, since, 15)
	reviews, _ := s.store.RecentReviews(org.ID, 10)
	counts, _ := s.store.CountPRsByStatus(org.ID)
	acts, _ := s.store.RecentActivity(org.ID, 10, "", 0)
	repos, _ := s.store.ListRepos(org.ID)
	enabled := 0
	for _, rp := range repos {
		if rp.Enabled {
			enabled++
		}
	}
	_, custom := settings.ParsePolicy(org.Policy)
	s.render(w, r, "org.html", page{Title: org.Login, Nav: "orgs", Org: org, Tab: "overview", D: map[string]any{
		"Stats": stats, "Days": perDay, "Range": days, "Ranges": statsRanges, "RepoStats": repoStats,
		"Reviews": reviews, "Counts": counts, "Activity": acts, "Repos": len(repos), "Enabled": enabled,
		"CustomPolicy": custom,
	}})
}

func (s *Server) orgPRs(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	all := r.URL.Query().Get("all") == "1"
	prs, err := s.store.ListPRs(org.ID, all, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "prs.html", page{Title: org.Login + " · Pull requests", Nav: "orgs", Org: org, Tab: "prs",
		D: map[string]any{"PRs": prs, "All": all, "Base": orgBase(org) + "/prs"}})
}

func (s *Server) orgReviews(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	s.reviewList(w, r, page{Title: org.Login + " · Reviews", Nav: "orgs", Org: org, Tab: "reviews"}, org.ID, orgBase(org)+"/reviews")
}

func (s *Server) orgActivity(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	acts, _ := s.store.RecentActivity(org.ID, 300, "", 0)
	s.render(w, r, "activity.html", page{Title: org.Login + " · Activity", Nav: "orgs", Org: org, Tab: "activity", D: acts})
}

// ---------- repositories ----------

func (s *Server) orgRepos(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	repos, err := s.store.ListRepos(org.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := reposData(repos, orgBase(org)+"/repos", me(r).CanManage(org.ID))
	s.render(w, r, "repos.html", page{Title: org.Login + " · Repositories", Nav: "orgs", Org: org, Tab: "repos", D: d})
}

func (s *Server) orgReposAll(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, true)
	if org == nil {
		return
	}
	if err := s.store.SetAllReposEnabled(org.ID, r.FormValue("enabled") == "1"); err != nil {
		redirectErr(w, r, orgBase(org)+"/repos", err)
		return
	}
	s.bot.Wake()
	redirectMsg(w, r, orgBase(org)+"/repos", "Updated all repositories.")
}

func (s *Server) orgRepoToggle(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, true)
	if org == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rp, err := s.store.GetRepo(id)
	if err != nil || rp == nil || rp.AccountID != org.ID {
		s.notFound(w, r)
		return
	}
	if err := s.store.SetRepoEnabled(id, r.FormValue("enabled") == "1"); err != nil {
		redirectErr(w, r, orgBase(org)+"/repos", err)
		return
	}
	s.bot.Wake()
	http.Redirect(w, r, orgBase(org)+"/repos", http.StatusSeeOther)
}

// ---------- members ----------

func (s *Server) orgMembers(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	members, err := s.store.ListMembers(org.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "members.html", page{Title: org.Login + " · Members", Nav: "orgs", Org: org, Tab: "members", D: map[string]any{
		"Members": members, "Me": me(r).ID,
	}})
}

func (s *Server) orgMemberAdd(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, true)
	if org == nil {
		return
	}
	back := orgBase(org) + "/members"
	login := store.NormLogin(r.FormValue("login"))
	role := r.FormValue("role")
	if role != store.RoleOwner {
		role = store.RoleMember
	}
	if login == "" {
		redirectErr(w, r, back, errors.New("enter a GitHub username"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	gu, err := s.bot.LookupUser(ctx, org.InstallationID, login)
	switch {
	case github.IsStatus(err, http.StatusNotFound):
		redirectErr(w, r, back, fmt.Errorf("there's no GitHub user called %q", login))
		return
	case err != nil:
		redirectErr(w, r, back, fmt.Errorf("couldn't look up %q on GitHub: %w", login, err))
		return
	case gu.Type != "" && gu.Type != "User":
		redirectErr(w, r, back, fmt.Errorf("%s is a GitHub %s, not a person", gu.Login, gu.Type))
		return
	}
	// Adding never changes an existing member's role, even one added by a
	// request racing this one: role changes go through the list, which keeps
	// at least one owner.
	added := false
	err = s.store.EnsureUser(gu.ID, gu.Login)
	if err == nil {
		added, err = s.store.AddMember(org.ID, gu.ID, role)
	}
	if err == nil && !added {
		err = fmt.Errorf("@%s already has access; change their role in the list", gu.Login)
	}
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("%s added @%s to %s as %s", me(r).Display(), gu.Login, org.Login, role))
	redirectMsg(w, r, back, fmt.Sprintf("Added @%s as %s. They can now sign in with GitHub.", gu.Login, role))
}

// memberFor loads the member in the URL for an owner-only change.
func (s *Server) memberFor(w http.ResponseWriter, r *http.Request) (*store.Org, *store.Member) {
	org := s.orgFor(w, r, true)
	if org == nil {
		return nil, nil
	}
	uid, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil || uid <= 0 {
		s.notFound(w, r)
		return nil, nil
	}
	m, err := s.store.GetMembership(org.ID, uid)
	if err != nil || m == nil {
		s.notFound(w, r)
		return nil, nil
	}
	return org, m
}

func (s *Server) orgMemberRole(w http.ResponseWriter, r *http.Request) {
	org, m := s.memberFor(w, r)
	if m == nil {
		return
	}
	back := orgBase(org) + "/members"
	role := r.FormValue("role")
	if role != store.RoleOwner && role != store.RoleMember {
		redirectErr(w, r, back, errors.New("unknown role"))
		return
	}
	// A role set here is a deliberate choice, so sign-in won't change it.
	if err := s.store.SetMemberRole(org.ID, m.ID, role); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("%s made @%s %s of %s", me(r).Display(), m.Login, role, org.Login))
	redirectMsg(w, r, back, fmt.Sprintf("@%s is now %s.", m.Login, role))
}

func (s *Server) orgMemberRemove(w http.ResponseWriter, r *http.Request) {
	org, m := s.memberFor(w, r)
	if m == nil {
		return
	}
	back := orgBase(org) + "/members"
	if err := s.store.RemoveMember(org.ID, m.ID); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("%s removed @%s from %s", me(r).Display(), m.Login, org.Login))
	msg := fmt.Sprintf("Removed @%s.", m.Login)
	if m.Source == store.SourceGitHub {
		msg += " They'll get access again the next time they sign in while they're a GitHub admin" +
			map[bool]string{true: " or member", false: ""}[org.AutoJoin] + " of " + org.Login + "."
	}
	redirectMsg(w, r, back, msg)
}

func (s *Server) orgAutoJoin(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, true)
	if org == nil {
		return
	}
	on := r.FormValue("on") == "1"
	if err := s.store.SetOrgAutoJoin(org.ID, on); err != nil {
		redirectErr(w, r, orgBase(org)+"/members", err)
		return
	}
	msg := "Members of the GitHub organization now get access when they sign in."
	if !on {
		msg = "Only people added here, and the GitHub organization's admins, get access."
	}
	redirectMsg(w, r, orgBase(org)+"/members", msg)
}

// ---------- review policy ----------

func (s *Server) orgSettings(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	base := s.bot.Settings()
	p, custom := settings.ParsePolicy(org.Policy)
	if custom {
		p = base.WithPolicy(p).Policy()
	} else {
		p = base.Policy()
	}
	s.render(w, r, "org_settings.html", page{Title: org.Login + " · Settings", Nav: "orgs", Org: org, Tab: "settings", D: map[string]any{
		"P": p, "Custom": custom, "Severities": settings.Severities,
		"ReadOnly": !me(r).CanManage(org.ID),
	}})
}

func (s *Server) orgSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, true)
	if org == nil {
		return
	}
	back := orgBase(org) + "/settings"
	if r.FormValue("inherit") == "1" {
		if err := s.store.SetOrgPolicy(org.ID, ""); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("%s reset %s's review settings to the instance defaults", me(r).Display(), org.Login))
		s.bot.Wake()
		redirectMsg(w, r, back, "Now using the instance's default review settings.")
		return
	}
	base := s.bot.Settings()
	cur, ok := settings.ParsePolicy(org.Policy)
	if !ok {
		cur = base.Policy()
	}
	raw, err := settings.EncodePolicy(base, parsePolicy(r, cur))
	if err == nil {
		err = s.store.SetOrgPolicy(org.ID, raw)
	}
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.store.LogFor(org.ID, "info", "", 0, fmt.Sprintf("%s changed %s's review settings", me(r).Display(), org.Login))
	s.bot.Wake()
	redirectMsg(w, r, back, "Review settings saved. They apply from the next review.")
}
