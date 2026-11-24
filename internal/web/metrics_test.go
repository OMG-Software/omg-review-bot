package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// TestMetricsPages: an organization's members see its metrics (and only
// its own); every organization's together are for administrators.
func TestMetricsPages(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	for _, c := range []store.ModelCall{
		// A name that contains the model name: members see neither.
		{AccountID: acmeID, ReviewID: 1, Kind: store.CallReview, ConfigID: "m1", Label: "Production shared-7b", Model: "shared-7b", OK: true, PromptTokens: 900},
		{AccountID: otherID, ReviewID: 2, Kind: store.CallReview, ModelSet: otherID, ConfigID: "m1", Label: "Secret own model", Model: "secret-70b", OK: true},
		// An instance model with no name of its own is labelled with its
		// model name: members see neither.
		{AccountID: acmeID, ReviewID: 1, Kind: store.CallReview, ConfigID: "m2", Label: "plain-13b", Model: "plain-13b", OK: true},
	} {
		if err := s.st.RecordModelCall(c); err != nil {
			t.Fatal(err)
		}
	}
	// A repository whose only review failed still shows, with its failure.
	id, err := s.st.CreateReview(&store.Review{Repo: "acme/broken", Number: 1, AccountID: acmeID, Status: "running", StartedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.UpdateReview(&store.Review{ID: id, Repo: "acme/broken", Number: 1, Status: "error", FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	s.signIn() // alice: an owner of acme only

	code, body, _ := s.get("/orgs/acme/metrics?days=90")
	if code != http.StatusOK {
		t.Fatalf("acme's metrics = %d", code)
	}
	for _, want := range []string{"Most active repositories", "acme/web", "Findings by repository and level", ">Instance model", "Removed instance model", // m1 is configured, m2 isn't
		"Reviews sent to each model", "Time to first review", `class="active">90 days`, "acme/broken"} {
		if !strings.Contains(body, want) {
			t.Errorf("acme's metrics lack %q", want)
		}
	}
	// The page says every chart has a table: the day, level, per-period and
	// findings-by-repository charts carry one each (the rest sit above
	// tables of exactly their rows).
	if n := strings.Count(body, `class="table-view"`); n != 4 {
		t.Errorf("acme's metrics have %d table views, want 4", n)
	}
	// Another organization's repositories and models stay out of it, and an
	// instance model's server isn't named to members.
	for _, leak := range []string{"other/secret", "Secret own model", "secret-70b", "shared-7b", "Production", "plain-13b"} {
		if strings.Contains(body, leak) {
			t.Errorf("acme's metrics show %q", leak)
		}
	}
	if code, _, _ := s.get("/orgs/other/metrics"); code != http.StatusNotFound {
		t.Errorf("another organization's metrics = %d, want 404", code)
	}
	if code, _, _ := s.get("/metrics"); code != http.StatusForbidden {
		t.Errorf("every organization's metrics for an owner = %d, want 403", code)
	}
	if _, body, _ := s.get("/orgs/acme"); !strings.Contains(body, "Most active repositories") {
		t.Error("the overview has no chart of the most active repositories")
	}

	if err := s.st.SetUserAdmin(aliceID, true); err != nil {
		t.Fatal(err)
	}
	code, body, _ = s.get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("every organization's metrics for an administrator = %d", code)
	}
	for _, want := range []string{"acme/web", "other/secret", "Findings by organization and level", "Secret own model",
		"secret-70b", "other's own", "plain-13b", "Production shared-7b", "/orgs/acme/reviews?repo="} {
		if !strings.Contains(body, want) {
			t.Errorf("the administrators' metrics lack %q", want)
		}
	}
	if _, body, _ := s.get("/"); !strings.Contains(body, "Most active repositories") || !strings.Contains(body, `href="/metrics"`) {
		t.Error("the dashboard has no chart of the most active repositories, or no link to the metrics")
	}
}

// TestHideInstanceModelNames: an instance model's number is its place in the
// configured list, the same whichever models a period used; a removed one,
// or an earlier model of a configuration, says so; own models keep names.
func TestHideInstanceModelNames(t *testing.T) {
	cfgs := []settings.LLM{{ID: "a", Model: "small"}, {ID: "b", Model: "mid"}, {ID: "c", Model: "large"}}
	names := func(ms []store.ModelUsage) []string {
		hideInstanceModelNames(ms, cfgs)
		var out []string
		for _, m := range ms {
			if m.Model != "" && m.ModelSet == 0 {
				t.Errorf("%s still names its model %q", m.Label, m.Model)
			}
			out = append(out, m.Label)
		}
		return out
	}
	// Seven days: "b" unused. Ninety: all three, plus history.
	week := names([]store.ModelUsage{{ConfigID: "c", Label: "Large one", Model: "large"}, {ConfigID: "a", Label: "small", Model: "small"}})
	quarter := names([]store.ModelUsage{
		{ConfigID: "c", Label: "Large one", Model: "large"}, {ConfigID: "b", Label: "mid", Model: "mid"},
		{ConfigID: "a", Label: "small", Model: "small"}, {ConfigID: "a", Label: "tiny", Model: "tiny"},
		{ConfigID: "z", Label: "gone", Model: "gone"}, {ModelSet: 5, ConfigID: "a", Label: "Ours", Model: "ours-7b"},
	})
	if fmt.Sprint(week) != "[Instance model 3 Instance model 1]" {
		t.Errorf("7 days = %v", week)
	}
	want := "[Instance model 3 Instance model 2 Instance model 1 Instance model 1 (earlier model) Removed instance model Ours]"
	if fmt.Sprint(quarter) != want {
		t.Errorf("90 days = %v, want %s", quarter, want)
	}
}

// TestRepoChartCaptions: a repository chart cut to its top rows says so,
// and the table's hint doesn't claim the charts cover every repository.
func TestRepoChartCaptions(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	now := time.Now().Unix()
	for i := range repoChartRows + 2 {
		repo := fmt.Sprintf("acme/r%02d", i)
		id, err := s.st.CreateReview(&store.Review{Repo: repo, Number: 1, AccountID: acmeID, Status: "running", StartedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.st.UpdateReview(&store.Review{ID: id, Repo: repo, Number: 1, Status: "posted", Comments: 1, Sev: [5]int{0, 0, 1, 0, 0}, FinishedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	s.signIn()
	code, body, _ := s.get("/orgs/acme/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics = %d", code)
	}
	top := fmt.Sprintf("the top %d of", repoChartRows)
	if strings.Count(body, top) != 2 || strings.Contains(body, "cover them all") {
		t.Errorf("captions: %d with %q, want 2 (one per chart), and no claim the charts cover all", strings.Count(body, top), top)
	}
}

// TestOrgSevRows: the organizations' chart shows the ones with the most
// findings, most first, at most the cap.
func TestOrgSevRows(t *testing.T) {
	var orgs []store.OrgMetrics
	for i := range repoChartRows + 3 {
		orgs = append(orgs, store.OrgMetrics{Login: fmt.Sprintf("org%02d", i), Findings: i})
	}
	if rows := orgSevRows(orgs, repoChartRows); len(rows) != repoChartRows || rows[0].Label != fmt.Sprintf("org%02d", repoChartRows+2) {
		t.Errorf("organizations' chart: %d rows, first %q; want the %d with most findings, most first", len(rows), rows[0].Label, repoChartRows)
	}
}

// TestRepoNames: across organizations, a repository on two accounts' rows,
// or not under its organization's name, says whose row it is.
func TestRepoNames(t *testing.T) {
	names := repoNames([]store.RepoStats{
		{Org: "acme", Repo: "acme/web"},
		{Org: "acme", Repo: "acme/api"},
		{Org: "Acme", Repo: "acme/api"}, // installed again: a new account
		{Org: "globex", Repo: "acme/moved"},
		{Org: "", Repo: "x/y"},
	})
	want := []string{"acme/web", "acme/api (acme)", "acme/api (Acme)", "acme/moved (globex)", "x/y (no organization)"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("name %d = %q, want %q", i, names[i], w)
		}
	}
	if segClass(0, 3.9) != "viz-seg viz-s1 short" || segClass(4, 4) != "viz-seg viz-s5" {
		t.Errorf("segment classes: %q %q", segClass(0, 3.9), segClass(4, 4))
	}
}

// TestByWeek: a long period is charted by week, the last week ending today
// and a part week, if any, first.
func TestByWeek(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	days := make([]store.DayStats, 10)
	for i := range days {
		days[i] = store.DayStats{Day: start.AddDate(0, 0, i), Posted: 1, Sev: [5]int{i, 0, 0, 0, 1}}
	}
	w := byWeek(days)
	if len(w) != 2 || w[0].Span != 3 || w[1].Span != 7 || !w[0].Day.Equal(start) || !w[1].Day.Equal(start.AddDate(0, 0, 3)) {
		t.Fatalf("weeks = %+v", w)
	}
	if w[0].Posted != 3 || w[1].Posted != 7 || w[0].Sev != [5]int{0 + 1 + 2, 0, 0, 0, 3} || w[1].Sev[4] != 7 {
		t.Errorf("weeks' sums = %+v", w)
	}
	if periodUnit(w) != "week" || periodUnit(days) != "day" || periodLabel(w[1]) != "Week of Sun 4 Jan" {
		t.Errorf("labels: %q %q %q", periodUnit(w), periodUnit(days), periodLabel(w[1]))
	}
	// A year is 52 weeks and a day: the part week first is a day long, and
	// the chart is still by week.
	if y := byWeek(make([]store.DayStats, 365)); len(y) != 53 || y[0].Span != 1 || periodUnit(y) != "week" {
		t.Errorf("a year's weeks: %d, first %d days, unit %q", len(y), y[0].Span, periodUnit(y))
	}
}
