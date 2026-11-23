package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// The metrics pages show what the bot has done and what it has caught: an
// organization's (its members and owners, /orgs/{org}/metrics) and every
// organization's together (administrators, /metrics).

// metricsRanges are the periods the metrics pages can show, in days.
var metricsRanges = []int{7, 30, 90, 365}

// rangeDays reads the ?days= period, one of ranges, else def.
func rangeDays(r *http.Request, ranges []int, def int) int {
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil {
		for _, ok := range ranges {
			if d == ok {
				return d
			}
		}
	}
	return def
}

// weeklyAfter is the longest period charted by day; longer ones are
// charted by week, as a year of daily columns would be a pixel wide.
const weeklyAfter = 120

// byWeek sums days up into weeks, the last ending with the last day.
func byWeek(days []store.DayStats) []store.DayStats {
	var out []store.DayStats
	for end := len(days); end > 0; end -= 7 {
		start := max(0, end-7)
		w := store.DayStats{Day: days[start].Day, Span: end - start}
		for _, d := range days[start:end] {
			w.Posted += d.Posted
			w.Errors += d.Errors
			w.Findings += d.Findings
			w.Serious += d.Serious
			w.Fixed += d.Fixed
			for i := range w.Sev {
				w.Sev[i] += d.Sev[i]
			}
		}
		out = append(out, w)
	}
	slices.Reverse(out)
	return out
}

// repoTableRows is how many repositories the metrics table lists, the most
// reviewed first.
const repoTableRows = 50

// hideInstanceModelNames keeps the instance's models from an organization's
// members: each is shown as "Instance model" (numbered, when there are
// several), as its label is its model name when it has no name of its own,
// and a name its administrators gave it can contain the model name too.
func hideInstanceModelNames(ms []store.ModelUsage) {
	var unnamed []int
	for i, m := range ms {
		if m.ModelSet == 0 {
			unnamed = append(unnamed, i)
		}
	}
	// Numbered in configuration order, not by use, so a model keeps its
	// number as the ranking changes.
	slices.SortFunc(unnamed, func(a, b int) int {
		if c := strings.Compare(ms[a].ConfigID, ms[b].ConfigID); c != 0 {
			return c
		}
		return strings.Compare(ms[a].Model, ms[b].Model)
	})
	for n, i := range unnamed {
		ms[i].Label, ms[i].Model = "Instance model", ""
		if len(unnamed) > 1 {
			ms[i].Label += " " + strconv.Itoa(n+1)
		}
	}
}

// modelTotals sums up every model's calls.
type modelTotals struct {
	Calls, Failed                  int
	PromptTokens, CompletionTokens int64
}

// FailRate is the share of calls that failed, in percent.
func (t modelTotals) FailRate() int {
	if t.Calls == 0 {
		return 0
	}
	return t.Failed * 100 / t.Calls
}

// metricsData gathers one organization's metrics (or every organization's,
// with AllAccounts) over the last days days. A part that can't be read is
// logged and left empty, so the rest still shows.
func (s *Server) metricsData(accountID int64, days int) map[string]any {
	now := time.Now()
	since := store.DayWindowStart(days, now) // the charts' first day, so totals match them
	all := accountID == store.AllAccounts
	warn := func(what string, err error) {
		if err != nil {
			s.log.Warn("metrics", "part", what, "account", accountID, "err", err)
		}
	}
	stats, err := s.store.ReviewStatsSince(accountID, since)
	warn("reviews", err)
	perDay, err := s.store.ReviewsPerDay(accountID, days, now)
	warn("days", err)
	if days > weeklyAfter {
		perDay = byWeek(perDay)
	}
	// Every repository, so each chart ranks over all of them; the table
	// shows the most reviewed.
	repos, err := s.store.RepoStatsSince(accountID, since, -1)
	warn("repositories", err)
	models, err := s.store.ModelUsageSince(accountID, since)
	warn("models", err)
	if !all {
		hideInstanceModelNames(models)
	}
	replies, err := s.store.RepliesSince(accountID, since)
	warn("replies", err)
	latency, err := s.store.FirstReviewLatency(accountID, since)
	warn("latency", err)
	var tot modelTotals
	for _, m := range models {
		tot.Calls += m.Calls
		tot.Failed += m.Failed
		tot.PromptTokens += m.PromptTokens
		tot.CompletionTokens += m.CompletionTokens
	}
	d := map[string]any{
		"Range": days, "Ranges": metricsRanges, "All": all,
		"Stats": stats, "Days": perDay, "Repos": repos[:min(len(repos), repoTableRows)], "RepoCount": len(repos),
		"RepoTableRows": repoTableRows, "Models": models, "ModelTotals": tot,
		"Replies": replies, "Latency": latency,
		"Commented":      max(0, stats.Posted-stats.Approvals-stats.ChangesRequested),
		"RepoReviewRows": repoReviewRows(repos, 15), "RepoSevRows": repoSevRows(repos, 15),
		"ModelRows": modelRows(models, all),
	}
	if all {
		orgs, err := s.store.OrgMetricsSince(since)
		warn("organizations", err)
		d["Orgs"], d["OrgSevRows"] = orgs, orgSevRows(orgs)
	}
	return d
}

// orgMetrics is an organization's metrics page, for anyone who can see the
// organization.
func (s *Server) orgMetrics(w http.ResponseWriter, r *http.Request) {
	org := s.orgFor(w, r, false)
	if org == nil {
		return
	}
	d := s.metricsData(org.ID, rangeDays(r, metricsRanges, 30))
	d["Base"] = orgBase(org)
	s.render(w, r, "metrics.html", page{Title: org.Login + " · Metrics", Nav: "orgs", Org: org, Tab: "metrics", D: d})
}

// adminMetrics is every organization's metrics together (routed adminOnly).
func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	d := s.metricsData(store.AllAccounts, rangeDays(r, metricsRanges, 30))
	d["Base"] = ""
	s.render(w, r, "metrics.html", page{Title: "Metrics", Nav: "metrics", D: d})
}
